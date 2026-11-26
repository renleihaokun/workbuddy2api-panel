// verify.go 会话是否真失效的「三态求证」。
//
// 为什么需要它（同类现场 + 本仓 issue #215）：上游 401 有两种成因，响应体
// 长得一模一样——
//  1. 会话真被注销：refresh 会被应用层明确拒绝（12153 / invalid_grant 类），只能重登；
//  2. 网关/边缘的瞬时拦截（APISIX HTML 401、风控抖动、并发限流）：同一 token
//     过一会儿又能用，refresh 也会成功。
//
// 只按 401 计数会误杀第 2 类（#215 里十个号掉了七个、睡一觉又自己好了）；只按
// 「不罚号」处理则会漏掉第 1 类（死号留在池里反复被选中）。唯一能分开两者的手段
// 是拿 refresh 端点问一次：refresh 成功=会话还活着；refresh 被应用层拒绝=真死；
// 网络/5xx/429/网关自己也拦=**没有判词**。
//
// 三条边界（踩过才知道）：
//   - 没有判词不能当判词：refresh 端点同样挂在 /v2/* 上，边缘拦截时它也回 HTML 页。
//     把「refresh 也失败」一律当死号，就是在同一层再误杀一次。
//   - 求证会轮换 RT：成功时必须立刻落盘，否则重启回滚到旧 RT = 本来没死也被弄死。
//   - 求证要节流：上游大面积 401 时每个失败请求都去 refresh，等于给刷新端点造风暴。
package upstream

import (
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// SessionVerdict 一次 401 求证的结论。
type SessionVerdict int

const (
	// SessionAlive 会话仍有效：refresh 成功，401 属瞬时拦截，不该罚号。
	SessionAlive SessionVerdict = iota
	// SessionDead 登录态确实失效：refresh 被上游应用层明确拒绝，只能重登。
	SessionDead
	// SessionInconclusive 无明确判词：网络错 / 5xx / 429 / 网关拦截 / refresh 端点
	// 自身被拦。既不能判活也不能判死——计死会换个地方复发同一个误杀。
	SessionInconclusive
)

func (v SessionVerdict) String() string {
	switch v {
	case SessionAlive:
		return "alive"
	case SessionDead:
		return "dead"
	default:
		return "inconclusive"
	}
}

const (
	// verifyIOTimeout 求证的网络上限：比常规 refresh（30s）短得多——它跑在已经
	// 失败的请求路径上，不该让客户端再等半分钟。
	verifyIOTimeout = 8 * time.Second
	// verifySweepAt 求证缓存超过该条数时清理过期项（生产账号量级最多几百，防御性阈值）。
	verifySweepAt = 512
)

// defaultSessionVerifyInterval 同一账号两次求证的默认间隔（窗口内复用上次结论）。
// 生产值来自 config pool.session_verify_interval（热更新，见 SetSessionVerifyInterval），
// 这里只是未配置时的兜底。
const defaultSessionVerifyInterval = 60 * time.Second

// SetSessionVerifyInterval 设置同账号两次 401 求证的最小间隔。
// <=0 视为「用默认值」；测试可用极小的正数关掉缓存复用。
func (c *Client) SetSessionVerifyInterval(d time.Duration) {
	c.verifyInterval.Store(int64(d))
}

// sessionVerifyWindow 当前生效的求证间隔。
func (c *Client) sessionVerifyWindow() time.Duration {
	if d := time.Duration(c.verifyInterval.Load()); d > 0 {
		return d
	}
	return defaultSessionVerifyInterval
}

// verifyRecord 一次求证的结论与时刻。
type verifyRecord struct {
	at      time.Time
	verdict SessionVerdict
}

// IsGatewayUnauthorized 报告 401 是否为「网关/边缘拦截」形态：HTTP 401 且 body
// 无业务信封（APISIX/openresty 的 HTML 页、空体、"Unauthorized" 纯文本均命中）。
// 带业务信封的 401（如 {"code":12153,...}）仍属应用层拒绝，由 Classify 判 session dead。
//
// 与 IsWafBlocked 并列而不合并：两者状态码与语义不同（403 是 WAF、401 是鉴权层），
// 合并会让「401 该不该求证」的判据失真。
func IsGatewayUnauthorized(status int, body string) bool {
	return status == http.StatusUnauthorized && !hasBusinessEnvelope(body)
}

// AccessUsableWithoutRefresh 报告账号此刻是否还能干活（access token 未过期）。
//
// 「refresh token 死了」与「账号不可用」是两件事：AT 没过期时上游 chat 仍正常。
// 把这种号按 session dead 计数，连续三次保活就会禁掉一个完全健康的账号——同类
// 项目实测过 29 个号里 4 个 refresh 恒回 12153、AT 却还有 26~333 天、chat 全 200。
//
// 只适用于「AT 尚未被上游拒绝」的路径（保活 / 请求前预刷新 / 搜索前预刷新）；
// chat 已经拿到 401 时 AT 已被拒过，那里不适用本函数。
func AccessUsableWithoutRefresh(a *auth.Auth) bool {
	return a != nil && !a.NeedsRefresh(0)
}

// VerifySession 用 refresh 求证会话是否仍有效（网关 401 的唯一可靠判据）。
//
// 判据：
//   - refresh 成功                          → SessionAlive
//   - refresh 被应用层明确拒绝（4xx 业务）   → SessionDead
//   - 其它（网络错 / 5xx / 429 / 408 / 网关拦截 / WAF）→ SessionInconclusive
//
// 副作用（重要）：
//   - 成功时上游已轮换 RT，本函数**负责落盘**（a.FilePath 非空时）；落盘失败只打
//     ERR 日志、不改变结论——结论来自上游，落盘是本地职责，不能因为磁盘问题
//     把「活」改判成「死」。
//   - 结论缓存 sessionVerifyWindow（默认 60s，见 SetSessionVerifyInterval）：窗口内的
//     后来者直接复用，不再打上游。
//
// 调用方职责：按结论决定罚不罚（本函数不做任何 pool 操作）。
func (c *Client) VerifySession(a *auth.Auth) SessionVerdict {
	if c == nil || a == nil {
		return SessionInconclusive
	}
	// 没有 RT 就无法求证：既不能判活也不能判死（重登才有 RT）。
	if strings.TrimSpace(a.RefreshTokenValue()) == "" {
		return SessionInconclusive
	}
	key := refreshKey(a)
	if v, ok := c.cachedVerify(key); ok {
		return v
	}

	started := time.Now()
	err := c.refresh(a, verifyIOTimeout)
	verdict := classifyVerifyError(err)
	elapsed := time.Since(started)

	c.storeVerify(key, verdict)
	if verdict == SessionAlive {
		c.persistAfterVerify(a)
	}
	log.Printf("INFO: [verify] %s 401 求证结论=%s 耗时=%s err=%v",
		logfmt.Label(a.UID, a.Nickname), verdict, elapsed.Round(time.Millisecond), err)
	return verdict
}

// persistAfterVerify 求证成功后的落盘：RT 已被上游轮换，不写回则重启回滚到旧票，
// 下次刷新就是 invalid_grant —— 本来没死的号会被自己弄死。
func (c *Client) persistAfterVerify(a *auth.Auth) {
	if a.FilePath == "" {
		return // 无落盘目标（CLI/测试）：上游轮换结果只活在内存里，调用方自理
	}
	if err := a.SaveAtomic(); err != nil {
		log.Printf("ERR: [verify] %s 求证成功但落盘失败（重启会回滚到旧 RT，届时只能重登）: %v",
			logfmt.Label(a.UID, a.Nickname), err)
	}
}

// classifyVerifyError 把 refresh 的错误映射成三态。
func classifyVerifyError(err error) SessionVerdict {
	if err == nil {
		return SessionAlive
	}
	// 传输层错误 / 响应缺 accessToken 等无信封错误：没有判词。
	var ue *Error
	if !errors.As(err, &ue) {
		return SessionInconclusive
	}
	switch {
	case ue.Status >= 500:
		return SessionInconclusive
	case ue.Status == http.StatusTooManyRequests, ue.Status == http.StatusRequestTimeout:
		// 限流与超时都是「这次没问出来」，不是「token 死了」。
		return SessionInconclusive
	case IsGatewayUnauthorized(ue.Status, ue.Msg), IsWafBlocked(ue.Status, ue.Msg):
		// refresh 端点自己也被边缘拦了：与 chat 的 401 同源，不能据此判死。
		return SessionInconclusive
	case ue.Status >= 400 && ue.Status < 500:
		return SessionDead
	default:
		return SessionInconclusive
	}
}

// cachedVerify 返回窗口内的求证结论。
func (c *Client) cachedVerify(key string) (SessionVerdict, bool) {
	c.verifyMu.Lock()
	defer c.verifyMu.Unlock()
	rec, ok := c.verifyLast[key]
	if !ok {
		return SessionInconclusive, false
	}
	if time.Since(rec.at) >= c.sessionVerifyWindow() {
		return SessionInconclusive, false
	}
	return rec.verdict, true
}

// storeVerify 记录求证结论，并按需清理过期项。
func (c *Client) storeVerify(key string, v SessionVerdict) {
	c.verifyMu.Lock()
	defer c.verifyMu.Unlock()
	if c.verifyLast == nil {
		c.verifyLast = make(map[string]verifyRecord)
	}
	c.verifyLast[key] = verifyRecord{at: time.Now(), verdict: v}
	if len(c.verifyLast) <= verifySweepAt {
		return
	}
	cutoff := time.Now().Add(-2 * c.sessionVerifyWindow())
	for k, rec := range c.verifyLast {
		if rec.at.Before(cutoff) {
			delete(c.verifyLast, k)
		}
	}
}

// clearVerify 清掉某账号的求证缓存（refresh 成功时调用：会话已被证明活着）。
func (c *Client) clearVerify(a *auth.Auth) {
	key := refreshKey(a)
	c.verifyMu.Lock()
	defer c.verifyMu.Unlock()
	delete(c.verifyLast, key)
}
