// 自动选路的**降级深度**行为锁定（R166）：
// 决策给出的 Chain 只有 fallback_depth(3) 条（给响应头读），降级必须走 Order 全序——
// 否则第 4 个候选起永远降不下去，三条链一起冷就把 503 砸给客户端，而池子里还有能用的模型。
package server

import (
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/oczen"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// newAutoFbHandler 装配 4 条渠道（oczen / workbuddy / traework / glm），各 1 个健康账号。
// 顺序即 order 里的顺序：前三条会被模型级冷却，第四条留容量。
func newAutoFbHandler(t *testing.T) *Handler {
	t.Helper()
	up := &codeUpstream{status: 200}

	pa := pool.New(t.TempDir() + "/state-oczen.json")
	pa.Add(oczen.AnonymousAuth())
	pb := pool.New(t.TempDir() + "/state-wb.json")
	pb.Add(&auth.Auth{UID: "wb-uid"})
	pc := pool.New(t.TempDir() + "/state-trae.json")
	pc.Add(&auth.Auth{UID: "trae-uid"})
	pd := pool.New(t.TempDir() + "/state-glm.json")
	pd.Add(&auth.Auth{UID: "glm-uid"})

	return NewHandler(Config{
		Runtimes: map[provider.Kind]*Runtime{
			provider.Oczen:     {Kind: provider.Oczen, Pool: pa, Upstream: up, SingleAccount: true},
			provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: pb, Upstream: up},
			provider.TraeWork:  {Kind: provider.TraeWork, Pool: pc, Upstream: up},
			provider.GLM:       {Kind: provider.GLM, Pool: pd, Upstream: up},
		},
		APIKey: "k", HardCooldown: time.Hour, SoftCooldown: time.Minute,
		ErrThreshold: 3, ErrCooldown: time.Minute,
	})
}

// TestPickAutoFallbackWalksBeyondDisplayChain 核心断言：
// order = 4 条候选、前 3 条（= 展示链的全部内容）都冷 ⇒ 必须降到第 4 条，而不是返回空。
//
// 这一条正是 R166 修掉的行为：旧实现拿 Chain(3) 当降级链，走到 chain[2] 就用尽，
// 于是 pickModelFallback 接手——而静态链的 From 是按具体模型名配的，对 auto 选出的
// 模型一条都不命中 ⇒ 返回空 ⇒ 请求失败。
func TestPickAutoFallbackWalksBeyondDisplayChain(t *testing.T) {
	h := newAutoFbHandler(t)

	order := []string{
		"oczen/mimo-v2.6-flash-free",
		"workbuddy/deepseek-v4-pro",
		"traework/DeepSeek-V4-Pro",
		"glm/glm-4.6", // 展示链（depth=3）够不到的那一条
	}
	// 前三条模型级冷却（账号本身健康——这是「额度/限流见底」而非「渠道挂了」）
	for _, m := range order[:3] {
		kind, upID, ok := provider.SplitModelID(m)
		if !ok {
			t.Fatalf("夹具模型 ID 非法：%q", m)
		}
		rt := h.getRuntime(kind)
		for _, s := range rt.Pool.List() {
			rt.Pool.CooldownModel(s.UID, upID, 10*time.Minute, "test")
		}
	}

	rt, model, err := h.runtimeForModel(order[0])
	if err != nil {
		t.Fatalf("runtimeForModel(%q) 失败：%v", order[0], err)
	}
	got, primaryAvail := h.pickAutoFallback(rt, model, order, false)
	if primaryAvail {
		t.Fatal("首要模型已全冷，primaryAvail 应为 false")
	}
	if got != order[3] {
		t.Fatalf("应降级到全序第 4 条 %q（展示链只到这里），实际 %q——降级被 fallback_depth 截断了", order[3], got)
	}
}

// TestPickAutoFallbackEmptyWhenAllDead 全序全冷 ⇒ 返回空串（由调用方透传/503），
// 不得靠「凑一个」把请求打到已冷却的账号上。
func TestPickAutoFallbackEmptyWhenAllDead(t *testing.T) {
	h := newAutoFbHandler(t)

	order := []string{
		"oczen/mimo-v2.6-flash-free",
		"workbuddy/deepseek-v4-pro",
		"traework/DeepSeek-V4-Pro",
		"glm/glm-4.6",
	}
	for _, m := range order {
		kind, upID, _ := provider.SplitModelID(m)
		rt := h.getRuntime(kind)
		for _, s := range rt.Pool.List() {
			rt.Pool.CooldownModel(s.UID, upID, 10*time.Minute, "test")
		}
	}
	rt, model, err := h.runtimeForModel(order[0])
	if err != nil {
		t.Fatalf("runtimeForModel 失败：%v", err)
	}
	if got, _ := h.pickAutoFallback(rt, model, order, false); got != "" {
		t.Fatalf("全序全冷时应返回空串，实际 %q", got)
	}
}

// TestPickAutoFallbackSkipsSelf 全序首项即当前模型时不得「自己降级到自己」：
// 否则 serveRotation 会拿同一个已冷的模型原样再跑一遍，白烧一次轮转。
func TestPickAutoFallbackSkipsSelf(t *testing.T) {
	h := newAutoFbHandler(t)
	order := []string{"workbuddy/deepseek-v4-pro", "glm/glm-4.6"}
	rt := h.getRuntime(provider.WorkBuddy)
	for _, s := range rt.Pool.List() {
		rt.Pool.CooldownModel(s.UID, "deepseek-v4-pro", 10*time.Minute, "test")
	}
	got, _ := h.pickAutoFallback(rt, "deepseek-v4-pro", order, false)
	if got == "workbuddy/deepseek-v4-pro" {
		t.Fatal("不得把自己当作备胎（空转）")
	}
	if got != "glm/glm-4.6" {
		t.Fatalf("应跳到 glm/glm-4.6，实际 %q", got)
	}
}
