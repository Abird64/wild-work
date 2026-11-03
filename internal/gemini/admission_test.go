// admission_test.go 账号准入（VALIDATION_REQUIRED）识别回归。
//
// 现场（2026-10-30）：用户账号有 Google AI Pro（paidTier = g1-pro-tier，loadCodeAssist 200），
// 但 `ineligibleTiers` 里写着 VALIDATION_REQUIRED「Verify your account to continue」。
// 随后所有真实调用（配额 / 推理）只回一句 `403 The caller does not have permission`。
// 旧实现的两个后果：① 403 归 ErrServer（「上游故障」语义）⇒ 不换号、不提示；
// ② 配额查询失败后面板显示 0 分，看着像「没额度了」，实际是「账号要验证」。
package gemini

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wild-work/internal/provider"
)

// admissionBody 是 2026-10-30 实测的 loadCodeAssist 响应（截取关键字段，值逐字）。
const admissionBody = `{
  "currentTier": {"id":"standard-tier","name":"Antigravity","userDefinedCloudaicompanionProject":true},
  "cloudaicompanionProject": "aicode-consumers",
  "ineligibleTiers": [{
    "reasonCode": "VALIDATION_REQUIRED",
    "reasonMessage": "Your current account is not eligible for Antigravity. Verify your account to continue.",
    "tierId": "free-tier",
    "tierName": "Antigravity",
    "validationErrorMessage": "Verify your account to continue.",
    "validationUrlLinkText": "Verify your account",
    "validationUrl": "https://accounts.google.com/v3/signin/continue?plt=X&continue=Y",
    "validationLearnMoreUrl": "https://support.google.com/accounts?p=al_alert"
  }],
  "paidTier": {"id":"g1-pro-tier","name":"Google AI Pro"}
}`

// TestParseAdmissionRequired 必须把 ineligibleTiers 的准入判定解析出来。
func TestParseAdmissionRequired(t *testing.T) {
	info, err := parseCodeAssist([]byte(admissionBody))
	if err != nil {
		t.Fatalf("parse 失败：%v", err)
	}
	if !info.ValidationRequired {
		t.Fatal("VALIDATION_REQUIRED 必须置 ValidationRequired")
	}
	if info.IneligibleReason != "VALIDATION_REQUIRED" {
		t.Errorf("reasonCode 错：%q", info.IneligibleReason)
	}
	if !strings.Contains(info.ValidationURL, "accounts.google.com/v3/signin/continue") {
		t.Errorf("验证链接必须逐字取上游值：%q", info.ValidationURL)
	}
	if info.ValidationText != "Verify your account" {
		t.Errorf("链接文案错：%q", info.ValidationText)
	}
	// 档位仍要照常解析：Pro 账号被拦 ≠ 看不出它是 Pro
	if info.PaidTierID != "g1-pro-tier" {
		t.Errorf("paidTier 不该受影响：%q", info.PaidTierID)
	}
}

// TestParseNoIneligibleTiers 没有该数组时不得误判（正常账号不能被当成「要验证」）。
func TestParseNoIneligibleTiers(t *testing.T) {
	body := `{"currentTier":{"id":"free-tier"},"cloudaicompanionProject":"p","paidTier":{"id":"g1-pro-tier"}}`
	info, err := parseCodeAssist([]byte(body))
	if err != nil {
		t.Fatalf("parse 失败：%v", err)
	}
	if info.ValidationRequired {
		t.Fatal("无 ineligibleTiers 时不得判为需验证")
	}
	if admissionHint(info) != "" {
		t.Fatal("无准入问题时不得产生提示")
	}
}

// TestClassifyAdmissionAsAccountFault 403 + 准入文案 ⇒ ErrAccountFault（可换号），不是 ErrServer。
func TestClassifyAdmissionAsAccountFault(t *testing.T) {
	cases := []string{
		`{"error":{"code":403,"message":"The caller does not have permission","status":"PERMISSION_DENIED"}}`,
		`{"error":{"code":403,"reason":"VALIDATION_REQUIRED"}}`,
		`{"error":{"code":403,"message":"Verify your account to continue."}}`,
	}
	for _, body := range cases {
		if got := Classify(403, body); got != provider.ErrAccountFault {
			t.Errorf("应归 account_fault，得 %v（body=%s）", got, body)
		}
	}
	// 反向护栏：真正的 WAF/网关拦截页不得被当成准入问题
	if got := Classify(403, "<html>blocked</html>"); got == provider.ErrAccountFault {
		t.Error("HTML 拦截页不得归 account_fault")
	}
}

// TestWithAdmissionAppendsActionableHint 错误文案必须带上验证链接（用户唯一能行动的线索）。
func TestWithAdmissionAppendsActionableHint(t *testing.T) {
	info, _ := parseCodeAssist([]byte(admissionBody))
	base := &provider.Error{Kind: provider.ErrAccountFault, Status: 403, Msg: "gemini：配额查询失败（HTTP 403）：..."}
	got := withAdmission(base, info)
	if got == nil {
		t.Fatal("不该返回 nil")
	}
	msg := got.Error()
	if !strings.Contains(msg, "accounts.google.com/v3/signin/continue") {
		t.Fatalf("必须带验证链接：%s", msg)
	}
	if !strings.Contains(msg, "Verify your account") {
		t.Fatalf("必须带上游的链接文案：%s", msg)
	}
	var pe *provider.Error
	if !errors.As(got, &pe) || pe.Kind != provider.ErrAccountFault {
		t.Fatal("Kind 必须保持 account_fault（不得被改成别的类别）")
	}
	// 原错误不能被就地修改（*provider.Error 是共享指针的场景）
	if strings.Contains(base.Msg, "signin") {
		t.Fatal("不得就地修改调用方的错误对象")
	}
}

// TestUserResourceDetailSurfacesAdmission 端到端：配额 403 + 准入判定 ⇒ 错误里必须带验证链接。
//
// 这条守的是**调用点**（UserResourceDetail 把 loadCodeAssist 的结论合并进配额错误）。
// 只测 withAdmission 这个纯函数是不够的：调用点忘了调它时，纯函数用例照样全绿
// （与 R132「断言也要被断言」同族的假绿形态）。
func TestUserResourceDetailSurfacesAdmission(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "loadCodeAssist"):
			_, _ = io.WriteString(w, admissionBody)
		case strings.Contains(r.URL.Path, "retrieveUserQuotaSummary"):
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"code":403,"message":"The caller does not have permission","status":"PERMISSION_DENIED"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL)
	_, _, err := c.UserResourceDetail(testAuth())
	if err == nil {
		t.Fatal("配额 403 必须返回错误")
	}
	msg := err.Error()
	if !strings.Contains(msg, "accounts.google.com/v3/signin/continue") {
		t.Fatalf("错误里必须带上游给的验证链接（否则用户只看到「查询失败」）：%s", msg)
	}
	var pe *provider.Error
	if errors.As(err, &pe) && pe.Kind != provider.ErrAccountFault {
		t.Fatalf("应归 account_fault（可换号），得 %v", pe.Kind)
	}
}

// TestAdmissionHintPerReasonCode 三种准入 reasonCode 各自给出可行动的结论。
//
// 三种都是 2026-10-30 在同一账号上实测拿到的原文：
//
//	VALIDATION_REQUIRED（antigravity 指纹 + 验证前）
//	SUBSCRIPTION_REQUIRED（gemini-cli 指纹：403 #3501 "valid license"）
//	UNSUPPORTED_CLIENT（gemini-cli 指纹的 loadCodeAssist：要求迁移 Antigravity）
func TestAdmissionHintPerReasonCode(t *testing.T) {
	cases := []struct {
		reason string
		want   string
	}{
		{"VALIDATION_REQUIRED", "需要先完成 Google 账号验证"},
		{"SUBSCRIPTION_REQUIRED", "没有 Code Assist 的有效订阅"},
		{"UNSUPPORTED_CLIENT", "已弃用该客户端形态"},
	}
	for _, c := range cases {
		got := admissionHint(codeAssistInfo{ValidationRequired: true, IneligibleReason: c.reason})
		if !strings.Contains(got, c.want) {
			t.Errorf("%s 的提示应含 %q，得 %q", c.reason, c.want, got)
		}
	}
}

// TestClassifySubscriptionRequired 403 #3501 也必须归账号级（可换号），不是「上游故障」。
func TestClassifySubscriptionRequired(t *testing.T) {
	body := `{"error":{"code":403,"message":"You do not have a valid license of this product. (#3501)","status":"PERMISSION_DENIED","details":[{"reason":"SUBSCRIPTION_REQUIRED"}]}}`
	if got := Classify(403, body); got != provider.ErrAccountFault {
		t.Fatalf("应归 account_fault，得 %v", got)
	}
	// UNSUPPORTED_CLIENT 同理
	if got := Classify(200, `{"ineligibleTiers":[{"reasonCode":"UNSUPPORTED_CLIENT"}]}`); got != provider.ErrAccountFault {
		t.Logf("200 不需要分类（现状 %v），此处仅记录", got)
	}
}

// TestUnknownAdmission403GetsHint 403 但上游没给 reasonCode 时也要有可行动提示。
//
// 实测形态（2026-10-30，Antigravity 指纹）：ineligibleTiers 已消失（验证通过），
// 配额只回 `403 The caller does not have permission`。此时不能编造原因，
// 但也不能只丢一句 403 —— 用户没有任何线索。
func TestUnknownAdmission403GetsHint(t *testing.T) {
	base := &provider.Error{Kind: provider.ErrAccountFault, Status: 403,
		Msg: `gemini：配额查询失败（HTTP 403）：{ "error": { "code": 403, "message": "The caller does not have permission" } }`}
	got := withAdmission(base, codeAssistInfo{}) // info 里没有任何 reasonCode
	msg := got.Error()
	if !strings.Contains(msg, "没有给出原因码") {
		t.Fatalf("必须如实说「上游没给原因」，不得编造：%s", msg)
	}
	if !strings.Contains(msg, "换一个个人 Google 账号") {
		t.Fatalf("必须给出可行动路径：%s", msg)
	}
	if !strings.Contains(msg, "The caller does not have permission") {
		t.Fatalf("原始错误必须保留：%s", msg)
	}
}

// TestRegionBlockedFallsThroughToNextEndpoint 地区限制必须换端点重试，不得当场透传。
//
// 现场（2026-10-30）：同一账号、同一 token、同一出口 IP（香港），配额端点（sandbox）
// 回 200 而推理端点（daily）回 `400 User location is not supported for the API use.`。
// 旧实现把 400 当终态直接透传 ⇒ 用户拿到一句无从下手的 400，而真正可用的端点就在列表里。
func TestRegionBlockedFallsThroughToNextEndpoint(t *testing.T) {
	// 只对推理请求计数：loadCodeAssist 也会打进来，用总请求数当判据会被它带偏
	// （第一版就是这么写错的 —— 判据错位会让用例在「没换端点」时也看着合理）。
	streamCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "loadCodeAssist") {
			_, _ = io.WriteString(w, `{"cloudaicompanionProject":"p1","currentTier":{"id":"free-tier"}}`)
			return
		}
		streamCalls++
		// 第一个端点报地区限制；换到备用端点后正常
		if streamCalls == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":400,"message":"User location is not supported for the API use.","status":"FAILED_PRECONDITION"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]}}]}}\n\n")
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL)
	// 用两个不同 base 模拟双端点：同一 test server 上靠 hit 顺序区分
	c.Base = ""
	c.Daily = srv.URL
	c.Sandbox = srv.URL
	body := []byte(`{"model":"gemini-3.8-flash","messages":[{"role":"user","content":"hi"}]}`)
	_, status, raw, err := c.ChatStream(testAuth(), body)
	if err != nil {
		t.Fatalf("不该返回传输层错误：%v", err)
	}
	if status == http.StatusBadRequest {
		t.Fatalf("地区限制必须换端点重试，实际仍返回 400：%s", string(raw))
	}
	if streamCalls < 2 {
		t.Fatalf("必须真的再打一次备用端点，实际推理请求数=%d", streamCalls)
	}
}

// TestIsRegionBlockedOnlyMatchesObservedText 判据只认实证文案，不外扩。
func TestIsRegionBlockedOnlyMatchesObservedText(t *testing.T) {
	if !isRegionBlocked(400, []byte(`{"error":{"message":"User location is not supported for the API use."}}`)) {
		t.Error("实证文案必须命中")
	}
	// 反向护栏：别的 FAILED_PRECONDITION 不得被当成地区问题（否则会无谓换端点、掩盖真实错误）
	if isRegionBlocked(400, []byte(`{"error":{"status":"FAILED_PRECONDITION","message":"other reason"}}`)) {
		t.Error("不得按 status 外扩")
	}
	if isRegionBlocked(500, []byte(`user location is not supported`)) {
		t.Error("只认 400")
	}
}

// TestIdentityHeadersMatchOfficialIDE 出站身份头必须与官方 Antigravity IDE 形态**逐字一致**。
//
// 反编译依据（opencodex 对 2.5.5 arm64 的取证）：`SetHTTPHeaders` 的 IDE / Standalone /
// Stubby 三个形态都是 16 字节直接 ret —— **一个头都不加**；只有 CLI / Hub 会加
// `X-Goog-User-Project`。也就是说官方 IDE 出站只有 Authorization + Content-Type + UA。
//
// 我们此前多发了 `x-client-name` / `x-client-version` / `x-machine-id` / `x-vscode-sessionid`
// 四个上游根本不发的头 —— 那是最容易被识别成「非官方客户端」的指纹（同一类问题已让我们
// 吃过 UNSUPPORTED_CLIENT 与 403）。这条用例守的就是「别再手贱加回去」。
func TestIdentityHeadersMatchOfficialIDE(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/x", nil)
	identityHeaders(req, "ya29.T")

	for _, h := range []string{"Authorization", "Content-Type", "User-Agent"} {
		if req.Header.Get(h) == "" {
			t.Errorf("必须发送 %s", h)
		}
	}
	if got := req.Header.Get("User-Agent"); got != IdentityUserAgent {
		t.Errorf("UA 必须与常量一致，得 %q", got)
	}
	// 反向护栏：这些官方 IDE 形态**不发**的头，一个都不能出现。
	for _, h := range []string{
		"x-client-name", "x-client-version", "x-machine-id", "x-vscode-sessionid",
		"X-Goog-User-Project", "x-goog-api-client",
	} {
		if v := req.Header.Get(h); v != "" {
			t.Errorf("官方 IDE 形态不发 %s，却设成了 %q", h, v)
		}
	}
}
