package server

import "github.com/linguo2625469/workbuddy2api-panel/internal/modelid"

// resolveModelDetail 解析模型名协议（PLAN D6），并区分「realm 是否由调用方显式写出」：
//
//	分布式前缀（两种分隔符等价）："[realm:]model" / "[realm/]model"
//
// 只认精确小写的 cn / global 作为前缀；其余写法（含 foo:bar、foo/bar、CN/x）
// 整体当裸名，realm=cn、bare=原串。解析顺序与历史实现一致：先看第一个 ":"，
// 再看 "cn/"、"global/" 字面前缀。bare 即出站/选号/账本使用的裸模型名。
//
// 斜杠写法是为 registry 驱动的客户端（Cherry Studio 等按 "/" 认命名空间）
// 准备的展示形态：网关内部两种写法完全等价，出站仍只发裸名。
//
// 第三个返回值 realmExplicit 供跨域回退（issue #135）判定：只有裸名（false）才允许
// 在首选域选不到号时改用另一域；显式 cn:/global:/cn//global/ 前缀永不被回退改写。
func resolveModelDetail(model string) (realm, bare string, realmExplicit bool) {
	return modelid.Split(model)
}

// resolveModel 是 resolveModelDetail 的两返回值形态（既有调用方语义零改动）。
func resolveModel(model string) (realm, bare string) {
	realm, bare, _ = resolveModelDetail(model)
	return realm, bare
}

// ResolveModel 是 resolveModel 的导出面（跨包调用）。
func ResolveModel(model string) (realm, bare string) { return resolveModel(model) }

// otherRealm 返回另一个选号域（"cn" ↔ "global"），供跨域回退使用。
func otherRealm(realm string) string {
	if realm == "global" {
		return "cn"
	}
	return "global"
}
