package doubaocdp

import (
	"encoding/base64"
	"runtime"
	"testing"
	"unicode/utf16"
)

// decodePSCommand 测试辅助：把 base64(UTF-16LE) 还原成字符串，验证编码方向正确。
func decodePSCommand(t *testing.T, enc string) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("不是合法 base64：%v", err)
	}
	if len(b)%2 != 0 {
		t.Fatalf("UTF-16LE 字节数必须是偶数，得 %d", len(b))
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = uint16(b[i*2]) | uint16(b[i*2+1])<<8
	}
	return string(utf16.Decode(u))
}

// isWindows 平台判定（Go 的 runtime 在测试里也是真值）。
func isWindows() bool { return runtime.GOOS == "windows" }
