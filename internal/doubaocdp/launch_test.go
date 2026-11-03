// launch_test.go 以调试模式拉起豆包客户端的辅助函数回归（2026-10-30 用户实测驱动）。
//
// 现场：用户的豆包是**开机自启**，那一路不带 --remote-debugging-port，双击图标也带不上，
// 于是本渠道每次连接都失败，而错误只说了「需要以 --remote-debugging-port 启动」——
// 用户无法靠自己解决。EnsureDebugPort 把那次重启变成工具的一个动作。
//
// 这里只测**纯函数部分**（端口解析、PowerShell 编码、就绪探测）：真正重启需要 Windows
// 上有豆包客户端，属实机验证（见决议 R137 的实测记录），不在单测里跑。
package doubaocdp

import (
	"net"
	"strings"
	"testing"
	"time"
)

// TestPortOf 端口解析：畸形输入必须退回默认端口，不能返回 0（0 会让客户端起在随机端口）。
func TestPortOf(t *testing.T) {
	cases := []struct {
		addr string
		want int
	}{
		{"127.0.0.1:30813", 30813},
		{"localhost:1234", 1234},
		{"127.0.0.1", DefaultDebugPort},
		{"", DefaultDebugPort},
		{"坏地址", DefaultDebugPort},
	}
	for _, c := range cases {
		if got := portOf(c.addr); got != c.want {
			t.Errorf("portOf(%q) = %d，期望 %d", c.addr, got, c.want)
		}
	}
}

// TestEncodePSCommandRoundTrip -EncodedCommand 必须是 base64(UTF-16LE)：
// 编码错的话 PowerShell 会直接报「命令行参数无效」，而中文路径还会被 ANSI 解码破坏
// （调研 §2.1 实测），故这条不是形式检查。
func TestEncodePSCommandRoundTrip(t *testing.T) {
	// 含中文（豆包安装目录常见）与单引号（脚本里要转义的那种）
	src := "Get-Process | Where-Object { $_.Path -like '*豆包*' }"
	enc := encodePSCommand(src)
	if enc == "" {
		t.Fatal("编码结果为空")
	}
	raw := decodePSCommand(t, enc)
	if raw != src {
		t.Fatalf("往返不一致：\n原文=%q\n回读=%q", src, raw)
	}
	// 中文字符必须占 2 字节（UTF-16），编码后再解码不能变成乱码
	if !strings.Contains(raw, "豆包") {
		t.Fatalf("中文被破坏：%q", raw)
	}
}

// TestPortReadyDetectsListener 就绪探测必须能区分「有人在听」与「没人听」。
func TestPortReadyDetectsListener(t *testing.T) {
	if portReady("127.0.0.1:1", 300*time.Millisecond) {
		t.Skip("端口 1 上有服务，环境不适用")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if !portReady(ln.Addr().String(), time.Second) {
		t.Fatalf("监听中的端口 %s 应判定为就绪", ln.Addr())
	}
}

// TestEnsureDebugPortNonWindows 非 Windows 平台必须给出可行动错误，而不是静默失败。
func TestEnsureDebugPortNonWindows(t *testing.T) {
	if isWindows() {
		t.Skip("仅在非 Windows 上验证平台守卫")
	}
	if _, err := EnsureDebugPort(t.Context(), "127.0.0.1:30813"); err == nil {
		t.Fatal("非 Windows 必须报错")
	}
}

// TestStripCLIXML PowerShell 的进度流会以 CLIXML 混进 stdout，必须被剥掉。
//
// 实机现象（2026-10-30）：脚本输出 `#< CLIXML\n<Objs Version="1.1.0.1" ...>` 加上真实路径，
// TrimSpace 之后得到的不是合法路径 ⇒ fileExists 失败 ⇒ 报「没找到豆包客户端」，
// 而路径其实找到了。没有这条，那个 bug 会以「探测不到安装位置」的形式反复出现。
func TestStripCLIXML(t *testing.T) {
	const cleaned = `D:\app\Doubao\Doubao.exe`
	cases := []string{
		"#< CLIXML\n<Objs Version=\"1.1.0.1\"><Obj S=\"progress\"/></Objs>" + cleaned,
		cleaned + "#< CLIXML\n<Objs Version=\"1.1.0.1\"/>",
		cleaned,
	}
	for _, in := range cases {
		if got := stripCLIXML(in); !strings.Contains(got, "Doubao.exe") {
			t.Errorf("剥 CLIXML 后应保留路径，得 %q", got)
		}
	}
	// 纯 CLIXML（没有载荷）→ 空串，调用方据此判定「没输出」
	if got := stripCLIXML("#< CLIXML\n<Objs Version=\"1.1.0.1\" />"); strings.TrimSpace(got) != "" {
		t.Errorf("纯 CLIXML 应剥成空，得 %q", got)
	}
}
