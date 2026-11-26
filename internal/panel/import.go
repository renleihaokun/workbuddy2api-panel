package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// cockpitAccount 映射 cockpit tools 导出格式的单个账号。
type cockpitAccount struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	UID           string `json:"uid"`
	Nickname      string `json:"nickname"`
	AccessToken   string `json:"access_token"`
	RefreshToken  string `json:"refresh_token"`
	TokenType     string `json:"token_type"`
	ExpiresAt     int64  `json:"expires_at"`
	Domain        string `json:"domain"`
	DosageNotify  string `json:"dosage_notify_code"`
	PaymentType   string `json:"payment_type"`
	Status        string `json:"status"`
	UsageUpdatedAt int64 `json:"usage_updated_at"`
	LastCheckin   int64  `json:"last_checkin_time"`
	CheckinStreak int    `json:"checkin_streak"`
	CreatedAt     int64  `json:"created_at"`
	LastUsed      int64  `json:"last_used"`
}

// importCockpit 接收 cockpit tools 导出的 JSON 文件，批量导入账号到池中。
//
//	POST /panel/api/import/cockpit
//	Content-Type: multipart/form-data
//	Body: file=<json>
//
// 返回 {ok, total, imported, skipped, errors}。
func (p *Panel) importCockpit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "parse form: "+err.Error())
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "missing file field: "+err.Error())
		return
	}
	defer file.Close()

	raw, err := io.ReadAll(file)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read file: "+err.Error())
		return
	}

	var accounts []cockpitAccount
	if err := json.Unmarshal(raw, &accounts); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if len(accounts) == 0 {
		writeErr(w, http.StatusBadRequest, "empty accounts array")
		return
	}

	var total, imported, overwritten, skipped int
	var errs []string
	// overwrite=1 才允许覆盖已存在的账号。默认跳过（覆盖会把好号写成死号：
	// 导出文件里的 RT 往往早已被轮换，直接盖上去 = 本来能用的号变成要重登）。
	overwrite := r.FormValue("overwrite") == "1"

	for _, acc := range accounts {
		uid := strings.TrimSpace(acc.UID)
		at := strings.TrimSpace(acc.AccessToken)
		rt := strings.TrimSpace(acc.RefreshToken)
		if uid == "" || at == "" || rt == "" {
			skipped++
			errs = append(errs, fmt.Sprintf("missing required fields (id=%s)", acc.ID))
			continue
		}
		if !validImportUID(uid) {
			skipped++
			errs = append(errs, fmt.Sprintf("invalid uid (id=%s)", acc.ID))
			continue
		}
		// 已存在（池里或 auths 目录里有同名文件）且未显式 overwrite → 跳过。
		existingFile := p.authFileForUID(uid)
		_, inPool := p.cfg.Pool.Status(uid)
		fileExists := false
		if _, err := os.Stat(existingFile); err == nil {
			fileExists = true
		}
		replacing := inPool || fileExists
		if replacing && !overwrite {
			skipped++
			errs = append(errs, fmt.Sprintf("uid=%s 已存在，已跳过（覆盖会替换现有凭证；确需覆盖请加 overwrite=1）", uid))
			continue
		}

		// 按 domain 推断 realm：workbuddy.ai 家族 → global，否则 cn。
		realm := auth.ResolveRealm("", acc.Domain)

		// cockpit tools 的 expires_at 为毫秒时间戳，转为秒。
		expiresAt := acc.ExpiresAt / 1000
		if expiresAt <= 0 {
			expiresAt = time.Now().Add(365 * 24 * time.Hour).Unix()
		}

		nickname := acc.Nickname
		if strings.TrimSpace(nickname) == "" {
			nickname = acc.Email
		}

		a := &auth.Auth{
			AccessToken:  at,
			RefreshToken: rt,
			ExpiresAt:    expiresAt,
			Domain:       acc.Domain,
			UID:          uid,
			Nickname:     nickname,
			// 就地写已存在的那份（同 authFileForUID 口径），不在旁边新建同 uid 第二份。
			FilePath: existingFile,
		}

		if realm == "global" {
			if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
				skipped++
				errs = append(errs, fmt.Sprintf("uid=%s: set realm failed: %v", uid, err))
				continue
			}
		} else {
			_, _ = a.BackfillRealm()
		}

		// 合并写：覆盖导入时保留 device_token / proxy / weight 等手写键
		//（同类项目 issue #210/#187 就是「刷新/导入把 proxy 冲掉了」）。
		if err := a.SaveAtomicMerged(); err != nil {
			skipped++
			errs = append(errs, fmt.Sprintf("uid=%s: save auth failed: %v", uid, err))
			continue
		}

		p.cfg.Pool.Add(a)
		// 新凭证 = RT 已换：旧的「RT 判死」标记不再成立（是否真活由后续请求说话）。
		p.cfg.Pool.ClearRefreshTokenDead(uid)
		if replacing {
			// 覆盖导入**不自动解禁**：导入的凭证可能本身就是死的，解禁只会把死号
			// 放回选号池反复被选中。解禁交给运维（面板 revive）或后续成功请求。
			overwritten++
		} else {
			p.cfg.Pool.Revive(uid)
			imported++
		}

		// 顺带签到/激活（幂等；失败仅记日志，不阻断导入）。
		if realm == "global" {
			if activated, err := p.cfg.Upstream.GlobalCompleteRegistration(a); err != nil {
				log.Printf("panel: import global 注册激活 uid=%s: %v", uid, err)
			} else if activated {
				log.Printf("panel: import global 注册激活 uid=%s 完成", uid)
			}
			if claimed, err := p.cfg.Upstream.ClaimTrial(a); err != nil {
				log.Printf("panel: import global trial uid=%s: %v", uid, err)
			} else if claimed {
				log.Printf("panel: import global trial uid=%s 已领", uid)
			}
		} else {
			if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
				log.Printf("panel: import checkin uid=%s: %v", uid, err)
			}
		}
		if rm, tt, err := p.cfg.Upstream.UserResource(a); err == nil {
			p.cfg.Pool.ReenableIfCredits(uid, rm, tt)
		}
	}

	total = len(accounts)
	log.Printf("panel: cockpit import finished total=%d imported=%d overwritten=%d skipped=%d",
		total, imported, overwritten, skipped)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"total":       total,
		"imported":    imported,
		"overwritten": overwritten,
		"skipped":     skipped,
		"errors":      errs,
	})
}

// validImportUID 校验导入 uid 是否可用于拼文件名（同 login.go validUID 口径）。
func validImportUID(uid string) bool {
	if uid == "" || len(uid) > 64 {
		return false
	}
	for _, c := range uid {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}
