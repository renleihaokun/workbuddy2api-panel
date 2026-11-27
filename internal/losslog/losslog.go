// Package losslog 维护「账号凭证失效 / 批量风控闸门」的 append-only 审计台账（JSONL）。
//
// 目的（见 docs/account-loss-log.md）：
//   - journal 滚动极快，排查批量掉号时往往已丢失现场；
//   - 在首次确证 RT 失效 / session_dead / banned / 批量闸门触发的瞬间，
//     把时间、事件、脱敏 UID、域、判词、RT sha256 指纹与 auth 文件 mtime
//     追加写入持久化的 JSONL 文件，供事后对照「是否我方刚刚刷新过」。
//
// 安全红线：永不写入原始 accessToken 或 refreshToken，只记录 sha256 前 10 位指纹。
package losslog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// Entry 单条账号失效 / 风控台账记录。
type Entry struct {
	Timestamp     string `json:"ts"`
	Event         string `json:"event"`
	UIDShort      string `json:"uid_short"`
	Realm         string `json:"realm,omitempty"`
	Reason        string `json:"reason,omitempty"`
	RTFingerprint string `json:"rt_fp,omitempty"`
	ATExpiresAt   string `json:"at_expires_at,omitempty"`
	AuthMTime     string `json:"auth_mtime,omitempty"`
}

// Logger append-only JSONL 记录器（并发安全）。
type Logger struct {
	mu   sync.Mutex
	path string
}

// New 构造台账记录器；path 为空表示禁用落盘（Record 为空操作）。
func New(path string) *Logger {
	return &Logger{path: path}
}

// Path 返回台账文件路径。
func (l *Logger) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Fingerprint 返回令牌的 sha256 前 10 位 hex 指纹；空串返回空。永不返回原文。
func Fingerprint(tok string) string {
	if tok == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])[:10]
}

// ShortUID 返回脱敏后的 UID 前 8 位。
func ShortUID(uid string) string {
	if len(uid) <= 8 {
		return uid
	}
	return uid[:8]
}

// EntryFromAuth 从账号凭证构造脱敏台账条目。a 可为 nil。
func EntryFromAuth(event, uid, reason string, a *auth.Auth) Entry {
	ev := Entry{
		Timestamp: time.Now().Format(time.RFC3339),
		Event:     event,
		UIDShort:  ShortUID(uid),
		Reason:    reason,
	}
	if a == nil {
		return ev
	}
	ev.Realm = a.Realm()
	ev.RTFingerprint = Fingerprint(a.RefreshTokenValue())
	if exp := a.ExpiresAtValue(); exp > 0 {
		ev.ATExpiresAt = time.Unix(exp, 0).Format(time.RFC3339)
	}
	if a.FilePath != "" {
		if st, err := os.Stat(a.FilePath); err == nil {
			ev.AuthMTime = st.ModTime().Format(time.RFC3339)
		}
	}
	return ev
}

// Record 追加写入一行 JSONL（0600 权限）。写盘失败只打告警，不影响主流程。
func (l *Logger) Record(ev Entry) {
	if l == nil || l.path == "" {
		return
	}
	if ev.Timestamp == "" {
		ev.Timestamp = time.Now().Format(time.RFC3339)
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return
	}
	line = append(line, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	if dir := filepath.Dir(l.path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("WARN: [losslog] open %s: %v", l.path, err)
		return
	}
	defer f.Close()
	if _, err := f.Write(line); err != nil {
		log.Printf("WARN: [losslog] write %s: %v", l.path, err)
	}
}
