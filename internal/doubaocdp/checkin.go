// 本文件实现豆包「每日签到领额度」的活动接口。
//
// # 为什么放在 doubaocdp 而不是某个渠道包里
//
// 豆包与豆包工作**共用同一个客户端、同一个调试端口、同一份账号额度**（见 provider.go 里
// Doubao/Doubaowork 的注释），而签到是**账号级**活动：同一个账号在任一形态里签到一次即可。
// 两个渠道包各自实现一遍，等于让「同一件事」有两个可能跑偏的副本 —— 放这里，两个渠道
// 都只是「dialPage 之后调一次」。
//
// # 协议取证（2026-10-27 实测，客户端 2.33.5 / Chromium 147）
//
// 面板侧栏「今日签到领 200 额度 → 立即签到」按钮发出的真实请求是：
//
//	POST https://www.doubao.com/alice/ug/campaign/landing/checkin?<一长串指纹参数>
//	content-type: application/json, body {}
//
// 响应（本人账号、当日已领时）：
//
//	{"code":0,"data":{"checked_in":true,"checkin_date":"2026-10-27","continuous_days":2,
//	  "idempotent":true,"reward_amount":200,"reward_code":"quota_grant","reward_status":3,
//	  "reward_type":"quota_grant"},"msg":"","message":""}
//
// 关键点：`idempotent=true` ⇒ **重复调用不会重复发放**，因此本实现不需要先查状态再决定是否
// 领取（少一个接口、少一次往返、也少一处「查到了但状态已变」的竞态）。`checked_in` 才是
// 「今天已经签过」的判据；`reward_amount/reward_code` 用于日志，让面板与 app.log 能看出
// 这次到底领到了什么。
//
// 查询参数里的 device_id / web_id / fp 是客户端指纹，按 §0.1 纪律**不在服务端伪造**：
// 能读到就带上（从页面 localStorage 取真值），读不到就不带 —— 实测只带 aid 也能拿到 200 与
// 正确状态，那些参数是归因用而非鉴权用。
package doubaocdp

import (
	"context"
	"fmt"
)

// epCampaignCheckin 每日签到领额度（页面内 fetch，跨域由客户端自身处理）。
const epCampaignCheckin = "https://www.doubao.com/alice/ug/campaign/landing/checkin"

// CheckinResult 一次签到的结果（供日志与面板文案使用）。
type CheckinResult struct {
	CheckedIn      bool   // 上游确认「今天已签到」
	CheckinDate    string // 签到归属日期（上游口径，yyyy-mm-dd）
	ContinuousDays int    // 连续签到天数
	Idempotent     bool   // 本次是否为幂等重放（即今天之前已经领过）
	RewardAmount   int    // 本次奖励数量（额度单位）
	RewardCode     string // 奖励类型码，如 quota_grant
}

// checkinPayload 是 landing/checkin 响应里本包消费的投影。
type checkinPayload struct {
	Code    int    `json:"code"`
	Msg     string `json:"msg"`
	Message string `json:"message"`
	Data    struct {
		CheckedIn      bool   `json:"checked_in"`
		CheckinDate    string `json:"checkin_date"`
		ContinuousDays int    `json:"continuous_days"`
		Idempotent     bool   `json:"idempotent"`
		RewardAmount   int    `json:"reward_amount"`
		RewardCode     string `json:"reward_code"`
		RewardStatus   int    `json:"reward_status"`
		RewardType     string `json:"reward_type"`
	} `json:"data"`
}

// Checkin 在页面上下文里发一次签到请求。
//
// conn 必须是**对话页**（PickChatTarget 挑出来的那个）—— 活动卡片挂在对话页侧栏，
// 其它 target（新标签页/后台页）里没有登录态的完整上下文。
func Checkin(ctx context.Context, conn *CDP) (CheckinResult, error) {
	var p checkinPayload
	if err := conn.Evaluate(ctx, checkinExpr, &p); err != nil {
		return CheckinResult{}, err
	}
	if p.Code != 0 {
		msg := p.Msg
		if msg == "" {
			msg = p.Message
		}
		if msg == "" {
			msg = "上游未给出原因"
		}
		return CheckinResult{}, fmt.Errorf("豆包签到失败（code=%d）：%s", p.Code, msg)
	}
	if !p.Data.CheckedIn {
		// code=0 但没标记 checked_in：不臆测成成功（同 R49「不编造」口径），
		// 让调度器与用户都能看见「签了但没生效」。
		return CheckinResult{}, fmt.Errorf("豆包签到未生效：上游未标记 checked_in（msg=%q）", p.Msg)
	}
	return CheckinResult{
		CheckedIn:      p.Data.CheckedIn,
		CheckinDate:    p.Data.CheckinDate,
		ContinuousDays: p.Data.ContinuousDays,
		Idempotent:     p.Data.Idempotent,
		RewardAmount:   p.Data.RewardAmount,
		RewardCode:     p.Data.RewardCode,
	}, nil
}

// checkinExpr 页面内执行的签到脚本。
//
// 指纹参数的处理原则：**能读到真值就带真值，读不到就省略**，绝不编造。
//   - device_id / tea_uuid ← localStorage["client_device_info"].deviceId
//   - web_id              ← localStorage["__tea_cache_tokens_582478"].web_id
//   - fp                  ← localStorage["s_v_web_id"]
//
// 其余是产品/通道标识（aid=582478 为豆包 PC 客户端），按抓包原样带上，避免奖励被记到
// 别的产品线名下。web_tab_id 每次生成即可（上游按 tab 归因，不做去重）。
var checkinExpr = `(async () => {
  const q = new URLSearchParams({
    version_code: '20800', language: 'zh', device_platform: 'web',
    doubao_device_platform: 'desktop', aid: '582478', real_aid: '582478',
    pkg_type: 'release_version', pc_version: '2.33.5', doubao_pc_version: '2.33.5',
    region: 'CN', sys_region: 'CN', samantha_web: '1', web_platform: 'desktop',
    'use-olympus-account': '1', runtime: 'web', runtime_version: '3.43.0',
    client_platform: 'pc_client', chromium_version: '147.0.7727.149', channel: 'win',
    web_tab_id: (self.crypto && self.crypto.randomUUID) ? self.crypto.randomUUID() : String(Date.now())
  });
  try {
    const di = JSON.parse(localStorage.getItem('client_device_info') || '{}');
    if (di && di.deviceId) { q.set('device_id', di.deviceId); q.set('tea_uuid', di.deviceId); }
  } catch (e) {}
  try {
    const t = JSON.parse(localStorage.getItem('__tea_cache_tokens_582478') || '{}');
    if (t && t.web_id) { q.set('web_id', t.web_id); }
  } catch (e) {}
  try {
    const f = localStorage.getItem('s_v_web_id');
    if (f) { q.set('fp', f); }
  } catch (e) {}
  const r = await fetch('` + epCampaignCheckin + `?' + q.toString(), {
    method: 'POST', credentials: 'include',
    headers: { 'content-type': 'application/json' }, body: '{}'
  });
  const text = await r.text();
  if (!r.ok) { throw new Error('checkin http ' + r.status + ' ' + text.slice(0, 120)); }
  return JSON.parse(text);
})()`
