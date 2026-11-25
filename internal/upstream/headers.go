// Package headers 构造各域上游请求头（common / chat / billing / refresh）。
//
// 头族按「真机抓包对齐」原则组织：每条出站路径只发官方客户端**确实会发**的头。
// 凭空多发的头（自造指纹）比少发的头更容易被识别——多出来本身就是可判别的形态。
// UA / 版本 / 归属头 / 身份三头的单一事实来源见 identity.go。
package upstream

import (
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
)

// resolveDeviceToken 解析本次请求的 X-Device-Token 取值。
// 优先级：auth.Auth.DeviceToken（每号）> Client.DeviceToken（config 全局）> 文件兜底。
// 三者皆空/读失败则返回空串（调用方不注入该头，优雅降级）。
//
// **绝不伪造**：该 token 由官方客户端的本机 Turing/QIMEI SDK 生成，网关没有等价
// 来源。凭空造一个随机值比不发更可疑——上游看到的是「格式对但校验不过」的令牌。
func (c *Client) resolveDeviceToken(a *auth.Auth) string {
	if a != nil && a.DeviceToken != "" {
		return a.DeviceToken
	}
	if c != nil && c.DeviceToken != "" {
		return c.DeviceToken
	}
	if c != nil && c.DeviceTokenFile != "" {
		return readDeviceTokenFile(c.DeviceTokenFile)
	}
	return ""
}

// injectDeviceToken 在 req 注入 X-Device-Token 头（仅当取到非空 token）。
func (c *Client) injectDeviceToken(req *http.Request, a *auth.Auth) {
	if tok := c.resolveDeviceToken(a); tok != "" {
		req.Header.Set("X-Device-Token", tok)
	}
}

// CommonHeaders 设置 RPC/REST 层共享的请求头。
//
// 这一层服务的是 refresh / report / console / portal / user-asset / models 这类
// **RPC 调用**（客户端用自建 fetch 层发出），形态与 chat 那条 SDK 链不同：
//
//   - Accept 是真机的宽松值 `application/json, text/plain, */*`（chat 用
//     ChatHeaders 收窄成 application/json）；
//   - 不发 Origin / Referer：RPC 层是 node fetch，没有浏览器同源语义；
//   - 不发 Accept-Language：桌面端 RPC 层一律不带（旧实现按 realm 发
//     zh-CN/en-US 是按账号域臆测，实测不成立）；
//   - 不发 X-Requested-With / X-CodeBuddy-Request：这两个**只有 chat**带；
//   - **不发 X-Machine-ID / X-Session-ID**：官方客户端从不发这两个头。设备标识
//     在 /v2/report 的 **body** 里（machineId / sessionId 字段）。旧实现按 uid
//     派生两个 36 hex 塞进头里，是纯粹的网关自造指纹（app.asar 与内置 cli/dist
//     全量字符串里 X-Machine-ID 命中 0 次，真机 40 个出站头逐一比对也没有）。
func (c *Client) CommonHeaders(req *http.Request, a *auth.Auth) {
	// 回退模式：整组还原改造前的形态（见 identity.go 的 ModeLegacy 注释）。
	// 放在最前面直接返回，保证「legacy = 逐字节旧行为」这条承诺不被后续
	// 字段顺序改动破坏。
	if c.identityModeIsLegacy() {
		c.injectLegacyIdentityHeaders(req, a)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	// X-Product 是 SKU 段，恒 SaaS（产品名走 X-IDE-*，见 injectAttribution）。
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("User-Agent", c.rpcUA(a))
	// X-Client-*：桌面端 5.11.3 起的 origin 级注入，按 realm 隔离（国际版不发）。
	c.injectClientIdentityHeaders(req, a)
}

// injectCodeBuddyRequest 在 req 注入 X-CodeBuddy-Request: 1。
// **仅 chat 路径调用**：真机只有 /v2/chat/completions 带该头，其余 30+ 条业务
// 路径一条都没有。旧实现放在 CommonHeaders 里「全出站覆盖」，等于给 refresh
// 等轻请求多发一个客户端从不发的头。
func (c *Client) injectCodeBuddyRequest(req *http.Request) {
	req.Header.Set("X-CodeBuddy-Request", "1")
}

// ChatMeta 一次 chat 出站的会话头族元数据。
//
// handler 在轮转循环外生成 conversationID / conversationRequestID /
// rootRequestID，循环内每次出站复用同值（换号/重试/降级同 ID → 后台按
// X-Conversation-Request-ID 聚合成一条，不再碎片化）。
// messageID（消息级，每条独立）由 ChatHeaders 内部生成。
type ChatMeta struct {
	// ConversationID 会话级 ID：多轮稳定（真机是带连字符的 UUIDv7）。
	// 入站透传优先；客户端没给则现造一个（真机三件套必须齐全，缺任一头
	// 上游回 400 / 11-128 "conversation id missing"）。
	ConversationID string
	// ConversationRequestID 对话轮级聚合主键，必发。一次 user send 内所有
	// tool call / 重试 / 换号复用同一个。
	ConversationRequestID string
	// RootRequestID 会话级**根**请求 ID（真机取 session.meta.rootRequestId，
	// 会话首个请求生成、整会话复用）。与轮级的 ConversationRequestID 是
	// **不同层**的两个值。空则回落轮级值（头不丢）。
	RootRequestID string
	// TraceID 链路 trace：入站透传值；空则独立新值——**不回落**轮级 request id。
	// 真机实测两族恒不相等，让它们恒等本身就是网关自造痕迹。
	TraceID string
	// AgentType X-Agent-Type 场景枚举：空 = main（用户主对话）；网关自发任务
	// （面板自动任务、夜猫子、标题摘要）填 auxiliary。
	AgentType string
}

// ChatHeaders 在 common 之上加 chat 专属的账号头。
//
// 这是唯一走「OpenAI Node SDK」形态的路径：带 x-stainless-*、X-Agent-*、
// X-Requested-With、X-CodeBuddy-Request，且不带 Origin/Referer/Accept-Language。
func (c *Client) ChatHeaders(req *http.Request, a *auth.Auth, clientIP string, meta ChatMeta) {
	c.CommonHeaders(req, a)
	if c.identityModeIsLegacy() {
		// 回退模式：CommonHeaders 已还原 chat 通用头，这里只补 chat 专属的账号头，
		// 并沿用改造前的「旧 world」形态（会话 ID 走旧口径由 injectConversationHeaders
		// 内部按模式分支处理）。
		c.injectLegacyChatHeaders(req, a, clientIP, meta)
		return
	}
	// Accept 收窄成真机 chat 值（客户端只发 application/json，照样收 SSE 流）。
	req.Header.Set("Accept", "application/json")
	// X-Requested-With：chat 是唯一带该头的业务链。
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	// chat UA 与 RPC 层不同（首段恒驼峰），覆盖 CommonHeaders 设的 RPC 形态。
	req.Header.Set("User-Agent", c.chatUA(a))

	// AccessToken 加锁快照：keepalive 定时刷新会在 a.mu 内改写它，锁外直读构成
	// 数据竞争（见 auth.AccessTokenValue 注释）。
	if at := a.AccessTokenValue(); at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
	}
	if a != nil && a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	// 安全红线：绝不在 chat 请求里携带 X-Refresh-Token。

	// 企业与域头：**只发有值的**。桌面端个人号不发 X-No-* 哨兵头——那是
	// CodeBuddy CLI 的约定，不是 WorkBuddy 桌面端的形态；多发一个客户端
	// 从不发的头同样是指纹。
	if a != nil && !a.IsGlobal() {
		if a.EnterpriseID != "" {
			req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		}
	}
	// X-Domain 恒发（真机 chat 必带）：global 固定国际域；CN 优先 auth 声明的
	// domain，空则回落 chat base 的主机名。不发该头本身就是可判别形态。
	req.Header.Set("X-Domain", c.chatDomain(a))

	// 用量归属头（X-IDE-* / X-Agent-Purpose / X-Product）。
	c.injectAttribution(req, a)
	// X-Private-Data：按发行版 + 企业态取（不是写死）。
	req.Header.Set("X-Private-Data", privateDataHeaderValue(a))
	// 身份头族（X-Auth-Method / X-Id-Source / X-Userinfo）：个人号三头全不发。
	c.injectIdentityHeaders(req, a)
	// 客户端 IP 透传（仅 PassthroughIP=true 且本次请求带 IP）。
	c.injectClientIP(req, clientIP)
	// 设备风控头：auth 每号 > config 全局 > 文件兜底；空则不注入。
	c.injectDeviceToken(req, a)
	// 会话头族（对话/请求/消息/B3 链路）。
	conv, convReqID := c.injectConversationHeaders(req, a, meta)
	// SDK / agent 段指纹头（x-stainless-* / x-agent-type / x-agent-intent）。
	c.injectAgentSDKHeaders(req, meta.AgentType)
	// X-CodeBuddy-Request: 1 —— 仅 chat 携带。
	c.injectCodeBuddyRequest(req)
	// X-Agent-Runtime：桌面端本地 agent 运行时。
	req.Header.Set("X-Agent-Runtime", agentRuntimeLocal)
	// baggage：W3C 传播头，成员集合与值都要与同请求的会话头一致。
	c.injectBaggage(req, conv, convReqID)
}

// injectConversationHeaders 注入官方客户端会话头族。
//
// 头族分层，各司其职（形态取自真机抓包）：
//   - X-Conversation-ID：会话级，**带连字符 UUIDv7**（如 01a1815d-dacf-75f1-…）。
//     多轮稳定。上游按三件套是否齐全做渠道校验，缺任一头即 400 / 11-128。
//   - X-Conversation-Request-ID：轮级聚合主键，必发。
//   - X-Conversation-Message-ID = X-Request-ID：消息级，**32 位 hex**，每条独立。
//   - X-Root-Request-ID：会话级根（与轮级不是同一个值）。
//   - X-Trace-ID：独立链路 ID，**不**回落 request id。
//   - X-B3-TraceId / X-B3-SpanId / X-B3-ParentSpanId / X-B3-Sampled：
//     链路族。SpanId 独立随机（复用 messageID 前缀会带出时间前缀）。
//   - b3 / traceparent：W3C 与 B3 单头，官方两版都双发。
//
// 返回最终生效的 (conversationID, conversationRequestID)，供 baggage 复用。
func (c *Client) injectConversationHeaders(req *http.Request, a *auth.Auth, meta ChatMeta) (string, string) {
	convReqID := meta.ConversationRequestID
	if convReqID == "" {
		// 零值 meta（直接调 ChatHeaders 的调用方/测试）也要保证聚合主键必发。
		convReqID = session.NewMessageID()
	}
	messageID := session.NewMessageID()

	conversationID := meta.ConversationID
	switch {
	case conversationID == "":
		// 客户端没声明会话：现造一个真机同形态的 UUIDv7。
		conversationID = session.NewTimeOrderedID()
	case strings.HasPrefix(conversationID, "d-"):
		// 派生粘性键（d-<32hex>）不能原样发给上游：d- 前缀会暴露网关特征，且无
		// 账号隔离段导致换号重试时多账号共用同一 X-Conversation-ID。按 uid+key
		// 派生为标准 32 位 hex（保持同账号同会话稳定，跨账号隔离）。
		conversationID = session.RequestIDForKey(uidOf(a) + "|" + conversationID)
	}

	req.Header.Set("X-Conversation-ID", conversationID)
	req.Header.Set("X-Conversation-Request-ID", convReqID)
	req.Header.Set("X-Conversation-Message-ID", messageID)
	req.Header.Set("X-Request-ID", messageID)

	// X-Root-Request-ID：会话根，整会话复用；调用方给不出时回落轮级（头不丢）。
	rootReqID := meta.RootRequestID
	if rootReqID == "" {
		rootReqID = convReqID
	}
	req.Header.Set("X-Root-Request-ID", rootReqID)

	// X-Trace-ID 与 request id **互不回落**：真机实测两者恒为不同的独立值。
	traceID := meta.TraceID
	if traceID == "" {
		traceID = session.NewMessageID()
	}
	req.Header.Set("X-Trace-ID", traceID)

	// B3 跟着 trace 走（官方 X-B3-TraceId == X-Trace-ID）。
	b3Trace := traceID
	if !validTraceID(b3Trace) {
		b3Trace = messageID // 入站透传值非法（含横线/超长/非 hex）→ 回落恒 32 hex
	}
	req.Header.Set("X-B3-TraceId", b3Trace)
	// SpanId 必须独立随机：真机 span 与 trace/request 均无关系。
	spanID := session.NewSpanID()
	req.Header.Set("X-B3-SpanId", spanID)
	// ParentSpanId：真机每条 chat 都带。网关没有真实 span 树，取独立随机值——
	// 缺这个头与「有 span 无 parent」在形态上是可区分的。
	parentSpanID := session.NewSpanID()
	req.Header.Set("X-B3-ParentSpanId", parentSpanID)
	req.Header.Set("X-B3-Sampled", "1")
	// B3 单头与 W3C traceparent：官方两版都双发。
	req.Header.Set("b3", b3Trace+"-"+spanID+"-1-"+parentSpanID)
	req.Header.Set("traceparent", "00-"+b3Trace+"-"+spanID+"-01")

	return conversationID, convReqID
}

// uidOf 取账号 uid（nil 安全）。
func uidOf(a *auth.Auth) string {
	if a == nil {
		return ""
	}
	return a.UID
}

// validTraceID 判断 B3 TraceId 是否合法：16 或 32 位 hex（大小写均可）。
// 入站透传值可能是任意形状（含横线/超长/非 hex），直接塞进 B3 头会破坏链路关联。
func validTraceID(s string) bool {
	if len(s) != 16 && len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')) {
			return false
		}
	}
	return true
}

// injectClientIP 在 PassthroughIP 开启时把 clientIP 参数透传给上游（三等价头）。
func (c *Client) injectClientIP(req *http.Request, clientIP string) {
	if c == nil || !c.PassthroughIP || clientIP == "" {
		return
	}
	req.Header.Set("X-Forwarded-For", clientIP)
	req.Header.Set("X-Real-IP", clientIP)
	req.Header.Set("X-Client-IP", clientIP)
}

// ExtractClientIP 从入站请求提取客户端 IP 首段（X-Forwarded-For 首段，回落 X-Real-IP）。
func ExtractClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' {
				return strings.TrimSpace(xff[:i])
			}
		}
		return strings.TrimSpace(xff)
	}
	if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
		return real
	}
	return ""
}

// BillingHeaders billing 接口请求头。
//
// 真机形态：UA 是 axios 默认串，带 X-Request-ID（纯随机 32 hex），**不带**
// X-CodeBuddy-Request 与 Accept-Language。X-Domain 恒发裸主机名。
func (c *Client) BillingHeaders(req *http.Request, a *auth.Auth) {
	// AccessToken 加锁快照（同 ChatHeaders：keepalive 可在 a.mu 内改写）。
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	// X-Request-ID：真机 billing 族必带，值为纯随机 32 hex（无时间前缀）。
	req.Header.Set("X-Request-ID", session.NewMessageID())
	if c != nil && c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	} else if ua := c.billingUA(); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	// X-Domain：优先 auth 声明的 domain，空则回落 billing base 主机名。
	if d := a.DomainValue(); d != "" {
		req.Header.Set("X-Domain", d)
	} else {
		req.Header.Set("X-Domain", c.billingHost(a))
	}
	// 设备风控头：billing 域（report/travel/balance/checkin）同样注入。
	c.injectDeviceToken(req, a)
	// X-Client-* 身份头：billing 真机同样带，与 RPC 层同口径按 realm 隔离。
	c.injectClientIdentityHeaders(req, a)
}

// RefreshHeaders refresh 端点专属头（X-Refresh-Token 只允许出现在这里）。
//
// 真机 /v2/plugin/auth/token/refresh 逐头对齐：Accept 宽松值、X-Requested-With、
// X-Product: SaaS、X-Refresh-Token / X-Auth-Refresh-Source、X-Request-ID、
// UA（三段）+ Authorization + X-User-Id + X-Domain。
// 该路径**不发** X-CodeBuddy-Request 与 Accept-Language。
func (c *Client) RefreshHeaders(req *http.Request, a *auth.Auth) {
	c.CommonHeaders(req, a)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Refresh-Token", a.RefreshToken)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	}
	// X-Auth-Refresh-Source 对齐官方客户端 refresh 渠道标识 "plugin"（D3）。
	req.Header.Set("X-Auth-Refresh-Source", "plugin")
	// Authorization：真机 refresh **同时**带旧的 access token 与 refresh token。
	// 注意与 chat 相反——chat 绝不带 X-Refresh-Token，refresh 则双发。
	if at := a.AccessTokenValue(); at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
	}
	req.Header.Set("X-Request-ID", session.NewMessageID())
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if d := a.DomainValue(); d != "" {
		req.Header.Set("X-Domain", d)
	} else {
		req.Header.Set("X-Domain", c.chatHost(a))
	}
}
