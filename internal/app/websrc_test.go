package app

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// webDir 是 Web UI 静态资源目录（相对包目录 internal/app）。
//
// 注意：`cmd/wild-work/web/` 里的 JS 是 **classic script + 共享全局**（无模块系统），
// 加载顺序由 index.html 的 <script src> 顺序决定（见 docs/refactor/03-web.md）。
const webDir = "../../cmd/wild-work/web"

// webScriptSrcRe 抓 index.html 里的 <script src="…">。只取 src，忽略内联脚本。
var webScriptSrcRe = regexp.MustCompile(`<script\b[^>]*\bsrc\s*=\s*["']([^"']+)["']`)

// webScriptsFromIndex 返回 index.html 中 <script src> 的**声明顺序**（唯一真相源）。
//
// 返回 nil 表示一个 src 都没解析到（调用方据此降级，见 webSource）。
// 每个 src 对应的文件必须在磁盘上真实存在，否则直接 fail —— 那是 web 侧改名/删文件后
// 最典型的静默失效：测试照旧"全绿"，因为它读的是拼接后的空字符串或别的残留文件。
func webScriptsFromIndex(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(webDir, "index.html"))
	if err != nil {
		t.Fatalf("读 %s/index.html 失败：%v", webDir, err)
	}
	var out []string
	for _, m := range webScriptSrcRe.FindAllStringSubmatch(string(raw), -1) {
		src := strings.TrimSpace(m[1])
		if src == "" {
			continue
		}
		// 外链脚本不参与源码拼接（本仓当前没有；留判断以免将来加入 CDN 时误判为缺文件）。
		if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") || strings.HasPrefix(src, "//") {
			continue
		}
		out = append(out, src)
	}
	for _, src := range out {
		fp := filepath.Join(webDir, strings.TrimPrefix(src, "./"))
		if _, err := os.Stat(fp); err != nil {
			t.Fatalf("index.html 声明了 <script src=%q>，但 %s 不存在：%v\n"+
				"（前端改名/删除文件后必须同步 index.html，否则下面的源码断言会静默失效）", src, fp, err)
		}
	}
	return out
}

// webSource 把 index.html 声明顺序上的全部前端 JS 拼成一份文本，供 app 包的
// 「前端契约」断言使用（这些断言以**源码文本**为判据，见 monkeycode/loomy/officeace 各测试）。
//
// 为什么需要它（B6-3，2026-10-29）：这批测试原本写死读 `cmd/wild-work/web/app.js`；
// 该文件已按 docs/refactor/03-web.md 拆成 21 个 `app-*.js`。写死单文件名有两个后果：
//  1. 一律失败（读不到）；
//  2. 若改成"读某个固定新文件"，则前端再拆分/再改名时又会静默失效。
//
// 这里以 index.html 为唯一真相源：前端**新增文件只要加一行 <script src> 就自动被覆盖**，
// 不需要再改任何 Go 测试。
//
// 降级：若 index.html 里一个 <script src> 都解析不到（例如整页改成动态注入），
// 退回"按文件名排序拼接 web 目录下所有 .js"，并在测试日志里**显式标注降级** ——
// 顺序不同只影响"跨文件相邻性"类断言，不影响 Contains 类断言。
func webSource(t *testing.T) string {
	t.Helper()
	var files []string
	srcs := webScriptsFromIndex(t)
	if len(srcs) > 0 {
		for _, s := range srcs {
			files = append(files, strings.TrimPrefix(s, "./"))
		}
	} else {
		entries, err := os.ReadDir(webDir)
		if err != nil {
			t.Fatalf("读 %s 失败：%v", webDir, err)
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".js") {
				files = append(files, e.Name())
			}
		}
		sort.Strings(files)
		t.Logf("⚠️ 降级：index.html 未解析到任何 <script src>，改按文件名排序拼接 %d 个 .js", len(files))
	}

	var b strings.Builder
	for _, f := range files {
		raw, err := os.ReadFile(filepath.Join(webDir, f))
		if err != nil {
			t.Fatalf("读前端脚本 %s 失败：%v", f, err)
		}
		b.WriteString("\n//=== WEB: ")
		b.WriteString(f)
		b.WriteString(" ===\n")
		b.Write(raw)
		b.WriteString("\n")
	}
	return b.String()
}

// TestWebScriptListMatchesDisk 锁住"index.html ↔ web 目录"的双向一致：
//   - index.html 声明的每个 src 都存在（webScriptsFromIndex 里已 fail）；
//   - web 目录里的每个 .js 都被 index.html 引用 —— 否则那是一个**永远不会加载**的文件，
//     而针对它写的源码断言会"通过"，因为断言读的是拼接文本里根本没有的内容（假绿）。
//
// 若将来确实需要放一个不参与加载的 .js（工具/参考实现），在下面的 allowlist 里登记并写原因。
func TestWebScriptListMatchesDisk(t *testing.T) {
	allowUnloaded := map[string]string{} // 目前没有例外

	srcs := webScriptsFromIndex(t)
	listed := make(map[string]bool, len(srcs))
	for _, s := range srcs {
		listed[strings.TrimPrefix(s, "./")] = true
	}
	if len(listed) == 0 {
		t.Fatal("index.html 未声明任何本地 <script src> —— 前端源码断言将失去依据")
	}

	entries, err := os.ReadDir(webDir)
	if err != nil {
		t.Fatalf("读 %s 失败：%v", webDir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".js") || listed[name] {
			continue
		}
		if _, ok := allowUnloaded[name]; ok {
			continue
		}
		t.Errorf("❌ %s 存在于 web 目录但未被 index.html 引用 —— 它不会被加载，针对它的断言会假绿；"+
			"要么加 <script src=\"./%s\">，要么在 allowUnloaded 里登记原因", name, name)
	}
}
