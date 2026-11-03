package login_gemini

import (
	"strings"
	"testing"
)

// TestTransientKeptForTimeoutMessage 传输层失败必须被记住，供超时文案如实报出。
func TestTransientKeptForTimeoutMessage(t *testing.T) {
	takeTransient() // 清干净

	setTransient(errDial{})
	got := takeTransient()
	if !strings.Contains(got, "i/o timeout") {
		t.Fatalf("应记录失败原因，得 %q", got)
	}
	if rest := takeTransient(); rest != "" {
		t.Fatalf("取值应清空，得 %q", rest)
	}
}

// TestTransientNilIgnored nil 不得覆盖已记录的原因（抖动后又成功不该抹掉线索）。
func TestTransientNilIgnored(t *testing.T) {
	takeTransient()
	setTransient(errDial{})
	setTransient(nil)
	if got := takeTransient(); got == "" {
		t.Fatal("nil 不应清空已有原因")
	}
}

type errDial struct{}

func (errDial) Error() string { return "dial tcp: i/o timeout" }
