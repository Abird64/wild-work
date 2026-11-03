package app

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestShortErrKeepsActionableTail 面板错误文案不得切掉「可行动线索」，也不得切坏中文。
//
// 现场（2026-10-30）：gemini 配额 403 的真实文案里，**唯一能行动的那句**
// （「账号需要先完成 Google 账号验证 + 链接」）排在末尾；旧实现 `s[:120]` 按字节截断，
// 既把链接整段切掉，又会把一个中文字符切成两半。用户看到的是「配额查询失败（HTTP 403）：{ "error"…」
// —— 完全看不出该去验证账号。
func TestShortErrKeepsActionableTail(t *testing.T) {
	long := "上游判定该账号需要先完成 Google 账号验证（code=VALIDATION_REQUIRED）：" +
		"Verify your account → https://accounts.google.com/v3/signin/continue?plt=X"
	if got := shortErr(errors.New(long)); !strings.Contains(got, "accounts.google.com") {
		t.Fatalf("验证链接不得被截掉：%s", got)
	}

	// 中文不能被切成半个 rune
	cn := strings.Repeat("账号验证需要", 40)
	got := shortErr(errors.New(cn))
	if !utf8.ValidString(got) {
		t.Fatalf("截断后必须是合法 UTF-8：%q", got)
	}
	// 超长时仍要有上限（不能无脑放行整段上游 JSON）
	huge := strings.Repeat("x", 5000)
	if n := len([]rune(shortErr(errors.New(huge)))); n > 320 {
		t.Fatalf("仍须有上限，得 %d rune", n)
	}
}

// TestShortErrNeverBreaksURL 上游给的验证链接必须完整可点。
//
// 现场（2026-10-30）：该链接实测 400+ 字符，按 300 固定切会让用户拿到
// 「https://accounts.google.com/v3/signin/continue?plt=AKgnsbuBBI3l…」这种半截地址。
func TestShortErrNeverBreaksURL(t *testing.T) {
	url := "https://accounts.google.com/v3/signin/continue?plt=AKgnsbsrG0H&continue=https%3A%2F%2Fdevelopers.google.com%2Fgemini-code-assist%2Fauth%2Fauth_success_gemini&sarp=1&scc=1&flowName=GlifWebSignIn"
	msg := "上游判定该账号需要先完成 Google 账号验证（code=VALIDATION_REQUIRED）" +
		strings.Repeat("：补充说明文字", 20) + "：Verify your account → " + url
	got := shortErr(errors.New(msg))
	if !strings.Contains(got, url) {
		t.Fatalf("完整 URL 必须保留，得：%s", got)
	}
}
