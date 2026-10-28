package doubao

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/doubaocdp"
	"wild-work/internal/provider"
)

// userTypeSelf 标记「这条消息是用户自己发的」。
//
// 实测抓包：用户提问的回显消息带 `user_type: 1` 且 `sender_id` 等于该账号的
// `merchant_user_id`（订阅信息里的用户 ID）。助手回复不带这个值。
// 不按内容判断（问句/答句没有可靠特征），只按上游给的发送者类型判断。
const userTypeSelf = 1

// DefaultBotID 是实测可用的 Bot。
//
// 交叉验证：`wangchuxiaoji-oss/doubao2api` 的 `BrowserClient.DEFAULT_BOT_ID`
// 记录为同一个值（7338286299411103781），独立实现口径一致。
const DefaultBotID = "7338286299411103781"

// ModelChat 是本渠道唯一的对话模型。
//
// 为什么只有一个：豆包客户端的模型选择器里只有「快速 / 豆包」两个**模式**，
// 而抓包确认 `/chat/completion` 的请求体**没有 model 字段**——模型/能力由
// 服务端按 bot_id 与会话配置决定。所以这里不编造多个模型名（R49：只透传
// 上游真实声明，绝不编造）。将来若摸到模式切换字段，再按真实维度扩展。
const ModelChat = "chat"

const (
	modelName        = "豆包（客户端）"
	modelContextWin  = 32768 // ⚠️ 上游未声明，属保守占位；面板按「未声明」展示
	modelMaxTokens   = 8192
)

// StaticModels 返回静态模型表（覆盖 provider 的「静态表 ∪ 动态目录」并集口径）。
func StaticModels() []provider.ModelInfo {
	return []provider.ModelInfo{{
		ID:            ModelChat,
		Name:          modelName,
		ContextWindow: modelContextWin,
		MaxTokens:     modelMaxTokens,
		// ContextFromAPI=false 且 HandMeasured=false ⇒ 面板如实显示「上游未声明」。
		// 不把占位值冒充成上游声明（R49 / R119 同口径）。
	}}
}

// FetchModels 拉上游模型目录。
//
// 豆包客户端没有 OpenAI 式的 /models 端点，模型维度也不存在（见 ModelChat 注释），
// 因此返回静态表 —— 这与 traework「上游列表不返回能力字段就回退不声明」同一口径。
func (c *Client) FetchModels(_ *auth.Auth) ([]provider.ModelInfo, error) {
	return StaticModels(), nil
}

// FetchModelPricing 费率。豆包客户端不暴露按模型的计价口径，返回空表示「无费率数据」，
// 面板会显示为未定价而不是 0 元。
func (c *Client) FetchModelPricing(_ *auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}

// RefreshToken 豆包没有可刷新的 token：凭据是客户端浏览器里的 cookie，
// 由客户端自己保活。这里返回 nil 表示「无需刷新」，而不是「刷新失败」。
func (c *Client) RefreshToken(_ *auth.Auth) error { return nil }

// UserResource 返回可消耗余额。
//
// 豆包没有「积分」概念，额度是**窗口限流**（5 小时 / 7 天两档 + 活动赠送）。
// 为了让 pool 的选号逻辑有数可用，这里用「活动赠送额度的剩余百分比 × 100」
// 作为可比较的标量——它单调、可比，且与真实可用性正相关。
// 真正的窗口余量通过 UserResourceDetail 以条目形式给出。
func (c *Client) UserResource(a *auth.Auth) (int64, error) {
	_, items, err := c.UserResourceDetail(a)
	if err != nil {
		return 0, err
	}
	var remain int64
	for _, it := range items {
		if it.Usable && it.Remain > 0 {
			remain += it.Remain
		}
	}
	return remain, nil
}

// quotaSummary 是 /alice/commerce/sale/subscription/quota/summary/ 的响应投影。
type quotaSummary struct {
	Data struct {
		WindowLimitSection struct {
			WindowLimitGroups []struct {
				FeatureGroup string `json:"feature_group"`
				WindowLimits []struct {
					WindowType    int   `json:"window_type"`
					UsedPercent   int   `json:"used_percent"`
					EndTime       int64 `json:"end_time"`
					LessThanOne   bool  `json:"less_than_one_percent"`
				} `json:"window_limits"`
			} `json:"window_limit_groups"`
		} `json:"window_limit_section"`
		IncentiveQuotaSection struct {
			EntitlementCount int `json:"entitlement_count"`
			WindowLimitGroups []struct {
				FeatureGroup     string `json:"feature_group"`
				FeatureGroupName string `json:"feature_group_name"`
				WindowLimits []struct {
					WindowType  int   `json:"window_type"`
					UsedPercent int   `json:"used_percent"`
					EndTime     int64 `json:"end_time"`
				} `json:"window_limits"`
			} `json:"window_limit_groups"`
		} `json:"incentive_quota_section"`
	} `json:"data"`
}

// windowTypeName 把 window_type 映射成面板文案。
//
// ⚠️ 1→5 小时、2→7 天 是**推断**（基于官方 UI 展示的两档额度 + DSH 插件描述的
// 「5 小时 / 7 天额度」），未做逐档 A/B 实证。因此条目 Name 里带上原始 window_type，
// 面板上一眼能看出映射依据，映射错了也便于纠正。
func windowTypeName(t int) string {
	switch t {
	case 1:
		return "5 小时窗口"
	case 2:
		return "7 天窗口"
	case 3:
		return "活动赠送额度"
	}
	return fmt.Sprintf("窗口 type=%d", t)
}

// UserResourceDetail 读取窗口额度明细。
//
// 数据来源：页面内 fetch 额度接口（自动签名）。
func (c *Client) UserResourceDetail(_ *auth.Auth) (int64, []provider.ResourceItem, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn, err := c.dialPage(ctx)
	if err != nil {
		return 0, nil, err
	}
	defer conn.Close()

	var sum quotaSummary
	expr := fmt.Sprintf(`(async () => {
		const r = await fetch(%q, {method:'POST', credentials:'include',
			headers:{'accept':'application/json','content-type':'application/json'}, body:'{}'});
		if (!r.ok) throw new Error('quota http ' + r.status);
		return await r.json();
	})()`, epQuotaSum)
	if err := conn.Evaluate(ctx, expr, &sum); err != nil {
		return 0, nil, err
	}

	var items []provider.ResourceItem
	var remain int64

	// 窗口限流：用「剩余百分比」折算成 0~100 的可比较数量。
	// 不把它当 token/积分展示——它是比例，Name 里已写明「窗口」。
	for _, g := range sum.Data.WindowLimitSection.WindowLimitGroups {
		for _, w := range g.WindowLimits {
			left := int64(100 - w.UsedPercent)
			if left < 0 {
				left = 0
			}
			items = append(items, provider.ResourceItem{
				Name:     fmt.Sprintf("%s (type=%d)", windowTypeName(w.WindowType), w.WindowType),
				Total:    100,
				Used:     int64(w.UsedPercent),
				Remain:   left,
				Usable:   true,
				ExpireAt: unixMilliToRFC3339(w.EndTime),
			})
			remain += left
		}
	}

	// 活动赠送额度：单独成条，便于面板区分「本来就有」和「活动送的」。
	for _, g := range sum.Data.IncentiveQuotaSection.WindowLimitGroups {
		for _, w := range g.WindowLimits {
			left := int64(100 - w.UsedPercent)
			if left < 0 {
				left = 0
			}
			name := g.FeatureGroupName
			if name == "" {
				name = windowTypeName(w.WindowType)
			}
			items = append(items, provider.ResourceItem{
				Name:     fmt.Sprintf("%s (type=%d)", name, w.WindowType),
				Total:    100,
				Used:     int64(w.UsedPercent),
				Remain:   left,
				Usable:   true,
				ExpireAt: unixMilliToRFC3339(w.EndTime),
			})
			remain += left
		}
	}
	return remain, items, nil
}

// DailyCheckin 每日签到领额度（满足 provider.Upstream 的最小接口）。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	_, err := c.DailyCheckinReport(a)
	return err
}

// DailyCheckinReport 结构化上报签到结果（provider.CheckinReporter）。
//
// 为什么不能只用 DailyCheckin 的 error 通道：上游对「今天已经领过」返回 code=0 +
// idempotent=true，那不是错误；但如果一律报成功，面板就分不清「刚领到 +200」与
// 「今天早就领过了」，ledger 也会把重放当成一次新发放（重复记账）。结构化上报把
// claimed / already_claimed 与本次发放额分开，调度器据此决定是否记 earn。
func (c *Client) DailyCheckinReport(_ *auth.Auth) (provider.CheckinReport, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	conn, err := c.dialPage(ctx)
	if err != nil {
		return provider.CheckinReport{Status: provider.CheckinError, Msg: err.Error()}, err
	}
	defer conn.Close()
	res, err := doubaocdp.Checkin(ctx, conn)
	if err != nil {
		return provider.CheckinReport{Status: provider.CheckinError, Msg: err.Error()}, err
	}
	if res.Idempotent {
		log.Printf("doubao checkin status=%s 日期=%s 连续=%d 天（未重复发放）",
			provider.CheckinAlready, res.CheckinDate, res.ContinuousDays)
		return provider.CheckinReport{
			Status: provider.CheckinAlready,
			Msg:    fmt.Sprintf("今日已领过（连续 %d 天）", res.ContinuousDays),
		}, nil
	}
	log.Printf("doubao checkin status=%s +%d %s 日期=%s 连续=%d 天",
		provider.CheckinClaimed, res.RewardAmount, res.RewardCode, res.CheckinDate, res.ContinuousDays)
	return provider.CheckinReport{
		Status: provider.CheckinClaimed,
		Amount: int64(res.RewardAmount),
		Msg:    fmt.Sprintf("签到成功 +%d 额度（连续 %d 天）", res.RewardAmount, res.ContinuousDays),
	}, nil
}

// Classify 把上游错误文案映射成 ErrKind。
//
// 豆包的错误以 HTTP 200 + 业务 code 或纯文本形式出现（客户端只把成功流回传），
// 所以这里按关键字判定，且顺序有意义：先判限流，再判额度，最后兜底。
func (c *Client) Classify(status int, body string) provider.ErrKind {
	low := strings.ToLower(body)
	switch {
	case status == 401 || status == 403,
		strings.Contains(low, "not login"), strings.Contains(low, "未登录"),
		strings.Contains(low, "session expired"):
		return provider.ErrSessionDead
	case status == 429, strings.Contains(low, "rate limit"), strings.Contains(low, "too many requests"),
		strings.Contains(low, "请求过于频繁"), strings.Contains(low, "限流"):
		return provider.ErrSoftRate
	case strings.Contains(low, "quota"), strings.Contains(low, "额度不足"),
		strings.Contains(low, "insufficient"), strings.Contains(low, "usage_exhausted"):
		return provider.ErrHardCredit
	case strings.Contains(low, "content"), strings.Contains(low, "审核"),
		strings.Contains(low, "违规"), strings.Contains(low, "sensitive"):
		return provider.ErrContentBlocked
	case status >= 500:
		return provider.ErrServer
	case status >= 400:
		return provider.ErrClient
	}
	return provider.ErrNone
}

// Stream 把豆包 SSE 转成 OpenAI Chat Completions 流式响应。
//
// 参数与返回值遵循 provider.Upstream 契约：model 是客户端请求的原始模型名
// （含 channel/ 前缀），必须在输出的 model 字段回填该值。
func (c *Client) Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	differ := newTextDiffer()
	flusher, _ := w.(http.Flusher)
	wroteAny := false

	emit := func(delta string) error {
		if delta == "" {
			return nil
		}
		wroteAny = true
		chunk := map[string]any{
			"id":      "chatcmpl-doubao",
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"content": delta},
			}},
		}
		b, _ := json.Marshal(chunk)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}

	err := doubaocdp.ScanSSE(r, func(f doubaocdp.SSEFrame) error {
		switch f.Event {
		case evHeartbeat, evAck:
			return nil
		case evFullMsg:
			// 用户提问的回显（user_type=1，sender_id=用户自己）。不是回答，跳过。
			// 早期实现把它当回答输出，客户端收到的是自己那句提问（联调实录）。
			return nil
		case evStreamMsg:
			// **助手回答在这里**：载荷是 content.content_block（不是 message）。
			var m streamMsgNotify
			if err := json.Unmarshal(f.Data, &m); err != nil {
				return nil // 单帧解析失败不炸整条流
			}
			for _, blk := range m.Content.ContentBlock {
				if blk.BlockType != blockTypeText {
					continue
				}
				delta, _ := differ.Delta(blk.BlockID, blk.Content.TextBlock.Text)
				if err := emit(delta); err != nil {
					return err
				}
			}
			return nil
		case evStreamChunk:
			// 增量补丁：多为收官帧（text 为空、is_finish=true），
			// 但若上游改推增量文本，这里同样能接住——差分逻辑对两种形态都成立。
			var c streamChunk
			if err := json.Unmarshal(f.Data, &c); err != nil {
				return nil
			}
			for _, blk := range c.Blocks() {
				if blk.BlockType != blockTypeText {
					continue
				}
				delta, _ := differ.Delta(blk.BlockID, blk.Content.TextBlock.Text)
				if err := emit(delta); err != nil {
					return err
				}
			}
			return nil
		case evReplyEnd:
			// 回答结束。读到即可收尾——上层会在 scanSSE 返回后统一发 stop/[DONE]。
			return nil
		default:
			// 没见过的帧：不报错也不吞掉，留给上层日志观察（见 sse.go 注释）
			return nil
		}
	})

	// R40 / R36：流内错误与截断都不得伪装成正常收尾。
	// 一字节未写时才允许向上返回错误；已经吐出正文则只能如实收尾，不能补 [DONE] 假装成功。
	if err != nil {
		if !wroteAny {
			return nil, err
		}
		return nil, err
	}
	if !wroteAny {
		return nil, fmt.Errorf("doubao: 上游未返回任何文本")
	}

	// 正常收尾：发 stop 帧 + [DONE]
	stop := map[string]any{
		"id": "chatcmpl-doubao", "object": "chat.completion.chunk",
		"created": time.Now().Unix(), "model": model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
	}
	sb, _ := json.Marshal(stop)
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", sb)
	if flusher != nil {
		flusher.Flush()
	}
	return nil, nil
}

// Aggregate 把豆包 SSE 聚合成 OpenAI 非流式响应。
func (c *Client) Aggregate(r io.Reader, model string) (map[string]any, error) {
	differ := newTextDiffer()
	var full strings.Builder

	collect := func(blocks []contentBlock) {
		for _, blk := range blocks {
			if blk.BlockType != blockTypeText {
				continue
			}
			if delta, _ := differ.Delta(blk.BlockID, blk.Content.TextBlock.Text); delta != "" {
				full.WriteString(delta)
			}
		}
	}
	err := doubaocdp.ScanSSE(r, func(f doubaocdp.SSEFrame) error {
		switch f.Event {
		case evStreamMsg:
			var m streamMsgNotify
			if err := json.Unmarshal(f.Data, &m); err == nil {
				collect(m.Content.ContentBlock)
			}
		case evStreamChunk:
			var c streamChunk
			if err := json.Unmarshal(f.Data, &c); err == nil {
				collect(c.Blocks())
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if full.Len() == 0 {
		return nil, fmt.Errorf("doubao: 上游未返回任何文本")
	}
	return map[string]any{
		"id":      "chatcmpl-doubao",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": full.String()},
			"finish_reason": "stop",
		}},
	}, nil
}

func unixMilliToRFC3339(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).Format(time.RFC3339)
}
