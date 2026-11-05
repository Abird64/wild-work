// schema_fix_test.go 工具 schema 出口一致性归一（2026-11-04，用户实测驱动）。
//
// 背景：编程 agent 会话出现**无 details** 的 `400 Request contains an invalid argument.`
// （详见 R162/R163）。单发对照发现上游对以下语义错误**会明确报错**，而它们全都是
// **我们自己的清洗动作制造出来的**：
//
//	required 指向不存在的键 → `parameters.required[0]: property is not defined`
//	有 properties 却没 type   → `parameters.properties: only allowed for OBJECT`
//	type=array 没有 items     → `parameters.items: missing field`
//	嵌套过深                  → `Message recursion depth exceeds maximum of 64`
//
// 也就是说：清洗越"完整"，越容易造出这些不一致。故在出口前统一补平。
package gemini

import (
	"encoding/json"
	"strings"
	"testing"
)

// schemaOf 跑一遍清洗并把结果反序列化出来断言。
func schemaOf(t *testing.T, in map[string]any) map[string]any {
	t.Helper()
	raw, err := canonicalRaw(sanitizeSchema(in))
	if err != nil {
		t.Fatalf("清洗失败：%v", err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("结果不是合法 JSON：%v\n%s", err, raw)
	}
	return out
}

// TestSanitizeDropsRequiredNotInProperties properties 里非对象的属性值会被丢弃，
// required 必须同步剔除，否则上游回 `property is not defined`。
func TestSanitizeDropsRequiredNotInProperties(t *testing.T) {
	out := schemaOf(t, map[string]any{
		"type":       "object",
		"properties": map[string]any{"a": map[string]any{"type": "string"}, "b": true}, // b 非对象 → 被丢
		"required":   []any{"a", "b"},
	})
	req, _ := out["required"].([]any)
	if len(req) != 1 || req[0] != "a" {
		t.Fatalf("required 应只剩 a（b 已不存在），得 %v", req)
	}
	if _, exists := out["properties"].(map[string]any)["b"]; exists {
		t.Fatal("b 不该出现在 properties 里")
	}
}

// TestSanitizeRequiredAllGoneDropsKey 全被剔掉时整个键不要（空 required 无意义）。
func TestSanitizeRequiredAllGoneDropsKey(t *testing.T) {
	out := schemaOf(t, map[string]any{
		"type":       "object",
		"properties": map[string]any{"a": 1}, // 非对象 → 丢
		"required":   []any{"a"},
	})
	if _, exists := out["required"]; exists {
		t.Fatalf("required 应被整键删除，得 %v", out["required"])
	}
}

// TestSanitizeAddsObjectTypeForProperties 有 properties 却没 type → 补 object。
func TestSanitizeAddsObjectTypeForProperties(t *testing.T) {
	out := schemaOf(t, map[string]any{
		"properties": map[string]any{"a": map[string]any{"type": "string"}},
	})
	if out["type"] != "object" {
		t.Fatalf("应补 type=object，得 %v", out["type"])
	}
}

// TestSanitizeAddsItemsForArray type=array 没 items → 补空 items（上游接受 `items:{}`）。
func TestSanitizeAddsItemsForArray(t *testing.T) {
	out := schemaOf(t, map[string]any{"type": "array"})
	items, ok := out["items"].(map[string]any)
	if !ok || len(items) != 0 {
		t.Fatalf("应补空 items，得 %v", out["items"])
	}
}

// TestSanitizeCapsRecursionDepth 深层嵌套必须在 30 层截断（上游上限 64，40 层即触发）。
func TestSanitizeCapsRecursionDepth(t *testing.T) {
	root := map[string]any{"type": "object", "properties": map[string]any{}}
	cur := root
	for i := 0; i < 50; i++ {
		nxt := map[string]any{"type": "object", "properties": map[string]any{}}
		cur["properties"].(map[string]any)["k"] = nxt
		cur = nxt
	}
	raw, err := canonicalRaw(sanitizeSchema(root))
	if err != nil {
		t.Fatal(err)
	}
	// 数一下 JSON 里 "properties" 出现次数：截断后应远小于 50
	if n := strings.Count(string(raw), `"properties"`); n > schemaMaxDepth+1 {
		t.Fatalf("深度未截断：properties 出现 %d 次（上限 %d）", n, schemaMaxDepth+1)
	}
}

// TestSanitizeKeepsValidSchemaUntouched 合法 schema 不得被改动（归一只能在必要时发生）。
func TestSanitizeKeepsValidSchemaUntouched(t *testing.T) {
	out := schemaOf(t, map[string]any{
		"type":       "object",
		"properties": map[string]any{"a": map[string]any{"type": "string"}},
		"required":   []any{"a"},
	})
	if out["type"] != "object" {
		t.Fatalf("type 被改：%v", out["type"])
	}
	req, _ := out["required"].([]any)
	if len(req) != 1 || req[0] != "a" {
		t.Fatalf("required 被改：%v", req)
	}
	if _, has := out["items"]; has {
		t.Fatal("不该凭空加 items")
	}
}
