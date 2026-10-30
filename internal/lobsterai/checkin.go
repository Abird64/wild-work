// 每日签到：DailyCheckin / DailyCheckinReport 与活动槽位解析。
//
// 从 client.go 拆出（结构重排，零行为变更）。
package lobsterai

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// DailyCheckin 实现 provider.Upstream：每日签到（协议 §7.1 的三步流程）。
//
// 「今日已领取」返回 nil（幂等语义：已领取不是错误）。需要区分
// 「本次领到 / 今天已领过 / 活动还没上线」的调度器请用 DailyCheckinReport。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	_, err := c.DailyCheckinReport(a)
	return err
}

// DailyCheckinReport 实现 provider.CheckinReporter：额外返回结构化状态。
//
// 为什么必须结构化（不变式 23 的同款理由）：error 通道分不清「活动还没上线」与
// 「上游故障」——两者都不是「已签到」，却都需要在当日窗口内继续重试，
// 而 Msg 是面板上唯一能说清差别的字段。
//
// 三步流程（每一步的判据都照抄协议 §7.1）：
//
//	① GET /api/client-activities/slot?placement=…&clientVersion=…&containerApiVersion=2&platform=win32
//	   只有 slotState=="available" 且 activityCode 非空才继续；
//	② GET /api/client-activities/{code}/context?configRevision=…
//	   claimedToday==true → 已领取；actions 不含 check_in → inactive；
//	③ POST /api/client-activities/{code}/actions/check_in
//	   body = {configRevision, idempotencyKey: uuid4, payload: {}}
func (c *Client) DailyCheckinReport(a *auth.Auth) (provider.CheckinReport, error) {
	tok := tokenOf(a)
	if tok == "" {
		return provider.CheckinReport{Status: provider.CheckinNoToken, Msg: "无 access_token"}, nil
	}

	// ── 第一步：活动槽位 ────────────────────────────────────────────────────
	status, slotRaw, err := c.getJSON(c.activitySlotURL(), tok)
	if err != nil {
		return provider.CheckinReport{Status: provider.CheckinError, Msg: err.Error()}, err
	}
	if e := c.businessErr(status, slotRaw); e != nil {
		return provider.CheckinReport{Status: provider.CheckinError, Msg: e.Error()}, e
	}
	top, perr := decodeObject(slotRaw)
	if perr != nil {
		return provider.CheckinReport{Status: provider.CheckinError, Msg: "槽位响应解析失败"}, nil
	}
	if code, msg, ok := envelopeCode(top); ok && code != 0 {
		e := c.envelopeError(status, code, msg, slotRaw)
		return provider.CheckinReport{Status: provider.CheckinError, Msg: e.Error()}, e
	}
	slotData := businessObject(top)
	slotState := strings.TrimSpace(strOfRaw(slotData["slotState"]))
	activityCode, revision := activityOf(slotData["activity"])
	if slotState != "available" || activityCode == "" {
		// 「活动还没上线／已下线」是可重试状态（协议 §7.1 的 inactive 家族），
		// 不是故障：返回 NoCampaign 并**把 slotState 透出**，面板才看得出区别。
		return provider.CheckinReport{Status: provider.CheckinNoCampaign,
			Msg: fmt.Sprintf("活动槽位不可用（slotState=%q）", slotState)}, nil
	}

	// ── 第二步：活动上下文 ──────────────────────────────────────────────────
	ctxURL := c.base() + ActivityContextPath(activityCode)
	if rev := revisionQuery(revision); rev != "" {
		ctxURL += "?configRevision=" + url.QueryEscape(rev)
	}
	status, ctxRaw, err := c.getJSON(ctxURL, tok)
	if err != nil {
		return provider.CheckinReport{Status: provider.CheckinError, Msg: err.Error()}, err
	}
	if e := c.businessErr(status, ctxRaw); e != nil {
		return provider.CheckinReport{Status: provider.CheckinError, Msg: e.Error()}, e
	}
	ctxTop, perr := decodeObject(ctxRaw)
	if perr != nil {
		return provider.CheckinReport{Status: provider.CheckinError, Msg: "活动上下文解析失败"}, nil
	}
	if code, msg, ok := envelopeCode(ctxTop); ok && code != 0 {
		e := c.envelopeError(status, code, msg, ctxRaw)
		return provider.CheckinReport{Status: provider.CheckinError, Msg: e.Error()}, e
	}
	ctxData := businessObject(ctxTop)
	if state, err := decodeObject(ctxData["state"]); err == nil {
		if claimed, ok := boolOfRaw(state["claimedToday"]); ok && claimed {
			// claimedToday===true → 已领取（幂等：不算错误，也不再发起签到）。
			return provider.CheckinReport{Status: provider.CheckinAlready, Msg: "今日已领取"}, nil
		}
	}
	actions := stringList(ctxData["actions"])
	if !contains(actions, ActivityCheckInAction) {
		// 活动存在但**不提供签到动作**（inactive）：可重试（活动可能刚创建，动作表未就绪）。
		return provider.CheckinReport{Status: provider.CheckinNoCampaign,
			Msg: fmt.Sprintf("活动不含 %s 动作（actions=%v）", ActivityCheckInAction, actions)}, nil
	}

	// ── 第三步：签到 ────────────────────────────────────────────────────────
	idem, err := newUUID4()
	if err != nil {
		return provider.CheckinReport{Status: provider.CheckinError, Msg: "幂等键生成失败"}, err
	}
	reqBody := map[string]any{
		// configRevision **原样**回传（数字/字符串都可能，不转换类型）。
		"idempotencyKey": idem,
		"payload":        map[string]any{},
	}
	if len(revision) > 0 {
		reqBody["configRevision"] = revision
	}
	bodyRaw, err := json.Marshal(reqBody)
	if err != nil {
		return provider.CheckinReport{Status: provider.CheckinError, Msg: "签到请求体编码失败"}, err
	}
	status, claimRaw, err := c.postJSON(c.base()+ActivityCheckInPath(activityCode), tok, bodyRaw, false)
	if err != nil {
		return provider.CheckinReport{Status: provider.CheckinError, Msg: err.Error()}, err
	}
	if e := c.businessErr(status, claimRaw); e != nil {
		return provider.CheckinReport{Status: provider.CheckinError, Msg: e.Error()}, e
	}
	claimTop, perr := decodeObject(claimRaw)
	if perr != nil {
		return provider.CheckinReport{Status: provider.CheckinError, Msg: "签到响应解析失败"}, nil
	}
	if code, msg, ok := envelopeCode(claimTop); ok && code != 0 {
		e := c.envelopeError(status, code, msg, claimRaw)
		return provider.CheckinReport{Status: provider.CheckinError, Msg: e.Error()}, e
	}
	result, err := decodeObject(businessObject(claimTop)["result"])
	if err != nil {
		result = nil
	}
	// 积分三级回退：creditsGranted → rewardCredits → credits；都没有则为 0。
	// 判据是**键是否存在**（不是值是否 > 0）：给了 0 就是 0，不该继续往下回退。
	granted := firstPresent(result, "creditsGranted", "rewardCredits", "credits")
	msg := "签到成功"
	if granted > 0 {
		msg = fmt.Sprintf("签到成功 +%d", granted)
	}
	// ⚠️ LobsterAI 签到响应**不含连续天数概念**，故 provider.CheckinReport 里
	// 没有 streak 字段可填（参考实现同样恒置 streakDays=0 / isStreakDay=false）。
	return provider.CheckinReport{Status: provider.CheckinClaimed, Amount: granted, Msg: msg}, nil
}

// activitySlotURL 槽位查询 URL（协议 §1.3 的固定四参数）。
func (c *Client) activitySlotURL() string {
	q := url.Values{}
	q.Set("placement", ActivityPlacement)
	q.Set("clientVersion", c.ClientVersion())
	q.Set("containerApiVersion", ActivityContainerAPIVersion)
	q.Set("platform", ActivityPlatform) // 伪装客户端形态，与运行环境无关
	return c.base() + EpActivitySlot + "?" + q.Encode()
}

// activityOf 取 slot 的 data.activity.{activityCode,configRevision}。
// configRevision 用 RawMessage 原样保留（数字/字符串都可能，转类型就会发错形状）。
func activityOf(raw json.RawMessage) (activityCode string, revision json.RawMessage) {
	obj, err := decodeObject(raw)
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(strOfRaw(obj["activityCode"])), obj["configRevision"]
}

// revisionQuery 把 configRevision 渲染成 query 值：JSON 字符串取其值，数字/布尔取原文。
func revisionQuery(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if s := strOfRaw(raw); s != "" {
		return s
	}
	s := strings.TrimSpace(string(raw))
	if s == "null" {
		return ""
	}
	return s
}
