package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// 本测试锁定一个**真实 bug**（2026-09-27 发现）：
//
//	新增 glm 渠道时，在 main.go 创建了 glmSch、注册进 runtimes、
//	设了观察者，**但漏了 `go glmSch.Run(sctx)`** ——
//	后果是「GLM 的自动签到与保活从不运行」，而手工触发仍可用，
//	**不报错、不崩溃**，极难发现（用户是手工签到后才发现的）。
//
// 本测试用静态检查兜住这类"新增渠道漏启动"的疏漏：
// 凡是 `Xxx := scheduler.New(...)` 定义的调度器，都必须有对应的 `.Run(`。
//
// 为什么用静态检查而不是运行时测试：
// 调度器是 `go ...Run(ctx)` 长期后台循环，无法在单测里断言"它跑起来了"；
// 而"定义了却没启动"这个疏漏**在源码层面就能判定**。

func TestAllSchedulersAreStarted(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("读 main.go 失败: %v", err)
	}
	code := string(src)

	// ① 收集所有 `xxxSch := scheduler.New(` 定义的调度器变量名
	defRe := regexp.MustCompile(`(\w+)\s*:=\s*scheduler\.New\(`)
	defs := map[string]bool{}
	for _, m := range defRe.FindAllStringSubmatch(code, -1) {
		defs[m[1]] = true
	}
	if len(defs) == 0 {
		t.Fatal("未在 main.go 中找到任何 scheduler.New 定义（正则失效？）")
	}
	t.Logf("发现 %d 个调度器定义", len(defs))

	// ③ 逐个比对：每个 scheduler.New 定义的调度器都必须挂进某条运行时的 Scheduler 字段。
	//
	// 2026-10-30 起本测试的判据变了（更严，也不再依赖"手抄 go .Run 行"）：
	// main.go 不再逐个 `go xxxSch.Run(sctx)`，改为
	// `for _, rt := range appRuntimes { if rt.Scheduler != nil { go rt.Scheduler.Run(sctx) } }`。
	// 于是「是否会被启动」的**唯一**判据变成「是否出现在 appRuntimes 的 Scheduler 字段里」，
	// 本断言直接盯这个字段——比旧版"有一行 go 调用"更接近真实语义
	// （旧版无法发现「写了两行 go 但其中一个是笔误变量名」这类问题）。
	declared := map[string]bool{}
	for _, m := range regexp.MustCompile(`Scheduler:\s*(\w+)`).FindAllStringSubmatch(stripComments(code), -1) {
		declared[m[1]] = true
	}
	var missing []string
	for name := range defs {
		if !declared[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("❌ 以下调度器已定义但**未挂进任何运行时**（自动签到/保活不会运行，且不报错）:\n  %s\n"+
			"修复：在 main.go 的 appRuntimes 字面量里给对应渠道补 `Scheduler: %s`",
			strings.Join(missing, "\n  "), missing[0])
	}

	// ④ 反向：不得再有逐个 `go xxxSch.Run(sctx)` 的手抄行（否则表驱动被绕过、又回到会漏的老路）。
	if m := regexp.MustCompile(`go\s+(\w+)\.Run\(sctx\)`).FindAllStringSubmatch(stripComments(code), -1); len(m) > 0 {
		var names []string
		for _, x := range m {
			names = append(names, x[1])
		}
		t.Errorf("❌ main.go 里仍有逐个启动的 `go %s.Run(sctx)`：表驱动启动已被绕过。\n"+
			"R34 的整类缺陷（漏一行不报错）会随之回来；请改用 appRuntimes 循环启动。",
			strings.Join(names, ", "))
	}
}

// TestSchedulerCountMatchesRuntimes 交叉校验：注册进 runtimes 的渠道数 == 调度器数。
//
// 防止「注册了渠道但没配调度器」与「配了调度器但渠道没注册」两个方向。
func TestSchedulerCountMatchesRuntimes(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("读 main.go 失败: %v", err)
	}
	code := string(src)

	nc := stripComments(code)
	defs := regexp.MustCompile(`(\w+)\s*:=\s*scheduler\.New\(`).FindAllStringSubmatch(nc, -1)
	declared := regexp.MustCompile(`Scheduler:\s*(\w+)`).FindAllStringSubmatch(nc, -1)

	// 两边都可能含非调度器标识（如 `Scheduler: nil` 不会匹配 \w+，安全）。
	if len(defs) != len(declared) {
		t.Errorf("调度器定义数 %d != 运行时里登记的 Scheduler 字段数 %d —— 有渠道只做了一半",
			len(defs), len(declared))
	}
	if len(defs) < 20 {
		t.Errorf("只解析到 %d 个调度器定义，疑似源码结构变化导致正则失效（当前应有 20+ 个渠道）", len(defs))
	}
}

// stripComments 去掉 Go 行注释，供下文的正则使用。
//
// 为什么必须剥（2026-10-30 实测踩到）：本文件的注释里**引用**了被断言代码的形状
// （例如「旧写法是 22 行手抄的 `go xxxSch.Run(sctx)`」「Scheduler: xxxSch」），
// 不剥注释时正则会把注释文字当成代码，于是刚改成表驱动就报"仍有逐个启动"、
// 刚对齐 22 个调度器就报"23 != 22"——两条都是**假红**。
// 静态文本断言的通病：断言对象与解释文字共处一个文本空间时必须先分离。
// 注意：这里只剥 `//`（本仓断言的目标都是语句行）；`/* */` 与字符串内的 `//` 不处理，
// 一旦将来真的用到，应换成 go/parser + go/ast 而非继续加正则。
func stripComments(code string) string {
	var b strings.Builder
	for _, line := range strings.Split(code, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}
