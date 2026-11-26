// Package responses 把 OpenAI Responses API（POST /v1/responses）的请求与响应
// 翻译成网关内部的 chat.completions 形态。
//
// 设计边界（与 README「Responses API」小节一致）：
//   - 入站先把 Responses 请求改写成 chat 请求，于是网关既有的提示词改写、realm
//     路由、会话粘性、轮转、错误分类、标记修复、用量记账全部零改动复用；
//   - 出站再把 chat 的 JSON / SSE 翻译回 Responses 的 Response 对象与事件流；
//   - 不做服务端会话存储：store 恒为 false，previous_response_id / background /
//     conversation / prompt 明确 400（静默降级会让客户端以为上下文还在）。
package responses

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"strings"
)

const (
	// defaultFunctionNamespace 是 Codex 顶层 function/custom 工具所在的命名空间。
	// 这个命名空间的子工具摊平后直接用裸名，响应里也不回 namespace。
	defaultFunctionNamespace = "functions"
	// chatToolNameSep 是 namespace 子工具摊平到 chat 工具名时使用的分隔符。
	// 若上游实测不接受点号，改成 "__" 即可；映射层不依赖具体分隔符。
	chatToolNameSep = "."
	// maxChatToolNameLen 与 OpenAI function name / 上游 DSML 工具名的上限一致。
	maxChatToolNameLen = 64
)

// ToolRef 是一个 namespace 子工具的原始身份。chat 侧只看到摊平后的名字，
// 响应侧要靠它还原成 Codex 能路由的 name + namespace。
type ToolRef struct {
	Namespace string
	Name      string
}

// Meta 保存原始 Responses 请求里需要回显到响应对象的字段。
//
// 回显不是装饰：OpenAI SDK 会把 response.tools / instructions / temperature 等
// 当成本次调用的权威快照，缺字段的部分客户端会按零值处理（例如 tools=[] 会让
// 上层以为工具声明被拒）。因此这里保留原值，由 buildResponse 统一写回。
type Meta struct {
	Model             string
	Instructions      any
	Tools             []any
	ToolChoice        any
	ParallelToolCalls *bool
	Temperature       *float64
	TopP              *float64
	MaxOutputTokens   *int64
	Reasoning         any
	Text              any
	Metadata          map[string]any
	Stream            bool
	// Store 仅记录客户端是否请求服务端持久化；网关无状态，响应里恒为 false。
	Store          bool
	Truncation     string
	ServiceTier    string
	PromptCacheKey string
	// CustomTools 记录「本次请求里以 type=custom 声明的工具名」。
	// 响应侧据此把对应的 function_call 还原成 custom_tool_call（apply_patch 这类
	// freeform 工具只认后者；混成 function_call 会被 Codex 判为不兼容 payload）。
	// namespace 子工具用摊平后的 chat 工具名做 key。
	CustomTools map[string]bool
	// ToolRefs 记录 namespace 子工具摊平后的 chat 工具名 → 原始 (namespace, name)。
	// 响应侧据此把模型调用的扁平名还原成 Codex 能路由的 name + namespace。
	ToolRefs map[string]ToolRef
	// Unsupported 非空表示请求带了本网关不支持且无法安全忽略的参数，
	// 由 handler 直接 400，不做静默降级。
	Unsupported string
}

// ConvertRequest 把 Responses 请求体改写成 chat.completions 请求体。
//
// 返回的 chatBody 里已含 model / messages / stream / tools 等 chat 必需字段；
// 未识别的 Responses 字段一律丢弃（OpenAI 的向前兼容语义：未知参数不报错）。
func ConvertRequest(body []byte) ([]byte, Meta, error) {
	meta := Meta{CustomTools: map[string]bool{}, ToolRefs: map[string]ToolRef{}}
	if len(body) == 0 {
		return nil, meta, fmt.Errorf("request body is empty")
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, meta, fmt.Errorf("request body must be a JSON object")
	}
	if req == nil {
		return nil, meta, fmt.Errorf("request body must be a JSON object")
	}

	model, _ := req["model"].(string)
	if strings.TrimSpace(model) == "" {
		return nil, meta, fmt.Errorf("model is required")
	}
	meta.Model = model
	// input 是 Responses 的必填字段：只带 instructions 的请求必须 400，
	// 不能悄悄变成一条孤零零的 system 消息发给上游。
	if !hasNonEmpty(req["input"]) {
		return nil, meta, fmt.Errorf("input is required")
	}

	if v, ok := req["stream"].(bool); ok {
		meta.Stream = v
	}
	if v, ok := req["store"].(bool); ok {
		meta.Store = v
	}

	// 不支持且不能静默忽略的参数：静默忽略会让客户端以为服务端记住了会话，
	// 下一轮只带 previous_response_id 时上下文会凭空消失，表现为「模型失忆」。
	if hasNonEmpty(req["previous_response_id"]) {
		meta.Unsupported = "previous_response_id is not supported: this gateway is stateless, send the full conversation in `input`"
	}
	if v, ok := req["background"].(bool); ok && v {
		meta.Unsupported = "background mode is not supported by this gateway"
	}
	if hasNonEmpty(req["conversation"]) {
		meta.Unsupported = "`conversation` is not supported: send the full conversation in `input`"
	}
	if hasNonEmpty(req["prompt"]) {
		meta.Unsupported = "prompt templates are not supported by this gateway"
	}

	chat := map[string]any{
		"model":  model,
		"stream": meta.Stream,
	}
	if meta.Stream {
		// 上游强制 stream:true + include_usage，网关的 payload 管线已负责补
		// stream_options；这里显式给出可让下游聚合路径行为更可预期。
		chat["stream_options"] = map[string]any{"include_usage": true}
	}

	// tools 必须先于 input 转换：input 里的 function_call / custom_tool_call
	// 历史可能带 namespace 字段，需要按本次工具声明算出的最终扁平名回填 chat
	// 的 tool_calls[].function.name；反过来会拿不到去重/截断后的名字。
	//
	// tools：Responses 的 function 工具是扁平结构（name/description/parameters 与
	// type 同级），chat 要求嵌在 function 子对象里；custom（freeform）工具在 chat
	// 侧没有对应概念，包装成「单个 input 字符串参数」的 function；namespace 组
	// 摊平成普通 function（见 convertTools）。
	if rawTools, ok := req["tools"].([]any); ok && len(rawTools) > 0 {
		// 回显用「实际受支持的工具子集」：被丢弃的 web_search/local_shell 等
		// 不应出现在响应快照里（客户端会以为它们生效了）。
		meta.Tools = filterEchoTools(rawTools)
		if tools := convertTools(rawTools, &meta); len(tools) > 0 {
			chat["tools"] = tools
			if tc := convertToolChoice(req["tool_choice"], tools, &meta); tc != nil {
				chat["tool_choice"] = tc
				// 回显用 Responses 形态（扁平 name），而不是 chat 的嵌套形态。
				meta.ToolChoice = echoToolChoice(req["tool_choice"])
			}
			// parallel_tool_calls 只在有工具时下发：空工具集带该字段会被部分
			// 上游当成非法参数。
			if v, ok := req["parallel_tool_calls"].(bool); ok {
				chat["parallel_tool_calls"] = v
			}
		}
	}

	// instructions → 首条 system。Codex 把系统提示词放在 instructions，而不是
	// messages 里；丢掉它等于把 agent 的行为约束整段删除。
	var messages []any
	if ins, ok := req["instructions"].(string); ok && strings.TrimSpace(ins) != "" {
		meta.Instructions = ins
		messages = append(messages, map[string]any{"role": "system", "content": ins})
	}

	inputMsgs, err := convertInput(req["input"], &meta)
	if err != nil {
		return nil, meta, err
	}
	if len(inputMsgs) == 0 {
		return nil, meta, fmt.Errorf("input contains no usable items")
	}
	messages = append(messages, inputMsgs...)
	if len(messages) == 0 {
		return nil, meta, fmt.Errorf("input is required")
	}
	chat["messages"] = messages
	if v, ok := req["parallel_tool_calls"].(bool); ok {
		meta.ParallelToolCalls = &v
	}
	if v, ok := req["truncation"].(string); ok && v != "" {
		meta.Truncation = v
	}
	if v, ok := req["service_tier"].(string); ok && v != "" {
		meta.ServiceTier = v
	}

	if v, ok := numberValue(req["max_output_tokens"]); ok && v > 0 {
		meta.MaxOutputTokens = &v
		chat["max_tokens"] = v
	}
	if v, ok := numberFloat(req["temperature"]); ok {
		meta.Temperature = &v
		chat["temperature"] = v
	}
	if v, ok := numberFloat(req["top_p"]); ok {
		meta.TopP = &v
		chat["top_p"] = v
	}

	// reasoning.effort → reasoning_effort。minimal 是 OpenAI 的档位，上游档位表
	// 从 low 起；其余档位交给网关既有的按模型降级管线处理（payload.go）。
	if r, ok := req["reasoning"].(map[string]any); ok {
		meta.Reasoning = r
		if effort, ok := r["effort"].(string); ok && effort != "" {
			switch strings.ToLower(effort) {
			case "minimal":
				effort = "low"
			}
			chat["reasoning_effort"] = effort
		}
	}

	// text.format → response_format（尽力映射；上游若不支持会忽略该字段）。
	if text, ok := req["text"].(map[string]any); ok {
		meta.Text = text
		if rf := convertTextFormat(text); rf != nil {
			chat["response_format"] = rf
		}
	}

	if md, ok := req["metadata"].(map[string]any); ok {
		meta.Metadata = md
		// 拷贝一份进 chat body：session 包按 metadata.conversation_id 做粘性聚合，
		// 客户端自带的 metadata 也要一并保留（响应侧还会回显）。
		cp := make(map[string]any, len(md))
		for k, v := range md {
			cp[k] = v
		}
		chat["metadata"] = cp
	}
	// prompt_cache_key 是 Responses 的会话缓存键。映射成 chat 的
	// metadata.conversation_id：既让 session 包的粘性/轮级聚合拿到稳定键，
	// 也让上游前缀缓存不至于退化成每个账号一个固定槽位。
	if key, ok := req["prompt_cache_key"].(string); ok && strings.TrimSpace(key) != "" {
		meta.PromptCacheKey = key
		// 原值透传给上游（upstream 的 InjectPromptCacheKey 对客户端自带值原样保留），
		// 同时映射成 metadata.conversation_id 让 session 包的粘性/轮级聚合拿到稳定键。
		chat["prompt_cache_key"] = key
		md, _ := chat["metadata"].(map[string]any)
		if md == nil {
			md = map[string]any{}
		}
		if _, exists := md["conversation_id"]; !exists {
			md["conversation_id"] = key
		}
		chat["metadata"] = md
	}

	out, err := json.Marshal(chat)
	if err != nil {
		return nil, meta, fmt.Errorf("failed to encode chat request: %w", err)
	}
	return out, meta, nil
}

// convertInput 把 Responses 的 input（字符串或 item 数组）展开成 chat messages。
//
// 关键约束：必须**保持原始顺序**。Responses 的 input 是「事件流式历史」——
// assistant 文本、function_call、function_call_output 交替出现；重排会破坏
// tool_calls 与 tool 结果的配对，上游会整段 400。
func convertInput(input any, meta *Meta) ([]any, error) {
	switch v := input.(type) {
	case nil:
		return nil, nil
	case string:
		if strings.TrimSpace(v) == "" {
			return nil, nil
		}
		return []any{map[string]any{"role": "user", "content": v}}, nil
	case []any:
		var out []any
		for _, raw := range v {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := item["type"].(string)
			switch typ {
			case "message", "":
				role, _ := item["role"].(string)
				if role == "" {
					role = "user"
				}
				if role == "developer" {
					// 网关的提示词改写/脱敏/SystemAsUser 都按 system 识别；在这里
					// 归一，避免 developer 消息绕过这些管线（payload 层的
					// normalizeRoles 只有在上游出站前才生效，太晚）。
					role = "system"
				}
				content, err := convertMessageContent(item["content"])
				if err != nil {
					return nil, err
				}
				if content == "" {
					// 空消息没有任何信息量，上游还可能因空 content 400。工具调用是独立
					// 的 function_call item，不依赖这条 message，跳过不会破坏配对。
					continue
				}
				out = append(out, map[string]any{"role": role, "content": content})
			case "function_call", "custom_tool_call":
				out = appendToolCall(out, item, typ, meta)
			case "function_call_output", "custom_tool_call_output":
				callID, _ := item["call_id"].(string)
				if callID == "" {
					continue
				}
				out = append(out, map[string]any{
					"role":         "tool",
					"tool_call_id": callID,
					"content":      convertToolOutput(item["output"]),
				})
			case "reasoning":
				// 模型私有推理。网关没有可回传的 encrypted_content，保留原文既不
				// 合法也没有价值（上游不接受 reasoning_text content part）。
			case "item_reference":
				log.Printf("WARN: [responses] dropped item_reference input item (stateless gateway)")
			default:
				log.Printf("WARN: [responses] dropped unsupported input item type=%q", typ)
			}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("input must be a string or an array of items")
	}
}

// appendToolCall 把一条 function_call / custom_tool_call 历史并入 messages：
// 连续多条调用合并进同一条 assistant 消息的 tool_calls 数组（chat 协议要求一次
// assistant 回合的所有并行调用放在同一条消息里）。
func appendToolCall(out []any, item map[string]any, typ string, meta *Meta) []any {
	name, _ := item["name"].(string)
	namespace, _ := item["namespace"].(string)
	// 历史里 Codex 回传的是原始 (namespace, name)；chat 侧要用本次工具声明里
	// 的扁平名，否则和 tools 数组对不上。
	flat := meta.FlatNameFor(namespace, name)
	if flat == "" {
		flat = name
	}
	callID, _ := item["call_id"].(string)
	if callID == "" {
		callID = NewID("call")
	}
	args := ""
	if typ == "custom_tool_call" {
		input, _ := item["input"].(string)
		args = encodeArguments(map[string]any{"input": input})
		// 历史里出现过 custom 调用 → 即便本次 tools 声明在链路上丢失，响应侧
		// 也能把同名调用还原成 custom_tool_call（tools 丢失是本仓库的实测常态）。
		if flat != "" {
			if meta.CustomTools == nil {
				meta.CustomTools = map[string]bool{}
			}
			meta.CustomTools[flat] = true
		}
	} else {
		args, _ = item["arguments"].(string)
	}
	meta.rememberToolRef(namespace, name, flat)
	call := map[string]any{
		"id":   callID,
		"type": "function",
		"function": map[string]any{
			"name":      flat,
			"arguments": args,
		},
	}
	if n := len(out); n > 0 {
		if last, ok := out[n-1].(map[string]any); ok && last["role"] == "assistant" {
			if tcs, ok := last["tool_calls"].([]any); ok {
				last["tool_calls"] = append(tcs, call)
				return out
			}
		}
	}
	return append(out, map[string]any{
		"role":       "assistant",
		"content":    "",
		"tool_calls": []any{call},
	})
}

// convertMessageContent 把 Responses 的 content（字符串或 part 数组）转成 chat
// content。纯文本时折叠成 string：上游对 string content 的兼容性最好。
func convertMessageContent(v any) (any, error) {
	switch c := v.(type) {
	case nil:
		return "", nil
	case string:
		return c, nil
	case []any:
		var texts []string
		var parts []any
		hasMedia := false
		for _, raw := range c {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := part["type"].(string)
			switch typ {
			case "input_text", "output_text", "text", "summary_text":
				text, _ := part["text"].(string)
				texts = append(texts, text)
				parts = append(parts, map[string]any{"type": "text", "text": text})
			case "input_image":
				url := imageURL(part)
				if url == "" {
					log.Printf("WARN: [responses] dropped input_image without image_url/file_id")
					continue
				}
				img := map[string]any{"url": url}
				if detail, ok := part["detail"].(string); ok && detail != "" {
					img["detail"] = detail
				}
				parts = append(parts, map[string]any{"type": "image_url", "image_url": img})
				hasMedia = true
			case "input_file", "file":
				// 上游没有文件输入能力。丢弃但保留可见日志，避免用户以为文件被读进去了。
				log.Printf("WARN: [responses] dropped input_file part (upstream has no file input)")
			case "refusal":
				if text, _ := part["refusal"].(string); text != "" {
					texts = append(texts, text)
					parts = append(parts, map[string]any{"type": "text", "text": text})
				}
			default:
				log.Printf("INFO: [responses] ignored unsupported content part type=%q", typ)
			}
		}
		if !hasMedia {
			return strings.Join(texts, ""), nil
		}
		return parts, nil
	default:
		return "", nil
	}
}

// imageURL 兼容 image_url 的两种形态（字符串 / 对象），以及 file_id 占位。
func imageURL(part map[string]any) string {
	switch v := part["image_url"].(type) {
	case string:
		return v
	case map[string]any:
		if u, ok := v["url"].(string); ok {
			return u
		}
	}
	if id, ok := part["file_id"].(string); ok && id != "" {
		// 网关无法把 file_id 解析成图片字节；交给上游必然失败，直接丢弃更诚实。
		log.Printf("WARN: [responses] dropped input_image referenced by file_id=%s", id)
	}
	return ""
}

// convertToolOutput 把 function_call_output.output（字符串或 part 数组）压成
// 文本。工具结果里的图片/文件一概丢弃——chat 的 tool 消息只承载文本。
func convertToolOutput(v any) string {
	switch out := v.(type) {
	case nil:
		return ""
	case string:
		return out
	case []any:
		var b strings.Builder
		for _, raw := range out {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := part["text"].(string); ok {
				b.WriteString(text)
			}
		}
		return b.String()
	default:
		// 非字符串/数组形态（例如工具直接回了一个 JSON 对象）：JSON 序列化比
		// fmt 的 Go 语法输出更接近模型期望的文本。
		if raw, err := json.Marshal(out); err == nil {
			return string(raw)
		}
		return fmt.Sprintf("%v", out)
	}
}

// convertTools 把 Responses 的工具声明转成 chat 的 tools。
//
//   - function：扁平字段 → 嵌套到 function 子对象；strict 丢弃（上游不支持结构化
//     输出约束，保留未知字段有 400 风险）；
//   - custom：freeform 工具包装成「单 input 字符串参数」的 function，响应侧再
//     还原成 custom_tool_call；
//   - namespace：把 tools 里的 function/custom 摊平成普通 function。chat 侧名字
//     用 <namespace>.<tool>（默认 functions 命名空间不加前缀），原始身份记进
//     Meta.ToolRefs，响应侧再还原成 Codex 能路由的 name + namespace；
//   - 其余内置工具：丢弃 + WARN（不整请求失败——Codex 默认会带 web_search 之类，
//     直接 400 会让所有请求都不可用）。
func convertTools(raw []any, meta *Meta) []any {
	if meta.CustomTools == nil {
		meta.CustomTools = map[string]bool{}
	}
	if meta.ToolRefs == nil {
		meta.ToolRefs = map[string]ToolRef{}
	}
	out := make([]any, 0, len(raw))
	used := make(map[string]bool, len(raw))
	type pendingNamespace struct {
		name        string
		description string
		tools       []any
	}
	var pending []pendingNamespace

	// 第一遍：顶层 function/custom 先占名字；namespace 组排队，等顶层工具全部
	// 注册完再摊平，这样默认 functions 命名空间里与顶层重名的子工具可以按
	// 重复声明跳过，而不是被加后缀。
	for _, item := range raw {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := tool["type"].(string)
		switch typ {
		case "function":
			name, desc, params := functionToolFields(tool)
			if name == "" {
				log.Printf("WARN: [responses] dropped function tool without name")
				continue
			}
			appendFunctionTool(&out, used, name, desc, params)
		case "custom":
			name, _ := tool["name"].(string)
			name = strings.TrimSpace(name)
			if name == "" {
				log.Printf("WARN: [responses] dropped custom tool without name")
				continue
			}
			desc, _ := tool["description"].(string)
			appendCustomTool(&out, used, meta, name, desc)
		case "namespace":
			name, _ := tool["name"].(string)
			name = strings.TrimSpace(name)
			if name == "" {
				log.Printf("WARN: [responses] dropped namespace tool group without name")
				continue
			}
			children, ok := tool["tools"].([]any)
			if !ok || len(children) == 0 {
				log.Printf("WARN: [responses] dropped namespace tool group %q without tools", name)
				continue
			}
			desc, _ := tool["description"].(string)
			pending = append(pending, pendingNamespace{name: name, description: desc, tools: children})
		default:
			log.Printf("WARN: [responses] dropped unsupported tool type=%q (gateway cannot execute it; model will not see it)", typ)
		}
	}

	// 默认 functions 命名空间先摊平：它和顶层工具是同一个命名空间，重名时
	// 应以顶层声明为准，不能先让非默认命名空间占掉名字。
	ordered := make([]pendingNamespace, 0, len(pending))
	for _, group := range pending {
		if normalizeNamespace(group.name) == "" {
			ordered = append(ordered, group)
		}
	}
	for _, group := range pending {
		if normalizeNamespace(group.name) != "" {
			ordered = append(ordered, group)
		}
	}

	groups, added, dropped := 0, 0, 0
	for _, group := range ordered {
		groups++
		ns := normalizeNamespace(group.name)
		for _, rawChild := range group.tools {
			child, ok := rawChild.(map[string]any)
			if !ok {
				dropped++
				continue
			}
			typ, _ := child["type"].(string)
			name, _ := child["name"].(string)
			if typ == "function" {
				// 子工具偶尔也会用 chat 嵌套形态声明 name。
				if fname, _, _ := functionToolFields(child); fname != "" {
					name = fname
				}
			}
			name = strings.TrimSpace(name)
			if name == "" {
				log.Printf("WARN: [responses] dropped namespace tool namespace=%q type=%q without name", group.name, typ)
				dropped++
				continue
			}

			flat := name
			if ns == "" {
				// 默认命名空间的 name 必须原样保留：响应侧不回 namespace，
				// Codex 会按 name 找默认命名空间里的工具。
				if used[flat] {
					log.Printf("INFO: [responses] skipped duplicate default-namespace tool %q", flat)
					continue
				}
			} else {
				flat = flattenToolName(ns, name)
				flat = uniqueChatToolName(flat, used)
				if flat == "" {
					log.Printf("WARN: [responses] dropped namespace tool namespace=%q name=%q: no usable chat tool name", group.name, name)
					dropped++
					continue
				}
			}

			childDesc, _ := child["description"].(string)
			var params any
			if typ == "function" {
				// 兼容子工具也用 chat 嵌套形态声明的情况；params/desc 以
				// functionToolFields 的结果为准。
				_, fdesc, fparams := functionToolFields(child)
				params = fparams
				if fdesc != "" {
					childDesc = fdesc
				}
			}
			desc := namespacedToolDescription(ns, group.description, childDesc)
			switch typ {
			case "function":
				appendFunctionTool(&out, used, flat, desc, params)
				if ns != "" {
					meta.ToolRefs[flat] = ToolRef{Namespace: ns, Name: name}
				}
				added++
			case "custom":
				appendCustomTool(&out, used, meta, flat, desc)
				if ns != "" {
					meta.ToolRefs[flat] = ToolRef{Namespace: ns, Name: name}
				}
				added++
			default:
				log.Printf("WARN: [responses] dropped unsupported namespace tool type=%q (namespace=%q name=%q)", typ, group.name, name)
				dropped++
			}
		}
	}
	if groups > 0 {
		log.Printf("INFO: [responses] flattened namespace tools groups=%d added=%d dropped=%d", groups, added, dropped)
	}
	return out
}

// functionToolFields 兼容两种 function 声明：Responses 扁平形态（name/description/
// parameters 与 type 同级）与 chat 嵌套形态（function:{...}，部分中转/客户端会混发）。
func functionToolFields(tool map[string]any) (name, desc string, params any) {
	nested, _ := tool["function"].(map[string]any)
	name, _ = tool["name"].(string)
	desc, _ = tool["description"].(string)
	params = tool["parameters"]
	if name == "" && nested != nil {
		name, _ = nested["name"].(string)
	}
	if desc == "" && nested != nil {
		desc, _ = nested["description"].(string)
	}
	if params == nil && nested != nil {
		params = nested["parameters"]
	}
	return strings.TrimSpace(name), desc, params
}

// appendFunctionTool 追加一个 chat function 工具。name 由调用方负责去重/截断。
func appendFunctionTool(out *[]any, used map[string]bool, name, desc string, params any) {
	fn := map[string]any{"name": name}
	if desc != "" {
		fn["description"] = desc
	}
	if params == nil {
		params = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	fn["parameters"] = params
	*out = append(*out, map[string]any{"type": "function", "function": fn})
	used[name] = true
}

// appendCustomTool 把 freeform 工具包装成「单 input 字符串参数」的 function。
// 把 freeform 约定写进描述：模型必须把原始文本放进 input 字段，否则响应侧拿不到
// 可执行的补丁。
func appendCustomTool(out *[]any, used map[string]bool, meta *Meta, name, desc string) {
	if meta.CustomTools == nil {
		meta.CustomTools = map[string]bool{}
	}
	meta.CustomTools[name] = true
	if desc != "" {
		desc += "\n"
	}
	desc += "Put the raw freeform text for this tool in the `input` argument."
	*out = append(*out, map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": desc,
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"input": map[string]any{"type": "string", "description": "Raw freeform tool input."},
				},
				"required": []any{"input"},
			},
		},
	})
	used[name] = true
}

// filterEchoTools 返回响应对象里要回显的工具子集：只保留本网关真正支持的
// function / custom / namespace（namespace 组里只保留能摊平的子工具；其余内置
// 工具已被丢弃，回显会让客户端误以为生效）。
func filterEchoTools(raw []any) []any {
	out := make([]any, 0, len(raw))
	for _, item := range raw {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch typ, _ := tool["type"].(string); typ {
		case "function", "custom":
			out = append(out, tool)
		case "namespace":
			children, _ := tool["tools"].([]any)
			kept := make([]any, 0, len(children))
			for _, rawChild := range children {
				child, ok := rawChild.(map[string]any)
				if !ok {
					continue
				}
				ctyp, _ := child["type"].(string)
				cname, _ := child["name"].(string)
				if strings.TrimSpace(cname) == "" {
					continue
				}
				if ctyp == "function" || ctyp == "custom" {
					kept = append(kept, child)
				}
			}
			if len(kept) == 0 {
				continue
			}
			cp := make(map[string]any, len(tool))
			for k, v := range tool {
				cp[k] = v
			}
			cp["tools"] = kept
			out = append(out, cp)
		}
	}
	return out
}

// echoToolChoice 把 tool_choice 归一成 Responses 的回显形态（function/custom 用扁平 name）。
func echoToolChoice(v any) any {
	switch tc := v.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(tc)) {
		case "auto", "none", "required":
			return strings.ToLower(strings.TrimSpace(tc))
		}
		return "auto"
	case map[string]any:
		if typ, _ := tc["type"].(string); typ == "namespace" {
			return "auto"
		}
		name, _ := tc["name"].(string)
		if name == "" {
			if fn, ok := tc["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
		}
		if name != "" {
			typ, _ := tc["type"].(string)
			if typ != "custom" && typ != "function" {
				typ = "function"
			}
			out := map[string]any{"type": typ, "name": name}
			if ns, _ := tc["namespace"].(string); strings.TrimSpace(ns) != "" {
				out["namespace"] = strings.TrimSpace(ns)
			}
			return out
		}
		return "auto"
	default:
		return "auto"
	}
}

// convertToolChoice 归一 tool_choice。tools 为空时返回 nil（上游对空工具集带
// tool_choice 会 400）。namespace 子工具要按 (namespace, name) 换成 chat 的扁平名。
func convertToolChoice(v any, tools []any, meta *Meta) any {
	if len(tools) == 0 {
		return nil
	}
	switch tc := v.(type) {
	case nil:
		return nil
	case string:
		switch strings.ToLower(strings.TrimSpace(tc)) {
		case "auto", "none", "required":
			return strings.ToLower(strings.TrimSpace(tc))
		default:
			return "auto"
		}
	case map[string]any:
		if typ, _ := tc["type"].(string); typ == "namespace" {
			log.Printf("WARN: [responses] tool_choice targeting a whole namespace is not supported; using auto")
			return "auto"
		}
		name, _ := tc["name"].(string)
		if name == "" {
			if fn, ok := tc["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
		}
		name = strings.TrimSpace(name)
		if name == "" {
			return "auto"
		}
		// 已经是本次声明的扁平名：直接用（客户端可能同时带了 stale namespace）。
		if meta != nil {
			if _, ok := meta.ToolRefs[name]; ok {
				return map[string]any{"type": "function", "function": map[string]any{"name": name}}
			}
		}
		flat := name
		if meta != nil {
			ns, _ := tc["namespace"].(string)
			flat = meta.FlatNameFor(ns, name)
		}
		if flat == "" {
			return "auto"
		}
		return map[string]any{"type": "function", "function": map[string]any{"name": flat}}
	default:
		return "auto"
	}
}

// ResolveTool 把 chat 侧扁平工具名还原成 Responses 的 (name, namespace)。
func (m Meta) ResolveTool(flat string) (string, string) {
	if ref, ok := m.ToolRefs[flat]; ok {
		return ref.Name, ref.Namespace
	}
	return flat, ""
}

// FlatNameFor 把 Responses 的 (namespace, name) 映射成本次请求实际使用的
// chat 工具名。优先查本次 tools 声明里算出的名字（可能被截断/加过后缀）；
// 没有声明时按同一规则确定性重建，供历史回传使用。
func (m Meta) FlatNameFor(namespace, name string) string {
	name = strings.TrimSpace(name)
	ns := normalizeNamespace(namespace)
	if ns == "" {
		return name
	}
	for flat, ref := range m.ToolRefs {
		if ref.Namespace == ns && ref.Name == name {
			return flat
		}
	}
	return flattenToolName(ns, name)
}

// rememberToolRef 在历史里第一次见到某个 namespace 调用、且本次 tools 声明
// 没覆盖到它时，补一条映射；这样响应侧仍能把模型调用还原成 name + namespace。
func (m *Meta) rememberToolRef(namespace, name, flat string) {
	ns := normalizeNamespace(namespace)
	if ns == "" || flat == "" || flat == name {
		return
	}
	if m.ToolRefs == nil {
		m.ToolRefs = map[string]ToolRef{}
	}
	if _, ok := m.ToolRefs[flat]; !ok {
		m.ToolRefs[flat] = ToolRef{Namespace: ns, Name: strings.TrimSpace(name)}
	}
}

// normalizeNamespace 把 Codex 的默认命名空间归一成空串；默认命名空间的工具
// 在 chat 侧用裸名，响应侧也不回 namespace。
func normalizeNamespace(namespace string) string {
	ns := strings.TrimSpace(namespace)
	if ns == "" || ns == defaultFunctionNamespace {
		return ""
	}
	return ns
}

// flattenToolName 生成 namespace 子工具的 chat 侧名字。子工具名若已带命名空间
// 前缀（Codex 的 MCP 工具常见），不重复加前缀。
func flattenToolName(namespace, name string) string {
	ns := normalizeNamespace(namespace)
	name = strings.TrimSpace(name)
	if ns == "" || name == "" {
		return name
	}
	for _, sep := range []string{chatToolNameSep, "__", "_"} {
		if strings.HasPrefix(name, ns+sep) {
			return capChatToolName(sanitizeChatToolName(name))
		}
	}
	return capChatToolName(sanitizeChatToolName(ns + chatToolNameSep + name))
}

// sanitizeChatToolName 把上游可能不接受的字符替换成下划线。保留下划线、短横线、
// 点号和冒号：Codex 自己的工具名与上游 DSML 判定都接受这些字符。
func sanitizeChatToolName(name string) string {
	if name == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '-', r == '.', r == ':':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// capChatToolName 把超过 64 字符的工具名截断，并追加 fnv32 hash 保证确定性去重。
func capChatToolName(name string) string {
	if len(name) <= maxChatToolNameLen {
		return name
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	suffix := fmt.Sprintf("_%08x", h.Sum32())
	keep := maxChatToolNameLen - len(suffix)
	if keep > len(name) {
		keep = len(name)
	}
	for keep > 0 && name[keep]&0xC0 == 0x80 {
		keep--
	}
	return name[:keep] + suffix
}

// uniqueChatToolName 在已占用的名字集合里找一个可用的扁平名：先试原名，冲突
// 时依次尝试 _2、_3…；每次都重新截断，保证结果仍不超过 64 字符。
func uniqueChatToolName(base string, used map[string]bool) string {
	candidate := capChatToolName(base)
	if candidate == "" {
		return ""
	}
	if !used[candidate] {
		return candidate
	}
	for i := 2; i <= 99; i++ {
		candidate = capChatToolName(fmt.Sprintf("%s_%d", base, i))
		if !used[candidate] {
			return candidate
		}
	}
	return ""
}

// namespacedToolDescription 把 namespace 组的说明带进子工具描述。Codex 自动生成
// 的泛化说明（Tools in the <ns> namespace.）不重复加，避免每个子工具都背一遍。
func namespacedToolDescription(namespace, groupDesc, childDesc string) string {
	ns := strings.TrimSpace(namespace)
	groupDesc = strings.TrimSpace(groupDesc)
	childDesc = strings.TrimSpace(childDesc)
	if ns == "" || groupDesc == "" || groupDesc == fmt.Sprintf("Tools in the %s namespace.", ns) {
		return childDesc
	}
	if childDesc == "" {
		return fmt.Sprintf("Namespace %q: %s", ns, groupDesc)
	}
	return fmt.Sprintf("Namespace %q: %s\n\n%s", ns, groupDesc, childDesc)
}

// convertTextFormat 把 text.format 映射成 chat 的 response_format。
// json_schema 的上游支持性未经证实：映射失败时静默放弃，不阻断请求。
func convertTextFormat(text map[string]any) map[string]any {
	format, ok := text["format"].(map[string]any)
	if !ok {
		return nil
	}
	switch typ, _ := format["type"].(string); typ {
	case "json_object":
		return map[string]any{"type": "json_object"}
	case "json_schema":
		name, _ := format["name"].(string)
		schema := format["schema"]
		if schema == nil || strings.TrimSpace(name) == "" {
			return nil
		}
		strict, _ := format["strict"].(bool)
		return map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   name,
				"schema": schema,
				"strict": strict,
			},
		}
	default:
		return nil
	}
}

// hasNonEmpty 判断字段是否「真的存在且非空」。nil / 空串 / 空对象一律视为未提供，
// 避免客户端默认发 `"previous_response_id": null` 时被误判成不支持。
func hasNonEmpty(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(t) != ""
	case bool:
		return t
	case map[string]any:
		return len(t) > 0
	case []any:
		return len(t) > 0
	default:
		return true
	}
}

// numberValue 取整数型数值（max_output_tokens 等）。
func numberValue(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int:
		return int64(n), true
	case int64:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}

// numberFloat 取浮点型数值（temperature / top_p）。
func numberFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// encodeArguments 把 map 序列化成工具调用 arguments 字符串。
func encodeArguments(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(raw)
}
