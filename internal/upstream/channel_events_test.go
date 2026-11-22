// channel_events_test.go 出站改写的通道事件上报：developer→system 归一与
// 指纹清洗的条数统计（反代通道看板的数据源），以及 Client 回调的接线。
package upstream

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/channelstat"
)

// 归一与清洗各自计数，sanitize=false 时清洗恒为 0（归一不受开关影响）。
func TestPrepareBodyStatsCountsRewrites(t *testing.T) {
	fp := "You are Claude Code, Anthropic's official CLI for Claude."
	body := `{"model":"glm-5.2","messages":[` +
		`{"role":"developer","content":"` + fp + `"},` +
		`{"role":"developer","content":"` + fp + `"},` +
		`{"role":"user","content":"` + fp + `"}]}`

	_, stats := PrepareBodyOptStats([]byte(body), false, nil, nil)
	if stats.RoleNormalized != 2 {
		t.Errorf("RoleNormalized=%d want 2", stats.RoleNormalized)
	}
	if stats.Sanitized != 0 {
		t.Errorf("sanitize=false 时 Sanitized=%d want 0", stats.Sanitized)
	}

	_, stats = PrepareBodyOptStats([]byte(body), true, nil, nil)
	if stats.RoleNormalized != 2 {
		t.Errorf("sanitize=true 时 RoleNormalized=%d want 2", stats.RoleNormalized)
	}
	// system/developer/assistant 里的指纹被清洗；user 消息逐字原样 → 只算 2 条
	// （两条 developer 被归一成 system 后清洗）。
	if stats.Sanitized != 2 {
		t.Errorf("Sanitized=%d want 2（user 消息不改写）", stats.Sanitized)
	}
}

// 无改写的请求统计为 0（不能把日常流量算成「上游在识别我们」）。
func TestPrepareBodyStatsCleanRequest(t *testing.T) {
	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`
	_, stats := PrepareBodyOptStats([]byte(body), true, nil, nil)
	if stats.RoleNormalized != 0 || stats.Sanitized != 0 {
		t.Fatalf("stats=%+v want 全 0", stats)
	}
}

// Client 回调接线：prepareBody 把 role_normalized / sanitize 上报给注入的函数。
func TestClientChannelEventsHook(t *testing.T) {
	c := &Client{}
	c.SanitizeFingerprints.Store(true) // 生产默认 true（main 从配置注入）
	got := map[channelstat.Kind]int{}
	c.SetChannelEvents(func(kind channelstat.Kind, n int) { got[kind] += n })

	fp := "You are Claude Code, Anthropic's official CLI for Claude."
	body := `{"model":"glm-5.2","messages":[` +
		`{"role":"developer","content":"` + fp + `"},` +
		`{"role":"user","content":"hi"}]}`
	c.prepareBody([]byte(body), "cn", "u1", "")
	if got["role_normalized"] != 1 {
		t.Errorf("role_normalized=%d want 1", got["role_normalized"])
	}
	if got["sanitize"] != 1 {
		t.Errorf("sanitize=%d want 1", got["sanitize"])
	}
}

// 未注入回调时零开销、不 panic。
func TestClientChannelEventsNilHook(t *testing.T) {
	c := &Client{}
	body := `{"model":"glm-5.2","messages":[{"role":"developer","content":"x"}]}`
	if out := c.prepareBody([]byte(body), "cn", "u1", ""); len(out) == 0 {
		t.Fatal("prepareBody 不应返回空")
	}
}
