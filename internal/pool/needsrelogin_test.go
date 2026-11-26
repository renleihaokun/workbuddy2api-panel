// needsrelogin_test.go 「session 失效」分型与 RT 判死标记的池层契约。
//
// 两条正交事实：
//   - disabled_kind=session_dead（账号已出池，只能重登）；
//   - refresh_token_dead（凭证层事实，AT 可能仍有效、账号还能干活）。
//
// 二者都不是「文案匹配」：kind 是枚举、rtDeadAt 是时间戳，面板据此决定给什么按钮。
package pool

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestNoteSessionDeadSetsKindAndNeedsRelogin 三振禁用时写入死因枚举，派生 needs_relogin。
func TestNoteSessionDeadSetsKindAndNeedsRelogin(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	for i := 1; i <= SessionDeadThreshold(); i++ {
		last := p.NoteSessionDead("u1")
		if i < SessionDeadThreshold() {
			st, _ := p.Status("u1")
			if st.Disabled || st.NeedsRelogin {
				t.Fatalf("第 %d 次不应禁用: %+v", i, st)
			}
			continue
		}
		if !last {
			t.Fatalf("第 %d 次应达阈禁用", i)
		}
	}
	st, _ := p.Status("u1")
	if !st.Disabled || st.DisabledKind != DisableKindSessionDead || !st.NeedsRelogin {
		t.Fatalf("want disabled/session_dead/needs_relogin, got %+v", st)
	}
	if got := p.NeedsReloginCount(); got != 1 {
		t.Errorf("NeedsReloginCount=%d want 1", got)
	}
}

// TestDisableKindBannedIsNotNeedsRelogin 封号（11140）不是「待重登」：重登无效。
func TestDisableKindBannedIsNotNeedsRelogin(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.DisableWithKind("u1", "account banned by upstream (11140)", DisableKindBanned)
	st, _ := p.Status("u1")
	if !st.Disabled || st.NeedsRelogin {
		t.Fatalf("封号不应 needs_relogin: %+v", st)
	}
	if st.DisabledKind != DisableKindBanned {
		t.Errorf("disabled_kind=%q want %q", st.DisabledKind, DisableKindBanned)
	}
	if got := p.NeedsReloginCount(); got != 0 {
		t.Errorf("NeedsReloginCount=%d want 0", got)
	}
}

// TestMarkRefreshTokenDeadDoesNotDisable RT 判死是凭证层事实：不禁用、不冷却，
// 也不动连续计数（AT 过期后还要正常三振）。
func TestMarkRefreshTokenDeadDoesNotDisable(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	p.NoteSessionDead("u1") // 先吃 1 次，验证标记不清计数
	if !p.MarkRefreshTokenDead("u1") {
		t.Fatal("MarkRefreshTokenDead 应返回 true")
	}
	st, _ := p.Status("u1")
	if st.Disabled || st.Cooling {
		t.Fatalf("RT 判死不得禁用/冷却: %+v", st)
	}
	if !st.RefreshTokenDead || st.RefreshTokenDeadAt.IsZero() {
		t.Fatalf("应透出 refresh_token_dead + 时刻: %+v", st)
	}
	if st.NeedsRelogin {
		t.Errorf("账号未出池时不该 needs_relogin: %+v", st)
	}
	// 计数未被清零：再来 2 次应达阈（说明 Mark 没有偷偷重置进度）。
	if p.NoteSessionDead("u1") {
		t.Fatal("第 2 次不应禁用")
	}
	if !p.NoteSessionDead("u1") {
		t.Fatal("第 3 次应禁用（计数未被 MarkRefreshTokenDead 重置）")
	}
}

// TestClearRefreshTokenDeadOnlyByRefresh 只有刷新成功才清 RT 判死标记；人工复活不清。
func TestClearRefreshTokenDeadOnlyByRefresh(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.MarkRefreshTokenDead("u1")
	p.Revive("u1")
	if st, _ := p.Status("u1"); !st.RefreshTokenDead {
		t.Fatalf("revive 不该清 RT 判死标记（点解冻不会让上游复活旧票）: %+v", st)
	}
	p.ClearRefreshTokenDead("u1")
	if st, _ := p.Status("u1"); st.RefreshTokenDead {
		t.Fatalf("ClearRefreshTokenDead 后应归零: %+v", st)
	}
}

// TestReviveClearsDisabledKind 复活清死因（NeedsRelogin 随之消失），但保留 RT 判死标记。
func TestReviveClearsDisabledKind(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.MarkRefreshTokenDead("u1")
	for i := 0; i < SessionDeadThreshold(); i++ {
		p.NoteSessionDead("u1")
	}
	if st, _ := p.Status("u1"); !st.NeedsRelogin {
		t.Fatalf("precondition: 应 needs_relogin: %+v", st)
	}
	p.Revive("u1")
	st, _ := p.Status("u1")
	if st.Disabled || st.NeedsRelogin || st.DisabledKind != DisableKindNone {
		t.Fatalf("revive 应清 disabled/kind/needs_relogin: %+v", st)
	}
	if !st.RefreshTokenDead {
		t.Errorf("revive 不清 RT 判死标记: %+v", st)
	}
}

// TestStatusAccessTokenExpiresAt 面板要显示「AT 还剩 N 天」，Status 必须透出到期时刻。
func TestStatusAccessTokenExpiresAt(t *testing.T) {
	exp := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	p := New("")
	p.Add(&auth.Auth{UID: "u1", ExpiresAt: exp.Unix()})
	st, _ := p.Status("u1")
	if !st.AccessTokenExpiresAt.Equal(exp) {
		t.Errorf("access_token_expires_at=%v want %v", st.AccessTokenExpiresAt, exp)
	}
}

// TestStateRoundTripKindAndRTDead 新字段落盘后可恢复；旧文件（无 kind/rtDead）零值兼容。
func TestStateRoundTripKindAndRTDead(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1", ExpiresAt: 9999999999})
	p.MarkRefreshTokenDead("u1")
	for i := 0; i < SessionDeadThreshold(); i++ {
		p.NoteSessionDead("u1")
	}
	p.Flush()

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1", ExpiresAt: 9999999999})
	st, ok := p2.Status("u1")
	if !ok {
		t.Fatal("账号未恢复")
	}
	if !st.Disabled || st.DisabledKind != DisableKindSessionDead || !st.NeedsRelogin {
		t.Fatalf("禁用+死因应持久化: %+v", st)
	}
	if !st.RefreshTokenDead || st.RefreshTokenDeadAt.IsZero() {
		t.Fatalf("RT 判死时刻应持久化: %+v", st)
	}
}

// TestLegacyStateInfersSessionDeadKind 旧 state.json 没有 disabled_kind：按 reason 文案
// 一次性迁移，升级后既有的 session-dead 账号立刻能标「待重登」（否则要等下一次 12153）。
func TestLegacyStateInfersSessionDeadKind(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	legacy := `{"accounts":{"u1":{"disabled":true,"reason":"12153 session dead"}}}`
	if err := os.WriteFile(fp, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	st, _ := p.Status("u1")
	if !st.Disabled || st.DisabledKind != DisableKindSessionDead || !st.NeedsRelogin {
		t.Fatalf("旧文件应按 reason 迁移成 session_dead: %+v", st)
	}
}

// TestLegacyStateOtherReasonStaysUnclassified 非会话失效的旧禁用不猜死因。
func TestLegacyStateOtherReasonStaysUnclassified(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	legacy := `{"accounts":{"u1":{"disabled":true,"reason":"manual disable (panel)"}}}`
	if err := os.WriteFile(fp, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	st, _ := p.Status("u1")
	if !st.Disabled || st.DisabledKind != DisableKindNone || st.NeedsRelogin {
		t.Fatalf("非会话失效不应被猜成 session_dead: %+v", st)
	}
}
