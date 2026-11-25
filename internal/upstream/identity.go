// identity.go 出站身份（UA / 版本 / 归属头）的单一事实来源。
//
// 设计原则（2026-11 对齐真机抓包）：凡是能指到「官方分发包解包 / 真机抓包」
// 的地方就对齐；指不到的地方宁可不发。凭空补一个客户端从不发的头，比少发
// 一个头更容易被识别——「多出来」本身就是可判别的指纹。
//
// 三族 UA 必须分开，因为它们来自官方客户端**三条不同的代码路径**：
//
//	chat     → OpenAI Node SDK 的 buildUserAgent：
//	           `WorkBuddy/<v> <brand>/<v> CLI/<cliV>`，首段**恒驼峰 productName**
//	RPC/REST → 客户端自建 fetch 层：
//	           `<brand>/<v> <brand>/<v> CLI/<cliV>`，首段即品牌 slug
//	billing  → billing 域的 axios 实例：
//	           `axios/1.16.1`，完全不带品牌段
//
// 版本号来源：官方分发包 install-manifest.json（appVersion）+ cli/product.json
// （genieVersion）。CN 与国际版**分属两个发行档**，版本与头集都不通用——所以
// 下面每个默认值都按 realm 分。
package upstream

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
)

const (
	// defaultClientVersionCN 国内版 WorkBuddy 客户端版本（分发包 appVersion）。
	defaultClientVersionCN = "5.11.3"
	// defaultClientVersionGlobal 国际版 WorkBuddy AI 客户端版本。
	// 国际分发包更新更慢，与国内版不同档；套用国内版本等于给国际链路加一层
	// 它不存在的指纹。
	defaultClientVersionGlobal = "5.9.4"

	// defaultCliVersionCN 国内版分发包内置 CLI（@genie/agent-cli）版本。
	defaultCliVersionCN = "2.176.0"
	// defaultCliVersionGlobal 国际版分发包内置 CLI 版本。
	defaultCliVersionGlobal = "2.165.0"

	// billingUADefault billing 域出站 UA：axios 默认串。
	// 真机双版本实测，billing 层（summary / packages / checkin 等）走 axios
	// 实例而非 SDK 客户端，发的就是它，不带任何品牌段。
	billingUADefault = "axios/1.16.1"

	// originRefererCN / originRefererGlobal 只给 **web 层**（渲染器/登录页那类
	// 浏览器语义请求）用。RPC 层是 node fetch 调用，没有同源语义，一律不发。
	//
	// CN 值是 www.workbuddy.cn：codebuddy.cn 只是 billing 的 API 主机名，
	// 桌面端从不用它做 Origin（旧值属 CodeBuddy CLI 时代遗留）。
	originRefererCN     = "https://www.workbuddy.cn"
	originRefererGlobal = "https://www.workbuddy.ai"

	// clientIdentityPlatform / clientIdentityProduct 桌面端 5.11.3 新增的
	// X-Client-* 头里的两段固定值（国内版全量样本恒为 windows / WorkBuddy）。
	clientIdentityPlatform = "windows"
	clientIdentityProduct  = "WorkBuddy"

	// agentIntentCraft chat 出站的 X-Agent-Intent 固定值。
	agentIntentCraft = "craft"
	// agentPurposeConversation 用量归属头的 X-Agent-Purpose 固定值。
	agentPurposeConversation = "conversation"
	// agentRuntimeLocal 桌面端本地 agent 运行时枚举值。
	agentRuntimeLocal = "local-agent"

	// AgentTypeMain 用户主对话的 X-Agent-Type 取值。
	AgentTypeMain = "main"
	// AgentTypeAuxiliary 网关自发/辅助链（面板任务、夜猫子、标题摘要）的取值。
	// 真机另有 compaction / subagent / team 三档，网关没有对应路径，不臆造。
	AgentTypeAuxiliary = "auxiliary"
)

// OpenAI Node SDK 指纹常量（官方客户端 chat 请求由该 SDK 发出，这些头是
// SDK 自动加的）。网关是 Go 实现，没有对应运行时——按官方实测值固定，
// 理由是「客户端形态一致性」优先：缺这几个头，上游看到的是残缺的头集合。
const (
	clientSDKLang       = "js"
	clientSDKVersion    = "6.25.0"
	clientSDKOS         = "Windows"
	clientSDKArch       = "x64"
	clientSDKRuntime    = "node"
	clientSDKNodeVer    = "v22.21.1"
	clientSDKRetryCount = "0"
)

// originRefererFor 按账号 realm 返回 web 层的 Origin/Referer 基础域。
func originRefererFor(a *auth.Auth) string {
	if a != nil && a.IsGlobal() {
		return originRefererGlobal
	}
	return originRefererCN
}

// clientVersion 生效的 WorkBuddy 客户端版本：Client.ClientVersion 非空取之，
// 否则按 realm 取内置默认（CN 与国际版分属两个发行档）。
func (c *Client) clientVersion(a *auth.Auth) string {
	if c != nil && c.ClientVersion != "" {
		return c.ClientVersion
	}
	if a != nil && a.IsGlobal() {
		return defaultClientVersionGlobal
	}
	return defaultClientVersionCN
}

// cliVersion 生效的内置 CLI 版本：Client.CliVersion 非空取之，否则按 realm 取默认。
func (c *Client) cliVersion(a *auth.Auth) string {
	if c != nil && c.CliVersion != "" {
		return c.CliVersion
	}
	if a != nil && a.IsGlobal() {
		return defaultCliVersionGlobal
	}
	return defaultCliVersionCN
}

// uaBrandFor 出站 UA 的品牌段（chat 的**第二段**与 RPC 的**首段**）。
//
// 两版分发包的品牌改写规则不同，不能统一：
//   - 国内版 productName `WorkBuddy`，打包期原样沿用驼峰；
//   - 国际版 productName `WorkBuddy AI`，经 Electron userAgentFallback 的
//     asciiSlug 处理（非字母数字换 `-`、转小写）→ `workbuddy-ai`。
//
// 反例警示：曾把 CN 也写成小写 `workbuddy`，与实测不符——CN 每个业务请求
// 都是驼峰，写成 slug 反而成了最明显的非官方指纹。
func uaBrandFor(a *auth.Auth) string {
	if a != nil && a.IsGlobal() {
		return "workbuddy-ai"
	}
	return "WorkBuddy"
}

// chatUA chat 链路的出站 UA（OpenAI Node SDK 的 buildUserAgent 形态）。
// 首段**恒为驼峰 `WorkBuddy/`**，只有第二段随 realm 换品牌。
//
//	CN     5.11.3 → WorkBuddy/5.11.3 WorkBuddy/5.11.3 CLI/2.176.0
//	global 5.9.4  → WorkBuddy/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0
func (c *Client) chatUA(a *auth.Auth) string {
	if c != nil && c.UserAgent != "" {
		return c.UserAgent
	}
	ver := c.clientVersion(a)
	return "WorkBuddy/" + ver + " " + uaBrandFor(a) + "/" + ver + " CLI/" + c.cliVersion(a)
}

// rpcUA RPC/REST 层出站 UA（report / refresh / console / portal / user-asset）。
// 与 chat 的差别只在**首段**：RPC 层首段就是品牌 slug（global 是 workbuddy-ai）。
//
//	CN     5.11.3 → WorkBuddy/5.11.3 WorkBuddy/5.11.3 CLI/2.176.0
//	global 5.9.4  → workbuddy-ai/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0
func (c *Client) rpcUA(a *auth.Auth) string {
	if c != nil && c.UserAgent != "" {
		return c.UserAgent
	}
	ver := c.clientVersion(a)
	brand := uaBrandFor(a)
	return brand + "/" + ver + " " + brand + "/" + ver + " CLI/" + c.cliVersion(a)
}

// billingUA billing 域出站 UA：恒为 axios 默认串（显式 client_name="SaaS" 时
// 不设 UA，保留既有逃生开关——那会还原成 Go 默认 UA）。
func (c *Client) billingUA() string {
	if c == nil || c.attributionClientName() == "SaaS" {
		return ""
	}
	return billingUADefault
}

// v3ConfigUA /v3/config 目录探测的官方 UA：两段单品牌。
//
// 该端点对 UA 敏感且历史上有过模型集合差异，但 2026-10-30 双版本实测
// **两族返回逐字节相同**，真机唯一形态就是这条两段式。旧的
// `CodeBuddyIDE/4.12.0 CodeBuddy/4.12.0` 是从旧版 IDE 客户端抄的，WorkBuddy
// 分发包里根本不存在这两个串，属最显眼的非官方指纹。
func (c *Client) v3ConfigUA(a *auth.Auth) string {
	return "WorkBuddy/" + c.clientVersion(a) + " CLI/" + c.cliVersion(a)
}

// injectClientIdentityHeaders 注入 X-Client-* 三头（桌面端 5.11.3 起新增）。
//
// **只对国内版发**：国际分发包停在旧档，app.asar 里没有这层
// registerOutboundHeaderProvider 注入，官方国际客户端从不发这三个头。
// 版本号本来就按 realm 各取各的，头集也必须各归各的——不能因为「国内升级」
// 就让国际链路跟着变形状。
func (c *Client) injectClientIdentityHeaders(req *http.Request, a *auth.Auth) {
	if a == nil || a.IsGlobal() {
		return
	}
	req.Header.Set("X-Client-Version", c.clientVersion(a))
	req.Header.Set("X-Client-Platform", clientIdentityPlatform)
	req.Header.Set("X-Client-Product", clientIdentityProduct)
}

// dropClientIdentityHeaders 撤掉 X-Client-* 三头，供**小程序口径**的请求使用。
//
// 这三头来自桌面端的 origin 级注入，小程序链路根本不存在这一层。这些路径
// 先走 BillingHeaders（CN 号会把三头打上）再覆盖 X-Client-Platform:
// miniprogram，不撤就会发出 `5.11.3 / miniprogram / WorkBuddy` —— 桌面端不会
// 说 miniprogram，小程序也不会自报 5.11.3，是个谁都不像的第四种组合。
func (c *Client) dropClientIdentityHeaders(req *http.Request) {
	req.Header.Del("X-Client-Version")
	req.Header.Del("X-Client-Platform")
	req.Header.Del("X-Client-Product")
}

// privateDataHeaderValue 出站 X-Private-Data 取值。
//
// 客户端逻辑（cli/dist 反编译 applyPrivateDataHeaderPolicy）：
//
//	A = resolveModelOptimizationEnabled({explicit, productDefault, isEnterprise})
//	X-Private-Data = A ? "false" : "true"
//
// 即「开了模型优化 → 声明 false（允许用于改进）」。判定维度是**发行版 +
// 企业态**，不是账号个人属性：
//   - 企业版默认关优化 → "true"
//   - 国际版发行包 DefaultModelOptimizationOff=true → 个人号也 "true"
//   - 国内版 DefaultModelOptimizationOff=false → 个人号 "false"
//
// 旧实现写死 "true"，与客户端默认态相反（每条 chat 都错）。
func privateDataHeaderValue(a *auth.Auth) string {
	if a == nil {
		return "false"
	}
	if a.IsEnterprise() {
		return "true"
	}
	if a.IsGlobal() {
		return "true"
	}
	return "false"
}

// injectAgentSDKHeaders 注入 x-stainless-* 与 x-agent-* 两族（仅 chat 路径）。
//
// 这些是官方 chat 出站的固定形态：x-stainless-* 来自 OpenAI Node SDK 自动
// 注入，x-agent-* 来自业务层。billing / checkin 等 REST 域没有真机对照证据，
// 不臆造。
func (c *Client) injectAgentSDKHeaders(req *http.Request, agentType string) {
	if agentType == "" {
		agentType = AgentTypeMain
	}
	req.Header.Set("X-Agent-Type", agentType)
	req.Header.Set("X-Agent-Intent", agentIntentCraft)
	req.Header.Set("X-Stainless-Lang", clientSDKLang)
	req.Header.Set("X-Stainless-Package-Version", clientSDKVersion)
	req.Header.Set("X-Stainless-OS", clientSDKOS)
	req.Header.Set("X-Stainless-Arch", clientSDKArch)
	req.Header.Set("X-Stainless-Runtime", clientSDKRuntime)
	req.Header.Set("X-Stainless-Runtime-Version", clientSDKNodeVer)
	req.Header.Set("X-Stainless-Retry-Count", clientSDKRetryCount)
}

// injectBaggage 注入 W3C baggage 头（chat 出站形态）。
//
// 真机逐字形态：`buddyId=,buddyName=,codebuddy.session_id=<conv>,codebuddy.conversation_request_id=<turn>`
//
// 只有 4 个成员，两个 buddy 键**留空值**——空值也发是 OTel baggage 序列化
// 的形态（`k=`），不是漏填。曾经额外注入 x-agent-runtime / x-agent-intent
// 两个成员，真机报文里没有；baggage 是整体序列化的串，成员集合本身就是
// 可比对的特征，多一个就露。
func (c *Client) injectBaggage(req *http.Request, conv, reqID string) {
	req.Header.Set("baggage",
		"buddyId=,buddyName=,codebuddy.session_id="+conv+
			",codebuddy.conversation_request_id="+reqID)
}

// attributionClientName 生效的用量归属名：ClientName 非空取之；空默认 WorkBuddy。
func (c *Client) attributionClientName() string {
	if c != nil && c.ClientName != "" {
		return c.ClientName
	}
	return "WorkBuddy"
}

// injectAttribution 注入用量归属头（X-Agent-Purpose / X-IDE-* / X-Product）。
//
// X-Product 恒为 "SaaS" —— 它是 SKU 段，不是产品名；产品名只出现在
// X-IDE-*。旧实现让 X-Product 跟随 client_name（默认 WorkBuddy），于是同一个
// 头官方发 SaaS、网关发 WorkBuddy——「同名不同值」比多一个头更容易被规则命中。
func (c *Client) injectAttribution(req *http.Request, a *auth.Auth) {
	name := c.attributionClientName()
	req.Header.Set("X-Product", "SaaS")
	if name == "SaaS" {
		// 显式 ClientName=SaaS：仅声明 SKU，不补 X-IDE-*（旧行为，保留兼容）。
		return
	}
	req.Header.Set("X-Agent-Purpose", agentPurposeConversation)
	req.Header.Set("X-IDE-Name", name)
	req.Header.Set("X-IDE-Type", name)
	req.Header.Set("X-IDE-Version", c.clientVersion(a))
}

// injectIdentityHeaders 注入官方客户端的身份头族（X-Auth-Method / X-Id-Source /
// X-Userinfo）。三个头都是**按账号形态条件发送**，不是常量：
//
//	有 uid        → X-User-Id = uid
//	有 enterprise → X-Enterprise-Id
//	有 auth.method → X-Auth-Method
//	有 idSource   → X-Id-Source
//	有 uid 且 (enterprise|idSource|method 任一) → X-Userinfo = base64(JSON{...})
//
// X-Userinfo 的每段都是条件拼接：缺哪个维度就不出现该键，而不是写空串。
// 个人号三个维度全缺 → 三头全不发（实测两个版本的个人号出站头里都没有）。
// 这不是省略，是与客户端一致：凭「个人号也该有」去补空值反而是伪造。
//
// token_source 只在企业态下才进 X-Userinfo：它描述的是 token 的来路（企业
// 切换），不是身份语义，个人号那条是干扰项。
func (c *Client) injectIdentityHeaders(req *http.Request, a *auth.Auth) {
	if a == nil || a.UID == "" {
		return
	}
	enterprise := strings.TrimSpace(a.EnterpriseID)
	method := a.TokenSource()
	idSource := a.IdSource()

	if idSource != "" {
		req.Header.Set("X-Id-Source", idSource)
	}
	if enterprise == "" && idSource == "" {
		return
	}
	if enterprise != "" && method != "" {
		req.Header.Set("X-Auth-Method", method)
	}
	payload := map[string]any{"uin": a.UID}
	if enterprise != "" {
		payload["owner_uin"] = enterprise
	}
	if idSource != "" {
		payload["id_source"] = idSource
	}
	if enterprise != "" && method != "" {
		payload["token_source"] = method
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	req.Header.Set("X-Userinfo", base64.StdEncoding.EncodeToString(raw))
}

// deviceID 由 uid + 用途派生一个**带连字符的 UUID 形态**设备标识。
//
// 用途：
//   - purpose="machine" → /v2/report body 里的 machineId（设备级，跨会话稳定）
//   - purpose="session" → /v2/report body 里的 sessionId（账号固定会话）
//   - purpose="webmachine" → web 域上报事件的 machineId
//
// 为什么是 UUID 形态而不是 36 hex：真机客户端的 machineId 取自 OS 硬件 UUID、
// sessionId 取自每进程的 randomUUID，**两者都是带连字符的 UUID**。此前这里输出
// 36 位无连字符 hex，形态对不上，本身就是一处「网关特征」。
//
// 为什么按账号派生而不是进程维度：官方「一个进程 = 一台设备」，网关是一个长驻
// 进程同时冒充几十台设备——按进程取值等于让所有账号共用同一个值，那是把整个
// 账号池串起来的关联键。
//
// 注意：这些是 **body 字段**，不是 HTTP 头。官方 chat 请求头里没有机器标识。
func deviceID(a *auth.Auth, purpose string) string {
	uid := ""
	if a != nil {
		uid = a.UID
	}
	if uid == "" {
		uid = "anonymous"
	}
	sum := sha256.Sum256([]byte("wb2a:" + purpose + ":" + uid))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x40 // 版本位 = 4（真机 machineId 是 v4 形态）
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 变体位
	return formatUUIDBytes(b)
}

// formatUUIDBytes 把 16 字节格式化成 8-4-4-4-12 的带连字符形态。
func formatUUIDBytes(b []byte) string {
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// webBrowserUA web 层（浏览器语义请求）的出站 UA。
//
// web 层的请求由渲染器（Chromium）发出，UA 是浏览器串而不是客户端三段式。
// 与 RPC / chat 两族混用会把「同一个号同一时刻存在两种客户端指纹」暴露出去。
const webBrowserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"

// userAgentForWeb web 层生效的 UA：显式配置优先，否则浏览器串。
func (c *Client) userAgentForWeb(a *auth.Auth) string {
	if c != nil && c.UserAgent != "" {
		return c.UserAgent
	}
	return webBrowserUA
}

// IdentityMode 出站身份实现模式。
//
// 取值由 config upstream.identity_mode 控制，原子热切换：
//   - ModeCapture（默认）：对齐真机抓包的新实现——不发自造头、UA 分三族、
//     带齐 x-stainless-*/X-Agent-*/baggage 等真机头族。
//   - ModeLegacy：**逐字节还原改造前**的出站形态（X-Machine-ID/X-Session-ID、
//     Origin/Referer/Accept-Language 全覆盖、旧 UA 串）。逃生门——若上游对
//     新形态有意外反应，改一行配置即可回退，不必回滚二进制。
//
// 为什么需要逃生门：本次所有真机口径来自同类项目的逆向结论，我们没有自己的
// 抓包能本地自证。生产上必须留一条「立刻回到已知可跑形态」的路。
type IdentityMode int32

const (
	// ModeCapture 对齐真机的出站身份（默认）。
	ModeCapture IdentityMode = iota
	// ModeLegacy 改造前的出站身份（回退用）。
	ModeLegacy
)

// legacyClientVersion / legacyCliVersion 改造前的内置版本段。
// 仅 ModeLegacy 使用：回退时必须连 UA 版本一起还原，否则「旧头族 + 新版本号」
// 是一个谁都没见过的组合。
const (
	legacyClientVersion = "5.5.4"
	legacyCliVersion    = "2.137.1"
)

// ParseIdentityMode 把配置字符串归一化为模式；空 = ModeCapture（默认）。
// 非法值报错而不是静默回落——静默回落会让「配了 legacy 却没生效」变成
// 一次危险的误判。
func ParseIdentityMode(s string) (IdentityMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "capture", "official":
		return ModeCapture, nil
	case "legacy", "compat":
		return ModeLegacy, nil
	default:
		return ModeCapture, fmt.Errorf("upstream.identity_mode 只支持 capture 或 legacy，收到 %q", s)
	}
}

// SetIdentityMode 原子切换出站身份模式。
func (c *Client) SetIdentityMode(m IdentityMode) { c.identityMode.Store(int32(m)) }

// identityModeIsLegacy 报告当前是否处于回退模式（零值 = ModeCapture）。
func (c *Client) identityModeIsLegacy() bool {
	return c == nil || IdentityMode(c.identityMode.Load()) == ModeLegacy
}

// legacyUA 改造前的三段式 UA（仅回退模式使用）。
func legacyUA(a *auth.Auth) string {
	platform := "WorkBuddy"
	if a != nil && a.IsGlobal() {
		platform = "WorkBuddy AI"
	}
	return "WorkBuddy/" + legacyClientVersion + " " + platform + "/" + legacyClientVersion +
		" CLI/" + legacyCliVersion
}

// legacyDeviceID 改造前的 36 hex 无连字符派生（仅回退模式使用）。
// 回退时 X-Machine-ID / X-Session-ID 必须复现原值，否则「退回旧模式」并不能
// 真正回到已知可跑的形态。
func legacyDeviceID(uid, purpose string) string {
	sum := sha256.Sum256([]byte("wb2a:" + purpose + ":" + uid))
	return hex.EncodeToString(sum[:18])
}

// injectLegacyIdentityHeaders 回退模式在 RPC 层补回改造前那一整套头。
//
// 这一组全是**自造或过多**的形态（官方客户端从不发 X-Machine-ID，
// Origin/Referer/Accept-Language 在 RPC 层也不发）——只在回退模式下出现。
func (c *Client) injectLegacyIdentityHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Accept", "application/json")
	origin := originRefererFor(a)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", legacyUA(a))
	req.Header.Set("X-CodeBuddy-Request", "1")
	if a != nil && a.IsGlobal() {
		req.Header.Set("Accept-Language", "en-US")
	} else {
		req.Header.Set("Accept-Language", "zh-CN")
	}
	if a != nil && a.UID != "" {
		req.Header.Set("X-Machine-ID", legacyDeviceID(a.UID, "machine"))
		req.Header.Set("X-Session-ID", legacyDeviceID(a.UID, "session"))
	}
}

// injectLegacyChatHeaders 回退模式的 chat 专属头（复现改造前那一套）。
//
// 与注入顺序无关：CommonHeaders 已设了通用头，这里只补 chat 差异项，
// 目标是与改造前的 ChatHeaders 输出逐字节一致。
//
// nil auth 安全：回退模式是生产逃生门，绝不能因为某个调用点传了 nil 就 panic
// （那会让「切到 legacy 保平安」变成服务不可用）。
func (c *Client) injectLegacyChatHeaders(req *http.Request, a *auth.Auth, clientIP string, meta ChatMeta) {
	req.Header.Set("Accept", "application/json, text/event-stream")
	if at := a.AccessTokenValue(); at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if a != nil && a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	// 改造前的企业/域头分支：CN 用 X-No-* 哨兵，global 声明国际域。
	if a != nil && a.IsGlobal() {
		req.Header.Set("X-No-Enterprise-Id", "1")
		req.Header.Set("X-Domain", "www.workbuddy.ai")
	} else {
		if a != nil && a.EnterpriseID != "" {
			req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		} else {
			req.Header.Set("X-No-Enterprise-Id", "1")
		}
		// 改造前：domain 空则发 X-No-Department-Info 而不发 X-Domain。
		if a != nil {
			if d := a.DomainValue(); d != "" {
				req.Header.Set("X-Domain", d)
			} else {
				req.Header.Set("X-No-Department-Info", "1")
			}
		} else {
			req.Header.Set("X-No-Department-Info", "1")
		}
	}
	// 改造前的归属头：X-Product 跟随 client_name（默认 WorkBuddy），四头齐发。
	name := c.attributionClientName()
	if name == "SaaS" {
		req.Header.Set("X-Product", "SaaS")
	} else {
		req.Header.Set("X-Agent-Purpose", "conversation")
		req.Header.Set("X-IDE-Name", name)
		req.Header.Set("X-IDE-Type", name)
		req.Header.Set("X-IDE-Version", legacyClientVersion)
		req.Header.Set("X-Product", name)
	}
	c.injectClientIP(req, clientIP)
	c.injectDeviceToken(req, a)
	// 改造前的会话头族：32 hex 会话 ID、Trace 回落 request id、Root = 轮级、
	// SpanId = messageID[:16]（这些正是本次要修的形态问题）。
	c.injectLegacyConversationHeaders(req, meta)
}

// injectLegacyConversationHeaders 改造前的会话头族（回退模式专用）。
func (c *Client) injectLegacyConversationHeaders(req *http.Request, meta ChatMeta) {
	convReqID := meta.ConversationRequestID
	if convReqID == "" {
		convReqID = session.NewMessageID()
	}
	messageID := session.NewMessageID()
	conversationID := meta.ConversationID
	if conversationID == "" {
		conversationID = session.NewMessageID()
	} else if strings.HasPrefix(conversationID, "d-") {
		conversationID = session.RequestIDForKey(req.Header.Get("X-User-Id") + "|" + conversationID)
	}
	req.Header.Set("X-Conversation-ID", conversationID)
	req.Header.Set("X-Conversation-Request-ID", convReqID)
	req.Header.Set("X-Conversation-Message-ID", messageID)
	req.Header.Set("X-Request-ID", messageID)
	req.Header.Set("X-Root-Request-ID", convReqID)
	traceID := meta.TraceID
	if traceID == "" {
		traceID = convReqID // 改造前：Trace 回落轮级（本次修掉的形态）
	}
	req.Header.Set("X-Trace-ID", traceID)
	b3Trace := convReqID
	if !validTraceID(b3Trace) {
		b3Trace = messageID
	}
	req.Header.Set("X-B3-TraceId", b3Trace)
	req.Header.Set("X-B3-SpanId", messageID[:16]) // 改造前：复用消息 ID 前缀
	req.Header.Set("X-B3-Sampled", "1")
}

// hostOf 从 base URL 剥出裸主机名（去 scheme 与路径）。X-Domain 恒为主机名，
// 不带 scheme/端口。空 base 回落 fallback。
func hostOf(base, fallback string) string {
	if i := strings.Index(base, "://"); i >= 0 {
		base = base[i+3:]
	}
	if i := strings.IndexByte(base, '/'); i >= 0 {
		base = base[:i]
	}
	if base == "" {
		return fallback
	}
	return base
}

// chatHost 当前 chat base 的裸主机名（X-Domain 用）。
func (c *Client) chatHost(a *auth.Auth) string {
	return hostOf(c.chatBase(a), "www.workbuddy.cn")
}

// chatDomain chat 出站的 X-Domain 取值（裸主机名）。**恒非空**——真机每条 chat
// 都带该头，缺了本身就是可判别形态。
//
//   - global：固定 www.workbuddy.ai（与 Origin/Referer 同域）；
//   - CN：优先 auth 文件声明的 domain，空则回落 chat base 的主机名。
func (c *Client) chatDomain(a *auth.Auth) string {
	if a != nil && a.IsGlobal() {
		return "www.workbuddy.ai"
	}
	if a != nil {
		if d := a.DomainValue(); d != "" {
			return hostOf("https://"+strings.TrimPrefix(strings.TrimPrefix(d, "https://"), "http://"), "")
		}
	}
	return c.chatHost(a)
}

// billingHost 当前 billing base 的裸主机名（X-Domain 兜底用）。
func (c *Client) billingHost(a *auth.Auth) string {
	return hostOf(c.billingBase(a), "www.workbuddy.cn")
}
