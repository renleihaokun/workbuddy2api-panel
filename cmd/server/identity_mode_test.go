// identity_mode_test.go 出站身份模式与抖动窗口的配置契约。
//
// 这两项都是「配错了会静默偏离预期」的类型：
//   - identity_mode 拼错若静默回落 capture，用户会以为已回退却没回退；
//   - jitter_minutes 的 nil 与 0 语义不同（未配置 = 默认 45，显式 0 = 关闭），
//     区分错了要么丧失抖动、要么关不掉。
package main

import (
	"testing"
)

// TestIdentityModeDefaultIsCapture 缺省（键缺席/空串）= capture。
func TestIdentityModeDefaultIsCapture(t *testing.T) {
	c, err := ParseConfig([]byte(`{"listen":":0"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.Upstream.IdentityMode != "" {
		t.Errorf("缺省 identity_mode = %q, want 空（运行时回落 capture）", c.Upstream.IdentityMode)
	}
}

// TestIdentityModeAcceptsKnownValues 合法值原样保留。
func TestIdentityModeAcceptsKnownValues(t *testing.T) {
	for _, v := range []string{"capture", "legacy"} {
		c, err := ParseConfig([]byte(`{"listen":":0","upstream":{"identity_mode":"` + v + `"}}`))
		if err != nil {
			t.Fatalf("%s: parse: %v", v, err)
		}
		if c.Upstream.IdentityMode != v {
			t.Errorf("identity_mode = %q, want %q", c.Upstream.IdentityMode, v)
		}
	}
}

// TestIdentityModeRejectsUnknown 非法值必须报错，不能静默回落——
// 「配了 legacy 却没生效」比启动失败危险得多。
func TestIdentityModeRejectsUnknown(t *testing.T) {
	if _, err := ParseConfig([]byte(`{"listen":":0","upstream":{"identity_mode":"leagcy"}}`)); err == nil {
		t.Error("拼错的 identity_mode 应被拒绝，而不是静默回落 capture")
	}
}

// TestJitterMinutesTriState nil（键缺席）与显式 0 必须可区分。
func TestJitterMinutesTriState(t *testing.T) {
	// 键缺席 → nil（运行时用默认 45）。
	c, err := ParseConfig([]byte(`{"listen":":0"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.Schedule.JitterMinutes != nil {
		t.Errorf("缺省 jitter_minutes = %v, want nil", *c.Schedule.JitterMinutes)
	}

	// 显式 0 → 关闭（不是回落默认）。
	c2, err := ParseConfig([]byte(`{"listen":":0","schedule":{"jitter_minutes":0}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c2.Schedule.JitterMinutes == nil || *c2.Schedule.JitterMinutes != 0 {
		t.Errorf("显式 0 应保留为 0（关闭），得到 %v", c2.Schedule.JitterMinutes)
	}

	// 显式正值 → 原样保留。
	c3, err := ParseConfig([]byte(`{"listen":":0","schedule":{"jitter_minutes":30}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c3.Schedule.JitterMinutes == nil || *c3.Schedule.JitterMinutes != 30 {
		t.Errorf("显式 30 应保留，得到 %v", c3.Schedule.JitterMinutes)
	}
}

// TestJitterMinutesNegativeClampedToZero 负值按 0（关闭）处理，不静默改成正数。
func TestJitterMinutesNegativeClampedToZero(t *testing.T) {
	c, err := ParseConfig([]byte(`{"listen":":0","schedule":{"jitter_minutes":-10}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.Schedule.JitterMinutes == nil || *c.Schedule.JitterMinutes != 0 {
		t.Errorf("负值应归一为 0（关闭），得到 %v", c.Schedule.JitterMinutes)
	}
}
