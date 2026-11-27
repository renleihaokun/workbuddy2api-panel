package upstream

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestMPEventBase(t *testing.T) {
	a := &auth.Auth{UID: "u-1", Nickname: "测试"}
	base := mpEventBase(a)
	for _, k := range []string{"ideType", "extName", "ideName", "platform", "userId"} {
		if _, ok := base[k]; !ok {
			t.Errorf("missing common field %s", k)
		}
	}
	if base["ideType"] != "WorkBuddy_MP" || base["extName"] != "workbuddy-mp" {
		t.Errorf("fingerprint ideType=%v extName=%v", base["ideType"], base["extName"])
	}
}

func TestSchoolChatTimesEvents(t *testing.T) {
	ev := SchoolChatTimesEvents("conv-1")
	if ev["eventCode"] != "chat_request_send" {
		t.Errorf("eventCode=%v", ev["eventCode"])
	}
	if ev["conversationId"] != "conv-1" || ev["codebuddy.session_id"] != "conv-1" {
		t.Errorf("conversation join fields missing: %v", ev)
	}
	b, _ := json.Marshal(ev)
	if !strings.Contains(string(b), "agentName") {
		t.Error("agentName missing")
	}
	if strings.Contains(strings.ToLower(string(b)), "wb2api") {
		t.Errorf("SchoolChatTimesEvents 仍含 wb2api 指纹: %s", b)
	}
}

func TestNoWb2apiInOutboundEventsAndHardwareVaries(t *testing.T) {
	pb, _ := json.Marshal(MiniPlaybookEvents("case-1", "demo"))
	if strings.Contains(strings.ToLower(string(pb)), "wb2api") {
		t.Errorf("MiniPlaybookEvents 含 wb2api 指纹: %s", pb)
	}
	dc, _ := json.Marshal(DesktopChatSequence("conv-1", "req-1", "msg-1", "glm-5.2", "GLM-5.2"))
	if strings.Contains(strings.ToLower(string(dc)), "wb2api") {
		t.Errorf("DesktopChatSequence 含 wb2api 指纹: %s", dc)
	}

	c := New()
	fp1 := c.desktopFingerprint(&auth.Auth{UID: "uid-alpha", Nickname: "a"})
	fp2 := c.desktopFingerprint(&auth.Auth{UID: "uid-alpha", Nickname: "a"})
	if fp1["osVersion"] != fp2["osVersion"] || fp1["cpuCores"] != fp2["cpuCores"] || fp1["memorySize"] != fp2["memorySize"] {
		t.Errorf("同 UID 硬件指纹不稳定: %v vs %v", fp1, fp2)
	}
	b1, _ := json.Marshal(fp1)
	if strings.Contains(strings.ToLower(string(b1)), "wb2api") {
		t.Errorf("desktopFingerprint 含 wb2api 指纹: %s", b1)
	}

	// 多个不同 UID 的硬件组合不能全塌缩成完全相同的一台机器。
	combos := map[string]struct{}{}
	for i := 0; i < 16; i++ {
		fp := c.desktopFingerprint(&auth.Auth{UID: string(rune('a'+i)) + "-uid-test"})
		key := string(rune(fp["cpuCores"].(int))) + "|" + string(rune(fp["memorySize"].(int))) + "|" + fp["osVersion"].(string)
		combos[key] = struct{}{}
	}
	if len(combos) < 2 {
		t.Errorf("16 个不同 UID 硬件画像完全相同（%d 组合）", len(combos))
	}
}

