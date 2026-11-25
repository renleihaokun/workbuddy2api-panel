package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// uuidShapeRe 带连字符的 UUID 形态（8-4-4-4-12）。真机的 machineId/sessionId
// 都是这个形状，36 位无连字符 hex 与之对不上。
var uuidShapeRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// TestReportDesktopEventFingerprint 断言桌面指纹上报：走 chatBase、UA 为桌面形状、
// 事件数组自动注入 workbuddy-desktop 公共指纹（覆盖同名业务键）。
func TestReportDesktopEventFingerprint(t *testing.T) {
	var got []map[string]any
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/report" {
			t.Errorf("path=%s want /v2/report", r.URL.Path)
		}
		gotUA = r.Header.Get("User-Agent")
		if r.Header.Get("X-User-Id") != "u-dt" {
			t.Errorf("X-User-Id=%q want u-dt", r.Header.Get("X-User-Id"))
		}
		if r.Header.Get("X-Product") != "SaaS" {
			t.Errorf("X-Product=%q want SaaS", r.Header.Get("X-Product"))
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("body must be array: %v", err)
		}
		w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	err := c.ReportDesktopEvent(&auth.Auth{AccessToken: "at", UID: "u-dt", Nickname: "RenJie"},
		DesktopEvent{"eventCode": "agent_task_created", "mode": "craft"})
	if err != nil {
		t.Fatalf("report desktop: %v", err)
	}
	// /v2/report 走 RPC 层 UA（不是 chat 三段式的首段驼峰形态）。
	wantUA := (&Client{ChatBaseCN: srv.URL}).rpcUA(&auth.Auth{UID: "u-dt"})
	if gotUA != wantUA {
		t.Errorf("UA=%q want %q", gotUA, wantUA)
	}
	if len(got) != 1 {
		t.Fatalf("events=%d want 1", len(got))
	}
	ev := got[0]
	if ev["extName"] != "workbuddy-desktop" {
		t.Errorf("extName=%v want workbuddy-desktop", ev["extName"])
	}
	if ev["ideType"] != "WorkBuddy" || ev["ideName"] != "WorkBuddy" {
		t.Errorf("ideType/ideName=%v/%v want WorkBuddy", ev["ideType"], ev["ideName"])
	}
	if ev["userId"] != "u-dt" {
		t.Errorf("userId=%v want u-dt", ev["userId"])
	}
	if ev["userNickname"] != "RenJie" {
		t.Errorf("userNickname=%v want RenJie", ev["userNickname"])
	}
	if ev["mode"] != "craft" {
		t.Errorf("业务字段 mode=%v 被指纹覆盖或缺失", ev["mode"])
	}
	// machineId 是**带连字符的 UUID 形态**（真机如此），不是 36 位无连字符 hex。
	// 形态对不上本身就是一处可判别的「网关特征」。
	machineID, _ := ev["machineId"].(string)
	if !uuidShapeRe.MatchString(machineID) {
		t.Errorf("machineId=%q 不是带连字符的 UUID 形态", machineID)
	}
	if sessID, _ := ev["sessionId"].(string); sessID == machineID {
		t.Error("sessionId 与 machineId 不应相同（真机是两套独立标识）")
	}
	// 版本段必须与本账号的 clientVersion 一致（旧实现这里是写死的 5.5.6）。
	if got, want := ev["ideVersion"], (&Client{ChatBaseCN: srv.URL}).clientVersion(&auth.Auth{UID: "u-dt"}); got != want {
		t.Errorf("ideVersion=%v want %v（版本必须来自单一来源）", got, want)
	}
}

// TestDesktopSkillShareSequenceShape 断言 skill_share 事件链的形状：
// entry_click → channel_action(click) → channel_action(success)，成功那条带
// isSuccessful=true，事件版本对齐任务要求的 5.9.x（覆盖 5.5.6 公共指纹）。
func TestDesktopSkillShareSequenceShape(t *testing.T) {
	events := DesktopSkillShareSequence("skill-x", "示例技能")
	want := []string{"skill_share_entry_click", "skill_share_channel_action", "skill_share_channel_action"}
	if len(events) != len(want) {
		t.Fatalf("events=%d want %d", len(events), len(want))
	}
	for i, code := range want {
		if got := events[i]["eventCode"]; got != code {
			t.Errorf("事件 %d eventCode=%v want %s", i, got, code)
		}
		if got := events[i]["skillId"]; got != "skill-x" {
			t.Errorf("事件 %d skillId=%v want skill-x", i, got)
		}
		if got := events[i]["ideVersion"]; got != desktopSkillShareVersion {
			t.Errorf("事件 %d ideVersion=%v want %s", i, got, desktopSkillShareVersion)
		}
	}
	if events[0]["clickAt"] == nil {
		t.Error("entry_click 缺 clickAt")
	}
	success := events[2]
	if success["panelAction"] != "success" || success["isSuccessful"] != true {
		t.Errorf("success 事件 panelAction/isSuccessful=%v/%v", success["panelAction"], success["isSuccessful"])
	}
	if success["panelType"] != "clipboard" || success["action"] != "link" {
		t.Errorf("success 事件 action/panelType=%v/%v", success["action"], success["panelType"])
	}
	if events[1]["panelAction"] != "click" {
		t.Errorf("中间事件 panelAction=%v want click", events[1]["panelAction"])
	}
}

// TestDesktopChatSequenceShape 断言完整对话事件链的事件码顺序与关键字段。
func TestDesktopChatSequenceShape(t *testing.T) {
	events := DesktopChatSequence("conv-1", "req-1", "msg-1", "fast-model", "fast-model")
	wantCodes := []string{
		"agent_task_created", "chat_message_send", "chat_request_send",
		"chat_message_response", "chat_message_status", "chat_request_response",
	}
	if len(events) != len(wantCodes) {
		t.Fatalf("events=%d want %d", len(events), len(wantCodes))
	}
	for i, want := range wantCodes {
		if events[i]["eventCode"] != want {
			t.Errorf("events[%d].eventCode=%v want %v", i, events[i]["eventCode"], want)
		}
	}
	resp := events[3]
	if resp["isSuccessful"] != true {
		t.Errorf("chat_message_response.isSuccessful=%v want true（RichMeow 判据）", resp["isSuccessful"])
	}
	if resp["finishReason"] != "stop" {
		t.Errorf("finishReason=%v want stop", resp["finishReason"])
	}
	if resp["conversationId"] != "conv-1" {
		t.Errorf("conversationId=%v want conv-1", resp["conversationId"])
	}
}

// TestSetAppearanceTheme 断言主题设置端点形状（点亮 Hp_Appearance 的 API）。
func TestSetAppearanceTheme(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/user-asset/appearance/set" {
			t.Errorf("path=%s want /v2/user-asset/appearance/set", r.URL.Path)
		}
		if got := r.Header.Get("User-Agent"); got == "" || strings.Contains(got, "Go-http-client") {
			t.Errorf("UA=%q 缺失或仍是 Go 默认串（该路径此前完全没有 UA）", got)
		}
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &got)
		w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}
	if err := c.SetAppearanceTheme(&auth.Auth{AccessToken: "at", UID: "u1"}, "theme-tkmw7j"); err != nil {
		t.Fatalf("set theme: %v", err)
	}
	if got["kind"] != "theme" || got["resource_key"] != "theme-tkmw7j" {
		t.Errorf("body=%v want kind=theme resource_key=theme-tkmw7j", got)
	}
}
