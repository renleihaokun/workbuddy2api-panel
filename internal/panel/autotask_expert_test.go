package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestExpertIDFromJumpURL 任务元数据取 expertId：上游给的是
// https://www.workbuddy.cn/work/launch/?deeplink=workbuddy%3A%2F%2Fexperts%3FexpertId%3Dex_xxx
// 这种 URL 编码形态，也可能是明文 query 或 workbuddy:// 协议。
func TestExpertIDFromJumpURL(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			"deeplink 编码形态",
			"https://www.workbuddy.cn/work/launch/?deeplink=workbuddy%3A%2F%2Fexperts%3FexpertId%3Dex_GDl31WahyIH0&buddy_type=workbuddy",
			"ex_GDl31WahyIH0",
		},
		{
			"明文 query 形态",
			"workbuddy://experts?expertId=ex_FbY5nlWTqDod",
			"ex_FbY5nlWTqDod",
		},
		{"大小写不敏感", "workbuddy://experts?EXPERTID=ex_AbC123", "ex_AbC123"},
		{"无 expertId", "workbuddy://expert", ""},
		{"空串", "", ""},
		{"脏数据", "expertId=not-an-expert-id", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := expertIDFromJumpURL(c.raw); got != c.want {
				t.Errorf("expertIDFromJumpURL(%q)=%q want %q", c.raw, got, c.want)
			}
		})
	}
}

// TestTaskExpertName 标题取对象名：体验「中国电商运营专家」→ 中国电商运营专家。
func TestTaskExpertName(t *testing.T) {
	if got := taskExpertName(&upstream.Task{Title: "体验「中国电商运营专家」"}); got != "中国电商运营专家" {
		t.Errorf("taskExpertName=%q", got)
	}
	if got := taskExpertName(&upstream.Task{Title: "没有引号的标题"}); got != "没有引号的标题" {
		t.Errorf("taskExpertName fallback=%q", got)
	}
	if got := taskExpertName(nil); got != "" {
		t.Errorf("taskExpertName(nil)=%q", got)
	}
}

// expertTestServer 起一个最小上游：任务列表 / 专家市场 / SSE chat / 事件上报。
type expertTestServer struct {
	mu      sync.Mutex
	events  []map[string]any
	reports int
}

func (s *expertTestServer) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/activity/growth/tasks":
			json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "msg": "OK",
				"data": map[string]any{"tasks": []map[string]any{
					{
						"task_code": "expert_use_maidehao", "title": "体验「中国电商运营专家」",
						"target": 1, "current": 0, "accept_status": "accepted",
						"jump_url": "https://www.workbuddy.cn/work/launch/?deeplink=workbuddy%3A%2F%2Fexperts%3FexpertId%3Dex_GDl31WahyIH0&buddy_type=workbuddy",
					},
				}},
			})
		case r.URL.Path == "/portal/operation-platform/market/expert/list":
			var body struct {
				Keyword string `json:"keyword"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			name, id := "中国电商运营专家", "ex_GDl31WahyIH0"
			if body.Keyword == "无畏契约拼豆专家" {
				name, id = "无畏契约拼豆专家", "ex_xQHHD1u5Jkua"
			}
			json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "msg": "OK",
				"data": map[string]any{"experts": []map[string]any{
					{"expert_id": id, "expert_type": "agent",
						"display_name_zh": name, "profession_zh": name,
						"version": "1.0.2", "categories": []any{"expert-all"}},
				}},
			})
		case r.URL.Path == "/v2/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte("data: {\"id\":\"cmb-0123456789abcdef0123456789abcdef\",\"choices\":[]}\n\n"))
		case r.URL.Path == "/v2/report":
			var evs []map[string]any
			_ = json.NewDecoder(r.Body).Decode(&evs)
			s.mu.Lock()
			s.reports++
			s.events = append(s.events, evs...)
			s.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "OK"})
		default:
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "OK", "data": map[string]any{}})
		}
	}
}

// TestRunExpertByTaskIDHappyPath 端到端：任务 jump_url 的 expertId 必须原样出现在
// expert_actual_use 事件里（服务端只认真实 id），且真实对话的 requestId 来自 SSE。
func TestRunExpertByTaskIDHappyPath(t *testing.T) {
	ts := &expertTestServer{}
	srv := httptest.NewServer(ts.handler(t))
	defer srv.Close()

	p := &Panel{cfg: Config{Upstream: &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
	}}}
	act := autoActionFor("expert_use_maidehao")
	if act == nil {
		t.Fatal("autoActionFor(expert_use_maidehao)=nil：新任务未登记")
	}
	msg, err := act.run(p, &auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(msg, "中国电商运营专家") {
		t.Errorf("msg=%q", msg)
	}

	ts.mu.Lock()
	defer ts.mu.Unlock()
	var got *map[string]any
	for i := range ts.events {
		if ts.events[i]["eventCode"] == "expert_actual_use" {
			got = &ts.events[i]
		}
	}
	if got == nil {
		t.Fatalf("未上报 expert_actual_use，events=%v", ts.events)
	}
	if (*got)["id"] != "ex_GDl31WahyIH0" {
		t.Errorf("actual_use id=%v want ex_GDl31WahyIH0", (*got)["id"])
	}
	if (*got)["requestId"] != "cmb-0123456789abcdef0123456789abcdef" {
		t.Errorf("actual_use requestId=%v", (*got)["requestId"])
	}
	if ts.reports < 2 {
		t.Errorf("上报批次=%d，want >=2（召唤链 + 使用链）", ts.reports)
	}
}

// TestRunExpertByTaskIDValorantByName jump_url 不给 expertId 时走 keyword 搜索：
// 必须用搜索命中的真实 id（ex_xQHHD1u5Jkua）完成调用链，而不是编造 id。
func TestRunExpertByTaskIDValorantByName(t *testing.T) {
	var gotID string
	var reports int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/activity/growth/tasks":
			json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "msg": "OK",
				"data": map[string]any{"tasks": []map[string]any{
					{"task_code": "WorkBuddy_Valorant", "title": "体验「无畏契约拼豆专家」",
						"target": 1, "current": 0, "accept_status": "accepted",
						"jump_url": "workbuddy://expert"},
				}},
			})
		case "/portal/operation-platform/market/expert/list":
			json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "msg": "OK",
				"data": map[string]any{"experts": []map[string]any{
					{"expert_id": "ex_xQHHD1u5Jkua", "expert_type": "agent",
						"display_name_zh": "无畏契约拼豆专家", "profession_zh": "无畏契约拼豆专家",
						"version": "1.3.1", "categories": []any{"03-GameSpatial"}},
				}},
			})
		case "/v2/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte("data: {\"id\":\"cmb-0123456789abcdef0123456789abcdef\",\"choices\":[]}\n\n"))
		case "/v2/report":
			var evs []map[string]any
			_ = json.NewDecoder(r.Body).Decode(&evs)
			reports++
			for _, e := range evs {
				if e["eventCode"] == "expert_actual_use" {
					gotID, _ = e["id"].(string)
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "OK"})
		default:
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "OK", "data": map[string]any{}})
		}
	}))
	defer srv.Close()

	p := &Panel{cfg: Config{Upstream: &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
	}}}
	act := autoActionFor("WorkBuddy_Valorant")
	if act == nil {
		t.Fatal("autoActionFor(WorkBuddy_Valorant)=nil：新任务未登记")
	}
	if _, err := act.run(p, &auth.Auth{AccessToken: "at", UID: "u1"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotID != "ex_xQHHD1u5Jkua" {
		t.Errorf("expert_actual_use id=%q want ex_xQHHD1u5Jkua", gotID)
	}
	if reports < 2 {
		t.Errorf("上报批次=%d，want >=2", reports)
	}
}

// TestRunExpertByTaskIDUnknownExpert 找不到专家时必须零上报（不能留下
// 「已接受未上报」的半程态）。
func TestRunExpertByTaskIDUnknownExpert(t *testing.T) {
	var reports int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/activity/growth/tasks":
			json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "msg": "OK",
				"data": map[string]any{"tasks": []map[string]any{
					{"task_code": "WorkBuddy_Valorant", "title": "体验「不存在的专家」",
						"target": 1, "current": 0, "accept_status": "accepted",
						"jump_url": "workbuddy://expert"},
				}},
			})
		case "/portal/operation-platform/market/expert/list":
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "OK", "data": map[string]any{"experts": []any{}}})
		case "/v2/report":
			reports++
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "OK"})
		default:
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "OK", "data": map[string]any{}})
		}
	}))
	defer srv.Close()

	p := &Panel{cfg: Config{Upstream: &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
	}}}
	act := autoActionFor("WorkBuddy_Valorant")
	if act == nil {
		t.Fatal("autoActionFor(WorkBuddy_Valorant)=nil：新任务未登记")
	}
	msg, err := act.run(p, &auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(msg, "未找到") {
		t.Errorf("msg=%q，应提示未找到专家", msg)
	}
	if reports != 0 {
		t.Errorf("找不到专家时不应上报，reports=%d", reports)
	}
}
