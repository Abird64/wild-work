// auto.go 自动选路（auto）的快照装配 + 决策缓存 + 预览接口（R165）。
//
// 分层（与 internal/auto 的纯打分引擎配套）：
//
//	装配层（本文件，app 包）：把「渠道账号状态 + 模型目录 + 定价缓存 + 运行统计」
//	  汇成一张 auto.Snapshot。**只读本地状态，零网络请求**——
//	  模型目录取 server.Handler.CachedChannelModels()（内存/静态缓存），
//	  费率取 app.pricingCache（磁盘缓存），性能取 stats.Snapshot()。
//	决策层（internal/auto 包）：纯函数，输入快照输出决策，单测可离线跑。
//
// 为什么装配层放在 app 而不是 server：
//   - 账号池状态、定价缓存、运行统计三样都在 app 层；
//   - app 已经持有 server.Handler（反之不然），能取到 CachedChannelModels；
//   - app 已有 config.Save/回滚范式（R127），面板保存 auto 配置天然落在这里。
//
// 装配层不碰上游（AGENTS.md §0.1 严禁批量探测）：所有数据都有本地副本。
package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"wild-work/internal/auto"
	"wild-work/internal/config"
	"wild-work/internal/provider"
)

// autoDecision 一条自动选路的决策结果（对外形状，JSON 友好）。
type autoDecision struct {
	// Chosen 首选候选的对外模型 ID（"channel/model"）。空 = 无可用候选。
	Chosen string `json:"chosen"`
	// Chain 备胎链（含首选），跨渠道交替，长度 = fallback_depth（展示用）。
	Chain []string `json:"chain,omitempty"`
	// Order 降级全序（含首选），覆盖全部候选（R166：Chain 只管展示，降级走这里）。
	Order []string `json:"order,omitempty"`
	// Reason 人话解释。
	Reason string `json:"reason,omitempty"`
	// Candidates 通过硬门的候选（按最终排序）。
	Candidates []autoCandidateView `json:"candidates,omitempty"`
	// Rejected 被硬门剔除的候选。
	Rejected []autoRejectionView `json:"rejected,omitempty"`
	// Origin 决策来源：auto（本次算出）/ explicit（客户端点名了具体模型）/ custom（别名命中）。
	Origin string `json:"origin"`
}

// autoCandidateView 候选的对外视图。
type autoCandidateView struct {
	Public       string             `json:"public"`
	Channel      string             `json:"channel"`
	Model        string             `json:"model"`
	Tier         string             `json:"tier"`
	Score        float64            `json:"score"`
	ChannelScore float64            `json:"channel_score"`
	ModelScore   float64            `json:"model_score"`
	Parts        map[string]float64 `json:"parts,omitempty"`
	Notes        []string           `json:"notes,omitempty"`
}

// autoRejectionView 硬门剔除原因的对外视图。
type autoRejectionView struct {
	Public string `json:"public"`
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

// autoEngine 自动选路引擎（挂在 App 上）。
type autoEngine struct {
	mu      sync.Mutex
	cfg     auto.Config
	enabled bool
	// snapFn 装配快照（由 App 注入）。
	snapFn func() *auto.Snapshot
	// cache 决策缓存（带 TTL）。
	cacheMu   sync.Mutex
	cache     *autoDecision
	cacheKey  string
	cacheAt   time.Time
	syncer    func(cfg auto.Config)
}

// autoEngineOf 返回（并按需初始化）App 的自动选路引擎。
//
// 每次取用都从 cfg 同步一次配置（面板热改后立即生效，成本可忽略）。
func (a *App) autoEngineOf() *autoEngine {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.autoEng == nil {
		a.autoEng = &autoEngine{cfg: a.cfg.Auto}
		a.autoEng.snapFn = a.autoSnapshot
	}
	a.autoEng.cfg = a.cfg.Auto
	a.autoEng.enabled = a.cfg.Auto.Enabled
	return a.autoEng
}

// autoSnapshot 装配当前快照（读本地状态，零网络）。
//
// 数据来源全部是本地副本：
//   - 渠道账号状态：pool.Status（allStatuses）
//   - 模型目录：server.Handler.CachedChannelModels()（内存/静态缓存，不发网络）
//   - 费率：app.pricingCache（磁盘缓存 pricing-cache.json）
//   - 性能：stats.Snapshot().Models（今日实测）
func (a *App) autoSnapshot() *auto.Snapshot {
	snap := &auto.Snapshot{
		Channels: map[string]*auto.ChannelInfo{},
		Perf:     map[string]auto.Perf{},
		Now:      time.Now(),
	}

	// ---- 渠道级事实 ----
	for _, st := range a.allStatuses() {
		group := a.accountGroup(st.UID)
		if group == "" {
			continue
		}
		ci := snap.Channels[group]
		if ci == nil {
			ci = &auto.ChannelInfo{Name: group}
			snap.Channels[group] = ci
		}
		ci.TotalAccounts++
		if !st.Disabled && !st.Cooling && !st.SessionDead {
			ci.HealthyAccounts++
		}
		if !a.creditsNAKind(group) {
			ci.Credits += st.Credits
			ci.ExpiringCredits += st.ExpiringCredits
		}
		// R164：签到回包说「无资格」= 当天终态，不可重试 ⇒ 整渠道退出自动选路。
		if !st.LastCheckinOK && isNoEligibilityMsg(st.LastCheckinMsg) {
			ci.Terminal = true
			if ci.TerminalReason == "" {
				ci.TerminalReason = "签到无资格（当天终态）：" + st.LastCheckinMsg
			}
		}
		if st.SessionDead && ci.TerminalReason == "" {
			ci.TerminalReason = "登录态失效，需重新登录"
		}
	}

	// 匿名免费池（oczen / kilo）：无账号概念，但有可用容量 ⇒ 标 AccountsNA，
	// 免得 G1「无健康账号」把免费池全灭掉。
	for _, group := range []string{provider.Oczen.String(), provider.Kilo.String()} {
		ci := snap.Channels[group]
		if ci == nil {
			continue
		}
		ci.AccountsNA = true
		ci.CreditsNA = true
	}

	// ---- 模型目录 + 能力 + 费率 ----
	modelsByKind := map[provider.Kind][]provider.ModelInfo{}
	if a.handler != nil {
		modelsByKind = a.handler.CachedChannelModels()
	}
	// 费率表：channel → model → pricing。
	pricingByChannel := map[string]map[string]provider.ModelPricing{}
	a.pricingMu.Lock()
	for _, p := range a.pricingCache {
		m := pricingByChannel[p.Channel]
		if m == nil {
			m = map[string]provider.ModelPricing{}
			pricingByChannel[p.Channel] = m
		}
		m[p.Model] = p
	}
	a.pricingMu.Unlock()

	kinds := make([]provider.Kind, 0, len(modelsByKind))
	for k := range modelsByKind {
		kinds = append(kinds, k)
	}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i] < kinds[j] })

	for _, k := range kinds {
		chName := k.String()
		infos := modelsByKind[k]
		if len(infos) == 0 {
			continue
		}
		// 渠道在快照里必须存在（即使账号为 0），否则模型会因为「渠道未知」被误判。
		if snap.Channels[chName] == nil {
			snap.Channels[chName] = &auto.ChannelInfo{
				Name:       chName,
				CreditsNA:  a.creditsNAKind(chName),
				AccountsNA: a.creditsNAKind(chName),
			}
		}
		prices := pricingByChannel[chName]
		for _, mi := range infos {
			public := provider.PublicModelID(k, mi.ID)
			mi2 := auto.ModelInfo{
				Channel:       chName,
				Model:         mi.ID,
				Public:        public,
				HasContext:    mi.ContextFromAPI || mi.HandMeasured || mi.ContextFromUser,
				ContextWindow: mi.ContextWindow,
				MaxTokens:     mi.MaxTokens,
			}
			// 能力三态：provider.ModelInfo 的 bool 字段只表示「确认支持」，
			// 无法区分「确认不支持」与「未声明」⇒ 统一按「未声明」处理（nil），
			// 硬门处不据此拒收，评分取中性。这与 provider 侧的语义一致
			// （不变量：能力字段 true 只在确认时出现）。
			mi2.SupportsTools = triState(mi.SupportsTools)
			mi2.SupportsImages = triState(mi.SupportsImages)
			mi2.SupportsReasoning = triState(mi.SupportsReasoning)
			if p, ok := prices[mi.ID]; ok {
				mi2.Rate = p.Rate
				mi2.Explicit = p.IsExplicit()
			}
			snap.Models = append(snap.Models, mi2)
		}
	}

	// ---- 性能实测（今日）----
	if a.stats != nil {
		st := a.stats.Snapshot(time.Now())
		for _, ms := range st.Models {
			key := provider.CanonicalModelID(ms.Model)
			p := auto.Perf{
				Requests: int(ms.Reqs),
				Samples:  int(ms.PerfSamples),
				TTFBMS:   float64(ms.AvgTTFBMs),
				TokPerSec: ms.AvgTokPerSec,
			}
			// 缓存维度：CRCov>0 才算「上游上报了缓存命中」（不变量 24：
			// 未上报 ≠ 0%）。
			if ms.CRCov > 0 {
				p.CacheApplicable = true
				p.CacheHit = float64(ms.CR) / float64(ms.CRCov)
				if p.CacheHit > 1 {
					p.CacheHit = 1
				}
			}
			snap.Perf[key] = p
		}
	}
	return snap
}

// triState 把 provider 的「确认支持」bool 翻成 auto 的三态指针。
//
// provider.ModelInfo 的 SupportsXxx 是 bool，且语义是「true 只在确认时出现」，
// false 混合了「确认不支持」与「上游未声明」两种情况 ⇒ 一律按「未声明」（nil）处理。
// 这样硬门不会误杀「其实支持但没声明」的模型，评分也取中性值。
func triState(v bool) *bool {
	if !v {
		return nil
	}
	b := true
	return &b
}

// autoDecide 按请求能力要求做一次决策（带 TTL 缓存）。
//
// 缓存键只取**影响硬门**的维度（能力要求），不取权重——权重改了立即重算。
func (e *autoEngine) autoDecide(req auto.Request) *autoDecision {
	key := fmt.Sprintf("t=%v,i=%v,r=%v,c=%d,m=%d", req.Tools, req.Images, req.Reasoning, req.ContextTokens, req.MaxTokens)
	ttl := time.Duration(e.cfg.TTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	e.cacheMu.Lock()
	defer e.cacheMu.Unlock()
	if e.cache != nil && e.cacheKey == key && time.Since(e.cacheAt) < ttl {
		return e.cache
	}
	// 快照**每次现装配**（R166）：此前这里读的是 e.snapshot，而它只被
	// /api/auto/preview 赋值、之后永不刷新 ⇒ 用户点过一次「预览」之后，
	// 真实请求就一直拿那份冻结的快照打分（余额/冷却/临期全是旧的），
	// 表现为「auto 永远选同一个模型，哪怕它的额度早用完了」。
	// 装配是纯本地读（pool.Status + 模型目录缓存 + 费率缓存 + 今日实测），零网络，
	// 且只发生在缓存未命中的那一次（TTL 内复用同一条决策）。
	var snap *auto.Snapshot
	if e.snapFn != nil {
		snap = e.snapFn()
	}
	if snap == nil {
		return nil
	}
	d := auto.Decide(snap, e.cfg, req)
	out := decisionToView(d)
	out.Origin = "auto"
	e.cache = out
	e.cacheKey = key
	e.cacheAt = time.Now()
	return out
}

// decisionToView 把 auto.Decision 转成对外视图。
func decisionToView(d auto.Decision) *autoDecision {
	out := &autoDecision{
		Chosen: d.Chosen,
		Chain:  d.Chain,
		Order:  d.Order,
		Reason: d.Reason,
	}
	for _, c := range d.Candidates {
		out.Candidates = append(out.Candidates, autoCandidateView{
			Public:       c.Public,
			Channel:      c.Channel,
			Model:        c.Model,
			Tier:         string(c.Tier),
			Score:        c.Score,
			ChannelScore: c.ChannelScore,
			ModelScore:   c.ModelScore,
			Parts:        c.Parts,
			Notes:        c.Notes,
		})
	}
	for _, r := range d.Rejected {
		out.Rejected = append(out.Rejected, autoRejectionView{
			Public: r.Public,
			Code:   r.Code,
			Detail: r.Detail,
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// 对外接口
// ---------------------------------------------------------------------------

// apiAutoPreview 返回自动选路的当前排名预览（只读，不改任何状态）。
//
// POST /api/auto/preview  body: {"tools":bool,"images":bool,"reasoning":bool,"context_tokens":N,"max_tokens":N}
// GET  /api/auto/preview  用默认要求（无能力要求）预览。
func (a *App) apiAutoPreview(w http.ResponseWriter, r *http.Request) {
	req := auto.Request{}
	if r.Method == http.MethodPost {
		var body struct {
			Tools         bool  `json:"tools"`
			Images        bool  `json:"images"`
			Reasoning     bool  `json:"reasoning"`
			ContextTokens int64 `json:"context_tokens"`
			MaxTokens     int64 `json:"max_tokens"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil && err.Error() != "EOF" {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		req.Tools = body.Tools
		req.Images = body.Images
		req.Reasoning = body.Reasoning
		req.ContextTokens = body.ContextTokens
		req.MaxTokens = body.MaxTokens
	}
	eng := a.autoEngineOf()
	// 预览用的快照与真实请求各装配一份、互不共享：预览是只读探针，
	// 不得把「我刚才预览时的世界」写进引擎（R166 的冻结根因就在这里）。
	snap := a.autoSnapshot()

	d := auto.Decide(snap, eng.cfg, req)
	view := decisionToView(d)
	view.Origin = "auto"
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":  eng.cfg.Enabled,
		"config":   eng.cfg,
		"decision": view,
		"channels": autoChannelViews(snap),
		"at":       snap.Now.Format(time.RFC3339),
	})
}

// autoChannelView 渠道事实的对外视图。
type autoChannelView struct {
	Name            string `json:"name"`
	TotalAccounts   int    `json:"total_accounts"`
	HealthyAccounts int    `json:"healthy_accounts"`
	CreditsNA       bool   `json:"credits_na"`
	AccountsNA      bool   `json:"accounts_na"`
	Terminal        bool   `json:"terminal"`
	TerminalReason  string `json:"terminal_reason,omitempty"`
	Credits         int64  `json:"credits"`
	ExpiringCredits int64  `json:"expiring_credits"`
}

// autoChannelViews 把快照的渠道事实转成有序视图。
func autoChannelViews(snap *auto.Snapshot) []autoChannelView {
	if snap == nil {
		return nil
	}
	out := make([]autoChannelView, 0, len(snap.Channels))
	for _, ci := range snap.Channels {
		out = append(out, autoChannelView{
			Name:            ci.Name,
			TotalAccounts:   ci.TotalAccounts,
			HealthyAccounts: ci.HealthyAccounts,
			CreditsNA:       ci.CreditsNA,
			AccountsNA:      ci.AccountsNA,
			Terminal:        ci.Terminal,
			TerminalReason:  ci.TerminalReason,
			Credits:         ci.Credits,
			ExpiringCredits: ci.ExpiringCredits,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// apiConfigAuto 保存自动选路配置（原子写 + 回滚，R127 范式）。
func (a *App) apiConfigAuto(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled           *bool         `json:"enabled"`
		PreferFree        *bool         `json:"prefer_free"`
		UnknownRatePolicy string        `json:"unknown_rate_policy"`
		MinPerfSamples    int           `json:"min_perf_samples"`
		FallbackDepth     int           `json:"fallback_depth"`
		TTLSeconds        int           `json:"ttl_seconds"`
		ExcludedChannels  []string      `json:"excluded_channels"`
		Weights           *auto.Weights `json:"weights"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	old := a.cfg.Auto
	next := old
	if body.Enabled != nil {
		next.Enabled = *body.Enabled
	}
	if body.PreferFree != nil {
		pf := *body.PreferFree
		next.PreferFree = &pf
	}
	if body.UnknownRatePolicy != "" {
		next.UnknownRatePolicy = body.UnknownRatePolicy
	}
	if body.MinPerfSamples > 0 {
		next.MinPerfSamples = body.MinPerfSamples
	}
	if body.FallbackDepth > 0 {
		next.FallbackDepth = body.FallbackDepth
	}
	if body.TTLSeconds > 0 {
		next.TTLSeconds = body.TTLSeconds
	}
	if body.ExcludedChannels != nil {
		next.ExcludedChannels = autoExcludedChannels(body.ExcludedChannels)
	}
	if body.Weights != nil {
		next.Weights = *body.Weights
	}

	a.mu.Lock()
	a.cfg.Auto = next
	err := config.Save(a.cfg, a.cfgPath)
	if err != nil {
		a.cfg.Auto = old // 回滚
		a.mu.Unlock()
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.mu.Unlock()

	// 热更新：决策器缓存作废 + 推给 server/gateway（若已注入）。
	eng := a.autoEngineOf()
	eng.cacheMu.Lock()
	eng.cache = nil
	eng.cacheAt = time.Time{}
	eng.cacheMu.Unlock()
	if eng.syncer != nil {
		eng.syncer(next)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "config": next})
}

// SetAutoConfigSyncer 注入自动选路配置热更新回调（main.go 装配）。
func (a *App) SetAutoConfigSyncer(fn func(cfg auto.Config)) {
	eng := a.autoEngineOf()
	eng.syncer = fn
}

// AutoResolver 返回给 server.Handler / gateway.Router 的决策函数。
//
// 签名：req 描述客户端的能力要求；返回首选模型全路径、备胎链（展示用）、
// 降级全序（R166）、是否命中。
// 未启用自动选路时返回 ok=false（调用方走原路径，零行为变更）。
func (a *App) AutoResolver() func(req auto.Request) (chosen string, chain, order []string, ok bool) {
	return func(req auto.Request) (string, []string, []string, bool) {
		eng := a.autoEngineOf()
		if !eng.cfg.Enabled {
			return "", nil, nil, false
		}
		d := eng.autoDecide(req)
		if d == nil || d.Chosen == "" {
			return "", nil, nil, false
		}
		return d.Chosen, d.Chain, d.Order, true
	}
}

// autoExcludedChannels 归一排除名单（去空白、去重、小写）。
func autoExcludedChannels(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
