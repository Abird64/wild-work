package auto

import (
	"fmt"
	"sort"
	"strings"
)

// 融合权重：综合分 = FUSION_CHANNEL·渠道分 + FUSION_MODEL·模型分。
//
// 为什么模型权重更高（0.65 vs 0.35）：渠道维度衡量的是「这家渠道靠不靠谱」
// （余额、健康、临期、稳定性、单价），模型维度衡量的是「这个模型本身好不好用」
// （质价比、实测速度、能力覆盖、成熟度、缓存）。同一模型换渠道拿到的能力是一样的，
// 渠道差异只在「能不能稳定跑」这一层；真正决定一次对话体验的是模型侧。
const (
	FUSION_CHANNEL = 0.35
	FUSION_MODEL   = 0.65
)

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// tierOf 按显式倍率判定成本档位。
//
// 关键：**Explicit=false（上游没给倍率）不等于免费**，落在 TierUnpriced。
// provider.ModelPricing.Explicit 的语义在这里被完整继承（IsExplicit 兼容 nil=true）。
func tierOf(m ModelInfo) Tier {
	if !m.Explicit {
		return TierUnpriced
	}
	if m.Rate <= 0 {
		return TierFree
	}
	return TierPriced
}

// tierRank 档位在排序中的先后（越小越靠前）。
//
// PreferFree=true（默认，用户诉求第一条）：免费 > 付费 > 未知。
// PreferFree=false：付费 > 免费 > 未知（未知始终垫底——「不知道多少钱」不该比「知道多少钱」更优先）。
// UnknownRatePolicy=deprioritize 时未知档再额外降一档并按 costFactor 打折。
func tierRank(t Tier, preferFree bool, policy string) int {
	switch t {
	case TierFree:
		if preferFree {
			return 0
		}
		return 1
	case TierPriced:
		if preferFree {
			return 1
		}
		return 0
	default: // TierUnpriced
		if policy == UnknownRateDeprioritize {
			return 3
		}
		return 2
	}
}

// ctxTier 上下文窗口档位（越大越好）：反映「能吃多长的上下文」。
//
// 门槛依据（经验值）：≥256k=1.0（长上下文 agent 的甜区）；≥128k=0.8；≥64k=0.55；
// ≥32k=0.35；其余 0.2。HasContext 为假（上游没给）时取中性 0.5——
// 缺失信息不当成确定结论（不变量：与 I6 同姿态）。
func ctxTier(m ModelInfo) float64 {
	if !m.HasContext || m.ContextWindow <= 0 {
		return 0.5
	}
	switch {
	case m.ContextWindow >= 256000:
		return 1.0
	case m.ContextWindow >= 128000:
		return 0.8
	case m.ContextWindow >= 64000:
		return 0.55
	case m.ContextWindow >= 32000:
		return 0.35
	default:
		return 0.2
	}
}

// capRichness 模型能力广度（0..1）：声明支持的能力越多、覆盖越全。
//
// 未知（未声明）计 0.5（中性）：既不因缺字段判死刑，也不白送满分。
func capRichness(m ModelInfo) float64 {
	sum, n := 0.0, 3
	for _, b := range []*bool{m.SupportsTools, m.SupportsImages, m.SupportsReasoning} {
		switch {
		case b == nil:
			sum += 0.5
		case *b:
			sum += 1
		default:
			// 明确不支持 = 0
		}
	}
	_ = n
	return sum / 3
}

// perfQuality 实测性能分（0..1），复用前端 rankCandidates 的公式：
//
//	pTtfb = clamp((12000 - ttfb) / 9000)
//	pTps  = clamp(tps / 200)
//	P     = 0.6·pTtfb + 0.4·pTps
//
// 样本不足（< MinPerfSamples）时返回中性 0.5 并标注「样本不足」——
// 不让一条 2 次请求的记录压过 600 次请求的实测。
func perfQuality(p Perf, minSamples int) (float64, bool) {
	if p.Samples < minSamples {
		return 0.5, false
	}
	ttfb := p.TTFBMS
	if ttfb < 0 {
		ttfb = 0
	}
	pTtfb := clamp01((12000 - ttfb) / 9000)
	pTps := clamp01(p.TokPerSec / 200)
	return 0.6*pTtfb + 0.4*pTps, true
}

// costFactor 单价因子（越大越便宜）：成本 = 1/factor。
//
// 免费档 = 1.0（无成本）；付费档按倍率折算，倍率 0.08 → 0.76、0.5 → 0.33；
// 未知档取 0.85（轻微折扣，因为它可能其实更便宜），unknown_rate_policy=deprioritize 时取 0.6。
func costFactor(m ModelInfo, policy string) float64 {
	switch tierOf(m) {
	case TierFree:
		return 1.0
	case TierPriced:
		return 1.0 / (1.0 + m.Rate*4.0)
	default:
		if policy == UnknownRateDeprioritize {
			return 0.6
		}
		return 0.85
	}
}

// maturity 成熟度分（0..1）：请求量 + 性能样本量的置信度。
func maturity(p Perf) float64 {
	req := clamp01(float64(p.Requests) / 100)
	smp := clamp01(float64(p.Samples) / 50)
	return 0.6*req + 0.4*smp
}

// cacheQuality 缓存命中分（0..1）。
//
// 不变量 24：上游未上报缓存命中 ⇒ **不是 0%**，取中性 0.5 并标注「缓存未上报」。
func cacheQuality(p Perf) (float64, bool) {
	if !p.CacheApplicable {
		return 0.5, false
	}
	return clamp01(p.CacheHit), true
}

// gate 对单个候选做硬门判定。返回空串 = 通过；否则返回原因码。
//
// 硬门语义（全部是「明确不满足」才拒收；**未声明/未知一律放行**）：
//
//	G0 渠道被黑名单排除
//	G0' 渠道处于终态（如 R164 无资格）
//	G1 渠道无任何健康账号（匿名免费池无账号概念 ⇒ 放行）
//	G2 该模型在渠道上处于模型级冷却
//	G3 请求需要工具调用，但模型明确不支持
//	G4 请求含图片，但模型明确不支持
//	G5 请求要思考，但模型明确不支持
//	G6 请求上下文超出模型已知窗口
//	G7 请求 max_tokens 超出模型已知输出上限
func gate(ch *ChannelInfo, m ModelInfo, req Request) (string, string) {
	if ch == nil {
		return RejectExcludedChannel, "渠道信息缺失"
	}
	if ch.Terminal {
		r := ch.TerminalReason
		if r == "" {
			r = "渠道处于终态"
		}
		return RejectTerminalChannel, r
	}
	if !ch.AccountsNA && ch.HealthyAccounts <= 0 {
		return RejectNoHealthyAcct, "渠道内无健康账号"
	}
	if ch.CoolingModels != nil && ch.CoolingModels[m.Model] {
		return RejectModelCooling, "该模型在此渠道处于模型级冷却"
	}
	if req.Tools && m.SupportsTools != nil && !*m.SupportsTools {
		return RejectNoTools, "模型不支持工具调用"
	}
	if req.Images && m.SupportsImages != nil && !*m.SupportsImages {
		return RejectNoImages, "模型不支持图片输入"
	}
	if req.Reasoning && m.SupportsReasoning != nil && !*m.SupportsReasoning {
		return RejectNoReasoning, "模型不支持思考/推理"
	}
	if req.ContextTokens > 0 && m.HasContext && m.ContextWindow > 0 && m.ContextWindow < req.ContextTokens {
		return RejectContextTooSmall, fmt.Sprintf("上下文窗口 %d < 需求 %d", m.ContextWindow, req.ContextTokens)
	}
	if req.MaxTokens > 0 && m.MaxTokens > 0 && m.MaxTokens < req.MaxTokens {
		return RejectMaxTokens, fmt.Sprintf("输出上限 %d < 需求 %d", m.MaxTokens, req.MaxTokens)
	}
	return "", ""
}

// channelScores 预计算渠道维度分（每个渠道一次）。
//
// C_credit 余额：按渠道余额占最大余额归一（CreditsNA 渠道取 1.0：匿名池不受余额约束）。
// C_health 健康度：健康账号占比（AccountsNA 取 1.0）。
// C_expiry  临期风险：1 - 临期/余额（CreditsNA 或余额为 0 时取 1.0）。
// C_stability 稳定性：1 - 失败率。
// C_cost    渠道单价：取该渠道候选里最便宜模型的 costFactor（即「这个渠道最便宜能到多便宜」）。
func channelScores(snap *Snapshot, cw ChannelWeights, policy string) map[string]*channelScore {
	out := make(map[string]*channelScore, len(snap.Channels))
	var maxCredits float64
	for _, ch := range snap.Channels {
		if ch == nil || ch.CreditsNA {
			continue
		}
		if float64(ch.Credits) > maxCredits {
			maxCredits = float64(ch.Credits)
		}
	}
	for name, ch := range snap.Channels {
		if ch == nil {
			continue
		}
		cs := &channelScore{}
		switch {
		case ch.CreditsNA:
			cs.credit = 1.0
		case maxCredits > 0:
			cs.credit = clamp01(float64(ch.Credits) / maxCredits)
		default:
			cs.credit = 0.0
		}
		switch {
		case ch.AccountsNA:
			cs.health = 1.0
		case ch.TotalAccounts > 0:
			cs.health = clamp01(float64(ch.HealthyAccounts) / float64(ch.TotalAccounts))
		default:
			cs.health = 0
		}
		switch {
		case ch.CreditsNA || ch.Credits <= 0:
			cs.expiry = 1.0
		default:
			cs.expiry = clamp01(1 - float64(ch.ExpiringCredits)/float64(ch.Credits))
		}
		cs.stability = clamp01(1 - ch.FailureRate)
		cs.cost = 0.5 // 下面按渠道内最便宜候选覆盖
		out[name] = cs
	}
	// 渠道单价：取该渠道内最便宜候选的 costFactor。
	for _, m := range snap.Models {
		cs, ok := out[m.Channel]
		if !ok {
			continue
		}
		f := costFactor(m, policy)
		if f > cs.cost {
			cs.cost = f
		}
	}
	return out
}

// channelScore 渠道维度分项。
type channelScore struct {
	credit    float64
	health    float64
	expiry    float64
	stability float64
	cost      float64
}

// total 按权重合成渠道总分。
func (c *channelScore) total(cw ChannelWeights) float64 {
	return cw.Credit*c.credit + cw.Health*c.health + cw.Expiry*c.expiry +
		cw.Stability*c.stability + cw.Cost*c.cost
}

// sortCandidates 排序：档位分层 → 综合分降序 → public ID 升序（保证确定性）。
func sortCandidates(cands []Candidate, preferFree bool, policy string) {
	sort.SliceStable(cands, func(i, j int) bool {
		ri := tierRank(cands[i].Tier, preferFree, policy)
		rj := tierRank(cands[j].Tier, preferFree, policy)
		if ri != rj {
			return ri < rj
		}
		if cands[i].Score != cands[j].Score {
			return cands[i].Score > cands[j].Score
		}
		return cands[i].Public < cands[j].Public
	})
}

// buildChain 从排好序的候选里派生备胎链。
//
// 规则：**强制跨渠道交替**——链上相邻两项不同渠道。理由（对齐 R94）：
// 同渠道兄弟模型共享同一批账号，重打一遍必然二次撞墙；跨渠道才是真正的备胎。
// 若全表只有单一渠道，则退化为同渠道有序链（总比没有强）。
func buildChain(cands []Candidate, depth int) []string {
	if depth <= 0 {
		depth = 3
	}
	chain := make([]string, 0, depth)
	lastCh := ""
	for _, c := range cands {
		if len(chain) >= depth {
			break
		}
		if lastCh != "" && c.Channel == lastCh {
			continue
		}
		chain = append(chain, c.Public)
		lastCh = c.Channel
	}
	if len(chain) == 0 && len(cands) > 0 {
		chain = append(chain, cands[0].Public)
	}
	return chain
}

// describeReason 生成首选的人话解释。
func describeReason(c Candidate, tierName string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "档位 %s｜渠道 %s｜综合分 %.3f（渠道 %.3f / 模型 %.3f）",
		tierName, c.Channel, c.Score, c.ChannelScore, c.ModelScore)
	if len(c.Notes) > 0 {
		fmt.Fprintf(&b, "｜备注：%s", strings.Join(c.Notes, "、"))
	}
	return b.String()
}
