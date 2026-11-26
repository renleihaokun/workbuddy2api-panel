// probe.go 上游目录 / 端点取证（只读诊断）。
//
// 定位：一条命令回答三件事——端点路径还活不活、每路 UA 下发哪些模型、上游到底给
// 这个账号标了多少 `disabled`。它是 cmd/catalogprobe 的实现，不进任何用户请求链路。
//
// 设计约束（都是踩过的坑）：
//   - 只发 GET，不发推理请求，不消费积分；
//   - 复用 getV3ConfigRaw / getEnterpriseModelsRaw 的请求头与 UA 构造，不另拼一份——
//     上游认指纹，多一份拼法就等于多一个会各自漂移的来源；
//   - 报告里不出现 access_token / refresh_token，账号只留 UID 的短 hash；
//   - `disabled` 只报告不处置：是否从目录里剔掉由 fetchV3ConfigModelMap 决定，
//     取证工具只负责把事实摆出来（先看清再决定）。
package upstream

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/modelid"
)

// RouteProbe 单条目录路径的探测结果。
//
// Total 是上游下发的总条数（含 disabled 与「企业端点不在 agents[cli] 里」的条目），
// Enabled 是最终能上架的条数。两者不等通常说明上游在收紧，值得看一眼 Disabled。
type RouteProbe struct {
	Label        string   `json:"label"`  // desktop-UA / IDE-UA / CLI-UA / enterprise
	Path         string   `json:"path"`   // /v3/config 或 /console/...
	Status       int      `json:"status"` // HTTP 状态；0 = 传输层失败
	Err          string   `json:"err,omitempty"`
	Total        int      `json:"total"`
	Enabled      int      `json:"enabled"`
	Models       []string `json:"models,omitempty"`        // 可上架模型 id（已排序）
	Disabled     []string `json:"disabled,omitempty"`      // disabled=true 的模型 id
	EnvelopeKeys []string `json:"envelope_keys,omitempty"` // data 段的键（判断信封是否改版）
}

// PublicProbe 无鉴权探测：只用来区分「路径还在」与「路径没了」。
// 401/403 = 路径在、要鉴权；404 = 路径改过；5xx/HTML = 边缘层拦了。
type PublicProbe struct {
	Path        string `json:"path"`
	Status      int    `json:"status"`
	ContentType string `json:"content_type,omitempty"`
	Err         string `json:"err,omitempty"`
}

// CatalogProbeReport 一次取证的完整结果（可直接 -json 存档）。
type CatalogProbeReport struct {
	Realm      string        `json:"realm"`
	AccountTag string        `json:"account_tag"` // UID 的短 hash，不是原值
	ChatBase   string        `json:"chat_base"`
	ProbedAt   string        `json:"probed_at"`
	Routes     []RouteProbe  `json:"routes"`
	Public     []PublicProbe `json:"public,omitempty"`
}

// RouteFingerprint 目录指纹：只留「有哪些 id、哪些被下架、状态码」，不留响应原文
// （原文含 requestId 等一次性字段，存进文件只会让 diff 噪音变大）。
type RouteFingerprint struct {
	Status   int      `json:"status"`
	Models   []string `json:"models"`
	Disabled []string `json:"disabled"`
}

// CatalogFingerprint 可落盘、可跨天 diff 的目录指纹。
type CatalogFingerprint struct {
	Saved      string                      `json:"saved"`
	Realm      string                      `json:"realm"`
	AccountTag string                      `json:"account_tag"`
	Routes     map[string]RouteFingerprint `json:"routes"`
}

// Fingerprint 把报告压缩成指纹（丢弃顺序差异，保留集合差异）。
func (r CatalogProbeReport) Fingerprint() CatalogFingerprint {
	fp := CatalogFingerprint{
		Saved:      time.Now().Format(time.RFC3339),
		Realm:      r.Realm,
		AccountTag: r.AccountTag,
		Routes:     make(map[string]RouteFingerprint, len(r.Routes)),
	}
	for _, rt := range r.Routes {
		models := append([]string(nil), rt.Models...)
		disabled := append([]string(nil), rt.Disabled...)
		sort.Strings(models)
		sort.Strings(disabled)
		fp.Routes[rt.Label] = RouteFingerprint{Status: rt.Status, Models: models, Disabled: disabled}
	}
	return fp
}

// DiffFingerprint 对比两份指纹，返回人读的行（空 = 没变）。
// 只比 Status / Models / Disabled 三项：名字变了、能力字段变了不在本工具职责内。
func DiffFingerprint(old, cur CatalogFingerprint) []string {
	var out []string
	labels := make([]string, 0, len(cur.Routes)+len(old.Routes))
	seen := map[string]bool{}
	for l := range cur.Routes {
		labels = append(labels, l)
		seen[l] = true
	}
	for l := range old.Routes {
		if !seen[l] {
			labels = append(labels, l)
		}
	}
	sort.Strings(labels)
	for _, l := range labels {
		o, okOld := old.Routes[l]
		c, okCur := cur.Routes[l]
		switch {
		case !okOld:
			out = append(out, fmt.Sprintf("%s: 新增路径（models=%d）", l, len(c.Models)))
			continue
		case !okCur:
			out = append(out, fmt.Sprintf("%s: 本次没探到（上次 models=%d）", l, len(o.Models)))
			continue
		}
		if o.Status != c.Status {
			out = append(out, fmt.Sprintf("%s: status %d → %d", l, o.Status, c.Status))
		}
		for _, id := range minusStrings(c.Models, o.Models) {
			out = append(out, fmt.Sprintf("%s: + %s", l, id))
		}
		for _, id := range minusStrings(o.Models, c.Models) {
			out = append(out, fmt.Sprintf("%s: - %s", l, id))
		}
		for _, id := range minusStrings(c.Disabled, o.Disabled) {
			out = append(out, fmt.Sprintf("%s: disabled + %s", l, id))
		}
		for _, id := range minusStrings(o.Disabled, c.Disabled) {
			out = append(out, fmt.Sprintf("%s: disabled - %s", l, id))
		}
	}
	return out
}

// minusStrings 返回 a - b（假定两者都已排序；用双指针避免 O(n*m)）。
func minusStrings(a, b []string) []string {
	if len(a) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(b))
	for _, s := range b {
		set[s] = struct{}{}
	}
	var out []string
	for _, s := range a {
		if _, ok := set[s]; !ok {
			out = append(out, s)
		}
	}
	return out
}

// ProbeCatalog 对一个账号做一次目录取证：三路 v3 UA + 企业端点，共 4 次 GET。
//
// 三路 UA 与生产探测同源（见 fetchV3Models / probeGlobalModels）：CN 的「桌面路」
// 用 v3ConfigUA，global 用 chatUA。IDE / CLI 两条历史探测 UA 两域都用。
func (c *Client) ProbeCatalog(a *auth.Auth) CatalogProbeReport {
	realm := a.Realm()
	rep := CatalogProbeReport{
		Realm:      realm,
		AccountTag: accountTag(a),
		ChatBase:   c.chatBase(a),
		ProbedAt:   time.Now().Format(time.RFC3339),
	}
	desktop := c.v3ConfigUA(a)
	if realm == modelid.RealmGlobal {
		desktop = c.chatUA(a)
	}
	for _, r := range []struct{ label, ua string }{
		{"desktop-UA", desktop},
		{"IDE-UA", legacyIDEProbeUA},
		{"CLI-UA", legacyCLIProbeUA},
	} {
		rep.Routes = append(rep.Routes, c.probeV3Route(r.label, r.ua, a))
	}
	rep.Routes = append(rep.Routes, c.probeEnterpriseRoute(a))
	return rep
}

// probeV3Route 探一路 /v3/config：状态码 + 信封键 + 模型集 + disabled 集。
func (c *Client) probeV3Route(label, ua string, a *auth.Auth) RouteProbe {
	rp := RouteProbe{Label: label, Path: "/v3/config"}
	raw, status, err := c.getV3ConfigRaw(a, ua)
	rp.Status = status
	if err != nil {
		rp.Err = err.Error()
		return rp
	}
	var env struct {
		Code int                        `json:"code"`
		Msg  string                     `json:"msg"`
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		rp.Err = "parse: " + err.Error()
		return rp
	}
	if env.Code != 0 {
		rp.Err = fmt.Sprintf("code=%d msg=%s", env.Code, env.Msg)
		return rp
	}
	rp.EnvelopeKeys = probeSortedKeys(env.Data)
	var models []struct {
		ID       string `json:"id"`
		Disabled bool   `json:"disabled"`
	}
	if rawModels, ok := env.Data["models"]; ok {
		// 空数组/null 都合法：无鉴权时上游就是 models:null。
		_ = json.Unmarshal(rawModels, &models)
	}
	for _, m := range models {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		rp.Total++
		if m.Disabled {
			rp.Disabled = append(rp.Disabled, id)
			continue
		}
		rp.Models = append(rp.Models, id)
	}
	sort.Strings(rp.Models)
	sort.Strings(rp.Disabled)
	rp.Enabled = len(rp.Models)
	return rp
}

// probeEnterpriseRoute 探企业端点。
//
// 与 fetchEnterpriseModels 同口径：只认 agents[cli] 里的模型 + disabled 剔除。
// 因此 Total 可能大于 Enabled+len(Disabled)——差值是「不在 cli agent 名单里」的条目，
// 这是上游按面（cli / ide / …）过滤的结果，不是异常。
func (c *Client) probeEnterpriseRoute(a *auth.Auth) RouteProbe {
	rp := RouteProbe{Label: "enterprise", Path: "/console/enterprises/personal/models"}
	raw, status, err := c.getEnterpriseModelsRaw(a)
	rp.Status = status
	if err != nil {
		rp.Err = err.Error()
		return rp
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Models []struct {
				ID       string `json:"id"`
				Disabled bool   `json:"disabled"`
			} `json:"models"`
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		rp.Err = "parse: " + err.Error()
		return rp
	}
	if env.Code != 0 {
		rp.Err = fmt.Sprintf("code=%d", env.Code)
		return rp
	}
	cliSet := make(map[string]struct{})
	for _, ag := range env.Data.Agents {
		if ag.Name != "cli" {
			continue
		}
		for _, id := range ag.Models {
			cliSet[id] = struct{}{}
		}
		break
	}
	for _, m := range env.Data.Models {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		rp.Total++
		if m.Disabled {
			rp.Disabled = append(rp.Disabled, id)
			continue
		}
		if _, ok := cliSet[id]; !ok {
			continue // 不在 agents[cli] 名单：生产解析同样不收
		}
		rp.Models = append(rp.Models, id)
	}
	sort.Strings(rp.Models)
	sort.Strings(rp.Disabled)
	rp.Enabled = len(rp.Models)
	return rp
}

// ProbePublicEndpoints 无鉴权探测若干路径，只回答「路径还在不在」。
//
// 默认不跑：无鉴权请求会从网关出口 IP 打上游，属于多余的噪音，只有排障时显式
// 打开（cmd/catalogprobe -public）。不带任何账号凭证。
func (c *Client) ProbePublicEndpoints(base string, paths []string) []PublicProbe {
	out := make([]PublicProbe, 0, len(paths))
	for _, p := range paths {
		pp := PublicProbe{Path: p}
		req, err := http.NewRequest(http.MethodGet, strings.TrimRight(base, "/")+p, nil)
		if err != nil {
			pp.Err = err.Error()
			out = append(out, pp)
			continue
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			pp.Err = err.Error()
			out = append(out, pp)
			continue
		}
		pp.Status = resp.StatusCode
		pp.ContentType = resp.Header.Get("Content-Type")
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		out = append(out, pp)
	}
	return out
}

// DefaultPublicProbePaths 目录 / 聊天 / 计费三条主链路各取一个代表路径。
var DefaultPublicProbePaths = []string{
	"/v3/config",
	"/console/enterprises/personal/models",
	"/v2/chat/completions",
}

// accountTag UID 的短 hash（8 位十六进制）。报告要贴进工单/issue，UID 原值不该出现。
func accountTag(a *auth.Auth) string {
	sum := sha256.Sum256([]byte(a.UID))
	return fmt.Sprintf("%x", sum[:4])
}

// probeSortedKeys map 的键排序后返回（报告 diff 需要稳定顺序）。
func probeSortedKeys(m map[string]json.RawMessage) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
