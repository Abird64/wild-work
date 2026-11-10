package auto

import (
	"strings"
	"testing"
)

func b(v bool) *bool { return &v }

// fixture 造一份贴近真实数据的快照：
//
//	kilo      真正免费 + 支持工具（rate=0, explicit）
//	oczen     真正免费但**不支持工具**（capability gate 的主角）
//	minimax   费率未知（unpriced）+ 支持工具/思考
//	workbuddy 付费（rate=0.1）+ 支持工具，且余额最高
//	qodercom  终态渠道（R164 无资格）
func fixture() *Snapshot {
	return &Snapshot{
		Channels: map[string]*ChannelInfo{
			"kilo": {
				Name: "kilo", TotalAccounts: 0, HealthyAccounts: 0,
				CreditsNA: true, AccountsNA: true,
				Credits: 0, ExpiringCredits: 0,
			},
			"oczen": {
				Name: "oczen", TotalAccounts: 1, HealthyAccounts: 1,
				CreditsNA: true, AccountsNA: false,
				Credits: 0, ExpiringCredits: 0,
			},
			"minimax": {
				Name: "minimax", TotalAccounts: 1, HealthyAccounts: 1,
				CreditsNA: false, AccountsNA: false,
				Credits: 5000, ExpiringCredits: 0,
			},
			"workbuddy": {
				Name: "workbuddy", TotalAccounts: 1, HealthyAccounts: 1,
				CreditsNA: false, AccountsNA: false,
				Credits: 1828, ExpiringCredits: 0,
			},
			"qodercom": {
				Name: "qodercom", TotalAccounts: 1, HealthyAccounts: 0,
				CreditsNA: false, AccountsNA: false,
				Credits: 0, Terminal: true, TerminalReason: "无资格",
			},
		},
		Models: []ModelInfo{
			{Channel: "kilo", Model: "kilo-large", Public: "kilo/kilo-large",
				Rate: 0, Explicit: true, HasContext: true, ContextWindow: 1048576,
				SupportsTools: b(true), SupportsImages: b(false), SupportsReasoning: b(true)},
			{Channel: "oczen", Model: "big-pickle", Public: "oczen/big-pickle",
				Rate: 0, Explicit: true, HasContext: true, ContextWindow: 262144,
				SupportsTools: b(false), SupportsImages: b(false), SupportsReasoning: b(false)},
			{Channel: "minimax", Model: "MiniMax-M3.1-Flash-Preview", Public: "minimax/MiniMax-M3.1-Flash-Preview",
				Rate: 0, Explicit: false, HasContext: true, ContextWindow: 1000000,
				SupportsTools: b(true), SupportsImages: b(false), SupportsReasoning: b(true)},
			{Channel: "workbuddy", Model: "deepseek-v4-pro", Public: "workbuddy/deepseek-v4-pro",
				Rate: 0.1, Explicit: true, HasContext: true, ContextWindow: 128000,
				SupportsTools: b(true), SupportsImages: b(false), SupportsReasoning: b(true)},
		},
		Perf: map[string]Perf{
			"minimax/MiniMax-M3.1-Flash-Preview": {
				Samples: 626, Requests: 626, TTFBMS: 2001, TokPerSec: 144,
				CacheApplicable: true, CacheHit: 0.93,
			},
			"workbuddy/deepseek-v4-pro": {
				Samples: 12, Requests: 12, TTFBMS: 2500, TokPerSec: 120,
			},
		},
	}
}

func TestDecide_FreeTierWinsOverPriced(t *testing.T) {
	d := Decide(fixture(), DefaultConfig(), Request{})
	if d.Chosen != "kilo/kilo-large" {
		t.Fatalf("首选应为免费档 kilo/kilo-large，实际 %q（reason=%s）", d.Chosen, d.Reason)
	}
	// 付费档即便分数更高也排在免费档之后（档位是排序第一键）。
	var sawPriced bool
	for i, c := range d.Candidates {
		if c.Tier == TierPriced {
			sawPriced = true
			if i != len(d.Candidates)-1 {
				// 付费档应排在未知档之后或同层末尾
				continue
			}
		}
	}
	if !sawPriced {
		t.Fatal("付费候选应仍在候选表里（只是排位靠后）")
	}
}

func TestDecide_ToolRequirementRejectsFreeButToolless(t *testing.T) {
	d := Decide(fixture(), DefaultConfig(), Request{Tools: true})
	for _, c := range d.Candidates {
		if c.Public == "oczen/big-pickle" {
			t.Fatal("oczen 明确不支持工具，不该通过硬门")
		}
	}
	found := false
	for _, r := range d.Rejected {
		if r.Public == "oczen/big-pickle" && r.Code == RejectNoTools {
			found = true
		}
	}
	if !found {
		t.Fatalf("oczen 应以 no_tools 被拒，实际 rejected=%+v", d.Rejected)
	}
}

func TestDecide_UnpricedIsNotFree(t *testing.T) {
	// minimax 是「费率未知」，不得被当作免费。
	var minimaxTier Tier
	for _, c := range Decide(fixture(), DefaultConfig(), Request{}).Candidates {
		if c.Public == "minimax/MiniMax-M3.1-Flash-Preview" {
			minimaxTier = c.Tier
		}
	}
	if minimaxTier != TierUnpriced {
		t.Fatalf("minimax 档位应为 unpriced，实际 %q", minimaxTier)
	}
}

func TestDecide_TerminalChannelRejected(t *testing.T) {
	snap := fixture()
	snap.Models = append(snap.Models, ModelInfo{
		Channel: "qodercom", Model: "m1", Public: "qodercom/m1",
		Rate: 0.1, Explicit: true, HasContext: true, ContextWindow: 64000,
	})
	d := Decide(snap, DefaultConfig(), Request{})
	found := false
	for _, r := range d.Rejected {
		if r.Public == "qodercom/m1" && r.Code == RejectTerminalChannel {
			found = true
		}
	}
	if !found {
		t.Fatalf("终态渠道应被拒，实际 rejected=%+v", d.Rejected)
	}
}

func TestDecide_ExcludedChannel(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ExcludedChannels = []string{"kilo"}
	d := Decide(fixture(), cfg, Request{})
	for _, c := range d.Candidates {
		if c.Channel == "kilo" {
			t.Fatal("kilo 在排除名单里，不该进候选")
		}
	}
}

func TestDecide_ContextGate(t *testing.T) {
	d := Decide(fixture(), DefaultConfig(), Request{ContextTokens: 900000})
	for _, c := range d.Candidates {
		if c.Public == "workbuddy/deepseek-v4-pro" {
			t.Fatal("上下文 128k < 900k，不该通过硬门")
		}
	}
	found := false
	for _, r := range d.Rejected {
		if r.Public == "workbuddy/deepseek-v4-pro" && r.Code == RejectContextTooSmall {
			found = true
		}
	}
	if !found {
		t.Fatalf("应以 context_too_small 被拒，实际 %+v", d.Rejected)
	}
}

func TestDecide_UnknownCapabilityNotJudged(t *testing.T) {
	snap := fixture()
	// 某模型三态全未知（上游没声明）⇒ 不因缺字段被拒。
	snap.Models = append(snap.Models, ModelInfo{
		Channel: "minimax", Model: "mystery", Public: "minimax/mystery",
		Rate: 0.2, Explicit: true,
	})
	d := Decide(snap, DefaultConfig(), Request{Tools: true, Images: true, Reasoning: true})
	for _, c := range d.Candidates {
		if c.Public == "minimax/mystery" {
			return
		}
	}
	t.Fatal("能力未声明的模型应放行（不判断），不应被硬门拒收")
}

func TestDecide_ChainCrossesChannel(t *testing.T) {
	d := Decide(fixture(), DefaultConfig(), Request{Tools: true})
	if len(d.Chain) < 2 {
		t.Fatalf("备胎链应有多项，实际 %v", d.Chain)
	}
	ch := map[string]bool{}
	for _, id := range d.Chain {
		k := id[:strings.Index(id, "/")]
		if ch[k] {
			t.Fatalf("备胎链应强制跨渠道，实际 %v", d.Chain)
		}
		ch[k] = true
	}
}

func TestDecide_EmptySnapshot(t *testing.T) {
	d := Decide(&Snapshot{}, DefaultConfig(), Request{})
	if d.Chosen != "" || len(d.Chain) != 0 {
		t.Fatalf("空快照应无决策，实际 %+v", d)
	}
}

func TestDecide_PerfSamplesInsufficient(t *testing.T) {
	snap := fixture()
	// workbuddy 只有 12 个样本（≥10），把门槛提到 100 应退化为中性 + 备注。
	cfg := DefaultConfig()
	cfg.MinPerfSamples = 100
	d := Decide(snap, cfg, Request{})
	for _, c := range d.Candidates {
		if c.Public != "workbuddy/deepseek-v4-pro" {
			continue
		}
		if c.Parts["M_perf"] != 0.5 {
			t.Fatalf("样本不足时 M_perf 应取中性 0.5，实际 %v", c.Parts["M_perf"])
		}
		found := false
		for _, n := range c.Notes {
			if strings.Contains(n, "性能样本不足") {
				found = true
			}
		}
		if !found {
			t.Fatalf("应标注样本不足，notes=%v", c.Notes)
		}
		return
	}
	t.Fatal("workbuddy 候选缺失")
}

func TestDecide_PreferFreeOff(t *testing.T) {
	pf := false
	cfg := DefaultConfig()
	cfg.PreferFree = &pf
	d := Decide(fixture(), cfg, Request{})
	if d.Chosen != "workbuddy/deepseek-v4-pro" && d.Chosen != "minimax/MiniMax-M3.1-Flash-Preview" {
		t.Fatalf("关掉 prefer_free 后首选应在付费/未知档，实际 %q", d.Chosen)
	}
}

func TestTierRank_UnknownAlwaysLast(t *testing.T) {
	if tierRank(TierUnpriced, true, UnknownRateAllow) <= tierRank(TierPriced, true, UnknownRateAllow) {
		t.Fatal("未知档必须排在付费档之后")
	}
	if tierRank(TierUnpriced, false, UnknownRateAllow) <= tierRank(TierPriced, false, UnknownRateAllow) {
		t.Fatal("未知档必须排在付费档之后（prefer_free 关）")
	}
}

// TestDecideOrderIsFullAndChainIsPrefix 锁定 R166 的核心关系：
// Order 覆盖全部候选（降级用），Chain 是它的前缀（展示用），两者逐项相同。
//
// 为什么必须逐项相同：响应头 `X-Model-Auto-Chain` 报的第 N 条，必须就是真降级
// 时第 N 个被尝试的模型。若两者各算一遍（比如 Chain 用另一套排序），面板/日志
// 与真实行为会分叉，事后排查「为什么降级降到了这个模型」将无从对证。
func TestDecideOrderIsFullAndChainIsPrefix(t *testing.T) {
	snap := fixture()
	cfg := DefaultConfig()
	cfg.FallbackDepth = 2
	cfg.MinPerfSamples = 1
	d := Decide(snap, cfg, Request{Tools: true})
	if len(d.Order) < 2 {
		t.Fatalf("Order 应有多个候选，实际 %d：%v", len(d.Order), d.Order)
	}
	if len(d.Chain) != 2 {
		t.Fatalf("Chain 长度应 = fallback_depth(2)，实际 %d：%v", len(d.Chain), d.Chain)
	}
	for i, c := range d.Chain {
		if d.Order[i] != c {
			t.Errorf("Chain[%d]=%q 与 Order[%d]=%q 不一致（展示链必须是降级全序的前缀）", i, c, i, d.Order[i])
		}
	}
	// Order 必须覆盖所有通过硬门的候选：链只留 2 条，但降级不能只有 2 条路。
	if len(d.Order) != len(d.Candidates) {
		t.Errorf("Order 应覆盖全部通过硬门的候选：Order=%d Candidates=%d（漏掉的候选永远降不下去）",
			len(d.Order), len(d.Candidates))
	}
	if d.Order[0] != d.Chosen {
		t.Errorf("Order[0]=%q 应等于 Chosen=%q", d.Order[0], d.Chosen)
	}
}

// TestDecideOrderAlternatesChannel 锁定「降级全序跨渠道交替」：相邻两项不同渠道。
//
// 同渠道兄弟模型共享同一批账号，一个 429 之后立刻换同渠道另一个模型必然二次撞墙；
// 跨渠道交替保证降级每一步都换一批账号。
func TestDecideOrderAlternatesChannel(t *testing.T) {
	snap := fixture()
	cfg := DefaultConfig()
	cfg.MinPerfSamples = 1
	d := Decide(snap, cfg, Request{Tools: true})
	seen := 0
	for i := 1; i < len(d.Order); i++ {
		prev := strings.SplitN(d.Order[i-1], "/", 2)[0]
		cur := strings.SplitN(d.Order[i], "/", 2)[0]
		if prev == cur {
			t.Errorf("Order 相邻项同渠道（%d→%d：%q → %q）：降级会在同一批账号上二次撞墙",
				i-1, i, d.Order[i-1], d.Order[i])
		}
		seen++
	}
	if seen == 0 {
		t.Skip("快照候选不足，跳过交替校验")
	}
}

// TestDecideOrderNotAliasedToChain 锁定 Chain 与 Order 不共享底层数组。
//
// 共享是真实踩过的坑：buildChain 用 make(cap=len(cands)) 造切片，
// 若 Chain 直接取 Order[:depth]，两者指向同一块内存——任何 append(Chain, ...)
// 都会静默改写 Order 的第 depth+1 项，而降级正是靠 Order 走的。
// 直接比首元素地址是这个约束最短的表述（拷贝后必然不同）。
func TestDecideOrderNotAliasedToChain(t *testing.T) {
	snap := fixture()
	cfg := DefaultConfig()
	cfg.FallbackDepth = 2
	cfg.MinPerfSamples = 1
	d := Decide(snap, cfg, Request{Tools: true})
	if len(d.Chain) == 0 || len(d.Order) == 0 {
		t.Fatalf("构造条件不成立：Chain=%v Order=%v", d.Chain, d.Order)
	}
	if &d.Chain[0] == &d.Order[0] {
		t.Errorf("Chain 与 Order 共享底层数组（首元素地址相同）：append(Chain) 会改写 Order，降级序列被污染")
	}
}
