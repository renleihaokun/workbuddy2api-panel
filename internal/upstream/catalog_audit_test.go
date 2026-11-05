// catalog_audit_test.go 线上模型目录审计（opt-in，默认 skip）。
//
// 用途：把线上 /v1/models 与静态兜底表 + 官方客户端快照对照，输出「缺表 /
// 档位冲突 / 上下文缺失 / 牌价漂移」报告。只读 /v1/models，不发推理请求、
// 不消耗积分；没有设置 WB2A_AUDIT_URL 时永远 skip，不影响常规 go test。
//
// 用法（在网关可达的机器上）：
//
//	WB2A_AUDIT_URL=http://127.0.0.1:7863 WB2A_AUDIT_KEY=sk-xxx \
//	  go test ./internal/upstream -run TestLiveCatalogAudit -v
package upstream

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// auditModel /v1/models 单条模型中审计关心的字段子集。
type auditModel struct {
	ID                string   `json:"id"`
	ContextLength     int64    `json:"context_length"`
	MaxOutputTokens   int64    `json:"max_output_tokens"`
	SupportsReasoning bool     `json:"supports_reasoning"`
	Efforts           []string `json:"reasoning_supported_efforts"`
	DefaultEffort     string   `json:"reasoning_default_effort"`
	Credits           string   `json:"credits"`
}

// splitAuditID 拆 /v1/models 的 "cn:model" / "cn/model" / 裸名。
// 裸名按请求侧历史口径归 CN（/v1/models 实际不会输出裸名，仅防御）。
func splitAuditID(id string) (realm, bare string) {
	switch {
	case strings.HasPrefix(id, "cn:"):
		return "cn", id[len("cn:"):]
	case strings.HasPrefix(id, "cn/"):
		return "cn", id[len("cn/"):]
	case strings.HasPrefix(id, "global:"):
		return "global", id[len("global:"):]
	case strings.HasPrefix(id, "global/"):
		return "global", id[len("global/"):]
	default:
		return "cn", id
	}
}

// fetchAuditModels 拉 /v1/models，兼容 OpenAI 信封 {data:[...]} 与裸数组。
func fetchAuditModels(t *testing.T, base, key string) []auditModel {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(base, "/")+"/v1/models", nil)
	if err != nil {
		t.Fatalf("构造审计请求失败: %v", err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("请求 /v1/models 失败: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		t.Fatalf("读取 /v1/models 响应失败: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/v1/models status=%d body=%s", resp.StatusCode, truncate(string(raw), 300))
	}
	var list []auditModel
	if err := json.Unmarshal(raw, &list); err == nil {
		return list
	}
	var env struct {
		Data []auditModel `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("解析 /v1/models 响应失败: %v", err)
	}
	return env.Data
}

// TestLiveCatalogAudit 线上目录与静态表的差异审计。
// 判定口径：
//   - FAIL：思考模型既无远端档位、静态表也没收录（面板会显示「固定档 · 默认 ?」）；
//   - FAIL：远端档位数组与静态表冲突（远端权威，但表该更新了）；
//   - FAIL：远端 context 缺失且静态表没收录；
//   - WARN：远端 max_output_tokens 缺失且静态表也没有（输出上限允许未知）；
//   - WARN：远端缺 credits（面板倍率列为「—」）；
//   - INFO：牌价与快照不一致（上游调价，属正常）、表里有但线上没有（退役）。
func TestLiveCatalogAudit(t *testing.T) {
	base := strings.TrimSpace(os.Getenv("WB2A_AUDIT_URL"))
	if base == "" {
		t.Skip("设置 WB2A_AUDIT_URL（可选 WB2A_AUDIT_KEY）后运行线上目录审计")
	}
	models := fetchAuditModels(t, base, os.Getenv("WB2A_AUDIT_KEY"))
	if len(models) == 0 {
		t.Fatalf("/v1/models 返回空列表（检查账号池/上游连通性）")
	}

	// 官方快照只覆盖 CN，用于牌价对照；global 只做结构检查。
	snap := loadRosterSnapshot(t)
	snapCredits := map[string]string{}
	for _, m := range append(append([]rosterModel{}, snap.Models...), snap.ExtraModels...) {
		if m.Credits != "" {
			snapCredits[m.ID] = m.Credits
		}
	}

	var fails, warns, infos []string
	liveIDs := map[string]bool{}
	for _, m := range models {
		realm, bare := splitAuditID(m.ID)
		liveIDs[realm+":"+bare] = true
		table := cnEffortFallback
		if realm == "global" {
			table = globalEffortFallback
		}
		cap, hasCap := table[bare]
		cc, hasCtx := contextCapFallback[bare]

		if m.SupportsReasoning && len(m.Efforts) == 0 && !hasCap {
			fails = append(fails, fmt.Sprintf("%s 支持思考但远端无 supportedEfforts、静态表也未收录", m.ID))
		}
		if len(m.Efforts) > 0 && hasCap && !equalStrings(m.Efforts, cap.efforts) {
			fails = append(fails, fmt.Sprintf("%s 远端档位=%v 与静态表=%v 冲突", m.ID, m.Efforts, cap.efforts))
		}
		if m.ContextLength <= 0 && !hasCtx {
			fails = append(fails, fmt.Sprintf("%s 远端缺 context_length 且静态表未收录", m.ID))
		}
		if m.MaxOutputTokens <= 0 && (!hasCtx || cc.maxOutput == 0) {
			warns = append(warns, fmt.Sprintf("%s 远端缺 max_output_tokens 且静态表无值（客户端看不到输出上限）", m.ID))
		}
		if !hasCap && !m.SupportsReasoning {
			infos = append(infos, fmt.Sprintf("%s 未收录静态兜底（不推断档位）", m.ID))
		}
		if realm == "cn" {
			if m.Credits == "" {
				warns = append(warns, fmt.Sprintf("%s 远端缺 credits（面板倍率列会显示 —）", m.ID))
			} else if want, ok := snapCredits[bare]; ok {
				if normalizeModelRate(m.Credits) != normalizeModelRate(want) {
					infos = append(infos, fmt.Sprintf("%s 牌价变化: 线上=%s 快照=%s（上游调价；无需改代码）", m.ID, m.Credits, want))
				}
			}
		}
	}

	for realm, table := range map[string]map[string]effortCap{"cn": cnEffortFallback, "global": globalEffortFallback} {
		for id := range table {
			if !liveIDs[realm+":"+id] {
				infos = append(infos, fmt.Sprintf("%s:%s 静态表有、线上目录没有（退役/未开放）", realm, id))
			}
		}
	}

	t.Logf("=== 模型目录审计：%d 个线上模型 / %d 个 CN 快照模型 ===", len(models), len(snap.Models))
	for _, s := range infos {
		t.Logf("INFO  %s", s)
	}
	for _, s := range warns {
		t.Logf("WARN  %s", s)
	}
	for _, s := range fails {
		t.Logf("FAIL  %s", s)
	}
	if len(fails) > 0 {
		t.Fatalf("发现 %d 个必须更新的静态表缺口（详见上方 FAIL 行）", len(fails))
	}
}
