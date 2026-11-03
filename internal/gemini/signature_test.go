// signature_test.go Gemini 4/3 思考签名（thoughtSignature）回归（2026-11-03 用户实测）。
//
// 现场：用 Gemini 跑工具调用必报
//
//	400 Function call is missing a thought_signature in functionCall parts…
//	    function call `default_api:bash`, position 39.
//
// 规则全部来自官方 thought-signatures 文档（不是推断）：
//
//	· 并行调用只有第一个 FC 带签名；
//	· **当前轮**每一步的第一个 FC 必须回传；
//	· 只校验当前轮（最近一条含文本的 user 消息之后）；
//	· 客户端丢签名时用占位签名 `skip_thought_signature_validator` 跳过校验。
package gemini

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// TestSignatureFromExtraContent 官方 OpenAI 兼容口径：extra_content.google.thought_signature。
func TestSignatureFromExtraContent(t *testing.T) {
	c := map[string]any{
		"id":   "call_1",
		"type": "function",
		"extra_content": map[string]any{
			"google": map[string]any{"thought_signature": "U0lHX0E="},
		},
		"function": map[string]any{"name": "bash", "arguments": `{"cmd":"ls"}`},
	}
	if got := thoughtSignatureOf(c); got != "U0lHX0E=" {
		t.Fatalf("应取到 U0lHX0E=，得 %q", got)
	}
	// 驼峰写法（部分实现）
	c2 := map[string]any{"extra_content": map[string]any{"google": map[string]any{"thoughtSignature": "U0lHX0I="}}}
	if got := thoughtSignatureOf(c2); got != "U0lHX0I=" {
		t.Fatalf("驼峰也应取到，得 %q", got)
	}
	// 反向护栏：没有签名时返回空（不能凭空造）
	if got := thoughtSignatureOf(map[string]any{"id": "x"}); got != "" {
		t.Fatalf("无签名应返回空串，得 %q", got)
	}
}

// TestEnsureSignaturesOnlyCurrentTurn 只给「当前轮」的每一步补签名。
//
// 关键边界：更早的历史轮**不校验也不补**——补了等于污染历史，
// 而不补又必须保证当前轮齐备，两者不能混。
func TestEnsureSignaturesOnlyCurrentTurn(t *testing.T) {
	mk := func(role string, text string, fcs ...string) content {
		var parts []part
		if text != "" {
			parts = append(parts, part{Text: text})
		}
		for _, n := range fcs {
			parts = append(parts, part{FunctionCall: &functionCall{Name: n, Args: alphaJSON("{}")}})
		}
		return content{Role: role, Parts: parts}
	}
	// 历史轮：user 提问 → model 旧工具调用（没有签名，不该被补）
	// 当前轮：user 提问 → model 新工具调用（必须补）
	out := []content{
		mk("user", "旧问题"),
		mk("model", "", "old_tool"),
		mk("user", "旧结果"), // 历史里这条没有文本 ⇒ 不算「当前轮起点」
		mk("model", "", "old_tool2"),
		mk("user", "新问题"),       // ← 当前轮从这里开始
		mk("model", "", "bash"), // 必须补签名
	}
	ensureSignatures(out)

	// 历史轮不得被补
	if out[1].Parts[0].ThoughtSignature != "" {
		t.Errorf("历史轮的 FC 不该补签名，得 %q", out[1].Parts[0].ThoughtSignature)
	}
	// 当前轮必须补
	got := out[5].Parts[0].ThoughtSignature
	if got != sigSkipValidator {
		t.Fatalf("当前轮首个 FC 必须补占位签名，得 %q", got)
	}
}

// TestEnsureSignaturesKeepsRealSignature 已有真签名时必须原样保留（不能被占位覆盖）。
func TestEnsureSignaturesKeepsRealSignature(t *testing.T) {
	out := []content{
		{Role: "user", Parts: []part{{Text: "问题"}}},
		{Role: "model", Parts: []part{{
			FunctionCall:     &functionCall{Name: "bash", Args: alphaJSON("{}")},
			ThoughtSignature: "UkVBTF9TSUc=",
		}}},
	}
	ensureSignatures(out)
	if out[1].Parts[0].ThoughtSignature != "UkVBTF9TSUc=" {
		t.Fatalf("真签名必须保留，得 %q", out[1].Parts[0].ThoughtSignature)
	}
}

// TestEnsureSignaturesParallelOnlyFirst 并行调用只给第一个 FC 补签名。
//
// 官方文档：并行时签名只挂在第一个 functionCall 上，后续同步骤的不带。
// 给每个都补等于伪造了一份上游没给的签名。
func TestEnsureSignaturesParallelOnlyFirst(t *testing.T) {
	out := []content{
		{Role: "user", Parts: []part{{Text: "查两个城市"}}},
		{Role: "model", Parts: []part{
			{FunctionCall: &functionCall{Name: "w", Args: alphaJSON("{}")}},
			{FunctionCall: &functionCall{Name: "w", Args: alphaJSON("{}")}},
		}},
	}
	ensureSignatures(out)
	if out[1].Parts[0].ThoughtSignature != sigSkipValidator {
		t.Fatalf("第一个 FC 应补签名，得 %q", out[1].Parts[0].ThoughtSignature)
	}
	if out[1].Parts[1].ThoughtSignature != "" {
		t.Fatalf("并行第二个 FC 不该有签名，得 %q", out[1].Parts[1].ThoughtSignature)
	}
}

// TestRequestCarriesSignatureEndToEnd 端到端：客户端回传的签名要出现在上游请求体里。
func TestRequestCarriesSignatureEndToEnd(t *testing.T) {
	body := []byte(`{"model":"gemini-3.8-flash","messages":[
		{"role":"user","content":"列出文件"},
		{"role":"assistant","content":"","tool_calls":[
			{"id":"call_1","type":"function",
			 "extra_content":{"google":{"thought_signature":"UkVBTF9B"}},
			 "function":{"name":"bash","arguments":"{\"cmd\":\"ls\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"a.txt"}
	]}`)
	out, _, err := buildInternalBody(body, "proj")
	if err != nil {
		t.Fatalf("buildInternalBody 失败：%v", err)
	}
	if !strings.Contains(string(out), `"thoughtSignature":"UkVBTF9B"`) {
		t.Fatalf("上游请求体必须带上客户端回传的签名：%s", out)
	}
}

// TestRequestFillsDummyWhenClientDroppedSignature 客户端丢了 extra_content 时补占位签名。
//
// 这是本仓的真实情形：pi / Codex 用标准 OpenAI 结构重放历史，非标的
// extra_content 会被丢掉 ⇒ 此前 100% 报 400。补哨兵值后校验被跳过。
func TestRequestFillsDummyWhenClientDroppedSignature(t *testing.T) {
	body := []byte(`{"model":"gemini-3.8-flash","messages":[
		{"role":"user","content":"列出文件"},
		{"role":"assistant","content":"","tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"cmd\":\"ls\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"a.txt"}
	]}`)
	out, _, err := buildInternalBody(body, "proj")
	if err != nil {
		t.Fatalf("buildInternalBody 失败：%v", err)
	}
	if !strings.Contains(string(out), sigSkipValidator) {
		t.Fatalf("缺签名时必须补哨兵值，否则上游 400：%s", out)
	}
}

// TestResponseEmitsSignature 上游返回的签名要透传给客户端（extra_content）。
func TestResponseEmitsSignature(t *testing.T) {
	src := sse(
		`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"bash","args":{"cmd":"ls"}},"thoughtSignature":"VV9TSUc="}]}}]}`,
		`{"candidates":[{"finishReason":"STOP"}]}`,
		"[DONE]",
	)
	got, err := io.ReadAll(newGeminiStream(strings.NewReader(src), "gemini/gemini-3.8-flash"))
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if !strings.Contains(string(got), `"thought_signature":"VV9TSUc="`) {
		t.Fatalf("必须按官方口径放进 extra_content.google.thought_signature：%s", got)
	}
	// 结构校验：不能是随手拼的字符串
	var ok bool
	for _, line := range strings.Split(string(got), "\n") {
		line = strings.TrimPrefix(line, "data: ")
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var f map[string]any
		if json.Unmarshal([]byte(line), &f) != nil {
			continue
		}
		ch, _ := f["choices"].([]any)
		if len(ch) == 0 {
			continue
		}
		d, _ := ch[0].(map[string]any)["delta"].(map[string]any)
		tcs, _ := d["tool_calls"].([]any)
		if len(tcs) == 0 {
			continue
		}
		tc, _ := tcs[0].(map[string]any)
		ec, _ := tc["extra_content"].(map[string]any)
		g, _ := ec["google"].(map[string]any)
		if g["thought_signature"] == "VV9TSUc=" {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("extra_content 结构不对：%s", got)
	}
}

// TestIsInvalidArgumentOnlyGeneric 只认泛化的「参数无效」文案，不吞有具体归因的 400。
//
// 现场（2026-11-03 用户实测）：先是 `missing a thought_signature`（有归因），
// 之后变成 `Request contains an invalid argument`（无归因）。后者才需要
// 「换哨兵重试」来区分原因；把前者也重试等于白打一次上游。
func TestIsInvalidArgumentOnlyGeneric(t *testing.T) {
	if !isInvalidArgument([]byte(`{"error":{"code":400,"message":"Request contains an invalid argument.","status":"INVALID_ARGUMENT"}}`)) {
		t.Error("泛化文案必须命中")
	}
	for _, exclude := range []string{
		`{"error":{"message":"Function call is missing a thought_signature in functionCall parts.","status":"INVALID_ARGUMENT"}}`,
		`{"error":{"message":"User location is not supported for the API use.","status":"FAILED_PRECONDITION"}}`,
	} {
		if isInvalidArgument([]byte(exclude)) {
			t.Errorf("有具体归因的 400 不该命中重试判据：%s", exclude)
		}
	}
}

// TestHasRealSignature 只有非哨兵签名才算「真签名」（决定要不要降级重试）。
func TestHasRealSignature(t *testing.T) {
	mk := func(sig string) []byte {
		return []byte(`{"request":{"contents":[{"parts":[{"functionCall":{"name":"bash"},"thoughtSignature":"` + sig + `"}]}]}}`)
	}
	if hasRealSignature(mk(sigSkipValidator)) {
		t.Error("哨兵值不算真签名")
	}
	if hasRealSignature(mk("")) {
		t.Error("空签名不算真签名")
	}
	if !hasRealSignature(mk("U09NRV9SRUFMX1NJRw==")) {
		t.Error("非哨兵签名必须算真签名")
	}
}

// TestForceSentinelReplacesReal 降级重试必须把所有真签名换成哨兵。
func TestForceSentinelReplacesReal(t *testing.T) {
	out := []content{
		{Role: "user", Parts: []part{{Text: "q"}}},
		{Role: "model", Parts: []part{
			{FunctionCall: &functionCall{Name: "a", Args: alphaJSON("{}")}, ThoughtSignature: "UkVBTF8x"},
			{FunctionCall: &functionCall{Name: "b", Args: alphaJSON("{}")}, ThoughtSignature: "UkVBTF8y"},
		}},
	}
	forceSentinelSignatures(out)
	for _, p := range out[1].Parts {
		if p.ThoughtSignature != sigSkipValidator {
			t.Fatalf("应全部换成哨兵，得 %q", p.ThoughtSignature)
		}
	}
}

// TestBuildInternalBodyModeSentinel 走 mode=true 时，请求体里不得残留真签名。
func TestBuildInternalBodyModeSentinel(t *testing.T) {
	body := []byte(`{"model":"gemini-3.8-flash","messages":[
		{"role":"user","content":"q"},
		{"role":"assistant","content":"","tool_calls":[
			{"id":"c1","type":"function",
			 "extra_content":{"google":{"thought_signature":"U1RBTEVfU0lH"}},
			 "function":{"name":"bash","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"ok"}
	]}`)
	out, _, err := buildInternalBodyMode(body, "p", true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "U1RBTEVfU0lH") {
		t.Fatalf("降级模式不得残留真签名：%s", out)
	}
	if !strings.Contains(string(out), sigSkipValidator) {
		t.Fatalf("降级模式必须用哨兵：%s", out)
	}
	// 默认模式仍保留真签名（不误伤正常路径）
	out2, _, err := buildInternalBody(body, "p")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out2), "U1RBTEVfU0lH") {
		t.Fatalf("默认模式应保留客户端回传的签名：%s", out2)
	}
}

// TestInvalidSignatureBecomesSentinel 非 base64 的签名**不得原样上行**。
//
// 现场（2026-11-03 用户实测，上游原样报错）：
//
//	Invalid value at 'request.contents[1].parts[0].thought_signature' (TYPE_BYTES),
//	Base64 decoding failed for "STALE_INVALID_SIGNATURE_XYZ"
//
// 即：`thought_signature` 是 TYPE_BYTES，必须是合法 base64。客户端可能把它截断/拼接/
// 换成自己的编码，原样转发就会得到那句零信息量的「Request contains an invalid argument」。
// 处置：非法值一律换成哨兵（跳过校验），而不是把客户端手里的坏值传上去。
func TestInvalidSignatureBecomesSentinel(t *testing.T) {
	// 各种非法形态：截断、含非 base64 字符、长度不是 4 的倍数
	for _, bad := range []string{
		"STALE_INVALID_SIGNATURE_XYZ", // 含下划线（URL 字母表）—— 实测上游报 Base64 decoding failed
		"not base64!!",
		"a-b_c",    // 短横/下划线都不收
		"========", // 纯 padding
	} {
		if isValidSignature(bad) {
			t.Errorf("%q 不该被判为合法 base64", bad)
		}
		if got := sanitizeSignature(bad); got != "" {
			t.Errorf("%q 应被清洗成空串，得 %q", bad, got)
		}
	}
	// 合法形态：标准/URL、有/无 padding 都要通过
	for _, ok := range []string{
		"U0lHX0E=",       // 标准带 padding
		"U0lHX0E",        // 标准无 padding
		"abc",            // 3 字符是合法 raw base64
		"aGVsbG8",        // "hello" 的 raw base64
		sigSkipValidator, // 哨兵是**特判**：上游先认字面量再解码，所以必须放行
	} {
		if !isValidSignature(ok) {
			t.Errorf("%q 应被判为合法 base64", ok)
		}
	}
}

// TestEndToEndInvalidSignatureDoesNotReachUpstream 端到端：坏签名不进请求体。
func TestEndToEndInvalidSignatureDoesNotReachUpstream(t *testing.T) {
	body := []byte(`{"model":"gemini-3.8-flash","messages":[
		{"role":"user","content":"q"},
		{"role":"assistant","content":"","tool_calls":[
			{"id":"c1","type":"function",
			 "extra_content":{"google":{"thought_signature":"BROKEN_NOT_BASE64!!"}},
			 "function":{"name":"bash","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"ok"}
	]}`)
	out, _, err := buildInternalBody(body, "p")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "BROKEN_NOT_BASE64") {
		t.Fatalf("坏签名绝不能上行：%s", out)
	}
	if !strings.Contains(string(out), sigSkipValidator) {
		t.Fatalf("应改用哨兵兜底：%s", out)
	}
}
