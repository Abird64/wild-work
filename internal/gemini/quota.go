// quota.go 配额窗口（`retrieveUserQuotaSummary`）与账号档位 / 项目探测（`loadCodeAssist`）。
//
// 协议依据：事实文档 §2.7 与参考实现 gemini-credits.ts / gemini-project.ts。
//
// ## ⚠️ 这不是「积分余额」
//
// Gemini 免费线是**配额窗口**制（5 小时 / 周两个窗口的**剩余比例**），不是充值积分。
// 面板如实显示「5 小时窗口 99% / 周窗口 99%」，**不**换算成任何积分数字——那会是编造。
// 本仓的 provider.ResourceItem 只有 int64 数字、没有单位字段，故本包用两处如实表达单位：
//   - Name 里带 `（%）`（例如「5 小时窗口（%）」）；
//   - **InfoOnly=true**：该条目只展示，不参与任何积分算术（Summarize 小计 / 临期 / ledger 差分）。
//     否则「99 + 99 = 198」会被当作 198 积分显示出来，比不显示更误导。
//
// ## ⚠️ 三条硬约束（照参考实现，别"优化"）
//
//  1. 请求体字段名是 **`project`**，不是 `cloudaicompanionProject`
//     （后者回 400 `Unknown name "cloudaicompanionProject": Cannot find field.`）；
//     且**必须带**：原版发 `{}` 是单账号抓包结论，实测第二个账号 `{}` 被拒
//     `403 PERMISSION_DENIED SUBSCRIPTION_REQUIRED (#3501)`，同 token 同头加 `project` 即 200。
//  2. 只认 `gemini-5h` / `gemini-weekly` 两个 **bucketId**，**不按** `displayName` / `window`
//     匹配（第三组 `3p-*` 属 Claude/GPT 产品线，与本渠道无关）。
//  3. `resetTime` 解析失败 ⇒ **整条桶丢弃**：拿不到可解析的重置时间就无法表达窗口周期，
//     编一个空串会让面板显示一个**永远不过期**的窗口（宁可少一个包，也不伪造）。
//
// ## ⚠️ 两条端点纪律
//
//   - 配额与探测**固定走 sandbox**（协议 §2.1：只有推理走轮换端点）；
//   - 未授权**不是错误**：没有 access_token 时返回 ErrSessionDead（上层据此 refresh +
//     重试一次、或提示重新登录），而不是把它当成配额查询失败。
package gemini

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// quotaLoc 配额重置时间的墙钟口径（UTC+8），与 provider 的 expireLoc 同值。
var quotaLoc = time.FixedZone("UTC+8", 8*60*60)

// codeAssistInfo loadCodeAssist 的解析结果（只取本包真正需要的字段）。
type codeAssistInfo struct {
	// Project 上游下发的项目号（免费档常为空 ⇒ 调用方回落 DefaultProject）。
	Project string
	// PaidTierID / PaidTierName 账号档位。⚠️ 判据是 **paidTier**，不是 currentTier：
	// 实测两个账号的 currentTier 都是 free-tier/Antigravity，真正区分「有没有 Pro」的是 paidTier。
	PaidTierID   string
	PaidTierName string
	// CurrentTierID / CurrentTierName 仅作 paidTier 缺席时的兜底。
	CurrentTierID   string
	CurrentTierName string

	// ---- 准入判定（2026-10-30 现场：Pro 账号仍被 403 拦住）----
	//
	// `ineligibleTiers[]` 是上游明说「这个账号**不够格**用哪些档位」的地方。实测样例
	// （Google AI Pro 账号，loadCodeAssist 200）：
	//
	//	"ineligibleTiers": [{
	//	  "reasonCode": "VALIDATION_REQUIRED",
	//	  "reasonMessage": "Your current account is not eligible for Antigravity. Verify your account to continue.",
	//	  "tierId": "free-tier", "tierName": "Antigravity",
	//	  "validationErrorMessage": "Verify your account to continue.",
	//	  "validationUrlLinkText": "Verify your account",
	//	  "validationUrl": "https://accounts.google.com/v3/signin/continue?...",
	//	  "validationLearnMoreUrl": "https://support.google.com/accounts?p=al_alert"
	//	}]
	//
	// 这条信息**只有 loadCodeAssist 会给**：随后所有真实调用（配额 / 推理）都只回一句
	// 干巴巴的 `403 The caller does not have permission`，用户完全看不出要去「验证账号」。
	// 所以必须在这里抓住它，否则面板只能显示「查询失败」。
	ValidationRequired bool
	ValidationURL      string // 上游给的验证入口（逐字使用，不自己拼）
	ValidationText     string // 链接文案（如「Verify your account」）
	IneligibleReason   string // reasonCode，如 VALIDATION_REQUIRED
	IneligibleMessage  string // reasonMessage 原文
}

// loadCodeAssist 探测账号档位与项目（`POST {sandbox}/v1internal:loadCodeAssist`）。
//
// 请求体是**逐字常量**（LoadCodeAssistBody，content-length 恰为 38）：刻意不带
// platform / pluginType / cloudaicompanionProject，多一个字段就可能被识别成非官方客户端。
func (c *Client) loadCodeAssist(a *auth.Auth) (codeAssistInfo, error) {
	token := tokenOf(a)
	if token == "" {
		return codeAssistInfo{}, &provider.Error{Kind: provider.ErrSessionDead, Msg: "gemini：尚未授权 Google 账号"}
	}
	status, raw, err := c.postJSON(c.sandboxHost()+EpLoadCodeAssist, token, []byte(LoadCodeAssistBody))
	if err != nil {
		return codeAssistInfo{}, err
	}
	if status >= 400 {
		return codeAssistInfo{}, classifyErr(status, raw, "账号档位/项目探测")
	}
	info, perr := parseCodeAssist(raw)
	if perr != nil {
		// 空响应/非 JSON **算探测失败**：协议 §2.7 明确「探测失败（网络 / 非 2xx / 空响应）
		// ⇒ 不发推理」。这里把解析失败也归入同一类，避免拿一个「看起来成功但读不出项目」
		// 的结果去发请求。
		return codeAssistInfo{}, perr
	}
	return info, nil
}

// parseCodeAssist 解析 loadCodeAssist 响应。
//
// 项目取值顺序（协议 §2.7）：顶层 `cloudaicompanionProject` → `currentTier.cloudaicompanionProject`。
// 档位取值顺序：`paidTier` → `currentTier`（paidTier 才是判据，currentTier 只兜底）。
func parseCodeAssist(raw []byte) (codeAssistInfo, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return codeAssistInfo{}, fmt.Errorf("gemini：loadCodeAssist 返回空响应")
	}
	var payload struct {
		CloudAICodeProject string `json:"cloudaicompanionProject"`
		PaidTier           *struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"paidTier"`
		CurrentTier *struct {
			ID                 string `json:"id"`
			Name               string `json:"name"`
			CloudAICodeProject string `json:"cloudaicompanionProject"`
		} `json:"currentTier"`
		IneligibleTiers []struct {
			ReasonCode             string `json:"reasonCode"`
			ReasonMessage          string `json:"reasonMessage"`
			TierID                 string `json:"tierId"`
			TierName               string `json:"tierName"`
			ValidationURL          string `json:"validationUrl"`
			ValidationURLLinkText  string `json:"validationUrlLinkText"`
			ValidationErrorMessage string `json:"validationErrorMessage"`
		} `json:"ineligibleTiers"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return codeAssistInfo{}, fmt.Errorf("gemini：loadCodeAssist 响应不是 JSON（%v）", err)
	}
	info := codeAssistInfo{
		Project: strings.TrimSpace(payload.CloudAICodeProject),
	}
	if payload.CurrentTier != nil {
		info.CurrentTierID = strings.TrimSpace(payload.CurrentTier.ID)
		info.CurrentTierName = strings.TrimSpace(payload.CurrentTier.Name)
		if info.Project == "" {
			info.Project = strings.TrimSpace(payload.CurrentTier.CloudAICodeProject)
		}
	}
	if payload.PaidTier != nil {
		info.PaidTierID = strings.TrimSpace(payload.PaidTier.ID)
		info.PaidTierName = strings.TrimSpace(payload.PaidTier.Name)
	}
	// 准入判定：取**第一条带 reasonCode 的**不符合格条目。上游按档位列出多条时，
	// 它们对用户而言是同一件事（这个账号要先验证），取首条足以表达。
	for _, it := range payload.IneligibleTiers {
		if strings.TrimSpace(it.ReasonCode) == "" {
			continue
		}
		info.IneligibleReason = strings.TrimSpace(it.ReasonCode)
		info.IneligibleMessage = strings.TrimSpace(it.ReasonMessage)
		info.ValidationURL = strings.TrimSpace(it.ValidationURL)
		info.ValidationText = strings.TrimSpace(it.ValidationURLLinkText)
		// 任一 reasonCode 都算「上游明说的准入判定」。原先只认 VALIDATION_REQUIRED，
		// 于是 SUBSCRIPTION_REQUIRED / UNSUPPORTED_CLIENT 这两类同样明确的结论被丢掉，
		// 用户只能看到 403 裸文案（2026-10-30 实测这三种都出现了，见下）。
		info.ValidationRequired = true
		break
	}
	if info.Project == "" && info.PaidTierID == "" && info.CurrentTierID == "" {
		// 三个阶段都读不出东西：按探测失败处理（照原版：失败就不发推理）。
		// ⚠️ 注意「200 但 project 为空」是**免费档常态**，那时 CurrentTier 通常仍在，
		// 故不会被判失败；这里拦的是「响应里什么都没有」的真失败形态。
		return codeAssistInfo{}, fmt.Errorf("gemini：loadCodeAssist 响应里既无 project 也无档位")
	}
	return info, nil
}

// tierLabel 档位 id / 名 → 面板短标签（gemini-credits.ts:123-129）。
//
// ⚠️ 顺序有意义：`Ultra` → `Pro` → `Free`。上游把「Pro 但已降级」也叫 `free-tier`
// （Antigravity Starter Quota），故不能只看 id 的 `-tier` 后缀。
func tierLabel(id, name string) string {
	combined := strings.ToLower(strings.TrimSpace(id) + " " + strings.TrimSpace(name))
	switch {
	case strings.Contains(combined, "ultra"):
		return "Ultra"
	case strings.Contains(combined, "pro"):
		return "Pro"
	case strings.Contains(combined, "free"):
		return "Free"
	}
	if n := strings.TrimSpace(name); n != "" {
		return n
	}
	return strings.TrimSpace(id)
}

// ---------------------------------------------------------------------------
// 配额窗口
// ---------------------------------------------------------------------------

// quotaBucket 一个上游配额桶（只取本包需要的四个字段）。
type quotaBucket struct {
	BucketID          string   `json:"bucketId"`
	Window            string   `json:"window"`
	ResetTime         string   `json:"resetTime"`
	RemainingFraction *float64 `json:"remainingFraction"`
}

// quotaResponse 配额响应形状（协议 §2.7 的实测样例）。
type quotaResponse struct {
	Groups []struct {
		DisplayName string        `json:"displayName"`
		Buckets     []quotaBucket `json:"buckets"`
	} `json:"groups"`
}

// quotaSummary 查询配额窗口并映射成 ResourceItem（结果按 QuotaCacheTTL 缓存）。
func (c *Client) quotaSummary(a *auth.Auth, project string) ([]provider.ResourceItem, error) {
	token := tokenOf(a)
	if token == "" {
		return nil, &provider.Error{Kind: provider.ErrSessionDead, Msg: "gemini：尚未授权 Google 账号"}
	}
	key := projectKey(a)
	if items, ok := c.cachedQuota(key); ok {
		return items, nil
	}
	// ⚠️ 字段名是 `project`（不是 cloudaicompanionProject），见文件头第 1 条。
	body, err := json.Marshal(map[string]string{"project": project})
	if err != nil {
		return nil, err
	}
	status, raw, err := c.postJSON(c.sandboxHost()+EpQuotaSummary, token, body)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, classifyErr(status, raw, "配额查询")
	}
	items, perr := parseQuotaItems(raw)
	if perr != nil {
		return nil, perr
	}
	c.cacheQuota(key, items)
	return items, nil
}

// withAdmission 把「账号准入」结论附加到配额/推理错误上。
//
// 为什么单独抽出来：配额 403 与推理 403 是**同一件事**的两种表现（账号不够格），
// 而只有 loadCodeAssist 会给出可行动的那句话（验证链接）。两个调用点都要附，
// 各写一份必然漂移。
//
// 不改 Kind / Status（那是给冷却状态机用的判据），只改用户可见文案。
func withAdmission(err error, info codeAssistInfo) error {
	if err == nil {
		return err
	}
	// 403 但上游**没给任何 reasonCode** 的形态（Antigravity 指纹下实测就是这样：
	// loadCodeAssist 的 ineligibleTiers 已消失，配额却回一句裸的
	// `403 The caller does not have permission`）。此时不能编造原因，但也不能
	// 只丢一句 403 —— 如实列出已知可能，并给出唯一能自证的验证方法。
	if !info.ValidationRequired && isAdmissionBlocked(strings.ToLower(err.Error())) {
		var pe *provider.Error
		if errors.As(err, &pe) {
			cp := *pe
			cp.Msg = unknownAdmissionHint() + "；原始错误：" + pe.Msg
			return &cp
		}
		return errors.New(unknownAdmissionHint() + "；原始错误：" + err.Error())
	}
	if !info.ValidationRequired {
		return err
	}
	// 提示放在**最前面**：面板/日志都会截断长文案，而「需要验证账号 + 链接」
	// 是用户唯一能行动的那句话，绝不能被上游那串 JSON 原文挤到末尾去。
	var pe *provider.Error
	if errors.As(err, &pe) {
		cp := *pe
		cp.Msg = strings.TrimPrefix(admissionHint(info), "；") + "；原始错误：" + pe.Msg
		return &cp
	}
	return errors.New(strings.TrimPrefix(admissionHint(info), "；") + "；原始错误：" + err.Error())
}

// unknownAdmissionHint 403 且上游未给 reasonCode 时的提示。
//
// 只列**已实测见过的**三种可能，并给出唯一能定性的动作（用官方 CLI 登录同账号），
// 不猜具体是哪一个。
func unknownAdmissionHint() string {
	return "⚠️ 上游拒绝了该账号的 Code Assist 访问，但**没有给出原因码**（403 The caller does not have permission）。" +
		"已实测的三种成因：① 该账号无 Code Assist 订阅/授权（同形态在另一份响应里是 403 #3501「valid license」）；" +
		"② 账号需先完成 Google 账号验证；③ 账号的 project 绑定异常（上游已知问题）。" +
		"定性办法：在该机用官方 `gemini` CLI 登录同一账号跑一句对话 —— CLI 也失败则属账号侧，" +
		"换一个个人 Google 账号最快"
}

// admissionHint 用上游**原样**给出的链接与文案拼一句可行动提示。
// 绝不自己拼 Google 的验证 URL（那串带 sarp/scc/continue 参数，自己拼必错）。
func admissionHint(info codeAssistInfo) string {
	if !info.ValidationRequired {
		return ""
	}
	// 按 reasonCode 分流成**可行动**的结论。三种都是 2026-10-30 在本机同一个账号上
	// 实测拿到的原文（换不同客户端指纹/不同端点会得到不同的一种）：
	//
	//	VALIDATION_REQUIRED  → 账号需先完成 Google 账号验证（上游带验证链接，照抄给它）
	//	SUBSCRIPTION_REQUIRED→ 该账号没有 Code Assist 的有效订阅（403 #3501
	//	                       "You do not have a valid license of this product"）
	//	UNSUPPORTED_CLIENT   → 该客户端已被弃用（gemini-cli 指纹现在会得到这句，
	//	                       要求迁移到 Antigravity）
	var hint string
	switch strings.ToUpper(info.IneligibleReason) {
	case "VALIDATION_REQUIRED":
		hint = "；⚠️ 上游判定该账号**需要先完成 Google 账号验证**才会放行（code=" + info.IneligibleReason + "）"
	case "SUBSCRIPTION_REQUIRED":
		hint = "；⚠️ 上游判定该账号**没有 Code Assist 的有效订阅/授权**（code=" + info.IneligibleReason +
			"，403 #3501「You do not have a valid license of this product」）——不是工具问题，也不是积分不足；" +
			"换一个已开通 Code Assist 的账号，或为该账号所在的 GCP project 开通订阅"
	case "UNSUPPORTED_CLIENT":
		hint = "；⚠️ 上游**已弃用该客户端形态**（code=" + info.IneligibleReason + "），" +
			"要求迁移到 Antigravity 客户端"
	default:
		hint = "；⚠️ 上游拒绝了该账号的准入（code=" + info.IneligibleReason + "）"
	}
	if info.ValidationURL != "" {
		label := info.ValidationText
		if label == "" {
			label = "处理入口"
		}
		hint += "：" + label + " → " + info.ValidationURL
	} else if info.IneligibleMessage != "" {
		hint += "：" + info.IneligibleMessage
	}
	return hint
}

// parseQuotaItems 解析配额响应 → ResourceItem 列表（顺序：5 小时窗口 → 周窗口）。
func parseQuotaItems(raw []byte) ([]provider.ResourceItem, error) {
	var resp quotaResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("gemini：配额响应不是 JSON（%v）", err)
	}
	var fiveHour, weekly *quotaBucket
	for gi := range resp.Groups {
		for bi := range resp.Groups[gi].Buckets {
			b := resp.Groups[gi].Buckets[bi]
			switch b.BucketID {
			case BucketFiveHour:
				if fiveHour == nil {
					fiveHour = &b
				}
			case BucketWeekly:
				if weekly == nil {
					weekly = &b
				}
			}
		}
	}
	if fiveHour == nil && weekly == nil {
		// 两个桶都没认出来 ⇒ 真失败，**不伪造 100%**（伪造比报错更误导：用户会以为额度用光了）。
		return nil, fmt.Errorf("gemini：配额响应里没有 Gemini 配额桶（%s / %s）", BucketFiveHour, BucketWeekly)
	}
	items := make([]provider.ResourceItem, 0, 2)
	for _, b := range []*quotaBucket{fiveHour, weekly} {
		if b == nil {
			continue
		}
		item, ok := quotaItem(*b)
		if !ok {
			continue // resetTime / remainingFraction 不可用 ⇒ 整条丢弃，不伪造窗口
		}
		items = append(items, item)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("gemini：配额桶存在但字段不可用（remainingFraction / resetTime 缺失或不可解析）")
	}
	return items, nil
}

// quotaItem 单个桶 → ResourceItem；ok=false 表示该桶字段不满足硬约束。
func quotaItem(b quotaBucket) (provider.ResourceItem, bool) {
	label := ""
	switch b.BucketID {
	case BucketFiveHour:
		label = WindowLabelFiveHour
	case BucketWeekly:
		label = WindowLabelWeekly
	default:
		return provider.ResourceItem{}, false
	}
	if b.RemainingFraction == nil || math.IsNaN(*b.RemainingFraction) || math.IsInf(*b.RemainingFraction, 0) {
		return provider.ResourceItem{}, false
	}
	reset, err := time.Parse(time.RFC3339, strings.TrimSpace(b.ResetTime))
	if err != nil {
		return provider.ResourceItem{}, false
	}
	percent := int64(math.Round(clamp01(*b.RemainingFraction) * 100))
	return provider.ResourceItem{
		// 单位写进名字：ResourceItem 没有单位字段，不写就会把 99 误读成 99 积分。
		Name: label + "（" + QuotaUnit + "）",
		// Total/Remain/Used 一律按百分比表达（Total=100 是「窗口总量 = 100%」）。
		Total:  100,
		Remain: percent,
		Used:   100 - percent,
		// ExpireAt 是窗口重置时刻（上游 resetTime），按 UTC+8 墙钟输出（不变量 18）。
		ExpireAt: reset.In(quotaLoc).Format(time.RFC3339),
		Key:      b.BucketID,
		// Usable=true：它是账号真实可用的额度；InfoOnly=true：它**不是积分**，
		// 不参与 Summarize 小计 / 临期 / ledger 差分（见文件头）。
		Usable:   true,
		InfoOnly: true,
	}, true
}

// clamp01 把剩余比例收敛到 [0,1]（越界值来自上游，不能让百分比溢出）。
func clamp01(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// cachedQuota 读配额缓存（TTL 内有效）。
func (c *Client) cachedQuota(key string) ([]provider.ResourceItem, bool) {
	if key == "" {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.quotaCache[key]
	if !ok || time.Since(e.at) > QuotaCacheTTL {
		return nil, false
	}
	return e.items, true
}

// cacheQuota 写配额缓存。
func (c *Client) cacheQuota(key string, items []provider.ResourceItem) {
	if key == "" {
		return
	}
	c.mu.Lock()
	if c.quotaCache == nil {
		c.quotaCache = map[string]quotaCacheEntry{}
	}
	c.quotaCache[key] = quotaCacheEntry{at: time.Now(), items: items}
	c.mu.Unlock()
}

// classifyErr 把 HTTP 层失败转成类型化的 *provider.Error。
func classifyErr(status int, raw []byte, what string) error {
	return &provider.Error{
		Kind:   Classify(status, string(raw)),
		Status: status,
		Msg:    fmt.Sprintf("gemini：%s失败（HTTP %d）：%s", what, status, snippet(raw)),
	}
}

// ---------------------------------------------------------------------------
// provider.Upstream：余额
// ---------------------------------------------------------------------------

// UserResource 实现 provider.Upstream。
//
// ⚠️ 返回 **0**：本渠道没有「可消耗积分」这个概念，配额是窗口百分比（见文件头）。
// 返回 0 是如实的「本工具口径下没有可消耗积分」，而不是「查询失败」——
// 配额明细由 UserResourceDetail 的 InfoOnly 条目承载。
func (c *Client) UserResource(a *auth.Auth) (int64, error) {
	_, _, err := c.UserResourceDetail(a)
	return 0, err
}

// UserResourceDetail 实现 provider.Upstream：返回 (0, 配额窗口条目, error)。
//
// 编排（与参考实现同构）：
//  1. project 取「凭据真值 → 未知则先探测」：配额请求**必须**带正确的 project
//     （协议 §2.7 第 1 条），拿默认值去问会得到 403；
//  2. 档位探测（loadCodeAssist）与配额查询**并行**，且**档位失败绝不污染配额结果**——
//     「账号规格」只是一栏附注，不能因为它挂了就把整行变成错误。
func (c *Client) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	token := tokenOf(a)
	if token == "" {
		return 0, nil, &provider.Error{Kind: provider.ErrSessionDead, Msg: "gemini：尚未授权 Google 账号"}
	}

	project := storedProject(a)
	info := codeAssistInfo{}
	probed := false
	if project == "" {
		// 项目未知：先探一次（探测失败也继续——配额用默认值试，错误如实透出）。
		if in, err := c.loadCodeAssist(a); err == nil {
			info, probed = in, true
			if p := strings.TrimSpace(in.Project); p != "" {
				project = p
			}
			c.cacheProject(projectKey(a), orDefaultProject(project))
		}
	}
	if project == "" {
		project = DefaultProject
	}

	tierCh := make(chan tierProbe, 1)
	go func() {
		if probed {
			tierCh <- tierProbe{info: info}
			return
		}
		in, err := c.loadCodeAssist(a)
		tierCh <- tierProbe{info: in, err: err}
	}()

	items, qerr := c.quotaSummary(a, project)
	tr := <-tierCh
	if tr.err == nil {
		c.persistProject(a, tr.info)
	}
	if qerr != nil {
		// 把 loadCodeAssist 里的准入判定拼进来（2026-10-30 现场）。
		// 真实调用的 403 只回一句 `The caller does not have permission`，
		// 而「账号需要先验证」这个唯一可行动的结论只在 loadCodeAssist 的
		// ineligibleTiers 里出现过 —— 不在这里合并，面板/日志就永远看不到它。
		return 0, nil, withAdmission(qerr, tr.info)
	}
	return 0, items, nil
}

// tierProbe 档位探测的并行结果。
type tierProbe struct {
	info codeAssistInfo
	err  error
}

// orDefaultProject 空串 → DefaultProject（缓存与请求体都不接受空 project）。
func orDefaultProject(p string) string {
	if strings.TrimSpace(p) == "" {
		return DefaultProject
	}
	return p
}
