package panel

import (
	"fmt"
	"testing"
	"time"
)

// TestRegisterLoginBounded 反复发起登录不能把待登录表撑成无界 map。
func TestRegisterLoginBounded(t *testing.T) {
	p := New(Config{Version: "test"})
	now := time.Now()
	total := maxPendingLogins * 3

	for i := 0; i < total; i++ {
		p.registerLoginLocked(fmt.Sprintf("state-%03d", i), "cn",
			now.Add(time.Duration(i)*time.Millisecond))
	}
	if got := len(p.logins); got != maxPendingLogins {
		t.Fatalf("logins = %d, want %d", got, maxPendingLogins)
	}
	// 按 created 淘汰最旧的：最新一条必须在，最早一条必须不在。
	if _, ok := p.logins[fmt.Sprintf("state-%03d", total-1)]; !ok {
		t.Error("最新 state 不应被淘汰")
	}
	if _, ok := p.logins["state-000"]; ok {
		t.Error("最旧 state 应被淘汰")
	}
}

// TestRegisterLoginPrunesExpired 超过 loginTTL 的 state 在下次登记时被清掉。
func TestRegisterLoginPrunesExpired(t *testing.T) {
	p := New(Config{Version: "test"})
	base := time.Now()

	p.registerLoginLocked("old", "cn", base.Add(-2*loginTTL))
	p.registerLoginLocked("fresh", "global", base)

	if _, ok := p.logins["old"]; ok {
		t.Fatal("过期 state 应被清理")
	}
	sess, ok := p.logins["fresh"]
	if !ok {
		t.Fatal("fresh state 丢失")
	}
	if sess.realm != "global" {
		t.Errorf("realm = %q, want global", sess.realm)
	}
}
