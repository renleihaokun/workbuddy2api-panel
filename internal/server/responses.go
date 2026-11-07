// responses.go 实现 OpenAI Responses API（POST /v1/responses）。
//
// 接入方式是「入站改写 + 出站垫片」，而不是重写一遍补全管线：
//  1. 用 responses.ConvertRequest 把 Responses 请求改写成 chat.completions 请求；
//  2. 直接调用既有的 chatCompletions（鉴权、提示词改写、realm 路由、粘性、轮转、
//     错误分类、标记修复、用量记账全部复用）；
//  3. 用一个 ResponseWriter 垫片截获 chat 的 JSON / SSE 输出，翻译成 Responses
//     的 Response 对象 / 事件流。
//
// 这样 /v1/chat/completions 的行为逐字节不变，新协议的唯一失败面收在翻译层。
package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/responses"
)

// responses 处理 POST /v1/responses。
func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	// 与 chat 端点共用同一道 64 MiB 安全网：上限内仍交上游判定，越过才由网关 413。
	body, err := readBodyLimited(w, r)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeResponsesError(w, http.StatusRequestEntityTooLarge, "request_too_large", bodyTooLargeMsg())
			return
		}
		writeResponsesError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	chatBody, meta, err := responses.ConvertRequest(body)
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if meta.Unsupported != "" {
		// previous_response_id / background / conversation / prompt：明确拒绝，
		// 不做「假装支持」——静默降级会让客户端以为服务端记住了上下文。
		writeResponsesError(w, http.StatusBadRequest, "unsupported_parameter", meta.Unsupported)
		return
	}

	// 子请求只改 body 与 path，其余头（Authorization / X-Conversation-* / UA）原样
	// 保留——会话头族与来源信息是网关既有能力的一部分。
	sub := r.Clone(r.Context())
	sub.Body = io.NopCloser(bytes.NewReader(chatBody))
	sub.ContentLength = int64(len(chatBody))
	sub.URL.Path = "/v1/chat/completions"
	sub.RequestURI = "/v1/chat/completions"

	rw := &responsesWriter{ResponseWriter: w, meta: meta}
	h.chatCompletions(rw, sub)
	if err := rw.finish(); err != nil {
		log.Printf("WARN: [server] responses %s: finish failed: %v", meta.Model, err)
	}
}

// responsesWriter 截获 chatCompletions 的写出并翻译成 Responses 形态。
//
// 分流规则：
//   - 状态码 >= 400：原样透传（既有 {"error":{...}} 信封与 Responses 的错误信封
//     同形，Codex/SDK 都能解析 error.message）；
//   - Content-Type 为 text/event-stream：逐帧喂给 Translator；
//   - 其余（application/json）：先缓冲，finish 时整体翻译成 Response 对象。
type responsesWriter struct {
	http.ResponseWriter
	meta responses.Meta

	status        int
	headerWritten bool
	passthrough   bool

	buf bytes.Buffer
	tr  *responses.Translator
	// trBroken 翻译写失败后置位：客户端已断连时后续帧直接丢弃，避免逐帧刷 WARN。
	trBroken bool
}

func (w *responsesWriter) WriteHeader(code int) {
	if w.headerWritten {
		return
	}
	w.headerWritten = true
	w.status = code
	if code >= 400 {
		w.passthrough = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *responsesWriter) Write(p []byte) (int, error) {
	if w.passthrough {
		if w.status >= 400 {
			// 错误体先缓冲：finish 时把 chat 管线的 api_error 词表改写成
			// Responses 的 invalid_request_error / rate_limit_error / server_error。
			return w.buf.Write(p)
		}
		return w.ResponseWriter.Write(p)
	}
	if w.headerWritten && w.status >= 400 {
		// 防御：WriteHeader 已把 >=400 置 passthrough；这里只兜底极端的
		// 直接写 Header 后不走 WriteHeader 的调用形态。
		w.passthrough = true
		return w.buf.Write(p)
	}
	if strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
		if w.trBroken {
			return len(p), nil
		}
		if w.tr == nil {
			w.tr = responses.NewTranslator(w.ResponseWriter, w.meta)
		}
		if err := w.tr.WriteFrame(p); err != nil {
			// 写失败 = 客户端已断连。必须把错误回灌给 StreamHint，让它立刻停止
			// 读上游（否则上游 token 继续计费、账号还会被记成功）。
			w.trBroken = true
			log.Printf("WARN: [server] responses %s: stream write failed: %v", w.meta.Model, err)
			return 0, err
		}
		return len(p), nil
	}
	return w.buf.Write(p)
}

// Flush 透传给底层 writer：StreamHint 依赖 http.Flusher 断言做逐帧刷新。
func (w *responsesWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 支持 http.ResponseController 一类的底层访问。
func (w *responsesWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// finish 在 chatCompletions 返回后收尾：
//   - 流式：关闭所有打开的 item，发 response.completed；
//   - 非流式：把缓冲的 chat.completion 翻译成 Response 对象写出。
func (w *responsesWriter) finish() error {
	if w.passthrough {
		if w.status >= 400 && w.buf.Len() > 0 {
			_, err := w.ResponseWriter.Write(rewriteResponsesErrorBody(w.buf.Bytes(), w.status))
			return err
		}
		return nil
	}
	if w.tr != nil {
		if w.trBroken {
			return nil
		}
		var streamErr error
		if !w.tr.SawDone() {
			// 没见到收尾 [DONE]：StreamHint 在读上游时出错提前返回了。半截正文
			// 绝不能标 completed，否则客户端会把截断内容当最终答案。
			streamErr = errors.New("upstream stream ended before [DONE]")
		}
		return w.tr.Finish(streamErr)
	}
	if w.tr == nil && w.buf.Len() == 0 {
		if w.meta.Stream {
			// 上游 200 响应头之后、首帧之前就断流：StreamHint 会直接返回且不写
			// 任何字节，客户端否则只能拿到「200 + 空 body」。补一个完整失败流。
			if w.Header().Get("Content-Type") == "" {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("Connection", "keep-alive")
			}
			tr := responses.NewTranslator(w.ResponseWriter, w.meta)
			return tr.Finish(errors.New("upstream stream ended before any frame"))
		}
		return nil
	}
	var chat map[string]any
	if err := json.Unmarshal(w.buf.Bytes(), &chat); err != nil {
		// 200 已写出，只能补一个可解析的错误体，避免客户端拿到空 body。
		fallback := map[string]any{"error": map[string]any{
			"message": "failed to translate upstream response: " + err.Error(),
			"type":    "server_error",
			"code":    "upstream_parse",
		}}
		raw, _ := json.Marshal(fallback)
		_, werr := w.ResponseWriter.Write(raw)
		return werr
	}
	resp := responses.FromChatCompletion(chat, w.meta)
	raw, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	_, err = w.ResponseWriter.Write(raw)
	return err
}

// rewriteResponsesErrorBody 把 chat 管线的 api_error 词表改写成 Responses 的
// 标准错误类型（message/code/gateway_hint 等字段原样保留）。
func rewriteResponsesErrorBody(raw []byte, status int) []byte {
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return raw
	}
	errObj, ok := payload["error"].(map[string]any)
	if !ok {
		return raw
	}
	typ, _ := errObj["type"].(string)
	if typ == "" || typ == "api_error" {
		errObj["type"] = responsesErrorType(status)
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return raw
	}
	return out
}

// writeResponsesError 按 Responses 的错误信封写出。
func writeResponsesError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    responsesErrorType(status),
			"param":   nil,
			"code":    code,
		},
	})
}

// responsesErrorType 把 HTTP 状态映射成 OpenAI 的错误类型词表。
func responsesErrorType(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status >= 500:
		return "server_error"
	default:
		return "invalid_request_error"
	}
}
