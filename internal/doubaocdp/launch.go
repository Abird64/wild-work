// launch.go 按需把豆包客户端以「带调试端口」的方式拉起。
//
// # 为什么需要它（2026-10-30 用户实测）
//
// 本渠道所有请求都在豆包客户端的页面上下文里发，靠前端 JS hook 注入 a_bogus 签名
// （纯算法复现不存在，见 cdp.go 头注释与 docs/豆包渠道调研.md）。这要求客户端**启动时**
// 带上 `--remote-debugging-port=30813`。
//
// 但用户的真实情况是：豆包设置成了**开机自启**，那一路启动不带调试端口 ——
// 于是面板每次连接都失败，提示「需要以 --remote-debugging-port 启动」，
// 而用户并不知道该怎么让它带上这个参数（双击图标是带不上的）。
//
// 本文件把这个动作变成工具自己的一步：探测端口 → 没有就以调试模式重启 → 等端口就绪。
// **只在用户显式点「连接」时执行**（不做后台偷偷重启别人的应用）。
//
// # 复用的实测事实
//
// 客户端路径、内核版本、`aha_doctor` 守护进程会妨碍清理、Stop-Process 吞掉失败等
// 全部来自 docs/豆包渠道调研.md §2.1（2026-10-26 实测），此处按同样顺序实现。
package doubaocdp

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ClientExeNames 豆包客户端主程序的可能文件名（按优先级）。
var ClientExeNames = []string{"Doubao.exe", "豆包.exe"}

// defaultInstallRoots 安装目录候选（相对各盘符/用户目录展开）。
//
// 不写死盘符与厂商目录：实测本机在 `D:\application\lnstallation\豆包\Doubao\app\`，
// 但别的机器装在 Program Files 或 AppData 也很常见。逐个探测，找到即可。
var defaultInstallRoots = []string{
	`Program Files\Doubao\Doubao\app`,
	`Program Files (x86)\Doubao\Doubao\app`,
	`Doubao\Doubao\app`,
	`AppData\Local\Doubao\app`,
	`AppData\Local\Programs\Doubao\app`,
}

// EnsureDebugPort 确保豆包客户端在 listenAddr 上提供调试端口；必要时以调试模式重启它。
//
// 返回 (是否执行了重启, 错误)。已有端口在听时**不重启**（幂等，可安全重复调用）。
//
// 只在 Windows 上有实现：客户端本身就是 Windows 应用；其它平台直接返回错误，
// 让上层给出「本渠道需要 Windows 客户端」这类可行动提示，而不是静默失败。
func EnsureDebugPort(ctx context.Context, listenAddr string) (restarted bool, err error) {
	if runtime.GOOS != "windows" {
		return false, fmt.Errorf("%w: 本渠道依赖 Windows 版豆包客户端", ErrClientUnavailable)
	}
	if portReady(listenAddr, 800*time.Millisecond) {
		return false, nil // 已经在听：什么都不做
	}
	exe, ferr := findClientExe()
	if ferr != nil {
		return false, ferr
	}
	if err := restartWithDebugPort(ctx, exe, portOf(listenAddr)); err != nil {
		return false, err
	}
	// 等端口就绪：客户端启动到能响应 CDP 通常 3~8 秒（实测），给到 25 秒。
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		if portReady(listenAddr, 500*time.Millisecond) {
			return true, nil
		}
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return true, fmt.Errorf("%w: 已按调试模式重启，但 %s 在 25 秒内没就绪", ErrClientUnavailable, listenAddr)
}

// portReady 探测调试端口是否可连。
func portReady(addr string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// portOf 从 "127.0.0.1:30813" 取端口号。
func portOf(addr string) int {
	if _, p, err := net.SplitHostPort(addr); err == nil {
		var n int
		if _, err := fmt.Sscanf(p, "%d", &n); err == nil {
			return n
		}
	}
	return DefaultDebugPort
}

// findClientExe 找到豆包客户端可执行文件。
//
// 顺序：① 已在运行的进程的 ExecutablePath（最准，用户装哪就是哪）；
// ② 常见安装目录；③ 返回可操作错误（告诉用户去设置里填路径）。
func findClientExe() (string, error) {
	// ① 从运行中的进程取真实路径。注意：**不能用进程名匹配中文**（编码会坏），
	// 故用 PowerShell 直接回 ExecutablePath，由它处理名称匹配。
	if p := exeFromRunningProcess(); p != "" {
		return p, nil
	}
	// ② 常见目录
	drives := []string{`C:`, `D:`, `E:`}
	for _, d := range drives {
		for _, rel := range defaultInstallRoots {
			for _, name := range ClientExeNames {
				cand := filepath.Join(d+`\`, rel, name)
				if fileExists(cand) {
					return cand, nil
				}
			}
		}
	}
	return "", fmt.Errorf("%w: 没找到豆包客户端可执行文件 —— 请先手动打开豆包，或确认安装路径", ErrClientUnavailable)
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// exeFromRunningProcess 从正在运行的豆包进程取可执行文件路径。
//
// ⚠️ **必须强制 UTF-8 输出**（脚本里的 `[Console]::OutputEncoding`）：中文版 Windows 的
// 控制台默认用 OEM 代码页（GBK），路径里的中文目录名（本机实测 `D:\…\豆包\…`）
// 会以 GBK 字节写出，Go 按 UTF-8 读就成了 `����` ⇒ `fileExists` 判否 ⇒
// 报「没找到豆包客户端」，而路径其实找到了（2026-10-30 实机踩到）。
//
// 用 -EncodedCommand 传 Powershell：调研 §2.1 实测，脚本内的中文路径字面量在
// WSL → powershell 调用下会被按 ANSI 解码破坏；编码成 base64(UTF-16LE) 可绕开。
func exeFromRunningProcess() string {
	// ⚠️ 必须**排除** aha_doctor 等辅助进程：它们同在一个 *Doubao* 目录下，
	// 「取第一个匹配」会拿到 `aha_doctor\aha_doctor.exe`（2026-10-30 实机踩到，
	// 结果报「没找到豆包客户端」——路径其实找到了，只是选错了 exe）。
	// 判据：文件名必须是主程序（Doubao.exe / 豆包.exe），而不是路径含 Doubao。
	// 关键：`[Console]::OutputEncoding` 必须设 UTF-8（见函数头注释）；
	// 且**不要用 `exit 0`** —— 在 -EncodedCommand 下它会连同 stdout 一起提前结束，
	// 让后续语句永远不执行（实机表现为「明明有进程却报找不到」）。
	const script = `$ErrorActionPreference='SilentlyContinue'
[Console]::OutputEncoding = [Text.Encoding]::UTF8
$names = @('Doubao.exe','豆包.exe')
$p = Get-Process | Where-Object {
  $_.Path -and ($names -contains [IO.Path]::GetFileName($_.Path))
} | Select-Object -First 1
if ($p) { [Console]::Out.Write($p.Path) } else { [Console]::Out.Write('') }`
	out, err := runPSEncoded(script, 8*time.Second)
	if err != nil {
		return ""
	}
	p := strings.TrimSpace(out)
	if p == "" || !fileExists(p) {
		return ""
	}
	return p
}

// restartWithDebugPort 停掉现有客户端（含 aha_doctor 守护），再以调试端口启动。
//
// 顺序照抄调研 §2.1 的实测结论：
//  1. 先停 aha_doctor —— 否则它会把主进程拉回来；
//  2. 再停所有豆包进程，**校验真的停了**（Stop-Process 会静默吞失败）；
//  3. 带 --remote-debugging-port 启动。
func restartWithDebugPort(ctx context.Context, exe string, port int) error {
	// ① 只让 PowerShell 负责「杀」。
	//
	// 为什么启动不交给 PowerShell（2026-10-30 实机踩到）：`Start-Process` 从**无控制台的
	// Go 进程**里拉起 Chromium 应用会返回 `exit status 0xc0000142`（STATUS_DLL_INIT_FAILED），
	// 豆包起不来。改由 Go 直接 exec 并**不等待**（Start 后立即返回），让它作为一个独立
	// 进程活着 —— 这也是 daemon 启动其它 GUI 程序的标准做法。
	killScript := fmt.Sprintf(`$ErrorActionPreference='SilentlyContinue'
[Console]::OutputEncoding = [Text.Encoding]::UTF8
$target = '%s'
# ① 守护进程先停（它会把主进程拉回来）
Get-Process -Name 'aha_doctor' -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
# ② 停主进程：按 ExecutablePath 精确匹配（不能按进程名，中文名会被编码破坏）
Get-Process | Where-Object { $_.Path -and $_.Path -eq $target } | Stop-Process -Force -ErrorAction SilentlyContinue
Start-Sleep -Milliseconds 1500
$left = @(Get-Process | Where-Object { $_.Path -and $_.Path -eq $target })
if ($left.Count -gt 0) { [Console]::Out.Write('STILL_RUNNING') } else { [Console]::Out.Write('KILLED') }`,
		escapePS(exe))

	out, err := runPSEncoded(killScript, 25*time.Second)
	if err != nil {
		return fmt.Errorf("%w: 停止旧客户端失败: %v", ErrClientUnavailable, err)
	}
	if strings.TrimSpace(out) != "KILLED" {
		return fmt.Errorf("%w: 豆包客户端没能停掉（可能被守护进程拉起），请手动退出豆包后重试", ErrClientUnavailable)
	}

	// ② 由 Go 自己带调试端口启动（不等待，让它独立存活）。
	cmd := exec.Command(exe, fmt.Sprintf("--remote-debugging-port=%d", port))
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%w: 以调试模式启动客户端失败: %v", ErrClientUnavailable, err)
	}
	// 立刻把子进程交出去：不等它退出，也不能在函数返回时把它杀掉。
	go func() { _ = cmd.Wait() }()
	return nil
}

// stripCLIXML 剥掉 PowerShell 混进 stdout 的 CLIXML 进度流，只保留纯文本载荷。
//
// 形态：`#< CLIXML\n<Objs Version="1.1.0.1" ...>...</Objs>`，且**总在载荷之前**
// （实机：`#< CLIXML…</Objs>D:\…\Doubao.exe`），故要「整段切除」而不是前缀截断。
func stripCLIXML(s string) string {
	for {
		i := strings.Index(s, "#< CLIXML")
		if i < 0 {
			break
		}
		if j := strings.Index(s[i:], "</Objs>"); j >= 0 {
			s = s[:i] + s[i+j+len("</Objs>"):]
			continue
		}
		s = s[:i]
		break
	}
	if i := strings.Index(s, "<Objs "); i >= 0 {
		if j := strings.Index(s[i:], "</Objs>"); j >= 0 {
			s = s[:i] + s[i+j+len("</Objs>"):]
		} else {
			s = s[:i]
		}
	}
	return strings.TrimSpace(s)
}

// escapePS 转义单引号，供 PowerShell 单引号字符串字面量使用。
func escapePS(s string) string { return strings.ReplaceAll(s, "'", "''") }

// runPSEncoded 以 base64(UTF-16LE) 方式执行 PowerShell 脚本，规避中文编码问题。
func runPSEncoded(script string, timeout time.Duration) (string, error) {
	enc := encodePSCommand(script)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-EncodedCommand", enc)
	// ⚠️ 必须用 Output（stdout）而不是 CombinedOutput：PowerShell 的进度流会写 stderr，
	// 而 -NonInteractive 下 stderr 是 **CLIXML**（形如 `#< CLIXML\n<Objs …>`）。
	// 两者混在一起时，路径后面会粘一大段 XML（2026-10-30 实机踩到：TrimSpace 后
	// 得到的不是合法路径 ⇒ fileExists 失败 ⇒ 报「没找到豆包客户端」）。
	out, err := cmd.Output()
	return stripCLIXML(string(out)), err
}

// encodePSCommand 把脚本编成 PowerShell -EncodedCommand 需要的 base64(UTF-16LE)。
func encodePSCommand(script string) string {
	// UTF-16LE：每个 ASCII/中文字符按 UTF-16 码元小端写入。
	u := utf16le(script)
	return base64Std(u)
}

// utf16le 手工编码，避免为了一个小工具引入 unicode/utf16 之外的分支判断。
func utf16le(s string) []byte {
	var b []byte
	for _, r := range s {
		if r > 0xFFFF {
			// 代理对
			r -= 0x10000
			hi := 0xD800 + (r >> 10)
			lo := 0xDC00 + (r & 0x3FF)
			b = append(b, byte(hi), byte(hi>>8), byte(lo), byte(lo>>8))
			continue
		}
		b = append(b, byte(r), byte(r>>8))
	}
	return b
}

// base64Std 标准 base64（PowerShell 接受的就是标准字母表）。
func base64Std(b []byte) string {
	const tbl = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var sb strings.Builder
	for i := 0; i < len(b); i += 3 {
		var n uint32
		rem := len(b) - i
		n = uint32(b[i]) << 16
		if rem > 1 {
			n |= uint32(b[i+1]) << 8
		}
		if rem > 2 {
			n |= uint32(b[i+2])
		}
		sb.WriteByte(tbl[(n>>18)&0x3F])
		sb.WriteByte(tbl[(n>>12)&0x3F])
		if rem > 1 {
			sb.WriteByte(tbl[(n>>6)&0x3F])
		} else {
			sb.WriteByte('=')
		}
		if rem > 2 {
			sb.WriteByte(tbl[n&0x3F])
		} else {
			sb.WriteByte('=')
		}
	}
	return sb.String()
}
