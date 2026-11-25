package upstream

import (
	"errors"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestMergeV3RoutesPriority 主路字段权威：同 id 在三路都出现时，字段取自主路
// （桌面端 UA），后续路不得覆盖其窗口/effort/credits。
func TestMergeV3RoutesPriority(t *testing.T) {
	mk := func(id string, ctx, out int64, credits string, efforts []string) ModelInfo {
		return ModelInfo{ID: id, ContextWindow: ctx, MaxTokens: out, Credits: credits, Efforts: efforts}
	}
	desktop := probeResult{
		names: []string{"shared", "only-desktop"},
		infos: []ModelInfo{
			mk("shared", 1000000, 128000, "x1.33", []string{"low", "high"}),
			mk("only-desktop", 500000, 64000, "x0.10", []string{"medium"}),
		},
	}
	ide := probeResult{
		names: []string{"shared", "only-ide"},
		infos: []ModelInfo{
			// 同 id 但字段不同：不得覆盖主路。
			mk("shared", 176000, 24000, "x9.99", []string{"max"}),
			mk("only-ide", 200000, 32000, "x0.20", nil),
		},
	}
	cli := probeResult{
		names: []string{"shared", "only-cli"},
		infos: []ModelInfo{
			mk("shared", 272000, 72000, "x8.88", []string{"xhigh"}),
			mk("only-cli", 256000, 32000, "x0.30", nil),
		},
	}
	got := mergeV3Routes([]struct {
		label string
		res   probeResult
	}{
		{"desktop-UA", desktop},
		{"IDE-UA", ide},
		{"CLI-UA", cli},
	})
	if got.err != nil {
		t.Fatalf("unexpected err: %v", got.err)
	}
	wantIDs := []string{"shared", "only-desktop", "only-ide", "only-cli"}
	if len(got.names) != len(wantIDs) {
		t.Fatalf("names = %v, want %v", got.names, wantIDs)
	}
	for i, id := range wantIDs {
		if got.names[i] != id {
			t.Errorf("names[%d] = %q, want %q (full %v)", i, got.names[i], id, got.names)
		}
	}
	// 主路字段权威：shared 的窗口必须是主路值。
	var shared *ModelInfo
	for i := range got.infos {
		if got.infos[i].ID == "shared" {
			shared = &got.infos[i]
		}
	}
	if shared == nil {
		t.Fatal("shared model missing from infos")
	}
	if shared.ContextWindow != 1000000 || shared.MaxTokens != 128000 {
		t.Errorf("shared window = %d/%d, want 1000000/128000 (primary route must win)", shared.ContextWindow, shared.MaxTokens)
	}
	if shared.Credits != "x1.33" {
		t.Errorf("shared credits = %q, want x1.33", shared.Credits)
	}
	if len(shared.Efforts) != 2 || shared.Efforts[0] != "low" {
		t.Errorf("shared efforts = %v, want [low high]", shared.Efforts)
	}
}

// TestMergeV3RoutesPartialFailure 单路失败不拖垮整次探测：失败的路由被跳过，
// 成功路并集照常产出；全路失败才返回 err。
func TestMergeV3RoutesPartialFailure(t *testing.T) {
	mk := func(id string) ModelInfo { return ModelInfo{ID: id} }
	ok := probeResult{names: []string{"a"}, infos: []ModelInfo{mk("a")}}
	bad := probeResult{err: errors.New("v3/config status 500")}

	got := mergeV3Routes([]struct {
		label string
		res   probeResult
	}{
		{"desktop-UA", bad},
		{"IDE-UA", ok},
		{"CLI-UA", bad},
	})
	if got.err != nil {
		t.Fatalf("partial failure should not error, got %v", got.err)
	}
	if len(got.names) != 1 || got.names[0] != "a" {
		t.Errorf("names = %v, want [a]", got.names)
	}

	// 全路失败 → err（取首路错误，供调用方降级到企业端点）。
	all := mergeV3Routes([]struct {
		label string
		res   probeResult
	}{
		{"desktop-UA", bad},
		{"IDE-UA", bad},
		{"CLI-UA", bad},
	})
	if all.err == nil {
		t.Fatal("all routes failed: want err, got nil")
	}
}

// TestNonChatModelGenerationTags 生成类（图片/视频）模型不得进对话目录。
// 回归用例：nonChatModel 早期只拦 text-to-image，桌面端目录下的
// text-to-video / image-to-video（seedance 系列）会漏过并混进 /v1/models。
func TestNonChatModelGenerationTags(t *testing.T) {
	cases := []struct {
		name string
		id   string
		out  int64
		tags []string
		want bool
	}{
		{"图片生成（既有规则）", "hunyuan-image-alpha", 0, []string{"text-to-image"}, true},
		{"图生图", "gpt-image-2.5-sunburst", 0, []string{"text-to-image", "image-to-image"}, true},
		{"文生视频（本次修复）", "seedance-2.5", 0, []string{"text-to-video", "image-to-video"}, true},
		{"图生视频（本次修复）", "seedance-2.5-pro", 1024, []string{"image-to-video"}, true},
		{"普通对话模型", "gpt-6-sol", 128000, nil, false},
		{"新模型无 tags", "grok-4.7", 128000, []string{}, false},
		{"对话模型带无关 tag", "balanced-model", 32000, []string{"craft"}, false},
		{"tiny 输出仍剔除", "completion-1.0", 256, nil, true},
		{"nes 前缀仍剔除", "nes-1.2", 8192, nil, true},
	}
	for _, c := range cases {
		if got := nonChatModel(c.id, c.out, c.tags); got != c.want {
			t.Errorf("%s: nonChatModel(%q, %d, %v) = %v, want %v", c.name, c.id, c.out, c.tags, got, c.want)
		}
	}
}

// TestChatUAForGlobalAccount chat 链路 UA 的品牌段按 realm 切：global 必须是
// 小写连字符 slug `workbuddy-ai`（Electron asciiSlug 的产物），且首段恒为驼峰
// `WorkBuddy/`（SDK buildUserAgent 的形态）。送错品牌段会触发上游 403
// code=11140 风控。探测主路用同一个 UA，与 chat 路径同源。
func TestChatUAForGlobalAccount(t *testing.T) {
	c := New()
	global := &auth.Auth{UID: "u1", Domain: "www.workbuddy.ai"}
	global.BackfillRealm()
	ua := c.chatUA(global)
	if want := "workbuddy-ai/"; !contains(ua, want) {
		t.Errorf("global chat UA = %q, want it to contain %q", ua, want)
	}
	// 首段恒驼峰 WorkBuddy（不论 realm）——这是 SDK 的 productName 段。
	if !strings.HasPrefix(ua, "WorkBuddy/") {
		t.Errorf("global chat UA = %q, want prefix WorkBuddy/（首段恒驼峰）", ua)
	}
	if !contains(ua, "CLI/") {
		t.Errorf("global chat UA = %q, want CLI segment", ua)
	}
	// 大写带空格的 "WorkBuddy AI" 是真的不存在的形态（只出现在 RPC 之外的旧实现）。
	if contains(ua, "WorkBuddy AI") {
		t.Errorf("global chat UA = %q, must not contain 'WorkBuddy AI'（真机是小写 slug）", ua)
	}

	// CN 账号：两段都是驼峰 WorkBuddy。
	cn := &auth.Auth{UID: "u2", Domain: "copilot.tencent.com"}
	cn.BackfillRealm()
	cnUA := c.chatUA(cn)
	if contains(cnUA, "workbuddy-ai") {
		t.Errorf("cn chat UA = %q, must not contain workbuddy-ai", cnUA)
	}
	if want := "WorkBuddy/" + defaultClientVersionCN + " WorkBuddy/" + defaultClientVersionCN; !strings.HasPrefix(cnUA, want) {
		t.Errorf("cn chat UA = %q, want prefix %q", cnUA, want)
	}
}

// TestRPCUADiffersFromChatUA RPC 层与 chat 层的 UA 是两个家族：RPC 首段就是品牌
// slug（global 为 workbuddy-ai），chat 首段恒驼峰。两族并存才是官方形态。
func TestRPCUADiffersFromChatUA(t *testing.T) {
	c := New()
	global := &auth.Auth{UID: "u1", Domain: "www.workbuddy.ai"}
	global.BackfillRealm()
	if got := c.rpcUA(global); !strings.HasPrefix(got, "workbuddy-ai/") {
		t.Errorf("global RPC UA = %q, want prefix workbuddy-ai/（首段即品牌 slug）", got)
	}
	cn := &auth.Auth{UID: "u2", Domain: "copilot.tencent.com"}
	cn.BackfillRealm()
	// CN 下两族同值（品牌段本来就是驼峰）。
	if c.rpcUA(cn) != c.chatUA(cn) {
		t.Errorf("cn RPC UA=%q 与 chat UA=%q 应同值", c.rpcUA(cn), c.chatUA(cn))
	}
}

// TestBillingUAIsAxios billing 域走 axios 实例，UA 是 axios 默认串，不带品牌段。
func TestBillingUAIsAxios(t *testing.T) {
	c := New()
	cn := &auth.Auth{UID: "u1", Domain: "copilot.tencent.com"}
	cn.BackfillRealm()
	if got := c.billingUA(); got != "axios/1.16.1" {
		t.Errorf("billing UA = %q, want axios/1.16.1", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
