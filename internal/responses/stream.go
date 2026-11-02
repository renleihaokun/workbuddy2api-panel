package responses

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// Translator 把 chat.completions 的 SSE 帧逐帧翻译成 Responses 的事件流。
//
// 事件顺序是硬约束：Codex 这类客户端要求「先 output_item.added、再
// content_part.added、最后才是 output_text.delta」，缺一帧就会丢弃整段正文
// （实测报错 "OutputTextDelta without active item"）。因此状态机严格按
// 「开 item → 开 part → 发 delta → 关 part → 关 item → completed」推进。
//
// 性能约定：一个上游 chunk 产生的 1–3 个事件先写入内存缓冲，最后一次 Write +
// 一次 Flush 发出——避免把 syscall 数量放大到事件粒度。
type Translator struct {
	w       http.ResponseWriter
	meta    Meta
	flusher http.Flusher

	respID    string
	createdAt int64
	seq       int
	started   bool
	finished  bool
	failed    bool

	items  []*streamItem
	byTool map[int]*streamItem
	msg    *streamItem
	// lastToolIdx 最近一次处理的 tool_call index：上游缺 index 的延续分片
	// 按它归位（byTool 的 key 可能出现空洞，不能拿 len-1 当"最近"）。
	lastToolIdx int

	usage        map[string]any
	finishReason string
	// sawDone 是否收到 StreamHint 收尾写出的 `data: [DONE]`。它是「上游/管线
	// 正常走完」的唯一可信信号：StreamHint 读错误会提前 return 而不写 DONE，
	// 没有它就必须按断流失败处理（否则半截正文会被伪装成 completed）。
	sawDone bool
	// forceIncomplete 出现畸形工具参数时置位：即使 finish_reason=stop，
	// 响应也不能标 completed（客户端会执行坏参数）。
	forceIncomplete bool
	// badFrames 无法识别的 data 帧数（非 JSON / 既无 choices 也无 usage）。
	// 用于区分「上游根本没给有效 SSE」与「合法的空响应」。
	badFrames int

	pending bytes.Buffer
	out     bytes.Buffer
}

// streamItem 一个输出项（message / function_call / custom_tool_call）的累积状态。
type streamItem struct {
	index  int
	kind   string // "message" | "function_call" | "custom_tool_call"
	id     string
	callID string
	name   string
	text   strings.Builder
	added  bool
}

// NewTranslator 构造翻译器。此时不写任何字节——Start 由首次 SSE 写入触发
// （确保 content-type 已由上游透传层设好）。
func NewTranslator(w http.ResponseWriter, meta Meta) *Translator {
	t := &Translator{
		w:           w,
		meta:        meta,
		respID:      NewID("resp"),
		createdAt:   time.Now().Unix(),
		byTool:      map[int]*streamItem{},
		lastToolIdx: -1,
	}
	if f, ok := w.(http.Flusher); ok {
		t.flusher = f
	}
	return t
}

// Start 发出 response.created 与 response.in_progress。
func (t *Translator) Start() error {
	if t.started {
		return nil
	}
	t.started = true
	created := t.response("in_progress", []any{}, nil, nil)
	t.emit("response.created", map[string]any{"response": created})
	t.emit("response.in_progress", map[string]any{"response": created})
	return t.flush()
}

// WriteFrame 喂入一帧原始 SSE 字节（"data: {...}\n\n"）。允许半帧/粘包：
// 内部按空行切分后逐帧处理。
func (t *Translator) WriteFrame(p []byte) error {
	if t.finished || t.failed || t.sawDone {
		return nil
	}
	if !t.started {
		if err := t.Start(); err != nil {
			return err
		}
	}
	// 兼容 CRLF 分帧（HTTP 层可能改写行尾）；StreamHint 正常恒为 \n\n。
	p = bytes.ReplaceAll(p, []byte("\r\n"), []byte("\n"))
	t.pending.Write(p)
	// 防御：上游只发注释/半帧且长期不闭合时不让缓冲区无限增长（正常帧由
	// StreamHint 逐帧写出，永远以 "\n\n" 结尾）。
	if t.pending.Len() > 8<<20 {
		t.pending.Reset()
	}
	for {
		idx := bytes.Index(t.pending.Bytes(), []byte("\n\n"))
		if idx < 0 {
			break
		}
		raw := make([]byte, idx)
		copy(raw, t.pending.Bytes()[:idx])
		t.pending.Next(idx + 2)
		hasData := false
		nonComment := false
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(strings.TrimRight(line, "\r"))
			if line == "" || strings.HasPrefix(line, ":") {
				continue // 空行 / SSE 注释（心跳）
			}
			if !strings.HasPrefix(line, "data:") {
				nonComment = true
				continue
			}
			hasData = true
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "" {
				continue
			}
			if err := t.handlePayload(payload); err != nil {
				return err
			}
		}
		if !hasData && nonComment {
			// 上游 200 + 非 SSE 响应（代理错误页/裸 JSON）：StreamHint 会把原始行透传，
			// 若不在第一个完整块里识别出来，后续 error 帧会被粘成一行吞掉，最终
			// 变成「空 output 的 completed」。
			t.fail("upstream_parse", "upstream returned a non-SSE response")
			return nil
		}
	}
	return t.flush()
}

// Finish 收尾：关闭所有打开的 item，并发 completed / incomplete / failed。
func (t *Translator) Finish(streamErr error) error {
	if t.finished {
		return nil
	}
	t.finished = true
	if t.failed {
		return t.flush()
	}
	if streamErr != nil {
		// 上游/管线中途断流：不管已产出多少，都不能发 completed。
		t.fail("upstream_stream_error", streamErr.Error())
		return t.flush()
	}
	if !t.started {
		// 一个有效帧都没有（上游 200 + 空流）：StreamHint 已写过 error 帧，
		// 正常路径不会走到这里；防御性兜底。
		t.fail("upstream_empty_stream", "upstream returned an empty stream")
		return t.flush()
	}

	status := "completed"
	var incomplete any
	switch t.finishReason {
	case "length":
		status = "incomplete"
		incomplete = map[string]any{"reason": "max_output_tokens"}
	case "content_filter":
		status = "incomplete"
		incomplete = map[string]any{"reason": "content_filter"}
	}
	itemStatus := "completed"
	if status == "incomplete" {
		itemStatus = "incomplete"
	}

	output := make([]any, 0, len(t.items))
	for _, it := range t.items {
		switch it.kind {
		case "message":
			t.emitItemAdded(it) // 防御：正常路径 appendText 已置 added；这里保证不变量
			item := messageItem(it.id, it.text.String(), itemStatus)
			t.emit("response.output_text.done", map[string]any{
				"item_id":       it.id,
				"output_index":  it.index,
				"content_index": 0,
				"text":          it.text.String(),
				"logprobs":      []any{},
			})
			t.emit("response.content_part.done", map[string]any{
				"item_id":       it.id,
				"output_index":  it.index,
				"content_index": 0,
				"part":          map[string]any{"type": "output_text", "text": it.text.String(), "annotations": []any{}},
			})
			t.emit("response.output_item.done", map[string]any{"output_index": it.index, "item": item})
			output = append(output, item)
		case "function_call":
			t.emitItemAdded(it)
			args := it.text.String()
			status := itemStatus
			if args != "" && !json.Valid([]byte(args)) {
				// 参数不是合法 JSON（截断或模型畸形输出）：item 标 incomplete，
				// 但仍要发 output_item.done 收口，不能留悬空的 added。
				status = "incomplete"
				t.forceIncomplete = true
				log.Printf("WARN: [responses] tool call %q has malformed JSON arguments", it.name)
			}
			t.emit("response.function_call_arguments.done", map[string]any{
				"item_id":      it.id,
				"output_index": it.index,
				"arguments":    args,
			})
			item := functionCallItem(it.id, it.callID, it.name, args, status)
			t.emit("response.output_item.done", map[string]any{"output_index": it.index, "item": item})
			output = append(output, item)
		case "custom_tool_call":
			t.emitItemAdded(it)
			rawArgs := it.text.String()
			input := customInputFromArguments(rawArgs)
			status := itemStatus
			if rawArgs != "" && !json.Valid([]byte(rawArgs)) {
				status = "incomplete"
				t.forceIncomplete = true
				log.Printf("WARN: [responses] custom tool call %q has malformed JSON arguments", it.name)
			}
			if input != "" {
				// custom 的增量只在这里发一次完整文本：partial JSON 无法安全解包，
				// 发 JSON 片段反而会让客户端拼出错误内容。
				t.emit("response.custom_tool_call_input.delta", map[string]any{
					"item_id":      it.id,
					"output_index": it.index,
					"delta":        input,
				})
			}
			t.emit("response.custom_tool_call_input.done", map[string]any{
				"item_id":      it.id,
				"output_index": it.index,
				"input":        input,
			})
			item := customToolCallItem(it.id, it.callID, it.name, input, status)
			t.emit("response.output_item.done", map[string]any{"output_index": it.index, "item": item})
			output = append(output, item)
		}
	}

	if t.forceIncomplete && status == "completed" {
		status = "incomplete"
	}
	if len(output) == 0 && t.badFrames > 0 && t.finishReason == "" {
		// 一个可用输出都没有、却收到过无法识别的 data 帧 → 上游给的不是有效
		// chat SSE（代理错误页/裸 JSON），按解析失败收尾，不伪造空 completed。
		t.fail("upstream_parse", "upstream returned malformed SSE data")
		return t.flush()
	}
	if len(output) == 0 {
		// 上游只发了 finish/usage（无正文、无工具）：仍给一个空 message item。
		// output:[] 会被部分客户端判为畸形响应（Codex / Hermes 均有此类实案）。
		it := &streamItem{index: len(t.items), kind: "message", id: NewID("msg")}
		t.items = append(t.items, it)
		output = append(output, t.emitEmptyMessage(it, itemStatus))
	}
	resp := t.response(status, output, usageFromChat(t.usage), incomplete)
	event := "response.completed"
	if status == "incomplete" {
		event = "response.incomplete"
	}
	t.emit(event, map[string]any{"response": resp})
	return t.flush()
}

// SawDone 报告是否收到了收尾的 `[DONE]` 帧。handler 用它区分「正常结束」与
// 「上游/管线中途断流」：没有 DONE 时不能发 completed。
func (t *Translator) SawDone() bool { return t.sawDone }

// handlePayload 处理一帧 data 负载。
func (t *Translator) handlePayload(payload string) error {
	if payload == "[DONE]" {
		t.sawDone = true
		return nil
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(payload), &obj); err != nil {
		t.badFrames++ // 非 JSON 帧（代理错误页/被粘住的裸文本）
		return nil
	}
	if upstream.ChunkErrorPresent(obj) {
		code, message := errorFieldsForChunk(obj)
		t.fail(code, message)
		return nil
	}
	t.handleChunk(obj)
	return nil
}

// recognizedChatChunk 判断一帧 JSON 是否属于 chat.completion.chunk 家族
// （含空 choices 元数据帧、usage-only 帧、Azure 的 prompt_filter_results）。
func recognizedChatChunk(obj map[string]any) bool {
	if _, ok := obj["choices"]; ok {
		return true
	}
	if _, ok := obj["usage"]; ok {
		return true
	}
	if _, ok := obj["prompt_filter_results"]; ok {
		return true
	}
	return false
}

// handleChunk 处理一个 chat.completion.chunk。
func (t *Translator) handleChunk(obj map[string]any) {
	if t.failed {
		return
	}
	if u, ok := obj["usage"].(map[string]any); ok && len(u) > 0 {
		t.usage = u
	}
	choices, _ := obj["choices"].([]any)
	if len(choices) == 0 {
		// usage-only / prompt_filter_results / 空 choices 元数据帧是合法收尾或过滤
		// 信息；只有完全不像 chat chunk 的 JSON（代理错误页 {"message":"bad gateway"}）
		// 才计入 badFrames，避免误伤合法的空响应/content_filter。
		if !recognizedChatChunk(obj) {
			t.badFrames++
		}
		return
	}
	for _, raw := range choices {
		choice, _ := raw.(map[string]any)
		if choice == nil {
			continue
		}
		if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
			t.finishReason = fr
		}
		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			// 少数上游把完整消息放在 message 里（非 delta）。只认第一份完整消息：
			// 若后续帧重复携带整条 message，按 delta 拼接会把正文复制 N 份。
			msg, ok := choice["message"].(map[string]any)
			if !ok || t.msg != nil {
				continue
			}
			delta = msg
		}
		switch content := delta["content"].(type) {
		case string:
			if content != "" {
				t.appendText(content)
			}
		case []any:
			// 少数上游把 content 发成 part 数组（text 字段拼接），与 Aggregate 同口径。
			if text := messageText(content); text != "" {
				t.appendText(text)
			}
		}
		if tcs, ok := delta["tool_calls"].([]any); ok {
			for _, rawCall := range tcs {
				call, _ := rawCall.(map[string]any)
				if call != nil {
					t.appendToolCall(call)
				}
			}
		}
	}
}

// appendText 追加一段正文，必要时先开 message item 与 content part。
func (t *Translator) appendText(text string) {
	if t.msg == nil {
		item := &streamItem{
			index: len(t.items),
			kind:  "message",
			id:    NewID("msg"),
		}
		t.msg = item
		t.items = append(t.items, item)
		item.added = true
		t.emit("response.output_item.added", map[string]any{
			"output_index": item.index,
			"item": map[string]any{
				"id":      item.id,
				"type":    "message",
				"status":  "in_progress",
				"role":    "assistant",
				"content": []any{},
			},
		})
		t.emit("response.content_part.added", map[string]any{
			"item_id":       item.id,
			"output_index":  item.index,
			"content_index": 0,
			"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
	}
	t.msg.text.WriteString(text)
	t.emit("response.output_text.delta", map[string]any{
		"item_id":       t.msg.id,
		"output_index":  t.msg.index,
		"content_index": 0,
		"delta":         text,
		"logprobs":      []any{},
	})
}

// emitEmptyMessage 补发一个「空文本 message item」的完整生命周期事件。
// 只在整条流没有任何输出时调用（上游只回了 finish/usage 或全部工具调用被截断）。
func (t *Translator) emitEmptyMessage(it *streamItem, status string) map[string]any {
	it.added = true
	t.emit("response.output_item.added", map[string]any{
		"output_index": it.index,
		"item": map[string]any{
			"id":      it.id,
			"type":    "message",
			"status":  "in_progress",
			"role":    "assistant",
			"content": []any{},
		},
	})
	t.emit("response.content_part.added", map[string]any{
		"item_id":       it.id,
		"output_index":  it.index,
		"content_index": 0,
		"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})
	t.emit("response.output_text.done", map[string]any{
		"item_id":       it.id,
		"output_index":  it.index,
		"content_index": 0,
		"text":          "",
		"logprobs":      []any{},
	})
	t.emit("response.content_part.done", map[string]any{
		"item_id":       it.id,
		"output_index":  it.index,
		"content_index": 0,
		"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})
	item := messageItem(it.id, "", status)
	t.emit("response.output_item.done", map[string]any{"output_index": it.index, "item": item})
	return item
}

// appendToolCall 追加一段工具调用分片（首个分片开 item，后续只发 delta）。
func (t *Translator) appendToolCall(call map[string]any) {
	idx := -1
	if v, ok := call["index"].(float64); ok {
		idx = int(v)
	}
	if idx < 0 {
		// 缺 index 的上游：延续最近一个调用槽位（与 sse.go 的 mergeToolCallsChunk 同口径）。
		if t.lastToolIdx >= 0 {
			idx = t.lastToolIdx
		} else {
			idx = 0
		}
	}
	t.lastToolIdx = idx
	fn, _ := call["function"].(map[string]any)
	name, _ := fn["name"].(string)
	argsFragment, _ := fn["arguments"].(string)

	item := t.byTool[idx]
	if item == nil {
		callID, _ := call["id"].(string)
		if callID == "" {
			callID = NewID("call")
		}
		item = &streamItem{
			index:  len(t.items),
			callID: callID,
			name:   name,
			kind:   t.kindFor(name),
		}
		t.byTool[idx] = item
		t.items = append(t.items, item)
		// name 未到的首片先不注册 item（否则 apply_patch 会被误判成
		// function_call）；等 name 到达再按最终类型补发 output_item.added。
		if name != "" {
			t.emitItemAdded(item)
		}
	} else if item.name == "" && name != "" {
		item.name = name
		if !item.added {
			item.kind = t.kindFor(name)
			t.emitItemAdded(item)
			if item.text.Len() > 0 {
				t.emitToolDelta(item, item.text.String())
			}
		}
	}
	if argsFragment == "" {
		return
	}
	item.text.WriteString(argsFragment)
	if item.added {
		t.emitToolDelta(item, argsFragment)
	}
}

// kindFor 按工具名判断输出项类型（custom/freeform → custom_tool_call）。
func (t *Translator) kindFor(name string) string {
	if name != "" && t.meta.CustomTools[name] {
		return "custom_tool_call"
	}
	return "function_call"
}

// emitItemAdded 补发 output_item.added（幂等），并在此时分配正确的 id 前缀。
func (t *Translator) emitItemAdded(it *streamItem) {
	if it.added {
		return
	}
	it.added = true
	t.ensureID(it)
	if it.kind == "message" {
		t.emit("response.output_item.added", map[string]any{
			"output_index": it.index,
			"item": map[string]any{
				"id":      it.id,
				"type":    "message",
				"status":  "in_progress",
				"role":    "assistant",
				"content": []any{},
			},
		})
		return
	}
	var added map[string]any
	if it.kind == "custom_tool_call" {
		added = customToolCallItem(it.id, it.callID, it.name, "", "in_progress")
	} else {
		added = functionCallItem(it.id, it.callID, it.name, "", "in_progress")
	}
	t.emit("response.output_item.added", map[string]any{"output_index": it.index, "item": added})
}

// ensureID 在 item 首次对外可见时分配 id（类型确定后才决定 fc_/ctc_ 前缀）。
func (t *Translator) ensureID(it *streamItem) {
	if it.id != "" {
		return
	}
	switch it.kind {
	case "message":
		it.id = NewID("msg")
	case "custom_tool_call":
		it.id = NewID("ctc")
	default:
		it.id = NewID("fc")
	}
}

// emitToolDelta 发一条工具参数增量。custom 的 arguments 是 {"input":"..."} JSON
// 包装，片段无法直接当 freeform 文本下发，故只在 Finish 发一条完整 input delta。
func (t *Translator) emitToolDelta(it *streamItem, fragment string) {
	if it.kind == "custom_tool_call" {
		return
	}
	t.emit("response.function_call_arguments.delta", map[string]any{
		"item_id":      it.id,
		"output_index": it.index,
		"delta":        fragment,
	})
}

// fail 发出 error + response.failed，并锁定状态（后续 chunk 不再翻译）。
func (t *Translator) fail(code, message string) {
	if t.failed {
		return
	}
	t.failed = true
	if !t.started {
		// 直接以 error 帧开场（上游 200 + 立即报错）时也要先补 created，
		// 否则客户端拿到孤立的 error/response.failed 事件可能直接判协议错误。
		t.started = true
		created := t.response("in_progress", []any{}, nil, nil)
		t.emit("response.created", map[string]any{"response": created})
		t.emit("response.in_progress", map[string]any{"response": created})
	}
	if code == "" {
		code = "upstream_error"
	}
	if strings.TrimSpace(message) == "" {
		message = "upstream returned an error"
	}
	// 失败前把尚未注册的 item 补发 added，保证 response.output 里的 id 都出现过，
	// 且 added 排在 error/failed 之前（客户端先看到 item 生命周期再看到失败）。
	for _, it := range t.items {
		t.emitItemAdded(it)
	}
	t.emit("error", map[string]any{
		"code":    code,
		"message": message,
		"param":   nil,
	})
	resp := failedResponse(t.respID, t.meta, t.currentOutput(), code, message)
	resp["created_at"] = t.createdAt
	t.emit("response.failed", map[string]any{"response": resp})
}

// currentOutput 返回当前已累积的输出项（用于 failed 事件的 response.output）。
func (t *Translator) currentOutput() []any {
	out := make([]any, 0, len(t.items))
	for _, it := range t.items {
		t.ensureID(it)
		switch it.kind {
		case "message":
			out = append(out, messageItem(it.id, it.text.String(), "incomplete"))
		case "function_call":
			out = append(out, functionCallItem(it.id, it.callID, it.name, it.text.String(), "incomplete"))
		case "custom_tool_call":
			out = append(out, customToolCallItem(it.id, it.callID, it.name, customInputFromArguments(it.text.String()), "incomplete"))
		}
	}
	return out
}

// response 构造带固定 id / created_at 的 Response 对象。
func (t *Translator) response(status string, output []any, usage any, incomplete any) map[string]any {
	resp := buildResponse(t.respID, t.meta, status, output, usage, incomplete)
	resp["created_at"] = t.createdAt
	return resp
}

// emit 追加一条 SSE 事件到输出缓冲（type + sequence_number 自动补齐）。
func (t *Translator) emit(typ string, payload map[string]any) {
	payload["type"] = typ
	payload["sequence_number"] = t.seq
	t.seq++
	raw, err := json.Marshal(payload)
	if err != nil {
		log.Printf("WARN: [responses] marshal %s event failed: %v", typ, err)
		return
	}
	t.out.WriteString("event: ")
	t.out.WriteString(typ)
	t.out.WriteString("\ndata: ")
	t.out.Write(raw)
	t.out.WriteString("\n\n")
}

// flush 把当前缓冲一次性写出并 flush（每上游 chunk 至多一次 syscall）。
func (t *Translator) flush() error {
	if t.out.Len() == 0 {
		return nil
	}
	_, err := t.w.Write(t.out.Bytes())
	t.out.Reset()
	if t.flusher != nil {
		t.flusher.Flush()
	}
	return err
}

// errorFields 从上游 error 对象里取 code/message。
func errorFieldsForChunk(obj map[string]any) (code, message string) {
	if e, ok := obj["error"]; ok {
		return errorFields(e)
	}
	if s, ok := obj["msg"].(string); ok && strings.TrimSpace(s) != "" {
		message = s
	}
	if s, ok := obj["message"].(string); ok && strings.TrimSpace(s) != "" && message == "" {
		message = s
	}
	if v, ok := obj["code"]; ok && v != nil {
		code = fmt.Sprint(v)
	}
	if code == "" {
		code = "upstream_error"
	}
	if message == "" {
		message = "upstream error"
	}
	return code, message
}

func errorFields(v any) (code, message string) {
	obj, _ := v.(map[string]any)
	if obj == nil {
		return "upstream_error", fmt.Sprintf("%v", v)
	}
	message, _ = obj["message"].(string)
	if message == "" {
		message, _ = obj["msg"].(string)
	}
	code, _ = obj["code"].(string)
	if code == "" {
		if v, ok := obj["code"]; ok && v != nil {
			code = fmt.Sprint(v) // 上游偶尔把 code 发成数字
		}
	}
	if data, ok := obj["data"].(map[string]any); ok {
		if code == "" {
			if v, ok := data["code"]; ok && v != nil {
				code = fmt.Sprint(v)
			}
		}
		if message == "" {
			if s, ok := data["msg"].(string); ok {
				message = s
			}
			if message == "" {
				if s, ok := data["message"].(string); ok {
					message = s
				}
			}
		}
	}
	if code == "" {
		code, _ = obj["type"].(string)
	}
	if code == "" {
		code = "upstream_error"
	}
	return code, message
}
