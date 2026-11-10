// Package auto 是「自动选路（auto）」的纯打分引擎。
//
// 设计要点（与 docs/决议项.md R165 对应）：
//
//  1. **纯函数、零 IO**：输入是一个已装配好的快照 Snapshot，输出是决策 Decision。
//     本包不读文件、不发网络请求、不碰全局状态 ⇒ 单测可完全离线跑（不变量：
//     上游探测只在装配快照那一层发生，且限频）。
//  2. **硬门在前（gate），软分在后（score）**：能力不匹配的候选**先被剔除**，
//     不参与任何加权——避免「免费但不支持工具的模型」靠低分混进前列。
//  3. **成本档位分层（tier）是排序第一键**：免费档 > 付费档 > 未知档。
//     未知档（上游没给费率）**不等于免费**（provider.ModelPricing.Explicit 语义）。
//  4. **能力元数据缺失 = 不判断**：三态（true/false/未知）���持，
//     未知在硬门处放行、在软分处取中性值，既不误杀也不虚高。
//
// 包内不 import 任何其它内部包：快照装配（渠道目录 / 定价缓存 / 账号池 / 统计）
// 由 internal/app 层完成，public ID 的规范化也在那一层做（provider.PublicModelID）。
package auto

import "time"

// Tier 成本档位。排序第一键。
type Tier string

const (
	// TierFree 免费档：上游显式给出倍率且 rate==0。
	TierFree Tier = "free"
	// TierPriced 付费档：上游显式给出倍率且 rate>0。
	TierPriced Tier = "priced"
	// TierUnpriced 未知档：上游未给费率（Explicit=false）——**不是免费**。
	TierUnpriced Tier = "unpriced"
)

// 未知费率处理策略。
const (
	// UnknownRateAllow 允许未知档参与竞争，但排在已知档之后（默认）。
	UnknownRateAllow = "allow"
	// UnknownRateDeprioritize 未知档额外降权并排在所有已知档之后。
	UnknownRateDeprioritize = "deprioritize"
)

// Request 客户端请求的能力要求（硬门的判据来源）。
//
// 零值 = 什么都不要求 ⇒ 只有「账号健康 + 模型在目录里 + 渠道未被排除」三道门。
type Request struct {
	// Tools 请求里有工具调用（function/tool 定义非空）。
	Tools bool
	// Images 请求里有图片输入。
	Images bool
	// Reasoning 请求显式要思考（reasoning_effort / thinking）。
	Reasoning bool
	// ContextTokens 本次请求预计需要的上下文 token 数（0 = 不限制）。
	ContextTokens int64
	// MaxTokens 请求的 max_tokens（0 = 不限制）。
	MaxTokens int64
}

// ChannelInfo 渠道级聚合事实（由 app 层从 pool.Status + stats 聚合而来）。
type ChannelInfo struct {
	// Name 渠道名（provider.Kind 字符串，如 "kilo"）。
	Name string
	// TotalAccounts 池内账号总数。
	TotalAccounts int
	// HealthyAccounts 未禁用且未整号冷却的账号数。
	HealthyAccounts int
	// CreditsNA 该渠道「积分」概念不适用（匿名免费池 / 无余额端点渠道）。
	CreditsNA bool
	// AccountsNA 该渠道「账号」概念不适用（匿名免费池：无账号可轮换）。
	//  ⇒ 健康度不参与硬门（G1 放行），软分里按中性值计。
	AccountsNA bool
	// Terminal 渠道处于终态（如 R164 的「无资格」：当天不可重试）。
	Terminal bool
	// TerminalReason 终态原因（展示用）。
	TerminalReason string
	// Credits 可消耗余额合计（CreditsNA 时无意义）。
	Credits int64
	// ExpiringCredits 7 天内到期的余额（CreditsNA 时为 0）。
	ExpiringCredits int64
	// FailureRate 该渠道近期失败率 0..1（来自 stats 的异常/降级统计；无数据 = 0）。
	FailureRate float64
	// CoolingModels 该渠道上处于**模型级冷却**的模型名集合。
	CoolingModels map[string]bool
}

// ModelInfo 候选模型的能力与定价事实。
type ModelInfo struct {
	// Channel 渠道名。
	Channel string
	// Model 上游模型名（不含渠道前缀）。
	Model string
	// Public 对外模型 ID（"channel/model" 全路径；命名空间前缀由装配层处理）。
	Public string
	// Rate 上游倍率（0 = 免费，仅当 Explicit 为真时才算免费）。
	Rate float64
	// Explicit 上游是否显式给了倍率。false ⇒ Rate 无意义 ⇒ TierUnpriced。
	Explicit bool
	// HasContext 该模型是否有可信的上下文窗口值。
	HasContext bool
	// ContextWindow 上下文窗口（HasContext 为假时无意义）。
	ContextWindow int64
	// MaxTokens 单次输出上限（0 = 未知）。
	MaxTokens int64
	// SupportsTools / SupportsImages / SupportsReasoning 能力三态：
	// nil = 上游未声明（**不判断**：不据此拒收，评分取中性），true/false = 明确。
	SupportsTools     *bool
	SupportsImages    *bool
	SupportsReasoning *bool
}

// Perf 模型的历史实测表现（来自 stats.Snapshot().Models）。
type Perf struct {
	// Samples 性能样本数（TTFB/吞吐的样本口径）。
	Samples int
	// Requests 该模型累计请求数（成熟度口径）。
	Requests int
	// TTFBMS 平均首字延迟（毫秒）。
	TTFBMS float64
	// TokPerSec 平均输出速度（token/s）。
	TokPerSec float64
	// CacheApplicable 上游是否上报缓存命中（不变量 24：未上报 ≠ 0%）。
	CacheApplicable bool
	// CacheHit 缓存命中率 0..1（CacheApplicable 为假时无意义）。
	CacheHit float64
}

// Snapshot 一次决策所需的全部快照。装配由 app 层负责，本包只读。
type Snapshot struct {
	// Channels 渠道级事实，键为渠道名。
	Channels map[string]*ChannelInfo
	// Models 候选模型清单（已含渠道 + public ID）。
	Models []ModelInfo
	// Perf 模型实测表现，键为 public ID。
	Perf map[string]Perf
	// Now 快照时刻（留空则用零值，仅用于展示）。
	Now time.Time
}

// ChannelWeights 渠道维度权重（和不必为 1，打分时按和归一）。
type ChannelWeights struct {
	Credit    float64 `json:"credit"`    // 余额多
	Health    float64 `json:"health"`    // 健康账号占比
	Expiry    float64 `json:"expiry"`    // 临期风险小
	Stability float64 `json:"stability"` // 失败率低
	Cost      float64 `json:"cost"`      // 渠道整体便宜
}

// ModelWeights 模型维度权重。
type ModelWeights struct {
	Value  float64 `json:"value"`  // 质价比
	Perf   float64 `json:"perf"`   // 实测性能
	Cap    float64 `json:"cap"`    // 能力覆盖
	Mature float64 `json:"mature"` // 成熟度
	Cache  float64 `json:"cache"`  // 缓存命中
}

// Weights 顶层权重。
type Weights struct {
	Channel ChannelWeights `json:"channel"`
	Model   ModelWeights   `json:"model"`
}

// Alias 自动选路对外暴露的模型名：客户端把 model 写成 "auto" 即走自动决策。
//
// 刻意不用 "auto/xxx" 形态：gateway.Router 见到 `/` 会按「渠道前缀」校验
// （auto 不是渠道名）⇒ 裸名才能同时被内层与兼容层识别（R165）。
const Alias = "auto"

// Config 自动选路的可调项。零值 Weights ⇒ 用默认值（老配置直接跑）。
type Config struct {
	// Enabled 是否启用自动选路。
	Enabled bool `json:"enabled"`
	// PreferFree 是否把免费档排在付费档之前。**nil = 默认 true**（用户诉求第一条：
	// 优先使用免费和积分最多的渠道）。指针形态是为了区分「没配」与「显式设 false」。
	PreferFree *bool `json:"prefer_free,omitempty"`
	// UnknownRatePolicy 未知费率策略：allow（默认）/ deprioritize。
	UnknownRatePolicy string `json:"unknown_rate_policy,omitempty"`
	// MinPerfSamples 性能样本数下限，低于此值 M_perf 取中性值（默认 10）。
	MinPerfSamples int `json:"min_perf_samples,omitempty"`
	// FallbackDepth 备胎链总长度（含首选），默认 3。
	FallbackDepth int `json:"fallback_depth,omitempty"`
	// TTLSeconds 决策缓存时长（秒），默认 60。
	TTLSeconds int `json:"ttl_seconds,omitempty"`
	// ExcludedChannels 禁用渠道名单（黑名单）。
	ExcludedChannels []string `json:"excluded_channels,omitempty"`
	// Weights 各维度权重；全零 ⇒ 默认权重。
	Weights Weights `json:"weights,omitempty"`
}

// DefaultConfig 返回默认配置。
func DefaultConfig() Config {
	pf := true
	return Config{
		Enabled:           true,
		PreferFree:        &pf,
		UnknownRatePolicy: UnknownRateAllow,
		MinPerfSamples:    10,
		FallbackDepth:     3,
		TTLSeconds:        60,
		Weights:           DefaultWeights(),
	}
}

// DefaultWeights 默认权重（设计稿已评审通过，改动需同步 docs/决议项.md）。
func DefaultWeights() Weights {
	return Weights{
		Channel: ChannelWeights{Credit: 0.30, Health: 0.25, Expiry: 0.20, Stability: 0.15, Cost: 0.10},
		Model:   ModelWeights{Value: 0.30, Perf: 0.25, Cap: 0.20, Mature: 0.15, Cache: 0.10},
	}
}

// normalized 返回权重（按维度和归一；全零维度退回默认）。
func (w Weights) normalized() (ChannelWeights, ModelWeights) {
	cw, mw := w.Channel, w.Model
	if sum := cw.Credit + cw.Health + cw.Expiry + cw.Stability + cw.Cost; sum <= 0 {
		cw = DefaultWeights().Channel
	} else {
		cw.Credit /= sum
		cw.Health /= sum
		cw.Expiry /= sum
		cw.Stability /= sum
		cw.Cost /= sum
	}
	if sum := mw.Value + mw.Perf + mw.Cap + mw.Mature + mw.Cache; sum <= 0 {
		mw = DefaultWeights().Model
	} else {
		mw.Value /= sum
		mw.Perf /= sum
		mw.Cap /= sum
		mw.Mature /= sum
		mw.Cache /= sum
	}
	return cw, mw
}

// Candidate 一个通过硬门、被打过分��候选。
type Candidate struct {
	// Public 对外模型 ID（"channel/model"）。
	Public string
	// Channel 渠道名。
	Channel string
	// Model 上游模型名。
	Model string
	// Tier 成本档位。
	Tier Tier
	// Score 综合分（0..1）。
	Score float64
	// ChannelScore 渠道维度分（0..1）。
	ChannelScore float64
	// ModelScore 模型维度分（0..1）。
	ModelScore float64
	// Parts 分项明细（键：C_credit/C_health/… / M_value/M_perf/…）。
	Parts map[string]float64
	// Notes 人话备注（"样本不足"、"缓存未上报"、"能力未声明"…）。
	Notes []string
}

// Rejection 一个被硬门剔除的候选及原因。
type Rejection struct {
	// Public 对外模型 ID。
	Public string
	// Code 机器可读的原因码（见下方常量）。
	Code string
	// Detail 人话说明。
	Detail string
}

// 硬门原因码。
const (
	RejectExcludedChannel = "excluded_channel"
	RejectTerminalChannel = "terminal_channel"
	RejectNoHealthyAcct   = "no_healthy_account"
	RejectModelCooling    = "model_cooling"
	RejectNoTools         = "no_tools"
	RejectNoImages        = "no_images"
	RejectNoReasoning     = "no_reasoning"
	RejectContextTooSmall = "context_too_small"
	RejectMaxTokens       = "max_tokens_exceeded"
)

// Decision 一次自动选路的完整结果。
type Decision struct {
	// Chosen 首选候选的 public ID（空 = 无可用候选）。
	Chosen string
	// Chain 备胎链（public ID 列表，含 Chosen；跨渠道交替，长度 = fallback_depth）。
	//
	// **仅用于展示与响应头**：链之所以只留 3 条，是因为响应头 `X-Model-Auto-Chain`
	// 要能一眼读完；它**不是**降级的上限，降级走 Order。
	Chain []string
	// Order 降级全序（public ID 列表，含 Chosen；跨渠道交替 + 同渠道补位，覆盖全部候选）。
	//
	// 与 Chain 的分工（R166）：Chain 是「给人看的 3 条」，Order 是「给降级用的全序」。
	// 早期实现拿 Chain 当降级链，于是第 4 个候选起就再也降不下去——而候选池通常有
	// 几十个模型，depth=3 的链一旦三条都冷（免费池同时限流是常态），请求就被打成
	// 503/透传，明明还有几十个能用的模型。Order 就是为了把这条出路打开。
	Order []string
	// Reason 人话解释（首选为什么被选中）。
	Reason string
	// Candidates 通过硬门的候选，按最终排序（分数降序 + 档位分层）。
	Candidates []Candidate
	// Rejected 被硬门剔除的候选。
	Rejected []Rejection
}
