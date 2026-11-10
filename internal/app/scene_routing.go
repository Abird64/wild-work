package app

import (
	"fmt"
	"log"
	"sort"
	"strings"

	"wild-work/internal/auto"
	"wild-work/internal/config"
	"wild-work/internal/provider"
)

// sceneAliases 可编辑场景别名枚举。
//
// R165 起从三场景（fast/code/complex）收编为**一个自定义场景** `custom`：
// 另半个名额给了「自动选路」——`auto` 是保留名，由 internal/auto 的打分引擎当场决策，
// 不写进 model_map（见 server.chatCompletions 的保留名判定），因此**不是**这里的一员。
//
// 三个旧别名仍可被客户端继续使用（config.migrateSceneAliases 保留其映射或已把 complex
// 的链继承到 custom），但面板不再提供编辑器，`SetSceneRouting` 也不再维护它们。
//
// 顺序即写入 model_map / model_fallback 的顺序，保证同一输入的提交结果深度相等（幂等）。
var sceneAliases = []string{"custom"}

// legacySceneAliases 已退役但仍可能活在配置里的三个旧场景别名（R165）。
//
// 它们不再可编辑，但**仍是客户端能写的模型名**（config.migrateSceneAliases 会把 complex
// 的链继承到 custom，fast/code 则可能残留在老配置里）。因此 I4 的「模型名不得写成别名」
// 判据必须把它们算进来：写 `workbuddy/fast` 同样是「永不触发的静默失效」。
var legacySceneAliases = []string{"fast", "code", "complex"}

// isSceneAlias 判断 s 是否为**可编辑**场景别名之一（提交端只认这一组）。
func isSceneAlias(s string) bool {
	for _, a := range sceneAliases {
		if s == a {
			return true
		}
	}
	return false
}

// isAliasName 判断 s 是否属于「任何会作为路由别名生效的名字」：可编辑别名 ∪ 退役别名 ∪
// 自动选路的保留名。I4 用它（比 isSceneAlias 宽），提交端仍用 isSceneAlias（严格）。
func isAliasName(s string) bool {
	if s == auto.Alias || isSceneAlias(s) {
		return true
	}
	for _, a := range legacySceneAliases {
		if s == a {
			return true
		}
	}
	return false
}

// sceneAliasIn 报告 v 里的「模型名部分」是否落在三场景别名上，返回命中的别名。
//
// 为什么不能只比整串：SD-8 说「from/to 不得等于别名」，但**真正会静默失效的形态是带渠道
// 前缀的** `workbuddy/fast`（MS-006 实测）。运行期 SplitModelID 拆出 up="fast"，而真实请求的
// model 是解析后的 deepseek-v4.1-flash ⇒ 永不相等 ⇒ 整条链静默永不触发，且不报任何错。
// 故整串命中与「前缀/模型名」命中共算一档。
func sceneAliasIn(v string) (string, bool) {
	if isAliasName(v) {
		return v, true
	}
	if i := strings.Index(v, "/"); i > 0 && isAliasName(v[i+1:]) {
		return v[i+1:], true
	}
	return "", false
}

// scenePrefixOf 取 scene 里模型 ID 的**字面前缀**（第一个 / 之前的部分，原样不改大小写）。
// 无法拆分（无 / 或 / 在首尾）时返回空串。
func scenePrefixOf(id string) string {
	if i := strings.IndexByte(id, '/'); i > 0 {
		return id[:i]
	}
	return ""
}

// sceneChannelKnown 判断一个 channel/model 的渠道前缀是否属于已知渠道集合。
//
// 判两档（先字面、后归一），因为「渠道前缀」有两种合法写法：
//  1. 字面前缀就是渠道 kind（`workbuddy/x`、运行时自定义渠道 `stepfun/x`）；
//  2. 字面前缀是**上游命名空间**（`cline-free/*`，provider.ModelNamespaces / R59）——
//     它对客户端直接透传上游 ID。这一档若只比字面就会误杀，故再用
//     provider.SplitModelID 把命名空间映射回渠道 kind 复核。
//
// set 的键统一小写（内建 kind 本身即小写；自定义渠道名 Validate 时也归一小写）。
func sceneChannelKnown(set map[string]bool, id string) bool {
	if pre := scenePrefixOf(id); pre != "" && set[strings.ToLower(pre)] {
		return true
	}
	if k, _, ok := provider.SplitModelID(id); ok && set[strings.ToLower(k.String())] {
		return true
	}
	return false
}

// sceneKindOf 把 scene 里的模型 ID 归一到渠道 kind（上游命名空间映射回其渠道）。
// 返回 false 表示无法拆分（形状问题，调用方已另有判据拦截）。
func sceneKindOf(id string) (provider.Kind, bool) {
	k, _, ok := provider.SplitModelID(id)
	return k, ok
}

// knownSceneChannels 返回「已知渠道」集合（键小写）+ 排序后的展示清单（I5 文案用）。
//
// 取源与 renameCustomRefs / isCustomChannel 同路，三层并集：
//  1. provider.BuiltinKinds()——它**已把 ModelNamespaces（如 cline-free）并进来**，
//     自己再补一份命名空间反而会重复且易漂移；
//  2. App 运行时表里的自定义渠道（rt.Custom，热更新后立刻生效的那一份）；
//  3. config.CustomChannels 里的名字（盘上那一份；与 2 互为兜底，改名窗口内两者可能不同步）。
//
// 展示清单排序后返回：同一份配置每次报错文案必须逐字一致（否则前端 diff 抖动）。
func (a *App) knownSceneChannels() (map[string]bool, []string) {
	set := provider.BuiltinKinds()
	for _, rt := range a.runtimesSnapshot() {
		if rt != nil && rt.Custom {
			if n := strings.ToLower(strings.TrimSpace(rt.Kind.String())); n != "" {
				set[n] = true
			}
		}
	}
	a.mu.Lock()
	for _, cc := range a.cfg.CustomChannels {
		if n := strings.ToLower(strings.TrimSpace(cc.Name)); n != "" {
			set[n] = true
		}
	}
	a.mu.Unlock()

	names := make([]string, 0, len(set))
	for k := range set {
		names = append(names, k)
	}
	sort.Strings(names)
	return set, names
}

// sceneChainFromSnapshot 返回「场景别名 → 它当前降级链的 From」快照（只读、加锁取副本）。
//
// 用途：旧端点 `SetCompat` 做 I8 跨字段一致校验时需要知道"链主模型现在是谁"，
// 而它自己的写入路径会把 `a.cfg.Compat.ModelMap` 覆盖掉 —— 必须在校验阶段先取快照。
//
// 只取**能对上的**别名：某个别名指向的模型没有对应规则时（已经是坏状态），不参与校验，
// 避免用一条不存在的链去阻止用户修好它。
func (a *App) sceneChainFromSnapshot() map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]string, len(sceneAliases))
	mm := a.cfg.Compat.ModelMap
	for _, alias := range sceneAliases {
		from := strings.TrimSpace(mm[alias])
		if from == "" {
			continue
		}
		for _, r := range a.cfg.ModelFallback {
			if strings.TrimSpace(r.From) == from {
				out[alias] = from
				break
			}
		}
	}
	return out
}

// sceneModelCatalog 返回「渠道 kind（小写）→ 该渠道当前可解析的模型 ID 集合」，供 I6 判存在性。
//
// 只读、**绝不发网络请求**：唯一取源是 server.Handler.CachedChannelModels()——它与
// `/api/fees` 面板同源（上次成功拉取的动态表，过期也照用；陈旧性由后台 30min 刷新收敛）。
// 不调 FetchModels 的原因：那是「现在去上游拉」，一次保存动作不该产生上游请求（也违反 §0.1）。
//
// 第二个返回值 false = 目录整体不可用（handler 未注入 / 尚无任何渠道目录条目）。
// 此时 I6 **一律不判**：从「没有目录」不能推出「模型不存在」，否则测试环境与冷启动会
// 给每一个模型都刷一条假警告。
func (a *App) sceneModelCatalog() (map[string]map[string]bool, bool) {
	if a.handler == nil {
		return nil, false
	}
	models := a.handler.CachedChannelModels()
	if len(models) == 0 {
		return nil, false
	}
	out := make(map[string]map[string]bool, len(models))
	for k, infos := range models {
		set := make(map[string]bool, len(infos)*2)
		for _, mi := range infos {
			raw := strings.TrimSpace(mi.ID)
			if raw == "" {
				continue
			}
			pub := provider.PublicModelID(k, raw)
			// 三种写法都算「目录里有」：上游裸 ID、对外 ID（`渠道/模型` 或命名空间整串）、
			// 以及归一后的规范 ID（cline 的新旧两种写法落同一串，R52/R59）。
			set[raw] = true
			set[pub] = true
			set[provider.CanonicalModelID(pub)] = true
		}
		if len(set) > 0 {
			out[strings.ToLower(k.String())] = set
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// sceneRoutingWarnings 计算两类「不阻断保存」的提醒（SR-001 §2 的 I6 / I7、§2.1 的理由）。
//
// rules 与 sceneAliases 必须**同序等长**（调用方在 sceneAliases 固定顺序上逐条 append 生成），
// 故这里按下标取别名，不另建索引。
//
// I6（模型存在性）：目录里查不到 ⇒ 警告。目录会漂移（上游按批次下发），把它当硬错误会让用户
// 无法「先把链配好、等上游补目录」。仅当该渠道**有目录条目**时才判——无条目 = 无从判断。
//
// I7（to[0] 跨渠道）：首选备胎与主模型同渠道 ⇒ 警告。请求内重入按 skipSameChannel=true 只跨渠道
// （server/handler.go 的 pickModelFallback），同渠道备胎会被跳过 ⇒ 第一顺位等于失效。
// 比较用归一后的渠道 kind：`cline-free/x` 与 `cline/cline-free/x` 是同一渠道的两种写法。
func (a *App) sceneRoutingWarnings(rules []config.ModelFallbackRule) []string {
	catalog, catalogOK := a.sceneModelCatalog()
	var out []string
	for i := range rules {
		r := rules[i]
		alias := ""
		if i < len(sceneAliases) {
			alias = sceneAliases[i]
		}
		ids := append([]string{r.From}, r.To...)
		for _, id := range ids {
			if !catalogOK {
				break
			}
			k, ok := sceneKindOf(id)
			if !ok {
				continue
			}
			set := catalog[strings.ToLower(k.String())]
			if set == nil {
				continue // 该渠道没有目录条目 ⇒ 无从判断，不警告
			}
			if set[id] || set[provider.CanonicalModelID(id)] {
				continue
			}
			out = append(out, fmt.Sprintf("场景 %q 的 %q 当前目录里没有——可能是上游暂未下发，保存后请留意", alias, id))
		}
		// I7：只在首选备胎上判（链上其它位置本就是「备而不用」，同渠道不构成误导）。
		if len(r.To) == 0 {
			continue
		}
		fk, fok := sceneKindOf(r.From)
		tk, tok := sceneKindOf(r.To[0])
		if !fok || !tok || fk != tk {
			continue
		}
		out = append(out, fmt.Sprintf("场景 %q 的首选备胎 %q 与主模型 %q 同渠道（%s）——"+
			"429 时会被跳过（skipSameChannel），建议换一个渠道的", alias, r.To[0], r.From, tk.String()))
	}
	return out
}

// sceneSubmit 一条场景输入（SR-001 §1：一个场景 = 一个原子对象）。
type sceneSubmit struct {
	Alias string   `json:"alias"`
	From  string   `json:"from"`
	To    []string `json:"to"`
}

// sceneRoutingReq POST /api/config/scene_routing 的请求体。
//
// ModelMap 是可选回填（SR-001 §3.1「整体替换 ⇒ 必须回填」）：nil 时端点从当前配置自带
// 其它键（更安全，调用方不必先 GET /api/state）；携带时只用来校验 I8。
type sceneRoutingReq struct {
	Scenes         []sceneSubmit     `json:"scenes"`
	DefaultChannel string            `json:"default_channel"`
	MaxTokensCap   int               `json:"max_tokens_cap"`
	ModelMap       map[string]string `json:"model_map,omitempty"`
	DryRun         bool              `json:"dry_run"`
}

// SceneRoutingResult 原子端点响应（dry_run 与真实提交同形，供 SR-005 的 diff 预览复用）。
type SceneRoutingResult struct {
	OK             bool                       `json:"ok"`
	DryRun         bool                       `json:"dry_run"`
	DefaultChannel string                     `json:"default_channel"`
	MaxTokensCap   int                        `json:"max_tokens_cap"`
	ModelMap       map[string]string          `json:"model_map"`
	ModelFallback  []config.ModelFallbackRule `json:"model_fallback"`
	// Warnings 不阻断保存的提醒（I6 目录里查不到该模型 / I7 首选备胎与主模型同渠道）。
	//
	// 为什么用 warning 而不是 error（SR-001 §2.1）：目录会漂移（上游按批次下发），把
	// 「此刻查不到」当硬错误会让用户无法先把链配好、等上游补目录。可保存 + 如实告知。
	// omitempty：无提醒时响应形状与升级前逐字节一致（老前端不受影响）。
	Warnings []string `json:"warnings,omitempty"`
}

// SetSceneRouting 把「三场景别名 + 三条降级链」作为**单一事务**写入（SD-6 裁决的唯一写入口）。
//
// 为什么必须是一个端点：`SetCompat` 与 `SetModelFallback` 各自整体替换字段 + 各调一次
// `config.Save`（写两遍全量 config.json），两次 POST 之间任何失败都留下真实中间态——
// 「别名指向一条没有规则的模型」（降级静默失效）或「规则的 From 成为孤儿」（链永不触发）。
// 前端补偿无解：无回滚端点，回滚自身也可能失败（问题递归一次）。
//
// 落盘语义：同一把 a.mu 锁内改 model_map 的三个别名键（其它键保留）+ DefaultChannel +
// MaxTokensCap + model_fallback 里对应三条规则（按 alias→from 对齐，其它规则不动），
// **只调一次 config.Save**；成功后同一函数体内连续推 compatSyncer 与 fbSyncer
// （两者必须同路径，否则「配置已新、内存仍旧」）。
//
// 硬校验（落盘前全过才写）：
//   - I4（SD-8）：from/to 任一项等于别名 ⇒ 拒收。机制层只校「含 /」，`workbuddy/fast` 会通过，
//     而运行期 SplitModelID 拆出的真实 model 永远不等于别名 ⇒ 整条链静默永不触发且不报错
//     （MS-006 实测坐实）。
//   - I8：回填 model_map[alias] 与 scene.from 不一致 ⇒ 拒收（两处视图分裂会让降级静默失效）。
//   - SD-9：别名集合必须恰为 {fast,code,complex}（不多不少；本版只做三个）。
//
// 其余不变量（To 非空 I2、禁自环 I3、From 唯一 I1、channel/model 形状）一律走
// validateFallbackRules —— 与 `SetModelFallback` 同源复用，**不重写判据**。
func (a *App) SetSceneRouting(req sceneRoutingReq) (*SceneRoutingResult, error) {
	// ③ SD-9：别名集合恰为 {fast,code,complex}。
	if len(req.Scenes) != len(sceneAliases) {
		return nil, fmt.Errorf("scene_bad_shape: 必须恰好提交 %d 个场景（%s），实际 %d 个",
			len(sceneAliases), strings.Join(sceneAliases, "/"), len(req.Scenes))
	}
	byAlias := make(map[string]sceneSubmit, len(req.Scenes))
	for _, sc := range req.Scenes {
		alias := strings.TrimSpace(sc.Alias)
		// `auto` 是保留名（R165）：允许它出现在别名表里，会让「写 auto 得到的是用户映射的
		// 固定模型、而不是自动选路」这种静默失效成立——这也是内层保留名判定要绕过
		// model_map 的原因（两道闸一起挡，任一处漏了都不会真的坏）。
		if alias == auto.Alias {
			return nil, fmt.Errorf("scene_reserved_alias: %q 是自动选路的保留名，不能作为场景别名——"+
				"想要自动选路请把客户端 model 写成 auto；想要固定模型请写 渠道/模型 全路径", alias)
		}
		if !isSceneAlias(alias) {
			return nil, fmt.Errorf("scene_bad_shape: 未知场景别名 %q（只能是 %s）",
				alias, strings.Join(sceneAliases, "/"))
		}
		if _, dup := byAlias[alias]; dup {
			return nil, fmt.Errorf("scene_bad_shape: 场景别名 %q 重复提交", alias)
		}
		byAlias[alias] = sc
	}

	// 逐场景形状 + ① I4（别名拒收）。按 sceneAliases 固定顺序生成，保证幂等。
	sceneFrom := make(map[string]string, len(sceneAliases))
	rules := make([]config.ModelFallbackRule, 0, len(sceneAliases))
	for _, alias := range sceneAliases {
		sc := byAlias[alias]
		from := strings.TrimSpace(sc.From)
		if hit, ok := sceneAliasIn(from); ok {
			return nil, fmt.Errorf("scene_alias_in_model: 场景 %q 的主模型写成了别名 %q（%q）——"+
				"要写 渠道/模型 全路径；写别名会让整条链永不触发且不报错", alias, hit, from)
		}
		if strings.Index(from, "/") <= 0 {
			return nil, fmt.Errorf("scene_bad_shape: 场景 %q 的主模型 %q 需为 channel/model 形式", alias, from)
		}
		sceneFrom[alias] = from
		to := make([]string, 0, len(sc.To))
		for _, one := range sc.To {
			one = strings.TrimSpace(one)
			if hit, ok := sceneAliasIn(one); ok {
				return nil, fmt.Errorf("scene_alias_in_model: 场景 %q 的备胎写成了别名 %q（%q）——"+
					"要写 渠道/模型 全路径；写别名会让整条链永不触发且不报错", alias, hit, one)
			}
			to = append(to, one)
		}
		rules = append(rules, config.ModelFallbackRule{From: from, To: to})
	}
	// 复用 SetModelFallback 判据（I2 To 非空 / I3 禁自环 / I1 From 唯一 / channel-model 形状）。
	if err := validateFallbackRules(rules); err != nil {
		return nil, fmt.Errorf("scene_bad_shape: %w", err)
	}

	// ② I8：回填的 model_map 里别名指向必须与本次 from 一致。
	for _, alias := range sceneAliases {
		if old, ok := req.ModelMap[alias]; ok && old != "" && old != sceneFrom[alias] {
			return nil, fmt.Errorf("scene_map_mismatch: 别名 %q 指向 %q，但它的链主模型是 %q——"+
				"两者不一致会让降级静默失效", alias, old, sceneFrom[alias])
		}
	}

	// ⑥ compat 两项基本校验（2026-10-29 审核的低危项）：
	// `max_tokens_cap` 负数语义上是脏值（clampMaxTokens 对 <=0 视作不限制，故 -5 无害但会原样落盘）；
	// `default_channel` 空串等于把「裸名兜底渠道」关掉（路由层会回 "needs an explicit channel prefix"，
	// 用户会以为模型丢了）。两项既有端点也不校验（非回归），但原子端点作为唯一写入口应当拦住。
	if req.MaxTokensCap < 0 {
		return nil, fmt.Errorf("scene_bad_shape: 封顶 tokens 不能为负数（%d）；0 表示不限制", req.MaxTokensCap)
	}
	dc := strings.TrimSpace(req.DefaultChannel)
	if dc == "" {
		return nil, fmt.Errorf("scene_bad_shape: 默认渠道不能为空——裸名模型（不带 渠道/ 前缀）会没有兜底渠道")
	}

	// ④ I5：from/to 每一项的渠道前缀必须属于「已知渠道集合」（内建 ∪ 运行时自定义 ∪ 上游命名空间）。
	// 必须在 a.mu.Lock() 之前做：knownSceneChannels 自己要拿 a.mu（锁不可重入）。
	// 文案列出已知渠道：用户自查靠它（「是不是我把渠道名拼错了」是最高频的一类）。
	known, knownList := a.knownSceneChannels()
	for _, alias := range sceneAliases {
		ids := append([]string{sceneFrom[alias]}, byAlias[alias].To...)
		for _, id := range ids {
			id = strings.TrimSpace(id)
			if id == "" {
				continue // 空项由 validateFallbackRules 去空/判空处理
			}
			if sceneChannelKnown(known, id) {
				continue
			}
			return nil, fmt.Errorf("scene_unknown_channel: 场景 %q 的 %q 渠道前缀 %q 不是已知渠道——"+
				"已知渠道：%s（自定义渠道请在「自定义渠道」里先建好；上游命名空间如 cline-free 也可直接写）",
				alias, id, scenePrefixOf(id), strings.Join(knownList, ", "))
		}
	}

	// ⑤ I6/I7：模型存在性与首选备胎跨渠道 ⇒ **警告不阻断**（SR-001 §2.1）。
	// 必须在 dry_run 与真实提交两条路径上都算出来：dry_run 的预览要能提前看到同样的提醒。
	warnings := a.sceneRoutingWarnings(rules)

	a.mu.Lock()
	oldMap := a.cfg.Compat.ModelMap
	// 场景规则的身份 = 「From 属于旧三场景主模型或新三场景主模型」。按此对齐替换，
	// 既不会把改名后的旧主模型留成孤儿规则，也不动用户自建的其它规则（SR-001 §3.1）。
	sceneSlots := make(map[string]bool, len(sceneAliases)*2)
	for _, alias := range sceneAliases {
		if v := oldMap[alias]; v != "" {
			sceneSlots[v] = true
		}
		sceneSlots[sceneFrom[alias]] = true
	}
	kept := make([]config.ModelFallbackRule, 0, len(a.cfg.ModelFallback))
	for _, r := range a.cfg.ModelFallback {
		if sceneSlots[strings.TrimSpace(r.From)] {
			continue
		}
		kept = append(kept, r)
	}
	merged := append(kept, rules...)
	// 合并后的整表仍须满足机制层不变量（也顺带挡住磁盘上本来就有重复的旧规则）。
	if err := validateFallbackRules(merged); err != nil {
		a.mu.Unlock()
		return nil, fmt.Errorf("scene_bad_shape: %w", err)
	}

	newMap := make(map[string]string, len(oldMap)+len(sceneAliases))
	for k, v := range oldMap {
		newMap[k] = v
	}
	for _, alias := range sceneAliases {
		newMap[alias] = sceneFrom[alias]
	}
	res := &SceneRoutingResult{
		OK:             true,
		DryRun:         req.DryRun,
		DefaultChannel: req.DefaultChannel,
		MaxTokensCap:   req.MaxTokensCap,
		ModelMap:       newMap,
		ModelFallback:  merged,
		Warnings:       warnings,
	}
	if req.DryRun {
		a.mu.Unlock()
		return res, nil // 只校验 + 回清单；不落盘、不推 syncer
	}

	// 改 cfg **之前**先留一份快照：Save 失败必须整体回滚。
	//
	// 为什么不能只 return err（2026-10-29 独立审核实测抓到的缺陷）：
	// 下面的赋值发生在 Save 之前，若 Save 失败而直接返回，内存里已经是"新值"而磁盘还是旧值。
	// 更糟的是它**不会自己恢复**——`GET /api/state` 立刻把没落盘的新值回显给面板（用户以为存了），
	// 之后任意一次无关保存（如 SetReasoningEfforts）会把这批值顺手写进 config.json（永久固化）。
	// 用户收到的是"保存失败"，实际却生效了，重启前无人能发现。
	// 回滚点只有这四个字段（其余 cfg 字段本端点不碰）。
	prevDC := a.cfg.Compat.DefaultChannel
	prevCap := a.cfg.Compat.MaxTokensCap
	prevMap := a.cfg.Compat.ModelMap
	prevFallback := a.cfg.ModelFallback

	a.cfg.Compat.DefaultChannel = req.DefaultChannel
	a.cfg.Compat.MaxTokensCap = req.MaxTokensCap
	a.cfg.Compat.ModelMap = newMap
	a.cfg.ModelFallback = merged
	err := config.Save(a.cfg, a.cfgPath)
	if err != nil {
		// 锁内回滚（还没 Unlock），保证"要么全成、要么全不动"。
		a.cfg.Compat.DefaultChannel = prevDC
		a.cfg.Compat.MaxTokensCap = prevCap
		a.cfg.Compat.ModelMap = prevMap
		a.cfg.ModelFallback = prevFallback
	}
	a.mu.Unlock()
	if err != nil {
		return nil, err
	}
	// 热更新一次性推送（R-3：两个 syncer 必须同一函数体内连续调用）。
	if a.compatSyncer != nil {
		a.compatSyncer(req.DefaultChannel, req.MaxTokensCap, newMap)
	}
	if a.fbSyncer != nil {
		a.fbSyncer(merged)
	}
	log.Printf("三场景路由已原子更新：default_channel=%q max_tokens_cap=%d 场景=%d 条 model_fallback=%d 条",
		req.DefaultChannel, req.MaxTokensCap, len(sceneAliases), len(merged))
	return res, nil
}
