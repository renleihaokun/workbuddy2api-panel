// cross_domain_model_fallback_test.go 同名模型跨域回退（pool.cross_domain_model_fallback）
// 端到端测试：开关默认关闭 = 零回归；开启后「本域目录明确没有该模型」与「本域整池
// 不可服务」两条路径都要回退，且**必须**先证明另一域目录里有同名模型；显式 cn:/global:
// 前缀也参与，cn↔global 双向。目录未探测到（Unknown）时不猜、不回退。
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// catalogStub 用固定表模拟目录快照：键 = "realm:model"，未列出的 = Unknown。
func catalogStub(states map[string]upstream.CatalogState) func(realm, model string) upstream.CatalogState {
	return func(realm, model string) upstream.CatalogState {
		if st, ok := states[realm+":"+model]; ok {
			return st
		}
		return upstream.CatalogUnknown
	}
}

// chatOnce 挂上假上游并发一条最小 chat 请求，返回响应与各账号被调次数。
func chatOnce(t *testing.T, h *Handler, model string, behavior func(authz string) (int, string, bool)) (*httptest.ResponseRecorder, map[string]int) {
	t.Helper()
	calls := map[string]int{}
	h.cfg.Upstream = newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return behavior(authz)
	})
	rec := httptest.NewRecorder()
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	return rec, calls
}

// newFallbackHandler 构造带目录桩的 handler；enable 控制两个开关。
func newFallbackHandler(t *testing.T, p *pool.Pool, catalog map[string]upstream.CatalogState, legacy, modelSwitch bool) *Handler {
	t.Helper()
	p.SetCrossDomainFallback(legacy)
	p.SetCrossDomainModelFallback(modelSwitch)
	return NewHandler(Config{
		Pool:         p,
		ModelCatalog: catalogStub(catalog),
	})
}

// 开关默认关闭：显式 global: 撞 11102 后不得跨域（与引入前逐位一致）。
func TestModelRealmFallbackDisabledByDefault(t *testing.T) {
	p := newRealmPool(t)
	if p.CrossDomainModelFallback() {
		t.Fatal("cross_domain_model_fallback 缺省必须为 false（零回归）")
	}
	h := newFallbackHandler(t, p, map[string]upstream.CatalogState{
		"cn:deepseek-v4.1-flash-sg": upstream.CatalogPresent,
	}, false, false)

	rec, calls := chatOnce(t, h, "global:deepseek-v4.1-flash-sg", func(authz string) (int, string, bool) {
		return 400, `{"code":11102,"msg":"model [deepseek-v4.1-flash-sg] service info not found"}`, false
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "model_unavailable") {
		t.Fatalf("code=%d body=%s (want 400 model_unavailable)", rec.Code, rec.Body)
	}
	if calls["Bearer at-cn"] != 0 {
		t.Errorf("calls=%v want cn 0 次（开关关闭不得跨域）", calls)
	}
}

// T1：本域目录明确没有该模型、另一域有 → 首个请求直接换域，不浪费 11102 轮转。
func TestModelRealmFallbackCatalogAbsent(t *testing.T) {
	p := newRealmPool(t)
	h := newFallbackHandler(t, p, map[string]upstream.CatalogState{
		"cn:deepseek-v4.1-flash-sg":     upstream.CatalogAbsent,
		"global:deepseek-v4.1-flash-sg": upstream.CatalogPresent,
	}, false, true)

	rec, calls := chatOnce(t, h, "cn:deepseek-v4.1-flash-sg", func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s (want 200: 本域目录无此模型应直接换域)", rec.Code, rec.Body)
	}
	if calls["Bearer at-cn"] != 0 || calls["Bearer at-gl"] != 1 {
		t.Errorf("calls=%v want cn 0 次、global 1 次（目录缺失路径不得先撞本域）", calls)
	}
}

// T2：本域对该模型整池不可服务（账号级冷却）、另一域可服务且目录同名 → 换域。
func TestModelRealmFallbackPoolExhausted(t *testing.T) {
	p := newRealmPool(t)
	p.Cooldown("cn1", pool.CoolSoft, time.Hour, "test: account-level cooldown")
	h := newFallbackHandler(t, p, map[string]upstream.CatalogState{
		"cn:deepseek-v4.1-flash":     upstream.CatalogPresent,
		"global:deepseek-v4.1-flash": upstream.CatalogPresent,
	}, false, true)

	rec, calls := chatOnce(t, h, "cn:deepseek-v4.1-flash", func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s (want 200: cn 整池冷却应回退 global)", rec.Code, rec.Body)
	}
	if calls["Bearer at-cn"] != 0 || calls["Bearer at-gl"] != 1 {
		t.Errorf("calls=%v want cn 0 次、global 1 次", calls)
	}
}

// 本域先撞 11102（目录未知/陈旧时的真实形态）：该域整池被模型级负缓存挡住后，
// 同一次请求内改走另一域并成功。
func TestModelRealmFallbackAfterModelBlocked(t *testing.T) {
	p := newRealmPool(t)
	h := newFallbackHandler(t, p, map[string]upstream.CatalogState{
		"cn:deepseek-v4.1-flash-sg":     upstream.CatalogPresent,
		"global:deepseek-v4.1-flash-sg": upstream.CatalogPresent,
	}, false, true)

	rec, calls := chatOnce(t, h, "cn:deepseek-v4.1-flash-sg", func(authz string) (int, string, bool) {
		if authz == "Bearer at-cn" {
			return 400, `{"code":11102,"msg":"model [deepseek-v4.1-flash-sg] service info not found"}`, false
		}
		return 200, sseOK, true
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s (want 200: cn 11102 后应改走 global)", rec.Code, rec.Body)
	}
	if calls["Bearer at-cn"] != 1 || calls["Bearer at-gl"] != 1 {
		t.Errorf("calls=%v want cn 1 次（11102）→ global 1 次（回退成功）", calls)
	}
}

// 前提不成立：另一域目录明确没有同名模型 → 不回退（报原错误）。
func TestModelRealmFallbackOtherRealmLacksModel(t *testing.T) {
	p := newRealmPool(t)
	h := newFallbackHandler(t, p, map[string]upstream.CatalogState{
		"cn:deepseek-v4.1-flash-sg":     upstream.CatalogPresent,
		"global:deepseek-v4.1-flash-sg": upstream.CatalogAbsent,
	}, false, true)

	rec, calls := chatOnce(t, h, "cn:deepseek-v4.1-flash-sg", func(authz string) (int, string, bool) {
		return 400, `{"code":11102,"msg":"model [deepseek-v4.1-flash-sg] service info not found"}`, false
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "model_unavailable") {
		t.Fatalf("code=%d body=%s (want 400: 另一域没有同名模型不得回退)", rec.Code, rec.Body)
	}
	if calls["Bearer at-gl"] != 0 {
		t.Errorf("calls=%v want global 0 次", calls)
	}
}

// 目录未知（未探测/过期）→ 不猜、不回退。宁可本次失败，也不拿旧目录决定路由。
func TestModelRealmFallbackCatalogUnknownNoSwitch(t *testing.T) {
	p := newRealmPool(t)
	h := newFallbackHandler(t, p, map[string]upstream.CatalogState{}, false, true)

	rec, calls := chatOnce(t, h, "cn:deepseek-v4.1-flash-sg", func(authz string) (int, string, bool) {
		return 400, `{"code":11102,"msg":"model [deepseek-v4.1-flash-sg] service info not found"}`, false
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s (want 400: 目录未知不得回退)", rec.Code, rec.Body)
	}
	if calls["Bearer at-gl"] != 0 {
		t.Errorf("calls=%v want global 0 次（目录未知严格不跨域）", calls)
	}
}

// 反向回退：global 请求在 global 整池不可服务时改走 cn（前提是 cn 目录同名）。
func TestModelRealmFallbackReverse(t *testing.T) {
	p := newRealmPool(t)
	p.Cooldown("gl1", pool.CoolSoft, time.Hour, "test: global account cooldown")
	h := newFallbackHandler(t, p, map[string]upstream.CatalogState{
		"cn:deepseek-v4.1-flash":     upstream.CatalogPresent,
		"global:deepseek-v4.1-flash": upstream.CatalogPresent,
	}, false, true)

	rec, calls := chatOnce(t, h, "global:deepseek-v4.1-flash", func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s (want 200: global 整池冷却应反向回退 cn)", rec.Code, rec.Body)
	}
	if calls["Bearer at-cn"] != 1 || calls["Bearer at-gl"] != 0 {
		t.Errorf("calls=%v want cn 1 次、global 0 次", calls)
	}
}

// 裸模型名同样参与（新开关不限于显式前缀）：cn 目录缺该模型 → 走 global。
func TestModelRealmFallbackBareName(t *testing.T) {
	p := newRealmPool(t)
	h := newFallbackHandler(t, p, map[string]upstream.CatalogState{
		"cn:deepseek-v4.1-flash-sg":     upstream.CatalogAbsent,
		"global:deepseek-v4.1-flash-sg": upstream.CatalogPresent,
	}, false, true)

	rec, calls := chatOnce(t, h, "deepseek-v4.1-flash-sg", func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s (want 200: 裸名同样参与同名模型回退)", rec.Code, rec.Body)
	}
	if calls["Bearer at-cn"] != 0 || calls["Bearer at-gl"] != 1 {
		t.Errorf("calls=%v want cn 0 次、global 1 次", calls)
	}
}

// 两个开关同开 + 裸名：沿用旧开关的宽松语义（不校验目录），另一域目录 absent 也回退。
func TestModelRealmFallbackLegacyLooseOnBareName(t *testing.T) {
	p := newRealmPool(t)
	h := newFallbackHandler(t, p, map[string]upstream.CatalogState{
		"cn:deepseek-v4.1-flash":     upstream.CatalogPresent,
		"global:deepseek-v4.1-flash": upstream.CatalogAbsent,
	}, true, true)

	rec, calls := chatOnce(t, h, "deepseek-v4.1-flash", func(authz string) (int, string, bool) {
		if authz == "Bearer at-cn" {
			return 400, `{"code":11102,"msg":"model [deepseek-v4.1-flash] service info not found"}`, false
		}
		return 200, sseOK, true
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s (want 200: 旧开关对裸名保持宽松语义)", rec.Code, rec.Body)
	}
	if calls["Bearer at-gl"] != 1 {
		t.Errorf("calls=%v want global 1 次", calls)
	}
}

// 只开旧开关 + 显式前缀：仍然零回退（旧开关语义不变，新开关是唯一能改显式前缀的开关）。
func TestExplicitRealmOnlyLegacySwitchNoFallback(t *testing.T) {
	p := newRealmPool(t)
	h := newFallbackHandler(t, p, map[string]upstream.CatalogState{
		"cn:deepseek-v4.1-flash-sg": upstream.CatalogPresent,
	}, true, false)

	rec, calls := chatOnce(t, h, "global:deepseek-v4.1-flash-sg", func(authz string) (int, string, bool) {
		return 400, `{"code":11102,"msg":"model [deepseek-v4.1-flash-sg] service info not found"}`, false
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "model_unavailable") {
		t.Fatalf("code=%d body=%s (want 400: 旧开关不得改写显式前缀)", rec.Code, rec.Body)
	}
	if calls["Bearer at-cn"] != 0 {
		t.Errorf("calls=%v want cn 0 次", calls)
	}
}

// 两域都不可服务：不换（换了也白换），报原错误。
func TestModelRealmFallbackBothRealmsUnavailable(t *testing.T) {
	p := newRealmPool(t)
	p.Cooldown("cn1", pool.CoolSoft, time.Hour, "test: cn cooldown")
	p.Cooldown("gl1", pool.CoolSoft, time.Hour, "test: global cooldown")
	h := newFallbackHandler(t, p, map[string]upstream.CatalogState{
		"cn:deepseek-v4.1-flash":     upstream.CatalogPresent,
		"global:deepseek-v4.1-flash": upstream.CatalogPresent,
	}, false, true)

	// 两域都冷却时 pick 会走全冷却兜底（CoolSoft 允许兜底探测），因此这里允许上游被
	// 调用一次；关键是不得出现「换域成功」的假象——两个域都会被当作最后手段尝试。
	rec, _ := chatOnce(t, h, "cn:deepseek-v4.1-flash", func(authz string) (int, string, bool) {
		return 503, `{"code":500,"msg":"upstream down"}`, false
	})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s (want 503)", rec.Code, rec.Body)
	}
}
