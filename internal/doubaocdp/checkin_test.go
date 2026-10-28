package doubaocdp

import (
	"encoding/json"
	"strings"
	"testing"
)

// 真实抓包（2026-10-27 14:5x，客户端 2.33.5，本人账号当日已领）——
// 逐字保留，用作解析契约的基线。
const realCheckinBody = `{"data":{"checked_in":true,"checkin_date":"2026-10-27",` +
	`"continuous_days":2,"idempotent":true,"reward_amount":200,"reward_code":"quota_grant",` +
	`"reward_status":3,"reward_type":"quota_grant"},"code":0,"msg":"","message":""}`

// TestCheckinPayloadParsing 锁住响应字段映射。
//
// 这里的价值不只是「能解析」：`checked_in` 与 `idempotent` 的语义必须先分清 ——
// 前者是「今天签到了没有」（判成功的唯一依据），后者是「这次是不是重放」。
// 一旦把 idempotent 当成失败，每天的自动签到会永远报错；反过来把 checked_in 忽略，
// 「code=0 但没签到」会被伪装成成功（R49 同口径的假成功）。
func TestCheckinPayloadParsing(t *testing.T) {
	var p checkinPayload
	if err := json.Unmarshal([]byte(realCheckinBody), &p); err != nil {
		t.Fatalf("解析真实响应失败：%v", err)
	}
	if p.Code != 0 {
		t.Fatalf("code = %d, want 0", p.Code)
	}
	if !p.Data.CheckedIn {
		t.Fatal("checked_in 应为 true")
	}
	if !p.Data.Idempotent {
		t.Fatal("idempotent 应为 true（当日已领的真实形态）")
	}
	if p.Data.CheckinDate != "2026-10-27" || p.Data.ContinuousDays != 2 {
		t.Fatalf("日期/连续天数映射错：%q %d", p.Data.CheckinDate, p.Data.ContinuousDays)
	}
	if p.Data.RewardAmount != 200 || p.Data.RewardCode != "quota_grant" {
		t.Fatalf("奖励映射错：%d %q", p.Data.RewardAmount, p.Data.RewardCode)
	}
}

// TestCheckinExprContract 断言脚本里三件不能少的东西：
// ① 打的是 landing/checkin（不是 report 之类的旁支）；
// ② POST + credentials:'include'（否则拿不到登录态，必然 401）；
// ③ 空 body `{}` 与 json content-type（抓包里就是这么发的）。
//
// 还要守住「指纹参数只能读取、不得编造」：脚本必须从 localStorage 取值，
// 不能写死 device_id/web_id/fp。
func TestCheckinExprContract(t *testing.T) {
	e := checkinExpr
	for _, must := range []string{
		epCampaignCheckin,
		"method: 'POST'",
		"credentials: 'include'",
		"'content-type': 'application/json'",
		"body: '{}'",
		"client_device_info",
		"__tea_cache_tokens_582478",
		"s_v_web_id",
		"aid: '582478'",
	} {
		if !strings.Contains(e, must) {
			t.Errorf("签到脚本缺少关键片段：%q", must)
		}
	}
	// 端点不得混入 report（那是埋点旁支，打了也不会发奖）
	if strings.Contains(e, "/campaign/landing/report") {
		t.Error("脚本不应调用 report 端点")
	}
}

// TestCheckinResultSemantics 记录「什么才算成功」的判据，供上层文案对齐。
func TestCheckinResultSemantics(t *testing.T) {
	// 已领：Idempotent=true、CheckedIn=true ⇒ 上层应记为成功但文案说「今日已领」
	var p checkinPayload
	_ = json.Unmarshal([]byte(realCheckinBody), &p)
	if !p.Data.CheckedIn {
		t.Fatal("已领形态的 checked_in 必须为 true（否则会被误判成失败）")
	}
	// 失败形态：code 非 0 或 checked_in=false —— 两种都不得当成功
	var bad checkinPayload
	if err := json.Unmarshal([]byte(`{"code":40001,"msg":"活动已结束","data":{}}`), &bad); err != nil {
		t.Fatal(err)
	}
	if bad.Code == 0 || bad.Data.CheckedIn {
		t.Fatal("失败形态被解析成了成功")
	}
}
