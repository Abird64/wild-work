package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"wild-work/internal/provider"
)

// feesChannel 一个渠道在「模型列表和费率」面板中的分组数据。
type feesChannel struct {
	Channel string         `json:"channel"`
	Models  []feesModelRow `json:"models"`
	// BaseURL/APIKeyMasked 仅自定义渠道填写（R119）：面板把费率页当「模型接入点总表」用，
	// 需要在一处看全每个模型的 endpoint 与 key。key 始终是**脱敏值**（maskKey），明文由前端点「复制」
	// 时另走 POST /api/config/custom_channel_key 按需取，不随本接口批量下发。
	BaseURL      string `json:"base_url,omitempty"`
	APIKeyMasked string `json:"api_key_masked,omitempty"`
}

// feesModelRow 一行模型：来自渠道 models 列表，尽可能配上费率。
type feesModelRow struct {
	Model string `json:"model"`
	// ID 是客户端实际要填的完整模型 ID（与 /v1/models 的 id、provider.PublicModelID 同源）：
	// 普通渠道为 `<渠道>/<模型>`，上游 ID 自带命名空间的渠道（cline 的 cline-free/*）
	// 直接就是上游 ID（R59）。面板一律拿它做复制，不在前端再拼前缀（R52 的教训：
	// 拼接规则散在多处必漏）。
	ID string `json:"id"`
	// Priced 表示上游费率接口对该模型返回了可用的倍率值。
	// 注意：credits 字段缺失（如 CN 的 auto 调度器）也算未定价，
	// 不能把「缺字段解析出的 0」当成免费。
	Priced bool    `json:"priced"`
	Rate   float64 `json:"rate"`           // 倍率（Priced=false 时无意义）
	Free   bool    `json:"free"`           // 上游明确标注倍率为 0（x0.00）
	Note   string  `json:"note,omitempty"` // 促销/标签说明（已剔除颜色）
	// Color 为促销标签颜色（形如 "#FF0000"），无则空。
	Color string `json:"color,omitempty"`
	// HasContext 标记该模型的上下文窗口/最大输出是否可展示：
	// 上游接口真实返回（ContextFromAPI）或本工具手工实测（HandMeasured）均置 true。
	// false 表示无可靠数据（硬编码估算或上游不返回且未实测），前端提示「未知」而非展示数字。
	HasContext    bool  `json:"has_context"`
	ContextWindow int64 `json:"context_window,omitempty"`
	MaxTokens     int64 `json:"max_tokens,omitempty"`
	// HandMeasured 标记上述上下文值是**本工具实测**（非上游声明），前端据此加「实测」标注。
	HandMeasured bool `json:"hand_measured,omitempty"`
	// CapSource 能力/上下文数据的来源，供前端逐行如实标注（R119）：
	// "api"=上游声明、"measured"=本工具实测、"user"=面板手填、""=未声明（前端显「未探测」）。
	// 只影响展示文案，不影响路由；与自定义渠道配置里的 cap_source 同源。
	CapSource string `json:"cap_source,omitempty"`
	// 能力标记（与 /v1/models 同源），供 UI 在模型 ID 后展示图标。
	SupportsImages    bool `json:"supports_images"`
	SupportsReasoning bool `json:"supports_reasoning"`
	SupportsTools     bool `json:"supports_tools"`
	// 思考档位（issue #96，与 /v1/models 同源）：上游声明的可选档位与默认档，
	// 空表 = 上游未声明（前端不展示，不编造）。
	ReasoningEfforts       []string `json:"reasoning_efforts,omitempty"`
	DefaultReasoningEffort string   `json:"default_reasoning_effort,omitempty"`
}

// feeChannelOrder 「模型列表和费率」面板的渠道分组顺序。
// 与 channelOrder 分开维护：本表把 Oczen 放首位（其免费模型最常被查），
// kilo 紧随其后（同为免密免费池），且含 TraeCode（与 TraeWork 共账号但模型集不同，需独立成组）。
var feeChannelOrder = []provider.Kind{
	provider.Oczen, provider.Kilo, provider.WorkBuddy, provider.WorkBuddyAI, provider.QoderCN,
	provider.QoderCOM, provider.TraeWork, provider.TraeCode, provider.QwenWork, provider.GLM,
	provider.Cline, // 漏在此表 = 面板永远不出该渠道 tab（buildFeesChannels 只遍历本表）
	provider.MonkeyCode,
	provider.Raccoon,
	provider.Loomy,
	provider.Minimax,
	provider.LobsterAI,
	provider.ZCode,
	provider.Gemini,
	provider.CodeArts,
	provider.OfficeAce,
}

// feeChannelOrderWithDynamic 返回费率表渠道顺序：静态 feeChannelOrder + 未登记的动态渠道
// （自定义渠道，R66）追加到末尾（按名排序）。仅当 modelsByKind 里确有该渠道的模型时
// 才追加，避免空渠道占位。
func feeChannelOrderWithDynamic(modelsByKind map[provider.Kind][]provider.ModelInfo) []provider.Kind {
	seen := make(map[provider.Kind]bool, len(feeChannelOrder))
	for _, k := range feeChannelOrder {
		seen[k] = true
	}
	var extra []provider.Kind
	for k, infos := range modelsByKind {
		if !seen[k] && len(infos) > 0 {
			extra = append(extra, k)
		}
	}
	if len(extra) == 0 {
		return feeChannelOrder
	}
	sort.Slice(extra, func(i, j int) bool { return extra[i] < extra[j] })
	return append(append([]provider.Kind{}, feeChannelOrder...), extra...)
}

// buildFeesChannels 以「渠道 models 列表」为基准组表：
//   - 每个渠道的模型行来自该渠道当前生效的 models（与 /v1/models 同源）；
//   - 再用该渠道费率接口返回的 model→rate 映射逐行填充；
//   - 未在费率接口中出现的模型标记 Priced=false（前端显示 unknown + tooltip）；
//   - 费率接口中多出的模型（models 列表中不存在）不展示，避免与 /v1/models 不一致。
func buildFeesChannels(modelsByKind map[provider.Kind][]provider.ModelInfo,
	pricing []provider.ModelPricing, order []provider.Kind) []feesChannel {
	// 渠道 → (model → pricing)
	byChannel := make(map[string]map[string]provider.ModelPricing)
	for _, p := range pricing {
		m := byChannel[p.Channel]
		if m == nil {
			m = make(map[string]provider.ModelPricing)
			byChannel[p.Channel] = m
		}
		m[p.Model] = p
	}
	out := make([]feesChannel, 0, len(order))
	for _, k := range order {
		// 旧 Qoder 渠道已从界面下线：不拉取/不展示其模型列表（路由保留，存量账号仍可用）
		if k == provider.Qoder {
			continue
		}
		infos := modelsByKind[k]
		if len(infos) == 0 {
			continue
		}
		prices := byChannel[k.String()]
		rows := make([]feesModelRow, 0, len(infos))
		for _, mi := range infos {
			row := feesModelRow{
				Model:         mi.ID,
				ID:            provider.PublicModelID(k, mi.ID),
				ContextWindow: mi.ContextWindow,
				MaxTokens:     mi.MaxTokens,
				// 仅上游接口返回的上下文可信；硬编码估算值不展示数字。
				// 上游声明（API）或本工具手工实测均视为「有上下文可展示」；
				// 前端据 hand_measured 区分并加「实测」标注，不把实测冒充成官方值。
				HasContext:   mi.ContextFromAPI || mi.HandMeasured || mi.ContextFromUser,
				HandMeasured: mi.HandMeasured,
				CapSource:    capSourceOf(mi),
				// 能力标记与 /v1/models 同源，保证 UI 图标与接口声明一致。
				SupportsImages:    mi.SupportsImages,
				SupportsReasoning: mi.SupportsReasoning,
				SupportsTools:     mi.SupportsTools,
				// 思考档位（issue #96）与 /v1/models 同源透传
				ReasoningEfforts:       mi.ReasoningEfforts,
				DefaultReasoningEffort: mi.DefaultReasoningEffort,
			}
			if p, ok := prices[mi.ID]; ok {
				row.Priced = p.IsExplicit() // 缺倍率字段（如 auto）不算已定价
				row.Rate = p.Rate
				row.Free = p.IsExplicit() && p.Rate == 0
				row.Note = p.Note
				row.Color = p.Color
			}
			rows = append(rows, row)
		}
		// 排序：免费在前（便于发现可用免费模型），其余按倍率升序，未定价置后
		sort.SliceStable(rows, func(i, j int) bool {
			a, b := rows[i], rows[j]
			if a.Priced != b.Priced {
				return a.Priced // 已定价的排前
			}
			if a.Rate != b.Rate {
				return a.Rate < b.Rate
			}
			return a.Model < b.Model
		})
		out = append(out, feesChannel{Channel: k.String(), Models: rows})
	}
	return out
}

// FeesInfo 返回「模型列表和费率」面板数据。
// 以各渠道当前生效的模型列表为基准（与 /v1/models 同源），逐行配上费率：
// 上游未返回倍率的模型标记 priced=false，前端显示 unknown。
func (a *App) FeesInfo() map[string]any {
	a.pricingMu.Lock()
	cached := a.pricingCache
	errMsg := a.pricingErr
	stale := time.Since(a.pricingFetched) > time.Hour
	a.pricingMu.Unlock()

	// 缓存过期时后台静默刷新
	if len(cached) > 0 && stale {
		go a.safeGo(func() { a.RefreshPricing() })
	}

	// 面板秒开：模型列表只用缓存/静态兜底（不触发上游网络请求）。
	// 后台 StartPricingAutoRefresh 每 30 分钟拉新并写入缓存，用户看到的是最近一次结果。
	var modelsByKind map[provider.Kind][]provider.ModelInfo
	if a.handler != nil {
		modelsByKind = a.handler.CachedChannelModels()
	} else {
		modelsByKind = a.staticModelFallback()
	}
	if len(modelsByKind) == 0 {
		modelsByKind = a.staticModelFallback()
	}

	// 首次加载且无缓存：异步拉取费率，先展示（unknown 占位）
	if len(cached) == 0 {
		go a.safeGo(func() { a.RefreshPricing() })
	}

	// 展示顺序固定（见 feeChannelOrder；旧 Qoder 跳过），**末尾追加动态自定义渠道**（R66）。
	// 必须追加：feeChannelOrder 是静态表，自定义渠道不在其中，若不追加则
	// 面板费率表永远不出现该渠道 tab（buildFeesChannels 只遍历传入的 order）——
	// 这与 /v1/models、用量流水、运行统计的可见性不一致。按渠道名排序保证稳定顺序。
	order := feeChannelOrderWithDynamic(modelsByKind)
	channels := buildFeesChannels(modelsByKind, cached, order)
	a.applyCustomFeesMeta(&channels)

	result := map[string]any{
		"note":       "本表以各渠道实际可用模型列表为准；倍率为空的模型表示上游未返回定价（未知）。",
		"channels":   channels,
		"disclaimer": "本工具仅聚合转发，不参与定价；渠道费率以各上游官方页面为准。",
	}
	if len(cached) > 0 {
		result["cached_at"] = a.pricingFetched.Format("01-02 15:04")
	}
	if errMsg != "" {
		result["error"] = errMsg
	}
	return result
}

// capSourceOf 把 ModelInfo 的三个互斥来源位归成一个字符串，给面板逐行标注（R119）。
// 优先级 api > measured > user：同时置位时以最权威的为准（与 provider.ModelInfo 注释一致）。
func capSourceOf(mi provider.ModelInfo) string {
	switch {
	case mi.ContextFromAPI:
		return "api"
	case mi.HandMeasured:
		return "measured"
	case mi.ContextFromUser:
		return "user"
	}
	return ""
}

// applyCustomFeesMeta 给费率表里的自定义渠道补上接入点与脱敏 key（R119）。
// 抽成方法而不是内联在 FeesInfo：单测要能单独证「明文没进来、脱敏串进来了」。
// 内建渠道的 endpoint 写死在代码里，无需展示 ⇒ meta 表不命中就保持空串。
func (a *App) applyCustomFeesMeta(channels *[]feesChannel) {
	meta := a.customChannelFeesMeta()
	if len(meta) == 0 {
		return
	}
	for i := range *channels {
		if m, ok := meta[(*channels)[i].Channel]; ok {
			(*channels)[i].BaseURL = m[0]
			(*channels)[i].APIKeyMasked = m[1]
		}
	}
}

// staticModelFallback 无 handler 时的渠道→静态模型表兜底。
// app.Runtime 不带静态表（那是 server.Runtime 的字段），但**自定义渠道例外**：
// 它的 FetchModels 是「配什么返回什么」，绝不发网络请求（R66，已有测试锁死），
// 因此可以在此安全取回——否则启动早期/单测里费率页拿不到自定义渠道的行。
// 内建渠道的目录接口会打上游，绝不在这里调 ⇒ 非 Custom 渠道仍返回空。
func (a *App) staticModelFallback() map[provider.Kind][]provider.ModelInfo {
	out := map[provider.Kind][]provider.ModelInfo{}
	for _, rt := range a.runtimesSnapshot() {
		if rt == nil || !rt.Custom || rt.Upstream == nil {
			continue
		}
		if ms, err := rt.Upstream.FetchModels(nil); err == nil && len(ms) > 0 {
			out[rt.Kind] = ms
		}
	}
	return out
}

// RefreshPricing 从所有已接入渠道拉取最新定价并更新缓存。
// 并发调用会被丢弃（已有一次在跑）——启动刷新与面板「刷新」可能重叠。
func (a *App) RefreshPricing() {
	a.pricingMu.Lock()
	if a.pricingBusy {
		a.pricingMu.Unlock()
		return
	}
	a.pricingBusy = true
	a.pricingMu.Unlock()
	defer func() {
		a.pricingMu.Lock()
		a.pricingBusy = false
		a.pricingMu.Unlock()
	}()

	allPricing := make([]provider.ModelPricing, 0)
	var errs []string

	for _, rt := range a.runtimesSnapshot() {
		// 旧 Qoder 渠道已从界面下线：不再自动刷新其模型/费率（也避免无谓的 token refresh 报错日志）
		if rt == nil || rt.Pool == nil || rt.Upstream == nil || rt.Kind == provider.Qoder || len(rt.Pool.List()) == 0 {
			continue
		}
		acct := rt.Pool.Pick()
		if acct == nil {
			continue
		}
		// 先确保 token 有效
		if acct.NeedsRefresh(10 * time.Minute) {
			if err := rt.Upstream.RefreshToken(acct); err != nil {
				errs = append(errs, fmt.Sprintf("%s: token refresh failed", rt.Kind))
				continue
			}
			// 必须落盘：refresh token 会轮换，不写回则下次启动用的是旧 refresh token，
			// 而旧 access token 已被上游作废 → 本地 expiresAt 仍显示有效 → 卡死在 401。
			if serr := acct.SaveAtomic(); serr != nil {
				log.Printf("pricing token save failed platform=%s uid=%s err=%v", rt.Kind, acct.UID, serr)
			}
		}
		pricing, err := rt.Upstream.FetchModelPricing(acct)
		if err != nil {
			// 401 同样先刷新再重试一次，避免费率因过期 token 长期拉不到。
			if a.refreshIfSessionDead(rt, acct, err) {
				pricing, err = rt.Upstream.FetchModelPricing(acct)
			}
		}
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %s", rt.Kind, shortErr(err)))
			log.Printf("pricing fetch failed platform=%s err=%v", rt.Kind, err)
			continue
		}
		allPricing = append(allPricing, pricing...)
		log.Printf("pricing fetched platform=%s count=%d", rt.Kind, len(pricing))
	}

	// 排序：按渠道 + 倍率
	sort.Slice(allPricing, func(i, j int) bool {
		if allPricing[i].Channel != allPricing[j].Channel {
			return allPricing[i].Channel < allPricing[j].Channel
		}
		return allPricing[i].Rate < allPricing[j].Rate
	})

	a.pricingMu.Lock()
	a.pricingCache = allPricing
	a.pricingFetched = time.Now()
	if len(errs) > 0 {
		a.pricingErr = strings.Join(errs, "; ")
	} else {
		a.pricingErr = ""
	}
	a.pricingMu.Unlock()
	a.savePricingCache()
}

// PricingRefreshInterval 模型列表与费率的后台自动刷新间隔。
// 两者都随上游变动，且面板依赖它们展示，故定期刷新保持新鲜。
const PricingRefreshInterval = 30 * time.Minute

// loadPricingCache 从文件加载定价缓存。
func (a *App) loadPricingCache() {
	raw, err := os.ReadFile(a.pricingFP)
	if err != nil {
		return
	}
	var cache struct {
		Models  []provider.ModelPricing `json:"models"`
		Fetched string                  `json:"fetched"`
	}
	if err := json.Unmarshal(raw, &cache); err != nil {
		return
	}
	a.pricingMu.Lock()
	a.pricingCache = cache.Models
	if cache.Fetched != "" {
		if t, err := time.Parse(time.RFC3339, cache.Fetched); err == nil {
			a.pricingFetched = t
		}
	}
	a.pricingMu.Unlock()
}

// savePricingCache 持久化定价缓存到文件。
func (a *App) savePricingCache() {
	a.pricingMu.Lock()
	cache := struct {
		Models  []provider.ModelPricing `json:"models"`
		Fetched string                  `json:"fetched"`
	}{
		Models:  a.pricingCache,
		Fetched: a.pricingFetched.Format(time.RFC3339),
	}
	a.pricingMu.Unlock()
	raw, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(a.pricingFP, raw, 0o644)
}

// StartPricingAutoRefresh 后台定期刷新「模型列表 + 费率」。
// 启动立即刷一次：否则刚启动时费率缓存为空，面板下方全是 unknown，观感不佳。
// 之后每隔 interval 一次；取消 ctx 即停止。
func (a *App) StartPricingAutoRefresh(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = PricingRefreshInterval
	}
	go func() {
		// 启动刷新：同时失效动态模型缓存，保证模型列表也是新的
		if a.handler != nil {
			a.handler.InvalidateModels()
		}
		a.refreshChannelModels() // 填充模型缓存（issue #40：面板只读缓存，必须有填充源）
		a.RefreshPricing()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				a.refreshChannelModels() // 同上：随费率一起周期性拉新
				a.RefreshPricing()
			}
		}
	}()
}

// refreshChannelModels 拉取各渠道模型列表并填充 handler 的模型缓存。
//
// 为什么必须有这一步（issue #40）：费率面板读 CachedChannelModels（只读缓存、
// 不发网络请求），而缓存 rt.models 此前**只有 /v1/models 请求会填充**——
// RefreshPricing 只拉费率不拉模型。于是冷启动后，凡无静态兑底表的渠道
// （qodercn/qodercom）在面板上整组消失，直到某个客户端碰巧请求了 /v1/models。
// 故模型列表的填充必须与费率刷新同样走后台定时路径。
//
// 网络开销可控：ChannelModels 内部 fetchRuntimeModels 自带 1h TTL 与
// 5min 失败负缓存，与本函数的 30min 调度配合，不会打爆上游。
func (a *App) refreshChannelModels() {
	if a.handler == nil {
		return
	}
	a.handler.ChannelModels()
}
