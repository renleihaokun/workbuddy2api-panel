// 出站头形态实测工具：起一个本地假上游，让真实的 upstream.Client 打过去，
// 把收到的请求头全部打印出来。用于人工核对 capture / legacy 两种模式的差异。
//
// 用法：go run ./cmd/hdrprobe <capture|legacy>
//
// 只依赖 internal/upstream 的公开构造方式（New + ChatBase 覆盖），不改生产代码。
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func main() {
	mode := "capture"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}

	got := make(chan http.Header, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case got <- r.Header.Clone():
		default:
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndn\n"))
	}))
	defer srv.Close()

	c := upstream.New()
	c.ChatBaseCN = srv.URL
	c.BillingBaseCN = srv.URL
	c.WebBaseCN = srv.URL
	// 关掉真出网依赖：本工具只发一次 chat。
	m, err := upstream.ParseIdentityMode(mode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bad mode: %v\n", err)
		os.Exit(2)
	}
	c.SetIdentityMode(m)

	a := &auth.Auth{AccessToken: "at", UID: "uid-probe-0001", RefreshToken: "rt"}
	rc, status, _, err := c.ChatStream(a, []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`), "", upstream.ChatMeta{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "chat: status=%d err=%v\n", status, err)
		os.Exit(1)
	}
	if rc != nil {
		rc.Close()
	}

	h := <-got
	fmt.Printf("=== mode=%s chat 出站头（%d 个）===\n", mode, len(h))
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := h.Get(k)
		if k == "Authorization" {
			v = "Bearer ***"
		}
		fmt.Printf("%-30s %s\n", k+":", v)
	}

	// 顺带断言几个关键形态，便于脚本化核对。
	fmt.Println("\n=== 关键判定 ===")
	checks := []struct {
		name string
		ok   bool
	}{
		{"有 UA 且不是 Go 默认", h.Get("User-Agent") != "" && !strings.Contains(h.Get("User-Agent"), "Go-http-client")},
		{"会话三件套齐全", h.Get("X-Conversation-ID") != "" && h.Get("X-Conversation-Request-ID") != "" && h.Get("X-Conversation-Message-ID") != ""},
		{"X-Machine-ID 不存在（capture 应为 true）", h.Get("X-Machine-ID") == ""},
		{"带 X-Stainless-Lang（capture 应为 true）", h.Get("X-Stainless-Lang") != ""},
		{"带 baggage（capture 应为 true）", h.Get("baggage") != ""},
		{"Trace != 轮级 request id", h.Get("X-Trace-ID") != h.Get("X-Conversation-Request-ID")},
	}
	for _, ck := range checks {
		mark := "✗"
		if ck.ok {
			mark = "✓"
		}
		fmt.Printf("%s %s\n", mark, ck.name)
	}
	// 语言标签仅供人工核对，不参与退出码。
	if h.Get("X-Machine-ID") != "" && mode == "legacy" {
		fmt.Println("\n(legacy 模式应带 X-Machine-ID —— 上面的 ✗ 属预期)")
	}
	_ = json.Marshal
}
