package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// pkgFileSentinel 是 pkgSource 拼接时插入的文件分隔标记（一行注释）。
//
// 为什么需要它：`scene_routing_test.go` 的 funcBody 用「到下一个顶层 `func ` 为止」定界。
// 拼接多个文件后，若被断言函数是某个文件的**最后一个声明**，它后面紧跟的是下一个文件的
// 注释/类型/变量，而不是 `func ` —— 于是 funcBody 会一路吃到下一个文件的第一个函数之前，
// 把别人的声明也吞进"函数体"。那会让 `strings.Count(body, "config.Save(")` 这类断言**假绿**
// （比假红危险：假红会被发现，假绿不会）。
// 有了哨兵，funcBody 在文件边界就停，拼接文本与单文件语义一致。
const pkgFileSentinel = "//=== FILE: "

// pkgGoFiles 返回本包全部生产源码文件名（非 _test.go），按名排序。
//
// 排序是为了让 pkgSource 的输出**稳定**（同一份代码每次拼出同一个字符串，
// 断言里的计数与位置才可复现）。
func pkgGoFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("读包目录失败: %v", err)
	}
	var files []string
	for _, e := range entries {
		n := e.Name()
		if strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
			files = append(files, n)
		}
	}
	sort.Strings(files)
	return files
}

// pkgSource 返回本包全部生产源码拼成的「单文件形态」：只保留一个 package 子句，
// 每个文件从**第一条非 import 声明**（含其文档注释）起拼接，文件之间插入
// pkgFileSentinel 分隔行。
//
// 为什么需要它（B6-0，2026-10-29）：本包有 8 处结构性护栏测试原本用
// `os.ReadFile("app.go")` 直接读单个文件。它们守的是**结构不变量**
// （登录能力单一来源表存在、SetSceneRouting 只写一次盘、登录收尾必须调 firstCheckinXxx…），
// 与「代码放在哪个文件」无关。`app.go` 由 5143 行拆成 18 个文件（docs/refactor/01-app.md）
// 后，绑定文件名会让这些断言集体假红 —— 而删断言等于销毁护栏。
//
// 注意：返回的是**超集**（全包源码），原有断言若因"符号不存在"而红，在这里同样红；
// 唯一的语义放宽是"同一符号出现在两个文件里也算通过"，而 `Count(...) >= 3` 这类断言本就允许更多。
func pkgSource(t *testing.T) []byte {
	t.Helper()
	var b strings.Builder
	b.WriteString("package app\n\n")
	for _, f := range pkgGoFiles(t) {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读 %s 失败: %v", f, err)
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, f, raw, parser.ParseComments)
		if err != nil {
			// 包装层读到的可能是未落盘的中间态：这里必须 fail（否则断言会静默跳过）。
			t.Fatalf("解析 %s 失败: %v", f, err)
		}
		start := len(raw)
		for _, d := range file.Decls {
			if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.IMPORT {
				continue
			}
			start = fset.Position(d.Pos()).Offset
			break
		}
		b.WriteString("\n")
		b.WriteString(pkgFileSentinel)
		b.WriteString(f)
		b.WriteString(" ===\n")
		b.Write(raw[start:])
		b.WriteString("\n")
	}
	return []byte(b.String())
}

// TestPkgSourceSentinelStopsFuncBody 锁住 B6-0 的哨兵契约：
// funcBody（scene_routing_test.go）必须**在文件边界停**，不得跨文件吃进下一个文件的声明。
//
// 为什么这是一条真护栏（Lead 2026-10-29 指出的陷阱）：拆分后若被断言的函数是某文件的
// 最后一个声明，funcBody 若继续按「到下一个 `func `」找边界，就会把下一个文件的
// 类型/变量/哨兵一并算进"函数体"。那样 `strings.Count(body, "config.Save(")` 之类的断言
// 会**假绿** —— 假绿比假红危险，因为没人会去看它。
func TestPkgSourceSentinelStopsFuncBody(t *testing.T) {
	src := string(pkgSource(t))
	body := funcBody(t, src, "func (a *App) SetSceneRouting(")
	if strings.Contains(body, pkgFileSentinel) {
		t.Fatalf("funcBody 跨过了文件分隔哨兵（%q）—— 断言可能假绿", pkgFileSentinel)
	}
	// 反向对照：被断言函数确实被切出来了（不是空串、也不是整个包）。
	if !strings.Contains(body, "config.Save(") {
		t.Fatal("funcBody 没切到 SetSceneRouting 的函数体（断言会失去意义）")
	}
	if strings.Contains(body, "func (a *App) SetModelFallback(") {
		t.Fatal("funcBody 溢出到了下一个函数 SetModelFallback")
	}
}
