// 别名（`compat.model_map`）在内层 `/v1/chat/completions` 生效的契约锁定（MS-006）。
//
// 背景（MS-003 实测）：`model_map` 原先只在 gateway 的三个兼容端点生效，
// 内层 `runtimeForModel` 只认 `channel/model` 全路径 ⇒ 别名对只走 chat 端点的
// 客户端（pi / dsh 的 openai-completions）完全不可用（400 invalid_model）。
//
// 本文件锁两层：
//  1. 解析语义（resolveAlias）与 `gateway.Router.Resolve` 同源：精确 → 通配最长前缀
//     → 未命中原样返回；含 `/` 不查表；**不做** default_channel 回退。
//  2. 调用点接线：别名经 chatCompletions 解析后**真的**用于路由（上游收到的是
//     映射到的上游模型名），且解析结果被写回用于 `pickModelFallback` 匹配
//     `rule.From`（别名请求同样能命中降级链）。
//
// 诚实边界：这里用的是 `httptest` + 假上游（`aliasUpstream`），覆盖的是
// 「HTTP 入口 → 解析 → 挑号 → 上游请求体」整段真实代码路径，但**未**对真实上游
// 发过任何网路请求，也**未**验证运行实例上的落点（见实现说明「已知限制」）。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/config"
	"wild-work/internal/oczen"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// aliasUpstream 假上游：记录最后一次请求体（用于断言内层真正发给上游的模型名），
// 非流式响应按 OpenAI 形状回填 model 字段（= handler 传入的 dispModel）。
type aliasUpstream struct {
	calls   int
	lastReq string
}

func (u *aliasUpstream) RefreshToken(*auth.Auth) error                        { return nil }
func (u *aliasUpstream) FetchModels(*auth.Auth) ([]provider.ModelInfo, error) { return nil, nil }
func (u *aliasUpstream) FetchModelPricing(*auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}
func (u *aliasUpstream) UserResource(*auth.Auth) (int64, error) { return 0, nil }
func (u *aliasUpstream) UserResourceDetail(*auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, nil
}
func (u *aliasUpstream) DailyCheckin(*auth.Auth) error { return nil }
func (u *aliasUpstream) Classify(st int, body string) provider.ErrKind {
	return (&oczen.Client{}).Classify(st, body)
}
func (u *aliasUpstream) ChatStream(_ *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	u.calls++
	u.lastReq = string(body)
	return io.NopCloser(strings.NewReader("data: [DONE]\n\n")), http.StatusOK, nil, nil
}
func (u *aliasUpstream) Stream(http.ResponseWriter, io.Reader, string) (map[string]any, error) {
	return nil, nil
}
func (u *aliasUpstream) Aggregate(_ io.Reader, model string) (map[string]any, error) {
	return map[string]any{"model": model, "choices": []any{}}, nil
}

// requestModelOf 取出假上游收到的请求体里的 model 字段。
func requestModelOf(t *testing.T, body string) string {
	t.Helper()
	var req struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("上游请求体不是合法 JSON：%v（body=%q）", err, body)
	}
	return req.Model
}

// newAliasHandler 只装别名表的 handler（无渠道，仅用于解析语义单测）。
func newAliasHandler(mm map[string]string) *Handler {
	h := NewHandler(Config{})
	h.SetModelMap(mm)
	return h
}

// newAliasRoutingHandler 装配 oczen（主）+ workbuddy（备）两渠道各一个账号。
func newAliasRoutingHandler(t *testing.T, mm map[string]string, rules []config.ModelFallbackRule) (*Handler, *aliasUpstream, *aliasUpstream, *pool.Pool) {
	t.Helper()
	pa := pool.New(t.TempDir() + "/alias-a.json")
	pa.Add(oczen.AnonymousAuth())
	pb := pool.New(t.TempDir() + "/alias-b.json")
	pb.Add(&auth.Auth{UID: "alias-backup-uid"})
	upA, upB := &aliasUpstream{}, &aliasUpstream{}
	h := NewHandler(Config{
		Runtimes: map[provider.Kind]*Runtime{
			provider.Oczen:     {Kind: provider.Oczen, Pool: pa, Upstream: upA},
			provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: pb, Upstream: upB},
		},
		APIKey: "k", HardCooldown: time.Hour, SoftCooldown: time.Minute,
		ErrThreshold: 1, ErrCooldown: time.Minute,
		ModelFallbacks: rules,
	})
	h.SetModelMap(mm)
	return h, upA, upB, pa
}

// TestResolveAlias 解析语义逐条锁定（与 gateway/router.go 的 Resolve 同源）。
func TestResolveAlias(t *testing.T) {
	h := newAliasHandler(map[string]string{
		"fast":   "traework/deepseek-v4.1-flash",
		"code":   " workbuddy/deepseek-v4.1-flash ", // 映射目标两侧空白必须裁掉
		"a*":     "kilo/aaa",
		"ab*":    "kilo/bbb", // 与 a* 同时命中时取最长前缀
		"fast*":  "kilo/fastprefix",
		"x*y":    "kilo/midstar", // * 在中间 = 普通字符，不参与通配
		"empty*": "",             // 空目标 = 配置残留，按未命中处理
		"Case*":  "kilo/case",
	})
	cases := []struct{ in, want, why string }{
		{"fast", "traework/deepseek-v4.1-flash", "精确命中"},
		{" fast ", "traework/deepseek-v4.1-flash", "裸名两侧空白先 TrimSpace 再查表"},
		{"code", "workbuddy/deepseek-v4.1-flash", "映射目标两侧空白被裁掉"},
		{"a/zz", "a/zz", "含 / 一律不查表（通配 key 也不参与匹配）"},
		{"fastest", "kilo/fastprefix", "无精确键时通配 fast* 命中"},
		{"x*y", "kilo/midstar", "* 在中间时该 key 按精确键命中"},
		{"xay", "xay", "* 在中间不参与通配 ⇒ 未命中原样返回"},
		{"unknown", "unknown", "未命中原样返回"},
		{"traework/deepseek-v4.1-flash", "traework/deepseek-v4.1-flash", "全路径零变化"},
		{"cline-free/glm-5", "cline-free/glm-5", "上游命名空间直通形态零变化"},
		{"FAST", "FAST", "大小写敏感（与 gateway 的 map 查表语义一致）"},
		{"caseX", "caseX", "通配大小写敏感（Case* 不匹配小写）"},
		{"CaseX", "kilo/case", "通配命中同一前缀"},
		{"emptyX", "emptyX", "通配命中但目标为空 ⇒ 按未命中处理，原样返回"},
		{"", "", "空串原样返回"},
	}
	for _, c := range cases {
		if got := h.resolveAlias(c.in); got != c.want {
			t.Errorf("resolveAlias(%q) = %q，want %q（%s）", c.in, got, c.want, c.why)
		}
	}
}

// TestResolveAliasWildcardLongestPrefix 单独锁「取最长前缀」：两个通配键都命中时
// 必须选更长的那个（否则 a* 会盖住 ab*，落点会随 map 遍历顺序漂移）。
// 注意通配只作用于**裸名**（含 / 的模型名在上一步就原样返回了，与 gateway 一致），
// 所以通配 key 的前缀里不会出现 "/"。
func TestResolveAliasWildcardLongestPrefix(t *testing.T) {
	h := newAliasHandler(map[string]string{
		"a*":  "kilo/short",
		"ab*": "kilo/long",
	})
	if got := h.resolveAlias("abc"); got != "kilo/long" {
		t.Errorf("abc → %q，want kilo/long（ab* 比 a* 长，必须取最长前缀）", got)
	}
	if got := h.resolveAlias("az"); got != "kilo/short" {
		t.Errorf("az → %q，want kilo/short（只有 a* 命中）", got)
	}
}

// TestResolveAliasEmptyOrNilIsNoop 空表/nil 表必须完全 no-op（零迁移不变量）。
func TestResolveAliasEmptyOrNilIsNoop(t *testing.T) {
	nilTable := NewHandler(Config{}) // 从未 SetModelMap ⇒ modelMap == nil
	nilSet := NewHandler(Config{})
	nilSet.SetModelMap(nil) // nil 归一为空 map
	emptySet := newAliasHandler(map[string]string{})

	for _, tc := range []struct {
		name string
		h    *Handler
	}{
		{"nil（未注入）", nilTable},
		{"SetModelMap(nil)", nilSet},
		{"空 map", emptySet},
	} {
		for _, in := range []string{"fast", "code", "complex", "fast/x", "traework/deepseek-v4.1-flash", " fast "} {
			if got := tc.h.resolveAlias(in); got != in {
				t.Errorf("%s：resolveAlias(%q) = %q，必须原样返回", tc.name, in, got)
			}
		}
	}
}

// TestSetModelMapCopies 注入的 map 必须被拷贝：调用方事后原地改写同一张表
// 不得影响 handler（否则热更新/并发读会看到半改状态）。
func TestSetModelMapCopies(t *testing.T) {
	src := map[string]string{"fast": "traework/a"}
	h := newAliasHandler(src)
	src["fast"] = "traework/b" // 调用方原地改写
	src["code"] = "traework/c" // 调用方新增

	if got := h.resolveAlias("fast"); got != "traework/a" {
		t.Errorf("resolveAlias(fast) = %q，want traework/a（注入后调用方改写不应影响 handler）", got)
	}
	if got := h.resolveAlias("code"); got != "code" {
		t.Errorf("resolveAlias(code) = %q，want code（注入后调用方新增不应影响 handler）", got)
	}
}

// TestChatCompletionsBareNameStillInvalidWithoutMap 空表时裸名必须仍报原来的错误串
// ——这是「不放宽成猜渠道」的硬证据（验收标准第 4 条）。
func TestChatCompletionsBareNameStillInvalidWithoutMap(t *testing.T) {
	for _, tc := range []struct {
		name string
		mm   map[string]string
	}{
		{"nil 表", nil},
		{"空表", map[string]string{}},
	} {
		h, upA, upB, _ := newAliasRoutingHandler(t, tc.mm, nil)
		rec := callModel(h, "fast")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s：裸名应仍 400，实际 %d：%s", tc.name, rec.Code, rec.Body.String())
		}
		if body := rec.Body.String(); !strings.Contains(body, "model must use explicit prefix") {
			t.Errorf("%s：错误串变了，want 含 \"model must use explicit prefix\"，实际：%s", tc.name, body)
		}
		if upA.calls != 0 || upB.calls != 0 {
			t.Errorf("%s：裸名未命中别名时不得派发上游，实际 oczen=%d workbuddy=%d", tc.name, upA.calls, upB.calls)
		}
	}
}

// TestChatCompletionsAliasRoutesToMappedModel 端到端（handler 内部真实代码路径）：
// 客户端发 `model: "fast"` ⇒ 解析为 workbuddy/glm-free ⇒ workbuddy 上游收到
// `model: "glm-free"`（上游模型名），oczen 一次没收到；响应 model 字段仍回显
// 客户端原值 "fast"（MS-003 已确认的回显语义，本轮不改）。
func TestChatCompletionsAliasRoutesToMappedModel(t *testing.T) {
	h, upA, upB, _ := newAliasRoutingHandler(t, map[string]string{
		"fast": "workbuddy/glm-free",
	}, nil)

	rec := callModel(h, "fast")
	if rec.Code != http.StatusOK {
		t.Fatalf("别名请求应 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if upB.calls != 1 {
		t.Fatalf("workbuddy 上游收到 %d 次请求，want 1", upB.calls)
	}
	if upA.calls != 0 {
		t.Errorf("oczen 上游不应收到请求，实际 %d 次", upA.calls)
	}
	if got := requestModelOf(t, upB.lastReq); got != "glm-free" {
		t.Errorf("上游收到 model = %q，want glm-free（别名必须解析成映射目标的上游模型名）", got)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON：%v（body=%s）", err, rec.Body.String())
	}
	if resp["model"] != "fast" {
		t.Errorf("响应 model = %v，want fast（回显客户端原值，回显语义不变）", resp["model"])
	}
}

// TestChatCompletionsAliasHitsFallbackChain 解析结果必须写回用于路由的模型变量：
// 降级链规则的 `From` 是真模型全路径，若 pickModelFallback 仍拿别名去比，
// 别名请求永远不命中降级链（口径 4 的回归护栏）。
func TestChatCompletionsAliasHitsFallbackChain(t *testing.T) {
	h, _, upB, pa := newAliasRoutingHandler(t,
		map[string]string{"fast": "oczen/big-pickle"},
		[]config.ModelFallbackRule{{From: "oczen/big-pickle", To: []string{"workbuddy/glm-free"}}},
	)
	// 主渠道唯一账号对 big-pickle 模型级冷却（账号本身健康）⇒ 入口降级链顶上
	pa.CooldownModel(oczen.AnonymousUID, "big-pickle", 10*time.Minute, "test")

	rec := callModel(h, "fast")
	if rec.Code != http.StatusOK {
		t.Fatalf("别名请求应命中降级链返回 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Model-Fallback"); got != "fast -> workbuddy/glm-free" {
		t.Errorf("X-Model-Fallback = %q，want \"fast -> workbuddy/glm-free\"（别名必须能匹配 rule.From）", got)
	}
	if upB.calls != 1 {
		t.Errorf("备胎渠道收到 %d 次请求，want 1", upB.calls)
	}
	if got := requestModelOf(t, upB.lastReq); got != "glm-free" {
		t.Errorf("备胎上游收到 model = %q，want glm-free", got)
	}
}

// TestChatCompletionsFullPathIgnoresMap 全路径请求零行为变化：即使 map 里存在
// 同名 key，含 `/` 的模型名也不得被改写。
func TestChatCompletionsFullPathIgnoresMap(t *testing.T) {
	h, upA, upB, _ := newAliasRoutingHandler(t, map[string]string{
		"workbuddy/glm-free": "oczen/big-pickle", // 键里带 / ⇒ 永不参与查表
	}, nil)

	rec := callModel(h, "workbuddy/glm-free")
	if rec.Code != http.StatusOK {
		t.Fatalf("全路径请求应 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if upB.calls != 1 || upA.calls != 0 {
		t.Errorf("全路径请求落点被改写：oczen=%d workbuddy=%d，want 0/1", upA.calls, upB.calls)
	}
	if got := requestModelOf(t, upB.lastReq); got != "glm-free" {
		t.Errorf("上游收到 model = %q，want glm-free", got)
	}
}

// TestChatCompletionsAliasHotUpdate 清空后立即回到原错误（面板清空别名 = 关闭解析）。
func TestChatCompletionsAliasHotUpdate(t *testing.T) {
	h, _, upB, _ := newAliasRoutingHandler(t, map[string]string{"fast": "workbuddy/glm-free"}, nil)
	if rec := callModel(h, "fast"); rec.Code != http.StatusOK {
		t.Fatalf("注入别名后应 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	h.SetModelMap(nil) // 模拟面板清空 model_map
	rec := callModel(h, "fast")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("清空别名后应回到 400，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "model must use explicit prefix") {
		t.Errorf("清空别名后错误串变了：%s", body)
	}
	if upB.calls != 1 {
		t.Errorf("清空后不得再派发上游，workbuddy 累计 %d 次（want 1）", upB.calls)
	}
}
