package provider

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestBuiltinKindsCoversAllKindConsts 护栏：provider.go 里声明的**每一个** `Kind` 字符串常量
// 都必须出现在 `BuiltinKinds()` 里。
//
// 为什么需要这条（2026-10-29 审核实测驱动）：`BuiltinKinds()` 是手写列表，与上面的 Kind 常量
// 是**两份名单**。漏一个的代价是「两条渠道同时坏」：
//   - 该渠道不能用于三场景（I5 白名单不认它，提交即报「渠道前缀不是已知渠道」，还提示"是不是拼错了"）
//   - 自定义渠道**可以**占用这个内建名（R66 的重名护栏靠的就是本函数）⇒ 面板打自定义池、
//     对话走内建渠道，静默错路由
//
// 实例：`Doubao` / `Doubaowork` 在 2026-10-27 接入后漏加，直到 10-29 独立审核才发现。
// 判据取「解析源码里的 Kind 常量」而不是「另写一份常量清单」——再写一份就又变成第三份名单。
func TestBuiltinKindsCoversAllKindConsts(t *testing.T) {
	kinds := kindConstNames(t)
	if len(kinds) == 0 {
		t.Fatal("没解析出任何 Kind 常量（解析逻辑或源码结构变了）")
	}
	builtin := BuiltinKinds()

	var missing []string
	for _, name := range kinds {
		// Kind 常量的**值**就是渠道标识（如 Doubao = "doubao"），BuiltinKinds 的键是值。
		val := constStringValue(t, name)
		if val == "" {
			t.Logf("跳过无法取值的常量 %s（可能不是字符串常量）", name)
			continue
		}
		if !builtin[val] {
			missing = append(missing, val)
		}
	}
	if len(missing) > 0 {
		t.Errorf("BuiltinKinds() 漏了 %d 个 Kind 常量：%v\n"+
			"后果：这些渠道不能用于三场景，且自定义渠道可以用它们的名字（R66 重名护栏失效）",
			len(missing), missing)
	}
	t.Logf("已覆盖 %d 个 Kind 常量", len(kinds))
}

// kindConstNames 从 provider.go 源码里解析出所有 `Kind` 类型的字符串常量名。
func kindConstNames(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "provider.go", nil, 0)
	if err != nil {
		t.Fatalf("解析 provider.go: %v", err)
	}
	var out []string
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			// 只认显式声明了类型 `Kind` 的（`Doubao Kind = "doubao"`）。
			ident, ok := vs.Type.(*ast.Ident)
			if !ok || ident.Name != "Kind" {
				continue
			}
			for _, n := range vs.Names {
				if n.Name == "_" {
					continue
				}
				out = append(out, n.Name)
			}
		}
	}
	return out
}

// constStringValue 取某常量名的实际值（直接读源码里的基本字面量）。
func constStringValue(t *testing.T, name string) string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "provider.go", nil, 0)
	if err != nil {
		t.Fatalf("解析 provider.go: %v", err)
	}
	var val string
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			ident, ok := vs.Type.(*ast.Ident)
			if !ok || ident.Name != "Kind" {
				continue
			}
			for i, n := range vs.Names {
				if n.Name != name || i >= len(vs.Values) {
					continue
				}
				bl, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || bl.Kind != token.STRING {
					continue
				}
				val = strings.Trim(bl.Value, `"`)
			}
		}
	}
	return val
}

var _ = os.Getwd // 保持 import 干净（若将来需要路径解析时用）
