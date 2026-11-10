package config

import "testing"

// R165：三场景收编为一个自定义场景。这些用例锁死迁移的四条边界——
// 尤其「旧名转发而不是删除」：删掉会让仍写 fast/complex 的客户端下次请求直接 400。
func TestMigrateSceneAliases(t *testing.T) {
	rule := func(from string, to ...string) ModelFallbackRule {
		return ModelFallbackRule{From: from, To: to}
	}
	cases := []struct {
		name  string
		in    map[string]string
		rules []ModelFallbackRule
		want  map[string]string
		wantR []ModelFallbackRule
	}{
		{
			name: "complex 被 custom 继承，旧三键转发同一个目标",
			in:   map[string]string{"fast": "minimax/M3F", "code": "traework/DS", "complex": "raccoon/K3"},
			rules: []ModelFallbackRule{rule("raccoon/K3", "traework/DS")},
			want:  map[string]string{"custom": "raccoon/K3", "fast": "raccoon/K3", "code": "raccoon/K3", "complex": "raccoon/K3"},
			wantR: []ModelFallbackRule{rule("raccoon/K3", "traework/DS")},
		},
		{
			name:  "custom 已存在时不覆盖用户手写的值",
			in:    map[string]string{"custom": "kilo/nemotron:free", "complex": "raccoon/K3"},
			want:  map[string]string{"custom": "kilo/nemotron:free", "fast": "kilo/nemotron:free", "code": "kilo/nemotron:free", "complex": "kilo/nemotron:free"},
			wantR: nil,
		},
		{
			name:  "无 custom 且无 complex ⇒ 一个字都不动（迁移不得制造新故障）",
			in:    map[string]string{"fast": "minimax/M3F"},
			rules: []ModelFallbackRule{rule("minimax/M3F", "x/y")},
			want:  map[string]string{"fast": "minimax/M3F"},
			wantR: []ModelFallbackRule{rule("minimax/M3F", "x/y")},
		},
		{
			name:  "完全没有场景别名时不动（空 Map 不被塞键）",
			in:    map[string]string{"gpt-4o": "workbuddy/x"},
			want:  map[string]string{"gpt-4o": "workbuddy/x"},
		},
		{
			name:  "旧主模型的备胎链挂到 custom 上（保住用户手写的顺序）",
			in:    map[string]string{"complex": "raccoon/K3"},
			rules: []ModelFallbackRule{rule("raccoon/K3", "a/1", "b/2")},
			want:  map[string]string{"custom": "raccoon/K3", "fast": "raccoon/K3", "code": "raccoon/K3", "complex": "raccoon/K3"},
			wantR: []ModelFallbackRule{rule("raccoon/K3", "a/1", "b/2")},
		},
		{
			name: "用户已自建 custom 规则时，旧规则的备胎不得并进来（避免两条同 From 抢优先级）",
			in:   map[string]string{"custom": "kilo/free", "complex": "raccoon/K3"},
			rules: []ModelFallbackRule{
				rule("kilo/free", "a/1"),
				rule("raccoon/K3", "b/2"),
			},
			want:  map[string]string{"custom": "kilo/free", "fast": "kilo/free", "code": "kilo/free", "complex": "kilo/free"},
			wantR: []ModelFallbackRule{rule("kilo/free", "a/1")},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{}
			c.Compat.ModelMap = tc.in
			c.ModelFallback = tc.rules
			migrateSceneAliases(c)
			for k, v := range tc.want {
				if got := c.Compat.ModelMap[k]; got != v {
					t.Errorf("model_map[%q] = %q，期望 %q（迁移后全表 %v）", k, got, v, c.Compat.ModelMap)
				}
			}
			if len(c.ModelFallback) != len(tc.wantR) {
				t.Fatalf("降级链条数 %d，期望 %d：%+v", len(c.ModelFallback), len(tc.wantR), c.ModelFallback)
			}
			for i, r := range tc.wantR {
				got := c.ModelFallback[i]
				if got.From != r.From || len(got.To) != len(r.To) {
					t.Errorf("规则 %d = %+v，期望 %+v", i, got, r)
					continue
				}
				for j := range r.To {
					if got.To[j] != r.To[j] {
						t.Errorf("规则 %d 备胎 %d = %q，期望 %q", i, j, got.To[j], r.To[j])
					}
				}
			}
		})
	}
}

// 幂等：normalize 每次加载都会跑迁移，第二次起必须一个字节都不再变。
func TestMigrateSceneAliasesIdempotent(t *testing.T) {
	c := &Config{}
	c.Compat.ModelMap = map[string]string{"fast": "minimax/M3F", "complex": "raccoon/K3"}
	c.ModelFallback = []ModelFallbackRule{{From: "raccoon/K3", To: []string{"traework/DS"}}}
	migrateSceneAliases(c)
	first := map[string]string{}
	for k, v := range c.Compat.ModelMap {
		first[k] = v
	}
	nRules := len(c.ModelFallback)
	migrateSceneAliases(c)
	if len(c.Compat.ModelMap) != len(first) {
		t.Fatalf("二次迁移改变了表长：%v → %v", first, c.Compat.ModelMap)
	}
	for k, v := range first {
		if c.Compat.ModelMap[k] != v {
			t.Errorf("二次迁移改动了 %q：%q → %q", k, v, c.Compat.ModelMap[k])
		}
	}
	if len(c.ModelFallback) != nRules {
		t.Errorf("二次迁移改变了降级链条数：%d → %d", nRules, len(c.ModelFallback))
	}
}
