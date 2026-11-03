// oauthclient.go 运行时获取 Google OAuth 客户端的 client_id / client_secret。
//
// 为什么不在源码里内嵌（2026-10-17 变更）：
// 这两枚值原本内嵌在 constants.go —— 它们是 Google 官方客户端内嵌的**公开**凭据
// （开源客户端里就有，任何人可提取），但 GitHub 的 Secret Scanning Push Protection
// **只看格式、不看是否公开**，会把它们判定为 secret 并**拒绝推送整个分支**（GH013）。
// 改为运行时获取，顺带与 glm / loomy / monkeycode 的「导入型」渠道保持同一思路。
//
// 获取顺序：
//  1. 环境变量 `CMDC_PAK_GOOGLE_CLIENT_ID` / `CMDC_PAK_GOOGLE_CLIENT_SECRET`（自建 OAuth 客户端或手动指定）；
//  2. 本机安装的官方 CLI 包（npm 全局 / npx 缓存 / 项目内 node_modules / Homebrew，见 cliRoots）；
//  3. 都拿不到 → 空串，调用方给出可操作错误（login_gemini.ErrOAuthClientMissing 会列出**实际查过的位置**）。
package gemini

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
)

// 两枚公开值的**形态**。正则刻意拆成「字符类 + 片段」，不含任何完整可识别的 secret 值 ——
// 否则本文件自己又会被 push protection 拦下（那正是这次要修的问题）。
var (
	reOAuthClientID     = regexp.MustCompile(`[0-9]{10,}-[a-z0-9]{20,}\.apps\.googleusercontent\.com`)
	reOAuthClientSecret = regexp.MustCompile(`GOCSPX-[A-Za-z0-9_\-]{20,}`)
)

// reOAuthPair 是**配对**匹配：一个 client_id 与紧随其后的 GOOGLE client_secret 组成一对。
//
// 为什么必须配对而不是「各取首个」（2026-10-30 实测量化）：
// 官方 CLI 0.66.0 的 `bundle/chunk-HJ5KJXNZ.js` 里有**两个** client_id——
//
//	① 偏移 5649522 处是 google-auth-library 的 `CLOUD_SDK_CLIENT_ID`（无配对 secret）；
//	② 偏移 12279769 处才是 `OAUTH_CLIENT_ID`，其后 102 字节就是 `OAUTH_CLIENT_SECRET`。
//
// 「按字典序遍历、各取首个命中」会取到 ① 的 id 与 ② 的 secret ⇒ 得到**从未配对过**的组合，
// 授权页会直接回 `invalid_client`，而文案会误导成「凭据没配好」。故按「就近配对」取值：
//
//	client_id 与其后最多 pairWindow 字节内出现的 secret 视为一对。
//
// 全包三个副本实测：secret 到其配对 id 的距离恒为 102 字节，远小于 pairWindow，
// 而 ①↔② 相距 660 万字节，不会被误配。

// pairWindow 配对窗口（字节）：secret 与它前面最近的 client_id 相距不超过此值即视为一对。
//
// 实测 102 字节（官方 CLI 的三个副本一致），取 4096 留足格式变动余量：
// 官方若改成 `client_id: "…",\n client_secret: "…"` 这种多行形式也不会掉出去。
const pairWindow = 4096

// findOAuthPair 在文本里找一对**就近配对**的 client_id + client_secret。
//
// 用位置配对而不是「正则一次匹配两枚」：Go 的 regexp 重复次数上限是 1000，
// 写 `{0,4096}` 会直接 panic（本实现第一版就踩了）。位置法既不受该上限约束，
// 也更容易解释判据。
func findOAuthPair(s string) (id, secret string) {
	idLocs := reOAuthClientID.FindAllStringIndex(s, -1)
	secLocs := reOAuthClientSecret.FindAllStringIndex(s, -1)
	if len(idLocs) == 0 || len(secLocs) == 0 {
		return "", ""
	}
	for _, sec := range secLocs {
		best := -1
		for i, loc := range idLocs {
			// 只认「id 在 secret 之前」的配对（官方形态是 id 先声明、secret 紧随其后）。
			if loc[1] > sec[0] {
				continue
			}
			gap := sec[0] - loc[1]
			if gap > pairWindow {
				continue
			}
			if best < 0 || gap < sec[0]-idLocs[best][1] {
				best = i
			}
		}
		if best >= 0 {
			return s[idLocs[best][0]:idLocs[best][1]], s[sec[0]:sec[1]]
		}
	}
	return "", ""
}

// ClientFlavor 客户端家族。**决定 redirect 形态、scope 集合与是否用 PKCE**，
// 三者必须成对使用（混搭必然 redirect_uri_mismatch 或 restricted_client）。
type ClientFlavor int

const (
	// FlavorAntigravity 内置的 Antigravity 客户端（默认）：完整 scope + PKCE + 127.0.0.1/callback。
	FlavorAntigravity ClientFlavor = iota
	// FlavorCLI 旧的 Gemini CLI 客户端（env 指定或本机提取）：精简 scope、无 PKCE、localhost/oauth-callback。
	// 保留它只为向后兼容（用户显式设了 env 时尊重其选择），不是推荐路径 ——
	// 该客户端已被上游标记为 UNSUPPORTED_CLIENT。
	FlavorCLI
)

// localOAuthClient 解析结果。
type localOAuthClient struct {
	ID     string
	Secret string
	// Flavor 这份凭据属于哪个客户端家族（决定 redirect/scope/PKCE 三件套）。
	Flavor ClientFlavor
	// SearchRoots 记录本次**实际考察过的**位置（含不存在的），供报错文案如实告知用户
	// 「到底查了哪里」。修复前只报「请设置环境变量」，用户照做仍然失败且无从排查。
	SearchRoots []string
}

var (
	oauthClientOnce sync.Once
	oauthClientVal  localOAuthClient
)

// oauthClient 解析一次并缓存（提取要扫目录，不能每个请求都做）。
//
// env 只给了一半时，用本机 CLI 补另一半：自建客户端理应两枚都给，但宽松处理比直接报错友好。
func oauthClient() localOAuthClient {
	oauthClientOnce.Do(func() {
		envID := strings.TrimSpace(os.Getenv(EnvClientID))
		envSecret := strings.TrimSpace(os.Getenv(EnvClientSecret))
		if envID != "" && envSecret != "" {
			// 用户显式指定了整套凭据：尊重它，按 CLI 家族使用（精简 scope、无 PKCE）。
			oauthClientVal = localOAuthClient{ID: envID, Secret: envSecret, Flavor: FlavorCLI}
			return
		}
		// 默认走内置的 Antigravity 客户端（2026-10-30，R135）：它是当前在役客户端，
		// 且登记了 Cloud Code 需要的全部 scope。此前默认回落到「从本机 CLI 提取」，
		// 而那个客户端已被上游弃用 —— 用户装了 CLI 反而更容易撞 403。
		oauthClientVal = localOAuthClient{
			ID:     AntigravityClientID(),
			Secret: AntigravityClientSecret(),
			Flavor: FlavorAntigravity,
		}
		if oauthClientVal.ID == "" || oauthClientVal.Secret == "" {
			roots := cliRoots()
			id, secret := extractOAuthClientFromRoots(roots)
			if oauthClientVal.ID == "" {
				oauthClientVal.ID = id
			}
			if oauthClientVal.Secret == "" {
				oauthClientVal.Secret = secret
			}
			oauthClientVal.SearchRoots = roots
			// 从本机 CLI 提取到的必然是 CLI 家族的凭据。
			if oauthClientVal.ID != "" && oauthClientVal.Secret != "" {
				oauthClientVal.Flavor = FlavorCLI
			}
		}
	})
	return oauthClientVal
}

// SetBuiltinClientForTest 临时替换内置的 Antigravity 凭据，返回恢复函数。
//
// **仅供测试**：R135 起内置凭据恒非空，「拿不到凭据」那条守门用例（以及它的可操作文案）
// 就再也走不到了 —— 不把它留成可构造的场景，那段文案会慢慢烂掉而无人发现。
// 生产路径不得调用。
func SetBuiltinClientForTest(id, secret string) func() {
	oldID, oldSecret := antigravityClientID, antigravityClientSecret
	antigravityClientID, antigravityClientSecret = id, secret
	ResetOAuthClientCacheForTest()
	return func() {
		antigravityClientID, antigravityClientSecret = oldID, oldSecret
		ResetOAuthClientCacheForTest()
	}
}

// ResetOAuthClientCacheForTest 清掉进程级解析缓存，让下一次 ClientID()/ClientSecret() 重新解析。
//
// **仅供测试**：解析结果被 sync.Once 缓存、进程内只算一次；跨包（login_gemini 的用例）
// 需要"按当时的 env 重新解析"时这是唯一手段。生产路径**不得**调用——那会让每次请求都扫一遍磁盘。
func ResetOAuthClientCacheForTest() {
	oauthClientOnce = sync.Once{}
	oauthClientVal = localOAuthClient{}
}

// OAuthClientSearchRoots 返回本机考察过的 CLI 位置（未取到凭据时用于报错文案）。
//
// 只暴露**路径**，不含任何凭据值：调用方可安全地把它拼进用户可见的错误文案。
func OAuthClientSearchRoots() []string {
	roots := oauthClient().SearchRoots
	out := make([]string, len(roots))
	copy(out, roots)
	return out
}

// oauthCLIPackageDirs 官方（及前后代）CLI 的**包目录名**，相对某个 node_modules。
//
// 为什么不止 `@google/gemini-cli`：
//   - `@google/antigravity`：2026-06-18 起官方 Gemini CLI 已被 Antigravity CLI 取代
//     （geminicli.com/docs/changelogs 原文「Gemini CLI was replaced by Antigravity CLI」）。
//     本渠道的身份头（`antigravity/4.3.0 (cmdc-pak)`）、端点（daily-cloudcode-pa）本就同源，
//     故新命令装出来的包才是**当前**最可能带这两枚值的本地来源。
//   - `gemini-cli`（无 scope）：社区 fork / 部分发行版的分发名，多扫一个目录代价可忽略。
var oauthCLIPackageDirs = [][]string{
	{"@google", "gemini-cli"},
	{"@google", "gemini-cli-core"},
	{"@google", "antigravity"},
	{"gemini-cli"},
}

// appendPackageRoots 把 base/node_modules/<pkg…> 逐个追加到 roots。
func appendPackageRoots(roots []string, base string) []string {
	if strings.TrimSpace(base) == "" {
		return roots
	}
	for _, pkg := range oauthCLIPackageDirs {
		roots = append(roots, filepath.Join(append([]string{base}, pkg...)...))
	}
	return roots
}

// cliRoots 官方 CLI 的常见安装位置。
//
// 只列**明确属于该 CLI**的目录：不扫整个 node_modules（动辄几十万文件，代价不可接受）。
//
// 2026-10-17 现场修复（逐条都有本机实测证据）：
//   - **Windows 的 npx 缓存不在 `~/.npm/_npx`**：npm 10+ 在 Windows 上用
//     `%LOCALAPPDATA%\npm-cache\_npx`（本机 `~/.npm` 根本不存在，而前者有 6568 个文件）。
//     旧代码只查后者 ⇒ Windows 上「npx 跑过的 CLI」这条来源等于白写。
//   - 补 `%LOCALAPPDATA%\npm\node_modules`：npm 在部分 Windows 配置下把全局前缀放在 LocalAppData
//     （与 Roaming 的 `%APPDATA%\npm` 并存），漏掉就查不到已装的 CLI。
//   - **Homebrew**：macOS 上 `brew install` 的 npm 全局前缀是 `/opt/homebrew/lib/node_modules`
//     （Apple Silicon）或 `/usr/local/lib/node_modules`（Intel），旧代码完全没覆盖。
//   - 包目录名见 oauthCLIPackageDirs（官方已换成 @google/antigravity）。
//
// 顺序即优先级：GEMINI_CLI_HOME（用户显式指定）> 全局安装 > npx 缓存 > 其它前缀。
func cliRoots() []string {
	var roots []string
	if v := strings.TrimSpace(os.Getenv("GEMINI_CLI_HOME")); v != "" {
		roots = append(roots, v)
	}

	// ---- 全局安装前缀（Windows 上 Roaming 与 Local 两种都可能）----
	if appdata := strings.TrimSpace(os.Getenv("APPDATA")); appdata != "" {
		roots = appendPackageRoots(roots, filepath.Join(appdata, "npm", "node_modules"))
	}
	if localApp := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); localApp != "" &&
		runtime.GOOS == "windows" {
		roots = appendPackageRoots(roots, filepath.Join(localApp, "npm", "node_modules"))
		// npm 10+ 的 npx 缓存真实位置（见上方说明）。
		roots = append(roots, filepath.Join(localApp, "npm-cache", "_npx"))
	}

	if home, err := os.UserHomeDir(); err == nil && home != "" {
		// 用户级全局前缀。
		roots = appendPackageRoots(roots, filepath.Join(home, ".npm-global", "lib", "node_modules"))
		roots = appendPackageRoots(roots, filepath.Join(home, ".local", "share", "npm", "node_modules"))
		// 项目内 / 家目录直装的 node_modules。
		roots = appendPackageRoots(roots, filepath.Join(home, "node_modules"))
		// npx 缓存：里面是 `_npx/<hash>/node_modules/@google/<pkg>`，多版本并存，整体下探。
		roots = append(roots, filepath.Join(home, ".npm", "_npx"))
	}

	// ---- 非 Windows 的 Homebrew / 系统级前缀 ----
	if runtime.GOOS != "windows" {
		roots = appendPackageRoots(roots, "/opt/homebrew/lib/node_modules")
		roots = appendPackageRoots(roots, "/usr/local/lib/node_modules")
		roots = appendPackageRoots(roots, "/usr/lib/node_modules")
	}

	return dedupeRoots(roots)
}

// dedupeRoots 去重并去掉空串（同一路径可能被多个前缀命中，重复扫描纯属浪费）。
func dedupeRoots(roots []string) []string {
	seen := make(map[string]struct{}, len(roots))
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		key := strings.ToLower(filepath.Clean(r))
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, r)
	}
	return out
}

// 扫描硬上限（每个 root 独立计）：
const (
	// scanMaxFiles 单个 root 最多读多少个文件。
	scanMaxFiles = 4000
	// scanMaxBytes 单个文件最大字节数（**超过即跳过**，不整片读进内存）。
	//
	// 2026-10-30 现场修复（issue：装对了也对不上）：官方 CLI 的分发形态已从
	// 「dist/ 下若干小 .ts」变成「bundle/ 下**单个 16.8MB** 打包 js」
	// （@google/gemini-cli 0.66.0 实测：`bundle/chunk-HJ5KJXNZ.js` 16784602 字节，
	// 两枚凭据就在这一个文件里）。旧上限 8MiB 把唯一含有凭据的文件整片跳过 ⇒
	// `npm i -g @google/gemini-cli` 之后 `ClientID()` 仍返回空串，用户照着错误文案
	// 装完还是失败。现改为**流式分段扫描**（见 scanFileForOAuth）：
	// 大文件不再整片跳过，而是按 CRC 分块逐块匹配，内存占用与文件大小无关。
	// 该常量因此退化为「是否走流式路径」的判据（scanStreamChunk 另立常量）。
	scanMaxBytes = 8 << 20
	// scanStreamChunk 流式扫描的分块大小。凭据两枚值的最大长度约 72 / 60 字节，
	// 8 个 1KiB 块的重叠窗口（scanOverlap）远大于此，跨块匹配不会漏。取 1MiB 是为了
	// 让 16.8MB 文件只做 17 次块内 FindString，而不是逐行 regex。
	scanStreamChunk = 1 << 20
	// scanOverlap 相邻块的重叠窗口：保证跨块断开的凭据仍能被匹配到。
	scanOverlap = 4096
	// scanByteBudget 单个 root 的总读取字节预算（防止"4000 个 8MB 文件"把内存吃光）。
	// 流式扫描同样计入（按实际读过的字节累加）。
	scanByteBudget = 512 << 20
)

// scanFileForOAuth 在**单个文件**里找两枚凭据：小文件整片读，大文件流式分块读。
//
// 返回 (clientID, clientSecret, 实际读取字节数, 错误)。wantID/wantSecret 为 false 时
// 对应那枚值已从别处拿到，不再浪费时间匹配。
//
// 为什么要分两条路径（2026-10-30 现场，见 scanMaxBytes 注释）：
// 官方 CLI 0.66.0 的凭据在 `bundle/chunk-HJ5KJXNZ.js`（16.8MB 单个文件）。旧实现
// 「整文件超 8MiB 就跳过」等于把**唯一**的凭据来源排除，用户装完 CLI 仍然失败，
// 且报错文案还在让他「去装 CLI」——死循环。改为分块流式扫描后，16.8MB 文件只占
// 一个块的常驻内存，跨块匹配由 scanOverlap 的窗口保证。
//
// ⚠️ 分块**不能**简单按字节切：块边界会把 60~72 字节的凭据切成两半，两半都不匹配。
// 故每次用「上一块尾部 scanOverlap + 本块」作为匹配窗口。
func scanFileForOAuth(p string, size int64, wantID, wantSecret bool) (id, secret string, read int64, err error) {
	readBytes := func(b []byte) (string, string) {
		s := string(b)
		var gotID, gotSec string
		// 优先取**配对**（见 reOAuthPair 注释）：两枚都缺时，只有配对命中的才算数，
		// 避免把另一处的孤立 client_id 与这里的 secret 拼成从未配对过的组合。
		if wantID && id == "" && wantSecret && secret == "" {
			return findOAuthPair(s)
		}
		// 只有一枚还缺时（另一枚已从别的文件拿到），退化为单值匹配。
		if wantID && id == "" {
			gotID = reOAuthClientID.FindString(s)
		}
		if wantSecret && secret == "" {
			gotSec = reOAuthClientSecret.FindString(s)
		}
		return gotID, gotSec
	}

	if size <= scanStreamChunk {
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return "", "", 0, rerr
		}
		gotID, gotSec := readBytes(b)
		return gotID, gotSec, int64(len(b)), nil
	}

	f, oerr := os.Open(p)
	if oerr != nil {
		return "", "", 0, oerr
	}
	defer f.Close()

	buf := make([]byte, scanStreamChunk+scanOverlap)
	var tail []byte // 上一块尾部重叠窗口
	for {
		n, rerr := f.Read(buf[len(tail):])
		if n <= 0 && rerr == nil {
			break // 文件比 stat 报的更小（被截断）：读到 EOF 即止
		}
		if rerr != nil && rerr != io.EOF {
			return id, secret, read, rerr
		}
		chunk := buf[:len(tail)+n]
		gotID, gotSec := readBytes(chunk)
		if gotID != "" {
			id = gotID
		}
		if gotSec != "" {
			secret = gotSec
		}
		read += int64(n)
		if (wantID == false || id != "") && (wantSecret == false || secret != "") {
			return id, secret, read, nil // 两枚都拿到，提前停
		}
		// 保留尾部重叠窗口供下一次匹配跨块凭据。
		keep := scanOverlap
		if keep > len(chunk) {
			keep = len(chunk)
		}
		tail = append(tail[:0], chunk[len(chunk)-keep:]...)
		if rerr == io.EOF {
			break
		}
	}
	return id, secret, read, nil
}

// extractOAuthClientFromLocalCLI 在本机 CLI 包里找这两枚值；找不到（或目录不存在）返回空串。
func extractOAuthClientFromLocalCLI() (id, secret string) {
	return extractOAuthClientFromRoots(cliRoots())
}

// extractOAuthClientFromRoots 在给定的候选根目录里找这两枚值。
//
// 扫描有硬上限：文件数、单文件字节数、总字节预算、扩展名白名单 —— 避免在意外巨大的目录上
// 卡住主流程或吃光内存。上限按 **root 独立计**：某个 root 特别大时，不该牺牲后续 root 的机会。
func extractOAuthClientFromRoots(roots []string) (id, secret string) {
	for _, root := range roots {
		if id != "" && secret != "" {
			return id, secret
		}
		if _, err := os.Stat(root); err != nil {
			continue // 未安装 / 无权限：跳过，不当错误
		}
		files, budget := 0, scanByteBudget
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				switch d.Name() {
				case ".git", "test", "tests", "docs", "coverage", "__tests__", "node_modules":
					// ⚠️ 只跳过 root **之下**的同名目录：root 自身可能就叫 node_modules
					// （GEMINI_CLI_HOME 指向某个 node_modules、或 %APPDATA%\npm\node_modules 这类前缀），
					// 那时跳过等于把整棵树都跳过。判据用相对深度，不用名字。
					if p == root {
						return nil
					}
					// 嵌套的 node_modules 要跳过：CLI 包自身依赖成百上千个包，真正的值只在
					// 自己的 dist/ 里；扁平化的重复目录只会白白吃掉文件数与字节预算。
					return fs.SkipDir
				}
				return nil
			}
			if files >= scanMaxFiles || budget <= 0 {
				return fs.SkipAll
			}
			switch strings.ToLower(filepath.Ext(p)) {
			// .ts 必须在白名单里：官方 CLI 的部分产物（types / 未打包的分发）以 .ts 落盘。
			// 它不是"顺手多扫一种"，而是本机假 CLI 用例实测出来的真实缺口。
			case ".js", ".mjs", ".cjs", ".json", ".ts":
			default:
				return nil
			}
			fi, err := d.Info()
			if err != nil {
				return nil
			}
			size := fi.Size()
			if size <= 0 || size > int64(budget) {
				return nil
			}
			// 两枚值可能分别在两个文件里（官方 CLI 把 client_id 留在 SDK 侧、
			// secret 留在账号管理器侧）；已经都拿到了就别再读了。
			foundID, foundSec, read, err := scanFileForOAuth(p, size, id == "", secret == "")
			files++
			budget -= int(read)
			if err != nil {
				return nil
			}
			if id == "" {
				id = foundID
			}
			if secret == "" {
				secret = foundSec
			}
			if id != "" && secret != "" {
				return fs.SkipAll
			}
			return nil
		})
	}
	return id, secret
}
