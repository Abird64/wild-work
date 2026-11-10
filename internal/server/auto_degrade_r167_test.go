// 自动选路的「降级到底」行为锁定（R167）。
//
// 用户报告原文：「好像不会自己降级模型阿，有的时候 auto 里的模型额度用完了或者什么时候
// 不能用了咋办」。R166 修的是"内层认得出 auto 时降级走全序"，但两条路径仍有洞：
//
//	① 兼容层路径（`/v1/responses`、`/v1/messages`）：网关先把 auto 解析成裸模型名再投给内层，
//	   内层的 `isAutoModel` 恒假 ⇒ `autoOrder` 为空 ⇒ 只走静态降级链 ⇒ 对 auto 选出的
//	   模型一条都不命中 ⇒ **永不降级**。修法：网关把决策全序经 `auto.OrderHeader` 带进来。
//	② 可降级的错误集太窄（只有 402/429）："模型在这个渠道下线/不存在"（11102/404）
//	   这类最字面的"不能用了"一个都不降级。
//	③ 单账号渠道（oczen/kilo）被 R22 的"惩罚豁免"顺带挡住了降级；但换到**另一个渠道**的
//	   模型既不是惩罚也不会反复捶唯一账号。
//
// 本文件把这三条逐条锁死，且锁住"非 auto 请求行为逐字不变"的反面。
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/auto"
	"wild-work/internal/oczen"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// newAutoDegradeHandler 装配 oczen(单账号) + workbuddy + glm 三渠道，各自独立上游。
// oczen 走 SingleAccount 语义（与生产一致），另两条走普通多账号语义。
func newAutoDegradeHandler(t *testing.T, upOczen, upWB, upGLM provider.Upstream) *Handler {
	t.Helper()
	po := pool.New(t.TempDir() + "/state-oczen.json")
	po.Add(oczen.AnonymousAuth())
	pw := pool.New(t.TempDir() + "/state-wb.json")
	pw.Add(&auth.Auth{UID: "wb-uid"})
	pg := pool.New(t.TempDir() + "/state-glm.json")
	pg.Add(&auth.Auth{UID: "glm-uid"})
	return NewHandler(Config{
		Runtimes: map[provider.Kind]*Runtime{
			provider.Oczen:     {Kind: provider.Oczen, Pool: po, Upstream: upOczen, SingleAccount: true},
			provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: pw, Upstream: upWB},
			provider.GLM:       {Kind: provider.GLM, Pool: pg, Upstream: upGLM},
		},
		APIKey: "k", HardCooldown: time.Hour, SoftCooldown: time.Minute,
		ErrThreshold: 3, ErrCooldown: time.Minute,
	})
}

// setAuto 注入一个固定决策（chosen + 全序）。
func setAuto(h *Handler, chosen string, order []string) {
	chain := order
	if len(chain) > 3 {
		chain = chain[:3]
	}
	h.SetAutoResolver(func(auto.Request) (string, []string, []string, bool) {
		return chosen, chain, order, true
	})
}

// callWithHeaders 发一次非流式 chat 请求，可附加任意请求头。
func callWithHeaders(h *Handler, model string, hdr map[string]string) *httptest.ResponseRecorder {
	body := `{"model":"` + model + `","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer k")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestAutoFallsBackOnModelBlocked 首选上游回 11102（该后端无此模型）⇒ auto 必须换下一个。
//
// 修复前：这条错误在 serveRotation 里 `transparentError + return`，连降级判定都到不了，
// 客户端直接收到上游 404 原文，而全序里还有两个能用的模型。
func TestAutoFallsBackOnModelBlocked(t *testing.T) {
	upO := &kindedUpstream{status: http.StatusOK, succeed: func(string) bool { return true }}
	upW := &kindedUpstream{status: http.StatusNotFound, kind: provider.ErrModelBlocked,
		body: `{"error":{"code":11102,"message":"the backend does not have this model"}}`}
	upG := &kindedUpstream{status: http.StatusOK, succeed: func(string) bool { return true }}
	h := newAutoDegradeHandler(t, upO, upW, upG)
	order := []string{"workbuddy/gone-model", "glm/glm-4.6", "oczen/mimo-v2.6-flash-free"}
	setAuto(h, order[0], order)

	rec := callModel(h, auto.Alias)
	if rec.Code != http.StatusOK {
		t.Fatalf("11102 后应降级到 glm 返回 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Model-Fallback"); got != "auto -> glm/glm-4.6" {
		t.Errorf("X-Model-Fallback = %q，want %q", got, "auto -> glm/glm-4.6")
	}
	if upW.count() != 1 {
		t.Errorf("首选渠道应只被调 1 次（(账号,模型) 负缓存已生效），实际 %d", upW.count())
	}
	if upG.count() != 1 {
		t.Errorf("备胎渠道应被调 1 次，实际 %d", upG.count())
	}
}

// TestNonAutoModelBlockedStaysTransparent 反面：**显式点名**的模型撞 11102 时不得换模型。
//
// 否则客户端把模型名写错（或点名一个当前套餐没有的模型）会得到"看起来成功"的
// 另一个模型的回答——比报错更难排查。
func TestNonAutoModelBlockedStaysTransparent(t *testing.T) {
	upO := &kindedUpstream{status: http.StatusOK, succeed: func(string) bool { return true }}
	upW := &kindedUpstream{status: http.StatusNotFound, kind: provider.ErrModelBlocked,
		body: `{"error":{"code":11102,"message":"the backend does not have this model"}}`}
	upG := &kindedUpstream{status: http.StatusOK, succeed: func(string) bool { return true }}
	h := newAutoDegradeHandler(t, upO, upW, upG)

	rec := callModel(h, "workbuddy/gone-model")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("非 auto 请求应原样透传 404，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "11102") {
		t.Errorf("应透传上游原文，实际：%s", rec.Body.String())
	}
	if upG.count() != 0 {
		t.Errorf("非 auto 请求不得触发跨渠道降级，glm 被调 %d 次", upG.count())
	}
}

// TestAutoSingleAccountRateLimitedFallsBack 单账号渠道（oczen）429 ⇒ auto 换到别的渠道。
//
// R22 说单账号渠道不施加账号级惩罚（不冷却、不计数），本用例验证扩面没有破它：
// 上游只被调 1 次、账号不进冷却，靠的是"换模型"而不是"换号"。
func TestAutoSingleAccountRateLimitedFallsBack(t *testing.T) {
	upO := &kindedUpstream{status: http.StatusTooManyRequests, kind: provider.ErrSoftRate,
		body: `{"error":{"code":14003,"message":"Model busy, please switch to another model"}}`}
	upW := &kindedUpstream{status: http.StatusOK, succeed: func(string) bool { return true }}
	upG := &kindedUpstream{status: http.StatusOK, succeed: func(string) bool { return true }}
	h := newAutoDegradeHandler(t, upO, upW, upG)
	order := []string{"oczen/mimo-v2.6-flash-free", "workbuddy/glm-free", "glm/glm-4.6"}
	setAuto(h, order[0], order)

	rec := callModel(h, auto.Alias)
	if rec.Code != http.StatusOK {
		t.Fatalf("单账号渠道 429 后应换模型返回 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Model-Fallback"); got != "auto -> workbuddy/glm-free" {
		t.Errorf("X-Model-Fallback = %q，want %q", got, "auto -> workbuddy/glm-free")
	}
	if upO.count() != 1 {
		t.Errorf("单账号渠道应只被调 1 次（不换号、不重试），实际 %d", upO.count())
	}
	// R22 不变：唯一账号不得被冷却/计数。
	st := h.getRuntime(provider.Oczen).Pool.List()[0]
	if st.Cooling {
		t.Errorf("单账号渠道不得进入冷却（R22），实际 cooling=true until=%s", st.Until)
	}
	if st.ErrCount != 0 {
		t.Errorf("单账号渠道不得累计错误计数（R22），实际 err_count=%d", st.ErrCount)
	}
}

// TestAutoOrderHeaderDrivesFallbackOnConcreteModel 兼容层路径的核心：模型名是具体的、
// 但带上了内部全序头 ⇒ 必须按全序降级（等价于内层自己认出 auto 的那条路径）。
//
// 这就是 Server 侧对 `auto.OrderHeader` 的解析与接线；网关侧发送由
// internal/gateway/auto_order_header_test.go 锁定。
func TestAutoOrderHeaderDrivesFallbackOnConcreteModel(t *testing.T) {
	upO := &kindedUpstream{status: http.StatusOK, succeed: func(string) bool { return true }}
	upW := &kindedUpstream{status: http.StatusPaymentRequired, kind: provider.ErrHardCredit,
		body: `{"error":{"code":402,"message":"insufficient credits"}}`}
	upG := &kindedUpstream{status: http.StatusOK, succeed: func(string) bool { return true }}
	h := newAutoDegradeHandler(t, upO, upW, upG)

	// 注意：这里**不注入** auto 解析器——模拟网关已经解析完、只把全序带进来的形态。
	rec := callWithHeaders(h, "workbuddy/glm-free", map[string]string{
		auto.OrderHeader: "workbuddy/glm-free,glm/glm-4.6,oczen/mimo-v2.6-flash-free",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("带头时应按全序降级返回 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Model-Fallback"); got != "workbuddy/glm-free -> glm/glm-4.6" {
		t.Errorf("X-Model-Fallback = %q，want %q", got, "workbuddy/glm-free -> glm/glm-4.6")
	}
	if upG.count() != 1 {
		t.Errorf("备胎渠道应被调 1 次，实际 %d", upG.count())
	}
}

// TestAutoOrderHeaderIgnoredWithoutFallbackNeed 反面：带头但上游成功 ⇒ 不得出现降级头。
func TestAutoOrderHeaderIgnoredWithoutFallbackNeed(t *testing.T) {
	upO := &kindedUpstream{status: http.StatusOK, succeed: func(string) bool { return true }}
	upW := &kindedUpstream{status: http.StatusOK, succeed: func(string) bool { return true }}
	upG := &kindedUpstream{status: http.StatusOK, succeed: func(string) bool { return true }}
	h := newAutoDegradeHandler(t, upO, upW, upG)

	rec := callWithHeaders(h, "workbuddy/glm-free", map[string]string{
		auto.OrderHeader: "workbuddy/glm-free,glm/glm-4.6",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Model-Fallback"); got != "" {
		t.Fatalf("未降级时不得出现 X-Model-Fallback，实际 %q", got)
	}
	if upG.count() != 0 {
		t.Errorf("未降级时备胎渠道不应被调用，实际 %d 次", upG.count())
	}
}

// TestAutoOrderHeaderParse 头解析的边界：空白、空项、重复项、超长。
func TestAutoOrderHeaderParse(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"absent", "", nil},
		{"single", "glm/glm-4.6", []string{"glm/glm-4.6"}},
		{"trim", " a/b , c/d ,, ", []string{"a/b", "c/d"}},
		{"dedup", "a/b,a/b,c/d", []string{"a/b", "c/d"}},
		{"only-commas", ",,,", nil},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		if c.raw != "" {
			req.Header.Set(auto.OrderHeader, c.raw)
		}
		got := autoOrderFromHeader(req)
		if len(got) != len(c.want) {
			t.Fatalf("%s: got %v，want %v", c.name, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%s: got %v，want %v", c.name, got, c.want)
			}
		}
	}
	// nil request 不得 panic（内部调用点可能传 nil）。
	if got := autoOrderFromHeader(nil); got != nil {
		t.Fatalf("nil request 应返回 nil，实际 %v", got)
	}
}

// count 本文件专用的调用计数（kindedUpstream 在别处只暴露 calls 切片）。
func (u *kindedUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.calls)
}
