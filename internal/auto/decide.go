package auto

import (
	"fmt"
	"strings"
)

// Decide 依据快照 + 请求要求做一次自动选路。
//
// 流程：硬门过滤 → 打分 → 排序 → 派生备胎链。纯函数，无 IO、无全局状态。
//
// 快照为空（没有任何渠道/模型）时返回空 Decision（Chosen 为空串），
// 调用方据此回落到「显式模型名」通路——自动选路只是**快捷方式**，不是唯一入口。
func Decide(snap *Snapshot, cfg Config, req Request) Decision {
	var d Decision
	if snap == nil {
		d.Reason = "无快照，自动选路不可用"
		return d
	}
	cw, mw := cfg.Weights.normalized()
	policy := cfg.UnknownRatePolicy
	if policy != UnknownRateDeprioritize {
		policy = UnknownRateAllow
	}
	minSamples := cfg.MinPerfSamples
	if minSamples <= 0 {
		minSamples = 10
	}
	excluded := make(map[string]bool, len(cfg.ExcludedChannels))
	for _, c := range cfg.ExcludedChannels {
		if c = strings.TrimSpace(c); c != "" {
			excluded[c] = true
		}
	}

	chScores := channelScores(snap, cw, policy)

	for _, m := range snap.Models {
		if m.Public == "" {
			continue
		}
		ch := snap.Channels[m.Channel]
		if ch == nil {
			d.Rejected = append(d.Rejected, Rejection{Public: m.Public, Code: RejectExcludedChannel, Detail: "渠道信息缺失"})
			continue
		}
		if excluded[ch.Name] {
			d.Rejected = append(d.Rejected, Rejection{Public: m.Public, Code: RejectExcludedChannel, Detail: "渠道在排除名单中"})
			continue
		}
		if code, detail := gate(ch, m, req); code != "" {
			d.Rejected = append(d.Rejected, Rejection{Public: m.Public, Code: code, Detail: detail})
			continue
		}

		cs, ok := chScores[m.Channel]
		if !ok {
			cs = &channelScore{credit: 0.5, health: 0.5, expiry: 0.5, stability: 0.5, cost: 0.5}
		}
		chTotal := cs.total(cw)

		p := snap.Perf[m.Public]
		perfV, perfOK := perfQuality(p, minSamples)
		cacheV, cacheOK := cacheQuality(p)
		ctxV := ctxTier(m)
		capV := capRichness(m)
		costV := costFactor(m, policy)
		matV := maturity(p)
		valueV := 0.6*ctxV + 0.4*costV

		mTotal := mw.Value*valueV + mw.Perf*perfV + mw.Cap*capV + mw.Mature*matV + mw.Cache*cacheV
		score := FUSION_CHANNEL*chTotal + FUSION_MODEL*mTotal

		notes := make([]string, 0, 4)
		if !perfOK {
			notes = append(notes, fmt.Sprintf("性能样本不足(%d)", p.Samples))
		}
		if !cacheOK {
			notes = append(notes, "缓存未上报")
		}
		if m.SupportsTools == nil {
			notes = append(notes, "工具能力未声明")
		}
		if !m.HasContext {
			notes = append(notes, "上下文窗口未声明")
		}
		if !m.Explicit {
			notes = append(notes, "费率未知")
		}

		d.Candidates = append(d.Candidates, Candidate{
			Public:       m.Public,
			Channel:      m.Channel,
			Model:        m.Model,
			Tier:         tierOf(m),
			Score:        score,
			ChannelScore: chTotal,
			ModelScore:   mTotal,
			Parts: map[string]float64{
				"C_credit":    cs.credit,
				"C_health":    cs.health,
				"C_expiry":    cs.expiry,
				"C_stability": cs.stability,
				"C_cost":      cs.cost,
				"M_value":     valueV,
				"M_perf":      perfV,
				"M_cap":       capV,
				"M_mature":    matV,
				"M_cache":     cacheV,
			},
			Notes: notes,
		})
	}

	sortCandidates(d.Candidates, preferFree(cfg), policy)
	// Order = 全部候选的跨渠道交替全序（降级用）；Chain = 它的前缀（展示用）。
	// 两者同源保证「响应头里报的第 N 条」与「真降级到的第 N 条」永远是同一个模型。
	d.Order = buildChain(d.Candidates, len(d.Candidates))
	depth := cfg.FallbackDepth
	if depth <= 0 {
		depth = 3
	}
	if depth > len(d.Order) {
		depth = len(d.Order)
	}
	// 显式拷贝：Order 是 make(cap=len(cands)) 出来的，直接用 Order[:depth] 切片会让
	// Chain 与 Order **共享底层数组**，将来任何 append(Chain, ...) 都会静默改写 Order 的第 4 项。
	d.Chain = make([]string, depth)
	copy(d.Chain, d.Order[:depth])
	if len(d.Chain) > 0 {
		d.Chosen = d.Chain[0]
		for _, c := range d.Candidates {
			if c.Public == d.Chosen {
				d.Reason = describeReason(c, tierLabel(c.Tier))
				break
			}
		}
	} else {
		d.Reason = "没有满足硬门约束的候选"
	}
	return d
}

// tierLabel 档位的中文标签。
func tierLabel(t Tier) string {
	switch t {
	case TierFree:
		return "免费"
	case TierPriced:
		return "付费"
	default:
		return "费率未知"
	}
}

// preferFree 取「是否优先免费」：未配置 ⇒ 默认 true。
func preferFree(cfg Config) bool {
	if cfg.PreferFree == nil {
		return true
	}
	return *cfg.PreferFree
}
