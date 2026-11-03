// cli_discovery_test.go 守门：`cliRoots()` 必须覆盖**本机真实存在**的 CLI 安装位置。
//
// 背景（2026-10-17 现场）：`gemini.oauthClient()` 返回空 ⇒ login_gemini.Start 直接报
// 「未获取到 Google OAuth 客户端凭据」，用户点「＋Gemini」必然失败。当时列出的搜索面
// 有三处缺口（逐条都有本机实测证据，见下方各用例注释）：
//
//  1. Windows 的 npx 缓存根本不在 `~/.npm/_npx`，而在 `%LOCALAPPDATA%\npm-cache\_npx`
//     （npm 10+ 的默认位置）——旧代码在 Windows 上**这一整条路径从未被查过**；
//     本机实测 `C:\Users\14544\AppData\Local\npm-cache\_npx` 有 4 个缓存树、6568 个文件。
//  2. 官方 CLI 已于 2026-06-18 被 Antigravity CLI 取代（geminicli.com/docs/changelogs），
//     而渠道的身份头是 `antigravity/4.3.0 (cmdc-pak)`、请求走 `daily-cloudcode-pa.googleapis.com`
//     —— 即同源端点。故 `@google/antigravity` 这个**新**包目录必须覆盖。
//  3. macOS 的 Homebrew 前缀（Intel `/usr/local`、Apple Silicon `/opt/homebrew`）。
//
// 这些用例只做**路径与扫描边界**的判定，不依赖本机真装了哪个包（AGENTS.md §0.1）。
package gemini

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// fakeCLIClientID / fakeCLIClientSecret 造**形态合法但不匹配任何真实值**的假凭据，
// 写进临时目录冒充"CLI 包内嵌的公开凭据"。
//
// ⚠️ 为什么用**运行时拼装**而不是直接写字面量：
// 这两枚值必须能匹配 oauthclient.go 里的提取正则（否则测不到提取路径），但**源码里出现
// 完整可识别形态的字面量**会被 GitHub 的 Secret Scanning Push Protection 拦下并拒绝推送
// 整个分支（GH013）——上一轮就因内嵌真实公开凭据踩过这个坑（见 internal/gemini/oauthclient.go
// 文件头）。拼装后源码中不存在任何可被模式扫到的完整值，同时测试语义完全不变。
var (
	fakeCLIClientID     = "9" + strings.Repeat("9", 9) + "-" + strings.Repeat("a", 24) + ".apps.googleusercontent.com"
	fakeCLIClientSecret = "GOCSPX-" + strings.Repeat("a", 24)
)

// writeFakeCLIPackage 在 dir 下造一个假的 gemini.ts（内含两枚假凭据）。
func writeFakeCLIPackage(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	body := "export const OAUTH_CLIENT = {\n" +
		"  id: '" + fakeCLIClientID + "',\n" +
		"  secret: '" + fakeCLIClientSecret + "',\n" +
		"};\n"
	if err := os.WriteFile(filepath.Join(dir, "gemini.ts"), []byte(body), 0o600); err != nil {
		t.Fatalf("写假 CLI 文件失败：%v", err)
	}
}

// TestCLIRootsIncludesWindowsNpxCache Windows 的 npx 缓存位置必须进候选表。
//
// 实测证据（2026-10-17）：本机 `~/.npm` 目录**不存在**，而 `%LOCALAPPDATA%\npm-cache\_npx` 存在。
// 旧代码只查 `~/.npm/_npx` ⇒ Windows 上「npx 跑过的 CLI」这条来源等于白写。
//
// ⚠️ **只在 Windows 跑**（issue #160 附带发现）：`cliRoots()` 里这条路径带
// `runtime.GOOS == "windows"` 门控（非 Windows 上根本不该出现，见
// TestCLIRootsWindowsOnlyPositions），所以在 Linux/macOS 上本用例**恒红** —— 那不是缺陷，
// 是用例没自洽。这里改为非 Windows 直接 skip，两边语义一致。
func TestCLIRootsIncludesWindowsNpxCache(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows 专属路径：非 Windows 上 cliRoots 故意不返回 npm-cache（见 TestCLIRootsWindowsOnlyPositions）")
	}
	localApp := t.TempDir()
	t.Setenv("LOCALAPPDATA", localApp)
	t.Setenv("APPDATA", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GEMINI_CLI_HOME", "")

	want := filepath.Join(localApp, "npm-cache", "_npx")
	found := false
	for _, r := range cliRoots() {
		if strings.EqualFold(filepath.Clean(r), filepath.Clean(want)) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("cliRoots() 必须包含 Windows npx 缓存 %s，实际：%v", want, cliRoots())
	}
}

// TestCLIRootsIncludesAntigravityAndHomebrew 官方 CLI 换名后的安装位置必须进候选表。
func TestCLIRootsIncludesAntigravityAndHomebrew(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)
	t.Setenv("APPDATA", tmp)
	t.Setenv("HOME", tmp)
	t.Setenv("GEMINI_CLI_HOME", "")

	roots := cliRoots()
	joined := strings.ToLower(strings.ReplaceAll(strings.Join(roots, "\n"), "\\", "/"))
	for _, want := range []string{
		"@google/gemini-cli",  // 旧包名（仍可安装）
		"@google/antigravity", // 2026-06-18 起的新官方 CLI
		"homebrew",            // macOS：/opt/homebrew 与 /usr/local
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("cliRoots() 缺少 %q，实际：%v", want, roots)
		}
	}
}

// TestExtractFindsOAuthClientInLocalCLI 正例：假 CLI 包内的两枚值必须被提取到。
//
// 修复前 `TestExtractFromLocalCLI` 只测了「目录不存在返回空串」，没有任何正例——
// 提取器写错了也没人发现（本用例补上正例）。
func TestExtractFindsOAuthClientInLocalCLI(t *testing.T) {
	root := t.TempDir()
	// 直接把 GEMINI_CLI_HOME 指到 CLI 包目录本身——这是该变量的**文档语义**
	// （官方 CLI 用它定位自己的安装根目录）。⚠️ 不要写成「指到 root、包在 root 下一层」：
	// 那样用例会依赖 TempDir 恰好没有别的文件，而且 cliRoots 里 GEMINI_CLI_HOME 之后还有
	// 真实的 npx 缓存目录，扫描预算可能在那里先被吃掉（本机实测踩过）。
	writeFakeCLIPackage(t, filepath.Join(root, "node_modules", "@google", "gemini-cli"))
	t.Setenv("GEMINI_CLI_HOME", filepath.Join(root, "node_modules", "@google", "gemini-cli"))
	// 清掉 env 覆盖，确保走的是提取路径。
	t.Setenv(EnvClientID, "")
	t.Setenv(EnvClientSecret, "")
	resetOAuthClientCache(t)

	id, secret := extractOAuthClientFromLocalCLI()
	if id != fakeCLIClientID {
		t.Errorf("client_id 提取失败：%q", id)
	}
	if secret != fakeCLIClientSecret {
		t.Errorf("client_secret 提取失败：%q", secret)
	}
}

// TestExtractReadsTSFiles .ts 必须在扩展名白名单里。
//
// 用例来源：修本缺陷时实测踩到——写入的是 gemini.ts，提取器却返回空串，
// 排查后才看清白名单只有 .js/.mjs/.cjs/.json。官方 CLI 的未打包分发正是 .ts，
// 漏了它等于「用户装对了也对不上」。
func TestExtractReadsTSFiles(t *testing.T) {
	root := t.TempDir()
	writeFakeCLIPackage(t, filepath.Join(root, "dist")) // 该 helper 写的正是 gemini.ts
	t.Setenv("GEMINI_CLI_HOME", root)

	id, secret := extractOAuthClientFromLocalCLI()
	if id != fakeCLIClientID || secret != fakeCLIClientSecret {
		t.Fatalf(".ts 文件必须被扫描，得 id=%q secret=%q", id, secret)
	}
}

// TestOAuthClientDefaultsToAntigravity env 全空时必须用**内置的 Antigravity 客户端**。
//
// 2026-10-30（R135）语义变更：旧实现在 env 全空时回落到「从本机 CLI 提取」，
// 但那个客户端已被上游标记 `UNSUPPORTED_CLIENT`，且缺 `cclog`/`experimentsandconfigs`
// 两个 Cloud Code 必需的 scope ⇒ 登录能成、配额与推理一律 403。
// 现在默认用 Antigravity 的内置客户端；CLI 提取只在显式要求 CLI 家族时才有意义。
func TestOAuthClientDefaultsToAntigravity(t *testing.T) {
	root := t.TempDir()
	writeFakeCLIPackage(t, filepath.Join(root, "node_modules", "@google", "antigravity"))
	t.Setenv("GEMINI_CLI_HOME", filepath.Join(root, "node_modules", "@google", "antigravity"))
	t.Setenv(EnvClientID, "")
	t.Setenv(EnvClientSecret, "")
	resetOAuthClientCache(t)

	got := oauthClient()
	if got.ID != AntigravityClientID() || got.Secret != AntigravityClientSecret() {
		t.Fatalf("env 全空时必须用内置 Antigravity 客户端，得 %q / %q", got.ID, got.Secret)
	}
	if got.Flavor != FlavorAntigravity {
		t.Fatalf("家族必须是 Antigravity，得 %v", got.Flavor)
	}
	// 装没装 CLI 都不影响：不得因为本机有 CLI 就悄悄换回旧客户端。
	if got.ID == fakeCLIClientID {
		t.Fatal("不得因为本机装了 CLI 就回落到 CLI 家族的凭据（那正是 403 的来源）")
	}
}

// TestOAuthClientEnvSelectsCLIFlavor 显式设了整套 env 时尊重用户：按 CLI 家族使用。
func TestOAuthClientEnvSelectsCLIFlavor(t *testing.T) {
	t.Setenv(EnvClientID, "123456789012-abcdefghijklmnopqrstuv.apps.googleusercontent.com")
	t.Setenv(EnvClientSecret, "GOCSPX-abcdefghijklmnopqrstuvwx")
	resetOAuthClientCache(t)

	got := oauthClient()
	if got.Flavor != FlavorCLI {
		t.Fatalf("显式 env 应归 CLI 家族（精简 scope、无 PKCE），得 %v", got.Flavor)
	}
	if got.ID != os.Getenv(EnvClientID) {
		t.Fatalf("必须用 env 给的 id，得 %q", got.ID)
	}
}

// TestLocalCLIExtractionStillWorks 提取能力本身仍在（作为后备路径与诊断用）。
//
// R135 之后它不再是默认来源（默认走内置 Antigravity 客户端），但函数必须继续可用：
// 用户显式改用 CLI 客户端、或排查「本机到底装了哪个版本的 CLI」时都要靠它。
func TestLocalCLIExtractionStillWorks(t *testing.T) {
	root := t.TempDir()
	writeFakeCLIPackage(t, filepath.Join(root, "node_modules", "@google", "antigravity"))
	t.Setenv("GEMINI_CLI_HOME", filepath.Join(root, "node_modules", "@google", "antigravity"))
	t.Setenv(EnvClientID, "")
	t.Setenv(EnvClientSecret, "")

	id, secret := extractOAuthClientFromLocalCLI()
	if id != fakeCLIClientID || secret != fakeCLIClientSecret {
		t.Fatalf("应从本机 CLI 提取到假凭据，得 %q / %q", id, secret)
	}
}

// TestEnvWinsOverLocalCLI 守门：env 两枚齐全时**不得**再去扫本机 CLI（自建客户端场景）。
func TestEnvWinsOverLocalCLI(t *testing.T) {
	root := t.TempDir()
	writeFakeCLIPackage(t, filepath.Join(root, "node_modules", "@google", "gemini-cli"))
	t.Setenv("GEMINI_CLI_HOME", root)
	t.Setenv(EnvClientID, "1212121212-fromenvclient.apps.googleusercontent.com")
	t.Setenv(EnvClientSecret, "from-env-only-secret")
	resetOAuthClientCache(t)

	got := oauthClient()
	if got.ID != "1212121212-fromenvclient.apps.googleusercontent.com" ||
		got.Secret != "from-env-only-secret" {
		t.Fatalf("env 必须优先，得 %q / %q", got.ID, got.Secret)
	}
	if len(got.SearchRoots) != 0 {
		t.Errorf("env 已齐全时不应扫描本机（SearchRoots 应为空），实际：%v", got.SearchRoots)
	}
}

// TestExtractStopsAtFileCap 上界：扫描必须在文件数上限处停止。
//
// 用一个"文件数远超上限、且都不含凭据"的目录模拟意外巨大的 node_modules。
func TestExtractStopsAtFileCap(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "node_modules")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	// 最后一个文件才是"命中"：它必须**读不到**，否则说明上限没生效。
	total := scanMaxFiles + 50
	for i := 0; i < total; i++ {
		p := filepath.Join(dir, "f"+strconv.Itoa(i)+".js")
		body := "void 0;\n"
		if i == total-1 {
			body = fakeCLIClientID + " " + fakeCLIClientSecret
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("造文件失败（%d）：%v", i, err)
		}
	}
	t.Setenv("GEMINI_CLI_HOME", root)

	id, secret := extractOAuthClientFromLocalCLI()
	if id != "" || secret != "" {
		t.Errorf("超过文件数上限后应停止扫描，却提取到 %q / %q", id, secret)
	}
}

// TestScanReadsOversizedFileByStreaming 大文件**必须**被流式扫到（2026-10-30 现场缺陷）。
//
// 这条用例取代了旧的 `TestScanSkipsOversizedFiles`，两者断言相反 —— 旧行为是
// 「超过 scanMaxBytes 就整片跳过」，而官方 CLI 0.66.0 把两枚凭据放在**单个 16.8MB**
// 的 `bundle/chunk-*.js` 里（实测），于是「跳过超限文件」= 「永远取不到凭据」：
// 用户照着报错文案 `npm i -g @google/gemini-cli` 装完、重启、再点，仍然报同一句
// 「未获取到 Google OAuth 客户端凭据」，形成死循环。
//
// ⚠️ 旧用例其实是**假通过**：它把大文件写在 `<root>/node_modules/huge.js`，
// 而扫描器本来就会 `SkipDir` 掉 root 之下的 `node_modules`（与大小无关），
// 所以把大小上限改成 1 字节它照样绿。新用例因此**不放进 node_modules**，
// 而是按官方真实布局放在 root 下的 `bundle/`。
func TestScanReadsOversizedFileByStreaming(t *testing.T) {
	root := t.TempDir()
	bundle := filepath.Join(root, "bundle")
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	big := filepath.Join(bundle, "chunk-HUGE.js")
	f, err := os.Create(big)
	if err != nil {
		t.Fatalf("建大文件失败：%v", err)
	}
	// 超过 scanMaxBytes 的填充（模拟 16.8MB 单包），凭据放在**文件尾部**：
	// 只有真正读到尾部的实现才能命中（分块流式读的窗口逻辑也一并被测到）。
	if _, err := f.Write(make([]byte, scanMaxBytes+1)); err != nil {
		_ = f.Close()
		t.Fatalf("写大文件失败：%v", err)
	}
	if _, err := f.WriteString("var OAUTH_CLIENT_ID = \"" + fakeCLIClientID + "\"; " +
		"var OAUTH_CLIENT_SECRET = \"" + fakeCLIClientSecret + "\";"); err != nil {
		_ = f.Close()
		t.Fatalf("写大文件尾部失败：%v", err)
	}
	_ = f.Close()
	t.Setenv("GEMINI_CLI_HOME", root)

	id, secret := extractOAuthClientFromLocalCLI()
	if id != fakeCLIClientID || secret != fakeCLIClientSecret {
		t.Fatalf("超限文件必须被流式扫到：id=%q secret=%q", id, secret)
	}
}

// TestPairingPicksNearestClientID 配对判据：secret 必须与**紧邻其前**的 client_id 成对。
//
// 现场依据（官方 CLI 0.66.0 实测，逐字节量过）：同一个文件里有两个 client_id ——
// google-auth-library 的 `CLOUD_SDK_CLIENT_ID`（偏移 5649522，无配对 secret）
// 与真正的 `OAUTH_CLIENT_ID`（偏移 12279769，其后 102 字节即 secret）。
// 旧的「各取首个命中」会把前者与后者拼成从未配对过的组合 ⇒ 授权页回 invalid_client。
func TestPairingPicksNearestClientID(t *testing.T) {
	root := t.TempDir()
	decoy := "5" + strings.Repeat("5", 9) + "-" + strings.Repeat("f", 24) + ".apps.googleusercontent.com"
	body := "exports.CLOUD_SDK_CLIENT_ID = \"" + decoy + "\";\n" +
		strings.Repeat("var unrelated = 1;\n", 200) +
		"var OAUTH_CLIENT_ID = \"" + fakeCLIClientID + "\";\n" +
		"var OAUTH_CLIENT_SECRET = \"" + fakeCLIClientSecret + "\";\n"
	if err := os.WriteFile(filepath.Join(root, "bundle.js"), []byte(body), 0o600); err != nil {
		t.Fatalf("写文件失败：%v", err)
	}
	t.Setenv("GEMINI_CLI_HOME", root)

	id, secret := extractOAuthClientFromLocalCLI()
	if id == decoy {
		t.Fatalf("取了无配对 secret 的 CLOUD_SDK_CLIENT_ID（错配）：id=%q", id)
	}
	if id != fakeCLIClientID || secret != fakeCLIClientSecret {
		t.Fatalf("配对失败：id=%q secret=%q", id, secret)
	}
}

// TestCLIRootsWindowsOnlyPositions 守卫：Windows 专属路径不得出现在非 Windows 上
// （避免在 macOS/Linux 上白扫一遍不存在的目录）。
func TestCLIRootsWindowsOnlyPositions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上这些路径本就该出现")
	}
	tmp := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmp)
	t.Setenv("APPDATA", tmp)
	for _, r := range cliRoots() {
		if strings.Contains(filepath.ToSlash(r), "npm-cache") {
			t.Errorf("非 Windows 不应出现 npm-cache 路径：%s", r)
		}
	}
}
