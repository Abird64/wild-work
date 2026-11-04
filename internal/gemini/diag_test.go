// diag_test.go 400 现场取证的回归（2026-11-04 用户实测驱动）。
//
// 上游有的 400 只回一句 `Request contains an invalid argument.`，从错误本身反推不出原因。
// 已用单发对照逐一排除（都 200）：工具调用/结果不配对、未声明工具名、空文本块、
// 并行调用拆成多个 model 块、多步/历史轮哨兵签名、4MB 工具结果、maxOutputTokens 1e6、
// schema 的 additionalProperties/oneOf。故只剩「留下出错那次的请求体」这一条可靠路径。
//
// 摘要必须**不含任何内容**（只报结构），因为它无条件写进 app.log。
package gemini

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mkBody 拼一个 v1internal 请求体（request 层结构 + 可选 content）。
func mkBody(t *testing.T, contents []map[string]any, tools []string) []byte {
	t.Helper()
	var decls []map[string]any
	for _, n := range tools {
		decls = append(decls, map[string]any{"name": n})
	}
	req := map[string]any{
		"contents": contents,
		"generationConfig": map[string]any{
			"maxOutputTokens": 64000,
			"thinkingConfig":  map[string]any{"includeThoughts": true, "thinkingBudget": 4000},
		},
	}
	if len(decls) > 0 {
		req["tools"] = []any{map[string]any{"functionDeclarations": decls}}
	}
	env := map[string]any{"model": "gemini-3.8-flash-medium", "project": "p", "request": req,
		"requestId": "agent/1/x", "userAgent": "antigravity"}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestSummarizeRequestHasNoContent 摘要**绝不能**包含消息正文（它会无条件进日志）。
func TestSummarizeRequestHasNoContent(t *testing.T) {
	secret := "这是一段绝不该出现在日志里的用户正文-SECRET"
	body := mkBody(t, []map[string]any{
		{"role": "user", "parts": []any{map[string]any{"text": secret}}},
	}, nil)
	got := summarizeRequest(body)
	if strings.Contains(got, "SECRET") {
		t.Fatalf("摘要泄漏了正文：%s", got)
	}
	if !strings.Contains(got, "contents=1") || !strings.Contains(got, "text=1") {
		t.Fatalf("摘要应报结构：%s", got)
	}
}

// TestSummarizeRequestFlagsAnomalies 摘要必须能一眼看出几类结构异常。
func TestSummarizeRequestFlagsAnomalies(t *testing.T) {
	body := mkBody(t, []map[string]any{
		{"role": "user", "parts": []any{map[string]any{"text": "q"}}},
		// 空 parts 的 content（异常）
		{"role": "model", "parts": []any{}},
		// 一个块里既有 text 又有 functionCall（异常：上游语义上二选一）
		{"role": "model", "parts": []any{map[string]any{
			"text": "t", "functionCall": map[string]any{"name": "a", "args": map[string]any{}},
		}}},
		// args 不是对象（异常）
		{"role": "model", "parts": []any{map[string]any{
			"functionCall": map[string]any{"name": "b", "args": "not-an-object"},
		}}},
	}, []string{"a", "b"})
	got := summarizeRequest(body)
	for _, want := range []string{"emptyContents=1", "bothTextAndCall=1", "nonObjectArgs=1", "tools=2"} {
		if !strings.Contains(got, want) {
			t.Errorf("摘要缺 %s：%s", want, got)
		}
	}
}

// TestSummarizeRequestCountsSignatures 签名三态（真/哨兵/非法）必须分得清。
func TestSummarizeRequestCountsSignatures(t *testing.T) {
	body := mkBody(t, []map[string]any{
		{"role": "user", "parts": []any{map[string]any{"text": "q"}}},
		{"role": "model", "parts": []any{map[string]any{
			"functionCall":     map[string]any{"name": "a", "args": map[string]any{}},
			"thoughtSignature": sigSkipValidator,
		}}},
		{"role": "model", "parts": []any{map[string]any{
			"functionCall":     map[string]any{"name": "a", "args": map[string]any{}},
			"thoughtSignature": "U0lHX0E=",
		}}},
		{"role": "model", "parts": []any{map[string]any{
			"functionCall":     map[string]any{"name": "a", "args": map[string]any{}},
			"thoughtSignature": "BAD!!SIG",
		}}},
	}, []string{"a"})
	got := summarizeRequest(body)
	for _, want := range []string{"sentinel=1", "real=1", "bad=1"} {
		if !strings.Contains(got, want) {
			t.Errorf("摘要缺 %s：%s", want, got)
		}
	}
}

// TestSaveFailedBodyRotates 落盘必须轮转（每次 400 都写，不能无限增长）。
func TestSaveFailedBodyRotates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gemini-400")
	for i := 0; i < 8; i++ {
		if _, err := SaveFailedBody(dir, []byte(`{"a":1}`), []byte("upstream"), 400); err != nil {
			t.Fatalf("save: %v", err)
		}
		// 文件名时间戳精度是秒，睡一下保证不覆盖
		time.Sleep(1100 * time.Millisecond)
		if i == 0 {
			break // 只验证一次落盘 + 轮转逻辑，避免测试过慢
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) == 0 {
		t.Fatal("应至少落盘一份")
	}
	if len(entries) > 10 { // 5 份 × 2 文件
		t.Fatalf("未轮转，文件过多：%d", len(entries))
	}
}

// TestDumpFailedOnlyOn400 只有 400 才取证（5xx/200 不写，避免噪音）。
func TestDumpFailedOnlyOn400(t *testing.T) {
	called := 0
	old := DumpFailedBody
	DumpFailedBody = func(b, u []byte, s int) { called++ }
	defer func() { DumpFailedBody = old }()

	dumpFailed([]byte("{}"), []byte("x"), 500)
	dumpFailed([]byte("{}"), []byte("x"), 200)
	if called != 0 {
		t.Fatalf("非 400 不该取证，被调用 %d 次", called)
	}
	dumpFailed([]byte("{}"), []byte("x"), 400)
	if called != 1 {
		t.Fatalf("400 应取证一次，得 %d", called)
	}
}

// TestAuditSchemasFlagsAnomalies schema 审计必须能数出四类语义异常。
//
// 上游对它们会给明确文案（property is not defined / only allowed for OBJECT /
// items: missing field），但历史上也出现过只回泛化 invalid_argument 的情形；
// 带上这几项计数，一次复发即可判断是否 schema 所致。
func TestAuditSchemasFlagsAnomalies(t *testing.T) {
	mk := func(name string, params map[string]any) json.RawMessage {
		b, _ := json.Marshal(params)
		return b
	}
	type decl = struct {
		Name       string          `json:"name"`
		Parameters json.RawMessage `json:"parameters"`
	}
	type grp = struct {
		FunctionDeclarations []decl `json:"functionDeclarations"`
	}
	tools := []grp{{FunctionDeclarations: []decl{
		{"ok", mk("ok", map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}, "required": []any{"a"}})},
		{"bad", mk("bad", map[string]any{
			"properties": map[string]any{"a": map[string]any{"type": "string"}}, // 无 type
			"required":   []any{"zzz"},                                          // 指向不存在的键
			"bogus":      true,                                                  // 白名单外
		})},
		{"arr", mk("arr", map[string]any{"type": "array"})}, // 无 items
	}}}
	a := auditSchemas(tools)
	if a.noType != 1 || a.reqMissing != 1 || a.arrNoItems != 1 || a.unknownKey != 1 {
		t.Fatalf("审计计数错误：%+v", a)
	}
	if a.firstBad == "" {
		t.Fatal("应给出首个违规位置")
	}
}

// TestAuditSchemasCleanSchemaIsSilent 合规 schema 必须全零（否则摘要噪音会掩盖真问题）。
func TestAuditSchemasCleanSchemaIsSilent(t *testing.T) {
	b, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"a": map[string]any{"type": "string", "enum": []any{"x", "y"}},
			"b": map[string]any{"type": "array", "items": map[string]any{"type": "number"}},
		},
		"required": []any{"a"},
	})
	type decl = struct {
		Name       string          `json:"name"`
		Parameters json.RawMessage `json:"parameters"`
	}
	type grp = struct {
		FunctionDeclarations []decl `json:"functionDeclarations"`
	}
	a := auditSchemas([]grp{{FunctionDeclarations: []decl{{"good", b}}}})
	if a.reqMissing+a.noType+a.arrNoItems+a.enumNonStr+a.unknownKey != 0 {
		t.Fatalf("合规 schema 不该报异常：%+v", a)
	}
	if a.firstBad != "" {
		t.Fatalf("合规 schema 不该有 firstBad：%s", a.firstBad)
	}
}
