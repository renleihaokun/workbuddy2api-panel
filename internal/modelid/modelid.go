// Package modelid 模型 ID 的 realm 前缀协议：请求侧解析 + 列表侧格式化。
//
// 协议（大小写敏感；前缀必须是精确的小写枚举）：
//
//	cn:<model>       国内版
//	global:<model>   国际版
//	cn/<model>       同上，斜杠写法
//	global/<model>
//	<model>          裸名 = 国内版（历史兼容）
//
// 为什么要有斜杠写法：Cherry Studio 2.x 用内置 provider registry 识别模型
// （能力/上下文/推理档位都来自 registry），其归一化只按 "/" 取最后一段
// （normalizeModelId 的 split("/")），冒号前缀不在任何剥离规则里 —— cn:xxx
// 永远匹配不到 registry 里的 xxx，只能降级成无元数据的自定义模型；而
// cn/xxx 取末段后能精确命中。斜杠只是"给客户端看的命名空间"：出站仍只发
// 裸名，两种写法在网关内部完全等价（realm 一样、路由一样、hidden 一样）。
//
// 解析顺序刻意与历史实现保持一致：先认第一个冒号，再认 cn/、global/；
// 其余写法（foo:bar、foo/bar、CN/x）一律整体当裸名，网关不猜测 —— 不认识
// 的前缀本来就可能属于上游模型名。
package modelid

import (
	"fmt"
	"strings"
)

// Style 模型列表输出使用的 ID 分隔符。
type Style string

const (
	// StyleColon 既有协议：cn:model / global:model（默认，零回归）。
	StyleColon Style = "colon"
	// StyleSlash 斜杠协议：cn/model / global/model（Cherry Studio 等按 "/"
	// 识别命名空间的客户端能命中内置 registry）。
	StyleSlash Style = "slash"
)

// Realm 取值。
const (
	RealmCN     = "cn"
	RealmGlobal = "global"
)

// ParseStyle 严格校验配置值 models.id_style。
//
// 接受空串（= 默认 colon，保证老配置键缺席时行为不变）、"colon"、"slash"；
// 前后空白忽略、大小写不敏感（" Slash " 合法）。非法值返回错误，由调用方
// 加上字段前缀（models.id_style）并 fail fast —— 拼错的配置静默回落会让
// 用户以为切了风格其实没切。
func ParseStyle(s string) (Style, error) {
	switch Style(strings.ToLower(strings.TrimSpace(s))) {
	case "", StyleColon:
		return StyleColon, nil
	case StyleSlash:
		return StyleSlash, nil
	default:
		return "", fmt.Errorf("%q 不是合法值（colon / slash）", strings.TrimSpace(s))
	}
}

// StyleOf 宽松读取：任何解析不了的值都当 colon。
//
// 用于运行期快照/静态字段的读取路径（配置在 Load/normalize 阶段已经严格
// 校验过，这里只是防御性兜底，避免把错误带进请求路径）。
func StyleOf(s string) Style {
	if st, err := ParseStyle(s); err == nil {
		return st
	}
	return StyleColon
}

// Split 解析请求侧模型名。
//
// 返回值：realm（cn/global）、bare（去掉 realm 前缀后的上游模型名）、
// explicit（调用方是否显式写了 realm 前缀 —— 显式域不参与跨域回退，
// 见 pool.cross_domain_fallback）。
//
// 规则：
//   - "cn:<m>" / "global:<m>"：取第一个冒号，前段恰为小写枚举才剥离；
//   - "cn/<m>" / "global/<m>"：字面前缀，同样只认小写枚举；
//   - 其余（含 "foo:bar"、"foo/bar"、"CN/x"、空串）：realm=cn、bare=原串、
//     explicit=false。
//
// 前缀大小写敏感是历史语义的一部分（"CN:" 一直是裸名），这里保持一致，
// 避免同一份配置在不同大小写下路由到不同域。
func Split(model string) (realm, bare string, explicit bool) {
	if i := strings.IndexByte(model, ':'); i >= 0 {
		switch model[:i] {
		case RealmCN, RealmGlobal:
			return model[:i], model[i+1:], true
		}
	}
	if strings.HasPrefix(model, "cn/") {
		return RealmCN, model[len("cn/"):], true
	}
	if strings.HasPrefix(model, "global/") {
		return RealmGlobal, model[len("global/"):], true
	}
	return RealmCN, model, false
}

// Format 按风格拼出列表/面板展示用的模型 ID。
//
// 非 slash 一律按 colon 输出：Style 零值与未知值都不会改变既有行为。
func Format(style Style, realm, bare string) string {
	if style == StyleSlash {
		return realm + "/" + bare
	}
	return realm + ":" + bare
}

// Key 内部规范键：realm + ":" + bare。
//
// 只用于网关内部（models.hidden 集合、注释与测试断言）；对客户端展示的
// 写法由 Format 决定，两者解耦 —— 这样 hidden 配置不受 id_style 影响，
// 换风格不会让黑名单失效。
func Key(realm, bare string) string { return realm + ":" + bare }

// Label 域名展示标签（realm_name_prefix 用）。
func Label(realm string) string {
	if realm == RealmGlobal {
		return "Global"
	}
	return "CN"
}
