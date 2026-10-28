package app

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"wild-work/internal/config"
)

// sceneBody 构造一次合法提交（三场景齐全），extra 覆盖/补充顶层键。
func sceneBody(t *testing.T, scenes []sceneSubmit, extra map[string]any) string {
	t.Helper()
	body := map[string]any{
		"scenes":          scenes,
		"default_channel": "workbuddy",
		"max_tokens_cap":  32000,
	}
	for k, v := range extra {
		body[k] = v
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return string(raw)
}

// goodScenes 三场景基线：三主模型互不相同、备胎非空且不自环、首备胎跨渠道。
func goodScenes() []sceneSubmit {
	return []sceneSubmit{
		{Alias: "fast", From: "workbuddy/deepseek-v4.1-flash", To: []string{"raccoon/sn-kimi-k3", "traework/deepseek-v3"}},
		{Alias: "code", From: "workbuddy/claude-sonnet-4.5", To: []string{"traework/claude-sonnet-4.5"}},
		{Alias: "complex", From: "raccoon/sn-kimi-k3-max", To: []string{"workbuddy/deepseek-v4.1-flash"}},
	}
}

// cfgFilePath 取该 App 的 config.json 路径。
func cfgFilePath(a *App) string { return a.cfgPath }

// readCfgBytes 读 config.json 原始字节（不存在 = nil）。
func readCfgBytes(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	return raw
}

// seedOtherEntries 在 a.cfg 里塞入「不属于三场景」的既有内容：
// model_map 其它键 + 一条无关降级链规则。用于验证整体替换陷阱（SR-001 §3.1）。
func seedOtherEntries(t *testing.T, a *App) {
	t.Helper()
	a.mu.Lock()
	a.cfg.Compat.ModelMap = map[string]string{
		"gpt-fast*": "workbuddy/deepseek-v4.1-flash", // 用户自建的通配别名 + 非场景键
		"fast":      "workbuddy/deepseek-v4.1-flash", // 场景键（会被本端点覆盖）
	}
	a.cfg.Compat.DefaultChannel = "raccoon"
	a.cfg.Compat.MaxTokensCap = 8192
	a.cfg.ModelFallback = []config.ModelFallbackRule{
		{From: "workbuddy/deepseek-v4.1-flash", To: []string{"traework/deepseek-v3"}}, // 场景槽位（旧值）
		{From: "oczen/glm-4.6", To: []string{"cline/muse-spark"}},                     // 用户自建，必须留存
	}
	a.mu.Unlock()
}

// TestSceneRoutingHappyPath 合法提交：200 + 三场景落地 + 一次落盘 + 双 syncer 各推一次。
func TestSceneRoutingHappyPath(t *testing.T) {
	a := newPanelApp(t, "127.0.0.1", "")
	seedOtherEntries(t, a)
	mux := panelMux(a)

	var compatCalls, fbCalls int
	var gotCompatMap map[string]string
	var gotRules []config.ModelFallbackRule
	a.SetCompatSyncer(func(dc string, cap int, mm map[string]string) {
		compatCalls++
		gotCompatMap = mm
	})
	a.SetModelFallbackSyncer(func(rules []config.ModelFallbackRule) {
		fbCalls++
		gotRules = rules
	})

	path := cfgFilePath(a)
	before := readCfgBytes(t, path)

	w := doReq(mux, "POST", "/api/config/scene_routing", sceneBody(t, goodScenes(), nil), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("合法提交应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	var res SceneRoutingResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("解析响应: %v (%s)", err, w.Body.String())
	}
	if !res.OK || res.DryRun {
		t.Errorf("响应 ok=%v dry_run=%v", res.OK, res.DryRun)
	}

	// 内存态：三个别名键就位。
	a.mu.Lock()
	mm := a.cfg.Compat.ModelMap
	fallback := a.cfg.ModelFallback
	dc, cap := a.cfg.Compat.DefaultChannel, a.cfg.Compat.MaxTokensCap
	a.mu.Unlock()
	for _, sc := range goodScenes() {
		if got := mm[sc.Alias]; got != sc.From {
			t.Errorf("model_map[%s] = %q，期望 %q", sc.Alias, got, sc.From)
		}
	}
	if dc != "workbuddy" || cap != 32000 {
		t.Errorf("default_channel=%q max_tokens_cap=%d，期望 workbuddy/32000", dc, cap)
	}

	// 三条场景规则 + 用户自建规则，共 4 条。
	if len(fallback) != 4 {
		t.Fatalf("model_fallback 应 4 条（3 场景 + 1 自建），实际 %d: %+v", len(fallback), fallback)
	}
	byFrom := map[string][]string{}
	for _, r := range fallback {
		byFrom[r.From] = r.To
	}
	for _, sc := range goodScenes() {
		got, ok := byFrom[sc.From]
		if !ok {
			t.Fatalf("缺少主模型 %q 的规则", sc.From)
		}
		if strings.Join(got, ",") != strings.Join(sc.To, ",") {
			t.Errorf("主模型 %q 的备胎 = %v，期望 %v（顺序有语义，SR-001 §1.1）", sc.From, got, sc.To)
		}
	}
	if _, ok := byFrom["oczen/glm-4.6"]; !ok {
		t.Error("用户自建规则 oczen/glm-4.6 被清掉了（整体替换陷阱）")
	}

	// 一次落盘：文件确实被写。
	after := readCfgBytes(t, path)
	if string(before) == string(after) {
		t.Error("config.json 未变化——保存没有落盘")
	}

	// 双 syncer 各 1 次。
	if compatCalls != 1 || fbCalls != 1 {
		t.Errorf("syncer 调用次数 compat=%d fb=%d，期望各 1", compatCalls, fbCalls)
	}
	if gotCompatMap["fast"] != "workbuddy/deepseek-v4.1-flash" {
		t.Errorf("compatSyncer 收到的 model_map 不含新值: %v", gotCompatMap)
	}
	if len(gotRules) != 4 {
		t.Errorf("fbSyncer 收到 %d 条规则，期望 4", len(gotRules))
	}
}

// TestSceneRoutingRejectsAliasInModel ① I4/SD-8：from 或 to 写成别名必须 400，且不落盘。
func TestSceneRoutingRejectsAliasInModel(t *testing.T) {
	cases := []struct {
		name  string
		mutFn func([]sceneSubmit) []sceneSubmit
	}{
		{"from 等于别名", func(s []sceneSubmit) []sceneSubmit {
			s[0].From = "fast"
			return s
		}},
		{"from 带渠道前缀但是别名", func(s []sceneSubmit) []sceneSubmit {
			s[0].From = "workbuddy/fast"
			return s
		}},
		{"to 等于别名", func(s []sceneSubmit) []sceneSubmit {
			s[0].To = []string{"code"}
			return s
		}},
		{"to 含别名（非首位）", func(s []sceneSubmit) []sceneSubmit {
			s[1].To = []string{"raccoon/sn-kimi-k3", "complex"}
			return s
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newPanelApp(t, "127.0.0.1", "")
			seedOtherEntries(t, a)
			mux := panelMux(a)
			path := cfgFilePath(a)

			w := doReq(mux, "POST", "/api/config/scene_routing", sceneBody(t, tc.mutFn(goodScenes()), nil), nil)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("应 400，实际 %d: %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "scene_alias_in_model") {
				t.Errorf("错误码应为 scene_alias_in_model，实际 %s", w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "永不触发") {
				t.Errorf("文案须说清后果（整条链永不触发），实际 %s", w.Body.String())
			}
			// 校验失败不得落盘（文件逐字节未变）。
			if got := readCfgBytes(t, path); string(got) != "" {
				t.Errorf("校验失败却写入了 config.json：%s", string(got))
			}
			// 内存态也不得被改。
			a.mu.Lock()
			dc := a.cfg.Compat.DefaultChannel
			a.mu.Unlock()
			if dc != "raccoon" {
				t.Errorf("校验失败却改了内存态 default_channel=%q", dc)
			}
		})
	}
}

// TestSceneRoutingRejectsMapMismatch ② I8：回填的 model_map[alias] 与 scene.from 不一致 ⇒ 400。
func TestSceneRoutingRejectsMapMismatch(t *testing.T) {
	a := newPanelApp(t, "127.0.0.1", "")
	seedOtherEntries(t, a)
	mux := panelMux(a)
	path := cfgFilePath(a)

	mismatch := map[string]string{
		"fast":     "workbuddy/deepseek-v4.1-flash", // 场景提交的是 raccoon/... → 冲突
		"gpt-fast": "workbuddy/deepseek-v4.1-flash",
	}
	scenes := goodScenes()
	// 换成一个既不撞自环、也与回填值不一致的主模型。
	scenes[0].From = "oczen/glm-4.6-free"
	scenes[0].To = []string{"raccoon/sn-kimi-k3"}
	w := doReq(mux, "POST", "/api/config/scene_routing", sceneBody(t, scenes, map[string]any{"model_map": mismatch}), nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("应 400，实际 %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "scene_map_mismatch") {
		t.Errorf("错误码应为 scene_map_mismatch，实际 %s", w.Body.String())
	}
	if got := readCfgBytes(t, path); string(got) != "" {
		t.Errorf("校验失败却写入了 config.json：%s", string(got))
	}
}

// TestSceneRoutingRejectsBadAliasSet ③ SD-9：别名集合不是恰好三个必须 400。
func TestSceneRoutingRejectsBadAliasSet(t *testing.T) {
	cases := []struct {
		name   string
		scenes func() []sceneSubmit
	}{
		{"少一个（只给两个）", func() []sceneSubmit {
			s := goodScenes()
			return s[:2]
		}},
		{"多一个（第四个未知别名）", func() []sceneSubmit {
			s := goodScenes()
			return append(s, sceneSubmit{Alias: "chat", From: "workbuddy/x", To: []string{"raccoon/y"}})
		}},
		{"重复别名顶替缺失别名", func() []sceneSubmit {
			s := goodScenes()
			s[2].Alias = "fast" // 3 个但集合缺 complex
			return s
		}},
		{"空数组", func() []sceneSubmit { return nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newPanelApp(t, "127.0.0.1", "")
			seedOtherEntries(t, a)
			mux := panelMux(a)
			path := cfgFilePath(a)

			w := doReq(mux, "POST", "/api/config/scene_routing", sceneBody(t, tc.scenes(), nil), nil)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("应 400，实际 %d: %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "scene_bad_shape") {
				t.Errorf("错误码应为 scene_bad_shape，实际 %s", w.Body.String())
			}
			if got := readCfgBytes(t, path); string(got) != "" {
				t.Errorf("校验失败却写入了 config.json：%s", string(got))
			}
		})
	}
}

// TestSceneRoutingDryRunDoesNotPersist dry_run 只校验+回清单，不落盘、不推 syncer。
func TestSceneRoutingDryRunDoesNotPersist(t *testing.T) {
	a := newPanelApp(t, "127.0.0.1", "")
	seedOtherEntries(t, a)
	mux := panelMux(a)
	var compatCalls, fbCalls int
	a.SetCompatSyncer(func(string, int, map[string]string) { compatCalls++ })
	a.SetModelFallbackSyncer(func([]config.ModelFallbackRule) { fbCalls++ })

	// 先真实提交一次，拿到一个稳定的落盘基线。
	if w := doReq(mux, "POST", "/api/config/scene_routing", sceneBody(t, goodScenes(), nil), nil); w.Code != 200 {
		t.Fatalf("基线提交应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	compatCalls, fbCalls = 0, 0 // 只统计 dry_run 期间的推送
	path := cfgFilePath(a)
	before := readCfgBytes(t, path)

	// 换一组值再 dry_run：内容应完全不变。
	alt := goodScenes()
	alt[0].To = []string{"oczen/glm-4.6"}
	w := doReq(mux, "POST", "/api/config/scene_routing", sceneBody(t, alt, map[string]any{"dry_run": true, "default_channel": "oczen", "max_tokens_cap": 4096}), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("dry_run 应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	var res SceneRoutingResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if !res.DryRun || res.DefaultChannel != "oczen" || res.MaxTokensCap != 4096 {
		t.Errorf("dry_run 清单不对: %+v", res)
	}
	var dryFastTo []string
	for _, r := range res.ModelFallback {
		if r.From == "workbuddy/deepseek-v4.1-flash" {
			dryFastTo = r.To
		}
	}
	if strings.Join(dryFastTo, ",") != "oczen/glm-4.6" {
		t.Errorf("dry_run 未回将发生的变更（fast 备胎应为 oczen/glm-4.6）: %v", dryFastTo)
	}

	after := readCfgBytes(t, path)
	if string(before) != string(after) {
		t.Errorf("dry_run 改了 config.json（应逐字节相同）\nbefore=%s\nafter=%s", string(before), string(after))
	}
	if compatCalls != 0 || fbCalls != 0 {
		t.Errorf("dry_run 不应推 syncer，实际 compat=%d fb=%d", compatCalls, fbCalls)
	}

	// 内存态未被 dry_run 覆盖。
	a.mu.Lock()
	dc, cap := a.cfg.Compat.DefaultChannel, a.cfg.Compat.MaxTokensCap
	fb := a.cfg.ModelFallback
	a.mu.Unlock()
	if dc != "workbuddy" || cap != 32000 {
		t.Errorf("dry_run 改了内存态 default_channel=%q cap=%d", dc, cap)
	}
	for _, r := range fb {
		if r.From == "workbuddy/deepseek-v4.1-flash" && strings.Join(r.To, ",") == "oczen/glm-4.6" {
			t.Error("dry_run 改了内存态 model_fallback")
		}
	}
}

// TestSceneRoutingIdempotent 幂等：同一组三场景连提交两次 ⇒ model_map/model_fallback 深度相等。
func TestSceneRoutingIdempotent(t *testing.T) {
	a := newPanelApp(t, "127.0.0.1", "")
	seedOtherEntries(t, a)
	mux := panelMux(a)

	if w := doReq(mux, "POST", "/api/config/scene_routing", sceneBody(t, goodScenes(), nil), nil); w.Code != 200 {
		t.Fatalf("第一次提交应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	a.mu.Lock()
	firstMap := map[string]string{}
	for k, v := range a.cfg.Compat.ModelMap {
		firstMap[k] = v
	}
	firstRules := append([]config.ModelFallbackRule(nil), a.cfg.ModelFallback...)
	path := cfgFilePath(a)
	firstBytes := readCfgBytes(t, path)
	a.mu.Unlock()

	if w := doReq(mux, "POST", "/api/config/scene_routing", sceneBody(t, goodScenes(), nil), nil); w.Code != 200 {
		t.Fatalf("第二次提交应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	a.mu.Lock()
	secondRules := a.cfg.ModelFallback
	secondMap := a.cfg.Compat.ModelMap
	a.mu.Unlock()

	if len(firstMap) != len(secondMap) {
		t.Fatalf("model_map 条数变了：%d → %d", len(firstMap), len(secondMap))
	}
	for k, v := range firstMap {
		if secondMap[k] != v {
			t.Errorf("model_map[%s] 从 %q 漂移成 %q", k, v, secondMap[k])
		}
	}
	if len(firstRules) != len(secondRules) {
		t.Fatalf("规则条数变了：%d → %d\n第一次 %+v\n第二次 %+v", len(firstRules), len(secondRules), firstRules, secondRules)
	}
	for i := range firstRules {
		if firstRules[i].From != secondRules[i].From {
			t.Errorf("规则 %d 主模型漂移 %q → %q", i, firstRules[i].From, secondRules[i].From)
		}
		if strings.Join(firstRules[i].To, ",") != strings.Join(secondRules[i].To, ",") {
			t.Errorf("规则 %q 备胎漂移 %v → %v", firstRules[i].From, firstRules[i].To, secondRules[i].To)
		}
	}
	// 幂等提交后连文件内容都相同（顺序稳定）。
	if got := readCfgBytes(t, path); string(got) != string(firstBytes) {
		t.Error("幂等重复提交后 config.json 字节发生变化（写入顺序不稳定）")
	}
}

// TestSceneRoutingPreservesUnrelatedConfig 整体替换陷阱：其它键/其它规则/未提及字段全不变。
func TestSceneRoutingPreservesUnrelatedConfig(t *testing.T) {
	a := newPanelApp(t, "127.0.0.1", "")
	seedOtherEntries(t, a)
	a.mu.Lock()
	a.cfg.Compat.ModelMap["code"] = "traework/old-code" // 场景键旧值（应被覆盖）
	otherRuleTo := append([]string(nil), a.cfg.ModelFallback[1].To...)
	a.mu.Unlock()
	mux := panelMux(a)

	if w := doReq(mux, "POST", "/api/config/scene_routing", sceneBody(t, goodScenes(), nil), nil); w.Code != 200 {
		t.Fatalf("应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	a.mu.Lock()
	mm := a.cfg.Compat.ModelMap
	var oczenTo []string
	for _, r := range a.cfg.ModelFallback {
		if r.From == "oczen/glm-4.6" {
			oczenTo = r.To
		}
	}
	a.mu.Unlock()

	if got := mm["gpt-fast*"]; got != "workbuddy/deepseek-v4.1-flash" {
		t.Errorf("非场景 model_map 键被改动/清空：%q", got)
	}
	if strings.Join(oczenTo, ",") != strings.Join(otherRuleTo, ",") {
		t.Errorf("非场景规则被改动：%v → %v", otherRuleTo, oczenTo)
	}
	if got := mm["code"]; got != "workbuddy/claude-sonnet-4.5" {
		t.Errorf("场景键 code 未覆盖旧值：%q", got)
	}
}

// TestSceneRoutingAdapterSignal 表格驱动不到的分支：非 JSON 请求体 ⇒ 400 可读文案。
func TestSceneRoutingBadJSON(t *testing.T) {
	a := newPanelApp(t, "127.0.0.1", "")
	mux := panelMux(a)
	w := doReq(mux, "POST", "/api/config/scene_routing", "{not json", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("非法 JSON 应 400，实际 %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "scene_bad_shape") {
		t.Errorf("错误码应为 scene_bad_shape，实际 %s", w.Body.String())
	}
}

// TestSceneRoutingMissingToIsRejected I2 走复用判据（validateFallbackRules），验证不是重写。
func TestSceneRoutingMissingToIsRejected(t *testing.T) {
	a := newPanelApp(t, "127.0.0.1", "")
	mux := panelMux(a)
	scenes := goodScenes()
	scenes[1].To = nil
	path := cfgFilePath(a)
	w := doReq(mux, "POST", "/api/config/scene_routing", sceneBody(t, scenes, nil), nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("To 为空应 400，实际 %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "降级链为空") {
		t.Errorf("应复用 SetModelFallback 的文案，实际 %s", w.Body.String())
	}
	if got := readCfgBytes(t, path); string(got) != "" {
		t.Errorf("校验失败却写入了 config.json")
	}
}
