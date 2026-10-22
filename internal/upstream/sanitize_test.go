package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const (
	ccIdentity = "You are Claude Code, Anthropic's official CLI for Claude."
	ccBranch   = "Main branch (you will usually use this for PRs)"
	ccHeader   = "x-anthropic-billing-header: cc_version=1.0; cc_entrypoint=cli;"
	// Codex instructions 首段（上游逐字精确指纹，三句缺一不可）。
	codexInstructions = "You are a coding agent running in the Codex CLI, a terminal-based coding assistant. Codex CLI is an open source project led by OpenAI. You are expected to be precise, safe, and helpful."
)

func TestIdentityRewritten(t *testing.T) {
	out := sanitizeText(ccIdentity)
	if !strings.Contains(out, "official CLI tool for Claude.") {
		t.Errorf("identity not rewritten: %q", out)
	}
	if strings.Contains(out, ccIdentity) {
		t.Errorf("original identity still present: %q", out)
	}
}

// 桌面版（claude-desktop-3p / Agent SDK）的身份句以逗号接后继内容，结尾不是句号。
// 回归用例：匹配串曾带结尾句号，导致该形态漏网、指纹原样发上游 → 400 code=11128。
func TestIdentityDesktopVariantRewritten(t *testing.T) {
	in := "You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK."
	out := sanitizeText(in)
	if strings.Contains(out, "official CLI for Claude") {
		t.Errorf("desktop identity not rewritten: %q", out)
	}
	if !strings.Contains(out, "official CLI tool for Claude, running within the Claude Agent SDK.") {
		t.Errorf("desktop identity suffix not preserved: %q", out)
	}
}

func TestBranchRewritten(t *testing.T) {
	out := sanitizeText(ccBranch)
	if !strings.Contains(out, "Default branch (you will usually use this for PRs)") {
		t.Errorf("branch not rewritten: %q", out)
	}
	if strings.Contains(out, "Main branch") {
		t.Errorf("original branch still present: %q", out)
	}
}

// 反馈句带 Anthropic 仓库链接，上游按整句拦截（实测只留链接或只留半边均不拦）。
// 回归用例：give→provide 一词之差即可绕过。
func TestFeedbackSentenceRewritten(t *testing.T) {
	in := "To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues"
	out := sanitizeText(in)
	if strings.Contains(out, "To give feedback") {
		t.Errorf("feedback sentence not rewritten: %q", out)
	}
	if !strings.Contains(out, "To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues") {
		t.Errorf("feedback sentence not rewritten as expected: %q", out)
	}
}

// 上游反探测：请求体里出现裸数字 11128 即整单拦截（与上下文无关）。
// 回归用例：该串会被改写为 11-128 以打断精确匹配。
func TestUpstreamErrorCodeRewritten(t *testing.T) {
	in := "upstream returned code=11128 for this request"
	out := sanitizeText(in)
	if strings.Contains(out, "11128") {
		t.Errorf("error code not rewritten: %q", out)
	}
	if !strings.Contains(out, "11-128") {
		t.Errorf("error code not rewritten as expected: %q", out)
	}
}

// 回归：工具调用消息的 content 常为 null，而旧版 sanitizeMessages 在 content 缺失时
// 直接 continue，整条消息连 tool_calls 一起被跳过 → arguments 里的被拦字符串原样漏出。
func TestToolCallArgumentsSanitized(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "user", "content": "run"},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "c1", "type": "function", "function": map[string]any{
				"name":      "Bash",
				"arguments": `{"command":"echo 11128"}`,
			}},
		}},
	}
	if !sanitizeMessages(msgs) {
		t.Fatal("sanitizeMessages 未报告任何改动，tool_calls 被跳过")
	}
	fn := msgs[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	got := fn["arguments"].(string)
	if strings.Contains(got, "11128") {
		t.Errorf("tool_call arguments 未被净化: %q", got)
	}
}

func TestBillingHeaderStrippedValueIrrelevant(t *testing.T) {
	out := sanitizeText(ccHeader)
	if strings.Contains(out, "x-anthropic-billing-header") {
		t.Errorf("header not stripped: %q", out)
	}
}

// 附加验证：正常对话里出现 github.com/anthropics/ 链接（但不是反馈句整句）
// 时，预检特征命中（进入净化），但改写层只动精确匹配的整句——普通链接文本
// 不该被改写。同理，既不含 11128 也不含反馈整句的文本原样返回。
func TestNormalAnthropicLinkNotRewritten(t *testing.T) {
	in := "see https://github.com/anthropics/anthropic-cookbook for examples"
	if out := sanitizeText(in); out != in {
		t.Errorf("normal anthropic link should be untouched: %q -> %q", in, out)
	}
}

// Codex instructions 首段：命中预告且整句改写，逐字指纹被破坏、语义保留。
func TestCodexInstructionsRewritten(t *testing.T) {
	out := sanitizeText(codexInstructions)
	if strings.Contains(out, codexInstructions) {
		t.Errorf("codex fingerprint still present: %q", out)
	}
	if !strings.Contains(out, "You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.") {
		t.Errorf("codex first sentence not rewritten: %q", out)
	}
	// 其余两句原样保留，语义不变。
	if !strings.Contains(out, "Codex CLI is an open source project led by OpenAI.") ||
		!strings.Contains(out, "You are expected to be precise, safe, and helpful.") {
		t.Errorf("codex remaining sentences altered: %q", out)
	}
}

// 预检必须能认出 Codex 特征（此前只含 Claude Code，导致提前放行）。
func TestCodexFingerprintDetected(t *testing.T) {
	if !hasFingerprint(codexInstructions) {
		t.Error("codex fingerprint not detected by precheck")
	}
}

// 非精确变体不应被改写：仅去掉其中一个词即视为已破坏，无需改动。
func TestCodexVariantNotTouched(t *testing.T) {
	in := "You are a coding agent running in a CLI, a terminal-based coding assistant."
	if out := sanitizeText(in); out != in {
		t.Errorf("already-broken variant should be untouched: %q -> %q", in, out)
	}
}

func TestBillingHeaderCaseInsensitive(t *testing.T) {
	alt := "X-Anthropic-Billing-Header: cc_version=1.0;"
	out := strings.ToLower(sanitizeText(alt))
	if strings.Contains(out, "billing") {
		t.Errorf("case-insensitive header not stripped: %q", out)
	}
}

func TestTrailingKVStripped(t *testing.T) {
	out := sanitizeText("...; cc_version=2.0; cc_entrypoint=cli;")
	if strings.Contains(out, "cc_version") || strings.Contains(out, "cc_entrypoint") {
		t.Errorf("trailing kv not stripped: %q", out)
	}
}

func TestExactMatchOnlyVariantNotTouched(t *testing.T) {
	in := "...official CLI for Claude!"
	if out := sanitizeText(in); out != in {
		t.Errorf("variant should be untouched: %q -> %q", in, out)
	}
}

func TestUserFreeTextNotTouched(t *testing.T) {
	in := "please use main branch for this repo"
	if out := sanitizeText(in); out != in {
		t.Errorf("free text should be untouched: %q -> %q", in, out)
	}
}

func TestNoFeatureReturnsSameString(t *testing.T) {
	in := "ordinary user message"
	if out := sanitizeText(in); out != in {
		t.Errorf("no-feature text should pass through unchanged: %q -> %q", in, out)
	}
}

func TestMultimodalTextPartOnly(t *testing.T) {
	imgPart := map[string]any{"type": "image", "source": map[string]any{"type": "base64", "data": "..."}}
	content := []any{
		map[string]any{"type": "text", "text": ccIdentity},
		imgPart,
	}
	out, changed := sanitizeContent(content)
	if !changed {
		t.Fatal("expected change")
	}
	parts := out.([]any)
	txt, _ := parts[0].(map[string]any)["text"].(string)
	if !strings.Contains(txt, "CLI tool") {
		t.Errorf("text part not sanitized: %q", txt)
	}
	img, _ := parts[1].(map[string]any)
	if img["type"] != "image" || img["source"].(map[string]any)["data"] != "..." {
		t.Error("image part modified")
	}
}

// 集成：完整请求体经 PrepareBodyOpt 净化后无残留指纹，且 stream/tool_choice 行为不受影响。
func TestPrepareBodyOptSanitizesSystem(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":"` + ccIdentity + ` ` + ccHeader + `"},` +
		`{"role":"user","content":"hi"}]}`)
	out := PrepareBodyOpt(body, true)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["stream"] != true {
		t.Error("stream not forced")
	}
	msgs := obj["messages"].([]any)
	sys, _ := msgs[0].(map[string]any)["content"].(string)
	if strings.Contains(sys, "x-anthropic-billing-header") || strings.Contains(sys, ccIdentity) || strings.Contains(sys, ccBranch) {
		t.Errorf("fingerprints remain: %q", sys)
	}
	if !strings.Contains(sys, "CLI tool") {
		t.Errorf("rewrite missing: %q", sys)
	}
}

func TestPrepareBodyOptDisabledPreservesFingerprints(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","messages":[{"role":"system","content":"` + ccIdentity + `"}]}`)
	out := PrepareBodyOpt(body, false)
	if !strings.Contains(string(out), ccIdentity) {
		t.Error("sanitize=false should preserve fingerprints")
	}
	// 但 stream 仍强制
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	if obj["stream"] != true {
		t.Error("stream should still be forced")
	}
}

// PrepareBody 默认行为 = 开启脱敏（保持向后兼容）。
func TestPrepareBodyDefaultSanitizes(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","messages":[{"role":"system","content":"` + ccIdentity + `"}]}`)
	out := PrepareBodyOpt(body, true)
	if strings.Contains(string(out), ccIdentity) {
		t.Error("PrepareBody default should sanitize")
	}
}

// 出站边界集成：ChatStream 发往上游的 wire body 必须无残留指纹。
func TestChatStreamWireBodySanitized(t *testing.T) {
	var gotBody []byte
	ts := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	})
	defer ts.Close()

	c := New()
	c.SanitizeFingerprints.Store(true)
	c.ChatBaseCN = ts.URL
	acct := &auth.Auth{AccessToken: "test-token", Domain: "copilot.tencent.com", UID: "u1"}

	body := []byte(`{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":"` + ccIdentity + ` ` + ccHeader + `"},` +
		`{"role":"user","content":"hi"}]}`)
	rc, status, respBody, err := c.ChatStream(acct, body, "", ChatMeta{})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if status >= 400 {
		t.Fatalf("upstream status %d: %s", status, respBody)
	}
	// 上游收到的 body：stream 强制 + 指纹已净化
	var obj map[string]any
	if err := json.Unmarshal(gotBody, &obj); err != nil {
		t.Fatalf("wire body not json: %v", err)
	}
	if obj["stream"] != true {
		t.Error("wire body stream not forced")
	}
	sys, _ := obj["messages"].([]any)[0].(map[string]any)["content"].(string)
	for _, fp := range []string{"x-anthropic-billing-header", ccIdentity, ccBranch} {
		if strings.Contains(sys, fp) {
			t.Errorf("wire body contains fingerprint %q: %q", fp, sys)
		}
	}
}

// 出站边界（Codex 场景）：CPA 把 instructions 折成 role=system 的首条消息，
// 净化后 wire body 不得残留 Codex 逐字指纹；其余消息不受影响。
func TestChatStreamWireBodyCodexInstructionsSanitized(t *testing.T) {
	var gotBody []byte
	ts := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	})
	defer ts.Close()

	c := New()
	c.SanitizeFingerprints.Store(true)
	c.ChatBaseCN = ts.URL
	acct := &auth.Auth{AccessToken: "test-token", Domain: "copilot.tencent.com", UID: "u1"}

	body := []byte(`{"model":"kimi-k3","messages":[` +
		`{"role":"system","content":"` + codexInstructions + `"},` +
		`{"role":"user","content":"say ok"}]}`)
	rc, status, respBody, err := c.ChatStream(acct, body, "", ChatMeta{})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if status >= 400 {
		t.Fatalf("upstream status %d: %s", status, respBody)
	}
	var obj map[string]any
	if err := json.Unmarshal(gotBody, &obj); err != nil {
		t.Fatalf("wire body not json: %v", err)
	}
	sys, _ := obj["messages"].([]any)[0].(map[string]any)["content"].(string)
	if strings.Contains(sys, codexInstructions) ||
		strings.Contains(sys, "in the Codex CLI, a terminal-based coding assistant.") {
		t.Errorf("wire body still contains codex fingerprint: %q", sys)
	}
	if !strings.Contains(sys, "running in the Codex CLI tool, a terminal-based") {
		t.Errorf("codex rewrite missing on wire: %q", sys)
	}
	user, _ := obj["messages"].([]any)[1].(map[string]any)["content"].(string)
	if user != "say ok" {
		t.Errorf("user message altered: %q", user)
	}
}

// 出站边界：关闭脱敏后 wire body 原样保留指纹（验证开关真实有效）。
func TestChatStreamWireBodySanitizeDisabled(t *testing.T) {
	var gotBody []byte
	ts := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
	defer ts.Close()

	c := New()
	c.SanitizeFingerprints.Store(false)
	c.ChatBaseCN = ts.URL
	acct := &auth.Auth{AccessToken: "test-token", Domain: "copilot.tencent.com", UID: "u1"}

	body := []byte(`{"model":"glm-5.2","messages":[{"role":"system","content":"` + ccIdentity + `"}]}`)
	rc, status, _, err := c.ChatStream(acct, body, "", ChatMeta{})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if status >= 400 {
		t.Fatalf("upstream status %d", status)
	}
	if !strings.Contains(string(gotBody), ccIdentity) {
		t.Error("sanitize disabled should preserve fingerprint on wire")
	}
}

// TestSanitizeMessagesScrubsReasoningField thinking.go 的回填会把客户端送来的
// reasoning_content 镜像进 reasoning 字段；此前只洗 content / reasoning_content /
// tool_calls → 镜像进 reasoning 的指纹（裸 "11128" 这类反探测串）原样出站，
// 而请求体里出现裸 11128 本身就是上游整单拦截条件。
func TestSanitizeMessagesScrubsReasoningField(t *testing.T) {
	ms := []any{map[string]any{
		"role":      "assistant",
		"content":   "hi",
		"reasoning": "upstream said 11128",
	}}
	if !sanitizeMessages(ms) {
		t.Fatal("reasoning 字段里的指纹未被净化")
	}
	got, _ := ms[0].(map[string]any)["reasoning"].(string)
	if strings.Contains(got, "11128") {
		t.Fatalf("reasoning 仍含裸指纹: %q", got)
	}
}

// DSH（DeepSeek Harness）的**整句**身份句是逐字指纹：上游按整句匹配拦
// （400 11-128 Illegal API invocation from an unapproved channel），换一句即绕过。
// 粒度与 magpie 插件一致：只换这一句，品牌词在别处（讨论、文档、包路径）一律不动。
func TestDeepSeekHarnessIdentitySentenceRewritten(t *testing.T) {
	in := "You are an AI agent powered by DeepSeek Harness.\n\nCheck the exit code."
	out := sanitizeText(in)
	if strings.Contains(out, "You are an AI agent powered by DeepSeek Harness") {
		t.Fatalf("identity sentence not rewritten: %q", out)
	}
	if !strings.HasPrefix(out, "You are an AI agent that helps the user with programming tasks.") {
		t.Errorf("identity sentence rewritten to unexpected text: %q", out)
	}
	if !strings.Contains(out, "Check the exit code.") {
		t.Errorf("surrounding text should be untouched: %q", out)
	}
	if again := sanitizeText(out); again != out {
		t.Errorf("not idempotent: %q -> %q", out, again)
	}
}

// 品牌词本身与包路径**不**是指纹：用户讨论 DSH、仓库文档引用、npm 包名原样保留。
func TestDeepSeekHarnessBrandWordAloneUntouched(t *testing.T) {
	for _, in := range []string{
		"You are interacting through the DeepSeek Harness Web GUI.",
		"the DeepSeek Harness checkout is here",
		"npm i @deepseek-ai/deepseek-harness",
		"DeepSeekHarness is the product name",
	} {
		if out := sanitizeText(in); out != in {
			t.Errorf("brand word should be untouched: %q -> %q", in, out)
		}
	}
}

// 预检快路径：整句身份句必须命中（否则整段净化被跳过、指纹原样出站）。
func TestDeepSeekHarnessDetected(t *testing.T) {
	if !hasFingerprint("You are an AI agent powered by DeepSeek Harness.") {
		t.Error("identity sentence should hit the fast path")
	}
	if hasFingerprint("the DeepSeek Harness checkout is here") {
		t.Error("bare brand word should not enter sanitization")
	}
}

// 全链路：system 里的整句被改写，user 里的品牌词逐字保留（角色范围）。
func TestPrepareBodyRewritesDeepSeekHarness(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":"You are an AI agent powered by DeepSeek Harness."},` +
		`{"role":"user","content":"the DeepSeek Harness checkout is here"}]}`)
	out := string(PrepareBodyOpt(body, true))
	if strings.Contains(out, "You are an AI agent powered by DeepSeek Harness") {
		t.Fatalf("system identity sentence survived prepareBody: %s", out)
	}
	if !strings.Contains(out, "You are an AI agent that helps the user with programming tasks.") {
		t.Errorf("system identity sentence not rewritten: %s", out)
	}
	if !strings.Contains(out, "the DeepSeek Harness checkout is here") {
		t.Errorf("user message must stay untouched: %s", out)
	}
}

// 角色范围：user 与 tool 消息逐字原样出站——上游通道校验不读它们（2026-10-21
// magpie 真机 A/B），改写只会污染用户内容（翻译原文、仓库文件、讨论里的错误码）。
func TestSanitizeLeavesUserAndToolMessagesUntouched(t *testing.T) {
	const userText = "You are Claude Code, Anthropic's official CLI for Claude. code=11128 Main branch (you will usually use this for PRs)"
	const toolText = "file content: {\"code\":11128} and Main branch (you will usually use this for PRs)"
	body := []byte(`{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":"You are Claude Code, Anthropic's official CLI for Claude."},` +
		`{"role":"user","content":` + strconv.Quote(userText) + `},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"read","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"c1","content":` + strconv.Quote(toolText) + `}]}`)
	out := string(PrepareBodyOpt(body, true))
	// system 被洗（断言改写痕迹）。
	if !strings.Contains(out, "official CLI tool for Claude") {
		t.Errorf("system should still be sanitized: %s", out)
	}
	// user / tool 原文逐字保留（按解析后的值比对，避开 JSON 转义差异）。
	var parsed struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("out body not json: %v", err)
	}
	byRole := map[string]string{}
	for _, m := range parsed.Messages {
		byRole[m.Role] = m.Content
	}
	if byRole["user"] != userText {
		t.Errorf("user message changed:\n got %q\nwant %q", byRole["user"], userText)
	}
	if byRole["tool"] != toolText {
		t.Errorf("tool message changed:\n got %q\nwant %q", byRole["tool"], toolText)
	}
}

// 审查插件（官方 dsh-auto-review + 三方 auto-review-zh）的提示词身份句同样是逐字
// 指纹：客户端把它们作为独立一次模型调用的正文发出，上游照样按「非官方渠道」拦。
// 口径与既有规则一致——只把 reviewer 换成 checker，语义不变、破坏逐字匹配。
func TestReviewPluginPromptsRewritten(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			"official REVIEW_SYSTEM_PROMPT",
			"You are a read-only safety reviewer for a coding agent. Follow the verdict rules in the user message exactly.",
			"You are a read-only safety checker for a coding agent.",
		},
		{
			"official auto-review request",
			"You are the auto-review safety reviewer of DeepSeek Harness. A tool call is about to cross a permission boundary.",
			"You are the auto-review safety checker of DeepSeek Harness.",
		},
		{
			"official read-only sentence",
			"the evidence below ONLY. You are a READ-ONLY reviewer: you cannot and must not execute",
			"You are a READ-ONLY checker: you cannot and must not execute",
		},
		{
			"official eval reviewer",
			"You are the eval reviewer of the dsh-eval engine. You judge whether one finished",
			"You are the eval checker of the dsh-eval engine.",
		},
		{
			"third-party auto-review-zh",
			"You are the final authorization reviewer for exactly one pending tool call. Your decision replaces human approval for this call.",
			"You are the final authorization checker for exactly one pending tool call.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := sanitizeText(tc.in)
			if strings.Contains(out, "reviewer") {
				t.Errorf("reviewer fingerprint survived: %q", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("rewrite missing: got %q want substring %q", out, tc.want)
			}
			if again := sanitizeText(out); again != out {
				t.Errorf("not idempotent: %q -> %q", out, again)
			}
		})
	}
}

// 预检快路径必须认出审查插件身份句（否则整段净化被跳过、指纹原样出站）。
func TestReviewPluginPromptsDetected(t *testing.T) {
	for _, s := range []string{
		"You are a read-only safety reviewer for a coding agent.",
		"You are the auto-review safety reviewer of DeepSeek Harness. A tool call",
		"You are a READ-ONLY reviewer: you cannot and must not execute",
		"You are the eval reviewer of the dsh-eval engine.",
		"You are the final authorization reviewer for exactly one pending tool call.",
	} {
		if !hasFingerprint(s) {
			t.Errorf("review-plugin prompt not detected: %q", s)
		}
	}
}

// 全链路：审查插件的 system 提示词经 prepareBody 后不再含原身份句。
func TestPrepareBodyRewritesReviewPluginPrompt(t *testing.T) {
	sys := "You are a read-only safety reviewer for a coding agent. Follow the verdict rules in the user message exactly. Return exactly one JSON object and nothing else."
	body := []byte(`{"model":"glm-5.2","messages":[{"role":"system","content":"` + sys + `"},{"role":"user","content":"review this call"}]}`)
	out := string(PrepareBodyOpt(body, true))
	if strings.Contains(out, "You are a read-only safety reviewer for a coding agent") {
		t.Fatalf("review-plugin system prompt survived: %s", out)
	}
	if !strings.Contains(out, "You are a read-only safety checker for a coding agent") {
		t.Errorf("review-plugin system prompt not rewritten: %s", out)
	}
}

// newTestUpstream 起一个假上游并捕获请求。
func newTestUpstream(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(h)
}
