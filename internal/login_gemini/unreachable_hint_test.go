package login_gemini

import (
	"strings"
	"testing"

	"wild-work/internal/backuplink"
)

// TestUnreachableHintNamesCurrentExit 「网络不可达」的文案必须点名**当前出口**。
//
// 现场依据（2026-10-30）：用户按提示配了备用通道，但填的是 `socks5://127.0.0.1:1080`
// （该端口没在监听），而机器上可用的是 `127.0.0.1:7890`。原文案只说「本机网络到
// Google 不可达」，用户看着面板「● 已生效」的绿点毫无线索 ⇒ 只能反复重试。
// 判据：配了就必须报出实际生效地址；没配就必须明说「直连」并给出可抄的示例。
func TestUnreachableHintNamesCurrentExit(t *testing.T) {
	t.Cleanup(func() { _ = backuplink.Set("") })

	_ = backuplink.Set("")
	hint := unreachableHint()
	if !strings.Contains(hint, "直连") {
		t.Fatalf("未配通道时应明说「直连」，得 %q", hint)
	}
	if !strings.Contains(hint, "备用上游通道") {
		t.Fatalf("应指出该去哪里配，得 %q", hint)
	}

	if err := backuplink.Set("socks5://127.0.0.1:1080"); err != nil {
		t.Fatal(err)
	}
	hint = unreachableHint()
	if !strings.Contains(hint, "socks5://127.0.0.1:1080") {
		t.Fatalf("已配通道时必须报出实际出口，得 %q", hint)
	}
	if strings.Contains(hint, "直连") {
		t.Fatalf("已配通道时不得说「直连」，得 %q", hint)
	}
}

// TestNetworkErrorCarriesHint 真实报错路径（networkError）也要带上提示。
func TestNetworkErrorCarriesHint(t *testing.T) {
	t.Cleanup(func() { _ = backuplink.Set("") })
	_ = backuplink.Set("http://127.0.0.1:7890")

	err := networkError("oauth2.googleapis.com", errTimeout{})
	msg := err.Error()
	if !strings.Contains(msg, "http://127.0.0.1:7890") {
		t.Fatalf("报错文案应点名当前出口，得 %q", msg)
	}
	if !strings.Contains(msg, "oauth2.googleapis.com") {
		t.Fatalf("报错文案应保留出错的 host，得 %q", msg)
	}
}

type errTimeout struct{}

func (errTimeout) Error() string { return "i/o timeout" }
