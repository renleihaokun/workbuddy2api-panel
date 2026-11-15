package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestRunSkillShareReportsChannelAction 固化 skill_share（2026-11 上游新任务）的判据：
// /v2/report 上的 skill_share_channel_action 事件（桌面指纹）。用例约束三件事——
//  1. autoActions 注册了 skill_share（否则扫描/执行队列永远扫不进它）；
//  2. 动作打 chatBase 的 /v2/report，事件链含 entry_click → click → success；
//  3. 成功事件带 isSuccessful=true 与技能身份，并按 5.9.3 版本指纹上报
//     （任务说明要求电脑端 ≥5.9.0）。
func TestRunSkillShareReportsChannelAction(t *testing.T) {
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/report" {
			t.Errorf("path=%s want /v2/report", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("body must be event array: %v", err)
		}
		w.Write([]byte(`{"code":0,"msg":"OK"}`))
	}))
	defer srv.Close()

	if autoActionFor("skill_share") == nil {
		t.Fatal("autoActions 未登记 skill_share")
	}
	p := &Panel{cfg: Config{Upstream: &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL}}}
	if _, err := runSkillShare(p, &auth.Auth{AccessToken: "at", UID: "u-share"}); err != nil {
		t.Fatalf("runSkillShare: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("events=%d want 3", len(got))
	}
	var sawSuccess bool
	for _, ev := range got {
		if ev["eventCode"] != "skill_share_channel_action" || ev["panelAction"] != "success" {
			continue
		}
		sawSuccess = true
		if ev["isSuccessful"] != true {
			t.Errorf("success 事件 isSuccessful=%v want true", ev["isSuccessful"])
		}
		if ev["skillId"] == "" || ev["skillName"] == "" {
			t.Errorf("success 事件缺技能身份: %v", ev)
		}
		if ev["ideVersion"] != "5.9.3" || ev["extVersion"] != "5.9.3" {
			t.Errorf("版本指纹=%v/%v want 5.9.3", ev["ideVersion"], ev["extVersion"])
		}
		if ev["extName"] != "workbuddy-desktop" {
			t.Errorf("extName=%v want workbuddy-desktop（公共指纹未注入）", ev["extName"])
		}
	}
	if !sawSuccess {
		t.Fatalf("未上报 skill_share_channel_action(success)，events=%v", got)
	}
}
