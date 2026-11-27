package losslog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestLossLogAppendAndNeverLeaksTokens(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "acct.json")
	if err := os.WriteFile(authPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "data", "account-loss.jsonl")
	l := New(logPath)

	const secretRT = "super-secret-refresh-token-value-123456"
	const secretAT = "super-secret-access-token-value-abcdef"
	a := &auth.Auth{
		UID:          "01234567-89ab-cdef-0123-456789abcdef",
		AccessToken:  secretAT,
		RefreshToken: secretRT,
		ExpiresAt:    time.Now().Add(24 * time.Hour).Unix(),
		Domain:       "www.codebuddy.cn",
		FilePath:     authPath,
	}

	l.Record(EntryFromAuth("rt_dead", a.UID, "12153 revoked", a))
	l.Record(EntryFromAuth("session_dead", a.UID, "12153 session dead", a))

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read loss log: %v", err)
	}
	s := string(raw)
	if strings.Contains(s, secretRT) || strings.Contains(s, secretAT) {
		t.Fatalf("台账不得泄露原始 token: %s", s)
	}
	if strings.Contains(s, a.UID) {
		t.Fatalf("台账应只记短 UID 前缀: %s", s)
	}
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines=%d want 2:\n%s", len(lines), s)
	}
	var e Entry
	if err := json.Unmarshal([]byte(lines[0]), &e); err != nil {
		t.Fatalf("unmarshal line 0: %v", err)
	}
	if e.Event != "rt_dead" || e.UIDShort != "01234567" || e.Realm != "cn" {
		t.Errorf("unexpected entry: %+v", e)
	}
	if e.RTFingerprint != Fingerprint(secretRT) || len(e.RTFingerprint) != 10 {
		t.Errorf("rt_fp=%q want %q", e.RTFingerprint, Fingerprint(secretRT))
	}
	if e.ATExpiresAt == "" || e.AuthMTime == "" {
		t.Errorf("missing at_expires_at or auth_mtime: %+v", e)
	}
}
