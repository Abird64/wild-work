package app

import (
	"errors"
	"fmt"
	"log"
	"strings"

	"wild-work/internal/backuplink"
	"wild-work/internal/config"
	"wild-work/internal/ledger"
	"wild-work/internal/loopguard"
	"wild-work/internal/oczen"
	"wild-work/internal/platform"
	"wild-work/internal/provider"
)

// SetBodyLog 开关请求体构成摘要记录（诊断用：定位上下文为何膨胀）。
func (a *App) SetBodyLog(on bool) error {
	a.mu.Lock()
	a.cfg.BodyLog = on
	err := config.Save(a.cfg, a.cfgPath)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	ledger.SetBodyTrace(on)
	if on {
		log.Printf("请求体摘要记录已开启（写 data/ledger/body-trace-*.jsonl）")
	} else {
		log.Printf("请求体摘要记录已关闭")
	}
	return nil
}

// SetCheckinTimes 更新自动签到时间（HH:MM）并立即唤醒两个调度器。
func (a *App) SetCheckinTimes(times []string) error {
	minutes, err := config.ParseClockTimes(times)
	if err != nil {
		return err
	}
	clean := normalizeMinutes(minutes)
	if len(clean) == 0 {
		return errors.New("请至少保留一个签到时间")
	}
	formatted := make([]string, 0, len(clean))
	for _, m := range clean {
		formatted = append(formatted, fmt.Sprintf("%02d:%02d", m/60, m%60))
	}
	a.mu.Lock()
	a.cfg.Schedule.CheckinTimes = formatted
	a.cfg.Schedule.CheckinHours = nil
	err = config.Save(a.cfg, a.cfgPath)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	for _, rt := range a.runtimesSnapshot() {
		if rt != nil && rt.Scheduler != nil {
			rt.Scheduler.SetCheckinMinutes(clean)
		}
	}
	log.Printf("自动签到时间已更新：%s", strings.Join(formatted, "、"))
	return nil
}

// SetLoopGuard 更新思考链死循环检测（发癫拦截）开关：保存配置并即时生效。
// 生效机制：loopguard 的全局 atomic 开关，各渠道 Stream 在每次请求时读取，
// 热切换无需重启（下一个流式请求即按新值）。
func (a *App) SetLoopGuard(on bool) error {
	a.mu.Lock()
	a.cfg.Features.LoopGuard = on
	err := config.Save(a.cfg, a.cfgPath)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	loopguard.SetEnabled(on)
	log.Printf("思考链死循环检测已%s", map[bool]string{true: "开启", false: "关闭"}[on])
	return nil
}

// SetListen 修改 API 监听主机 + 端口：保存配置并热切换监听。
func (a *App) SetListen(host string, port int) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("端口无效：%d", port)
	}
	next := config.Listen{Host: host, Port: port}

	a.mu.Lock()
	defer a.mu.Unlock()
	// 对外暴露（非环回地址）必须同时设置管理密码：否则局域网任何设备
	// 都可无凭据访问面板（含登录入口、退出按钮）。与 SetAdminPassword 互成先决。
	if requireRemoteAuth(next) && strings.TrimSpace(a.cfg.AdminPass) == "" {
		return errors.New("监听非 127.0.0.1 时必须先设置「管理密码」（设置面板中一并保存）")
	}
	if err := a.serveLocked(next.Addr()); err != nil {
		return fmt.Errorf("监听 %s 失败（可能被占用）：%v", next.Addr(), err)
	}
	a.cfg.Listen = next
	if err := config.Save(a.cfg, a.cfgPath); err != nil {
		log.Printf("save config after listen change: %v", err)
	}
	log.Printf("API 监听已切换至 %s", next.Addr())
	return nil
}

// SetAdminPassword 修改管理面板密码（空串 = 关闭面板鉴权）。
// 当前监听为对外地址时不允许清空（见 SetListen）；改密码后所有旧会话立即失效。
func (a *App) SetAdminPassword(pass string) error {
	pass = strings.TrimSpace(pass)
	a.mu.Lock()
	if pass == "" && requireRemoteAuth(a.cfg.Listen) {
		a.mu.Unlock()
		return errors.New("监听非 127.0.0.1 时不允许清空管理密码")
	}
	a.cfg.AdminPass = pass
	err := config.Save(a.cfg, a.cfgPath)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	a.auth.dropAll() // 密码变更：旧 cookie 的 label 失配，已自动失效，这里同步清内存
	log.Printf("管理密码已更新：%s", map[bool]string{true: "已启用", false: "已关闭"}[pass != ""])
	return nil
}

// SetAPIKey 修改 API 密钥。
func (a *App) SetAPIKey(key string) error {
	key = strings.TrimSpace(key)
	a.mu.Lock()
	a.cfg.APIKey = key
	err := config.Save(a.cfg, a.cfgPath)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	a.handler.SetAPIKey(key)
	log.Printf("API-Key 已更新")
	return nil
}

// SetAutostart 设置/取消开机自启。
func (a *App) SetAutostart(on bool) error {
	if err := platform.SetAutostart(on); err != nil {
		return err
	}
	if on {
		log.Printf("已开启开机自启")
	} else {
		log.Printf("已关闭开机自启")
	}
	return nil
}

// AutostartEnabled 当前是否已开机自启。
func (a *App) AutostartEnabled() bool { return platform.AutostartEnabled() }

// SetCompat 保存模型名路由配置（compat 段）并写回 config.json。
func (a *App) SetCompat(defaultChannel string, maxTokensCap int, modelMap map[string]string) error {
	if modelMap == nil {
		modelMap = map[string]string{}
	}
	for k, v := range modelMap {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("model_map 的值不能为空（应为 channel/model）")
		}
		// ⚠️ **别名判定必须排在「含 /」形状判定之前**：裸别名（如 `complex`）天然不含斜杠，
		// 先判形状会把它误报成「需为 channel/model 形式」，用户照着格式提示改还是错。
		// 这与 SR-005b 前端 sceneProblems 里踩过的是同一个坑（那边已修，这里补上）。
		if hit, ok := sceneAliasIn(v); ok {
			return fmt.Errorf("scene_alias_in_model: model_map 的 %q 指向了别名 %q（%q）——"+
				"要写 渠道/模型 全路径；写别名会让整条链永不触发且不报错（R127 I4/SD-8）", k, hit, v)
		}
		if strings.Index(v, "/") <= 0 {
			return fmt.Errorf("model_map 值 %q 需为 channel/model 形式", v)
		}
	}
	// I8 跨字段一致性：场景别名键的新值必须与该别名**当前**的降级链 From 一致，
	// 否则会写出「别名指向 A、链主模型是 B」的分裂态（降级静默失效）。
	// 判据在锁外取旧 cfg 快照：锁内的 a.cfg 即将被覆盖，拿不到"当前链 From"。
	curChainFrom := a.sceneChainFromSnapshot()
	for alias, from := range curChainFrom {
		if v, ok := modelMap[alias]; ok && strings.TrimSpace(v) != "" && strings.TrimSpace(v) != from {
			return fmt.Errorf("scene_map_mismatch: 别名 %q 指向 %q，但它的链主模型是 %q——"+
				"两者不一致会让降级静默失效；请改用 POST /api/config/scene_routing 一次改两处（R127 I8）",
				alias, v, from)
		}
	}
	a.mu.Lock()
	a.cfg.Compat.DefaultChannel = defaultChannel
	a.cfg.Compat.MaxTokensCap = maxTokensCap
	a.cfg.Compat.ModelMap = modelMap
	err := config.Save(a.cfg, a.cfgPath)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	// 热更新外层兼容层的路由表：不同步的话新映射要重启才生效。
	if a.compatSyncer != nil {
		a.compatSyncer(defaultChannel, maxTokensCap, modelMap)
	}
	log.Printf("模型名路由配置已更新：default_channel=%q max_tokens_cap=%d model_map=%d 条",
		defaultChannel, maxTokensCap, len(modelMap))
	return nil
}

// SetModelFallback 保存模型降级链（设置面板可视化编辑）。
// 校验：From/To 均须 "channel/model" 全路径；From 全局唯一；每链 To 至少一项、
// 不含 From 自身（不自环）；空规则列表 = 清空禁用。保存即热生效（fbSyncer）。
func (a *App) SetModelFallback(rules []config.ModelFallbackRule) error {
	if err := validateFallbackRules(rules); err != nil {
		return err
	}
	a.mu.Lock()
	a.cfg.ModelFallback = rules
	err := config.Save(a.cfg, a.cfgPath)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	if a.fbSyncer != nil {
		a.fbSyncer(rules)
	}
	log.Printf("模型降级链已更新（%d 条规则，保存即热生效）", len(rules))
	return nil
}

// SetModelFallbackSyncer 注入模型降级链热更新回调（由 main.go 注入：推给 server.Handler）。
func (a *App) SetModelFallbackSyncer(fn func(rules []config.ModelFallbackRule)) { a.fbSyncer = fn }

// SetReasoningEfforts 保存思考强度注入映射（设置面板可视化编辑）。
// 校验：键（模型 ID，含 `*` 全局默认）与值（档位）trim 后均非空；删除 = 键缺席；
// 空 map = 清空禁用。哑管道：不校验档位取值合法性，上游吃什么档位由渠道模型自己说了算。
// 保存即热生效（efSyncer）。
func (a *App) SetReasoningEfforts(efforts map[string]string) error {
	clean := make(map[string]string, len(efforts))
	for k, v := range efforts {
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k == "" {
			return fmt.Errorf("模型 ID 不能为空（`*` 为合法的全局默认键）")
		}
		if v == "" {
			return fmt.Errorf("模型 %q 的思考档位不能为空", k)
		}
		clean[k] = v
	}
	a.mu.Lock()
	a.cfg.ReasoningEfforts = clean
	err := config.Save(a.cfg, a.cfgPath)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	if a.efSyncer != nil {
		a.efSyncer(clean)
	}
	log.Printf("思考强度映射已更新（%d 条，保存即热生效）", len(clean))
	return nil
}

// SetReasoningEffortsSyncer 注入思考强度映射热更新回调（由 main.go 注入：推给 server.Handler）。
func (a *App) SetReasoningEffortsSyncer(fn func(efforts map[string]string)) { a.efSyncer = fn }

// SetOczenSyncer 注入 oczen key 热更新回调（由 main.go 注入：写入渠道 client）。
func (a *App) SetOczenSyncer(fn func(key string)) { a.oczenSyncer = fn }

// TestOczenKey 验证 OpenCodeZen 凭证可用性（设置面板「测试」按钮）。
// apiKey 非空时先用该 key 临时写入渠道 client（测试后恢复原值），空则直接测当前生效凭证。
// 判定：200/429 均 return ok（200=配额正常，429=连通但限流，对匿名 key 属正常现象）。
//
// 测试后经同一个 backuplink.Replay 编排（与真实对话同源，见 issue #117）：直连被上游按地区
// 拒绝时机内自动改走备用通道；viaBackup 报告答复是否来自备用出口，面板据此如实展示——
// 原先只看 403 是看不出「代理到底有没有被用上」的。
func (a *App) TestOczenKey(apiKey string) (int, string, bool, error) {
	rt := a.runtime(provider.Oczen)
	if rt == nil {
		return 0, "", false, fmt.Errorf("oczen 渠道未启用")
	}
	c, ok := rt.Upstream.(*oczen.Client)
	if !ok {
		return 0, "", false, fmt.Errorf("oczen 渠道类型异常")
	}
	// 临时写入候选 key（含空串 = 测匿名）；结束后恢复配置值，保证面板「取消」不残留半生效状态
	orig := a.cfg.OczenAPIKey
	c.SetAPIKey(apiKey)
	defer c.SetAPIKey(orig)
	status, body, via, err := c.TestKey()
	if err != nil {
		return 0, body, via, fmt.Errorf("无法连接 OpenCodeZen（检查网络/代理）：%v", err)
	}
	return status, body, via, nil
}

// SetBackupProxy 保存全局**备用上游通道**（单一代理地址，空串 = 关闭）并即时生效。
// 同时接收 oczenKey：设置 OpenCodeZen 自定义 API key（空 = 回匿名凭证）。
//
// 生效方式与旧的「每渠道代理」根本不同：此处**不重建任何渠道 Transport**，
// 只换 backuplink 内部的快照指针（决议 R57）——备用通道不参与常规请求，
// 渠道在命中地区拒绝时自己按需取用，因此没有「热更新接线表」可漏（旧实现
// 启动表 13 个渠道、热更新表只 10 个，traecode 两个表都没进）。
func (a *App) SetBackupProxy(raw, oczenKey string) error {
	raw = strings.TrimSpace(raw)
	if _, err := backuplink.Parse(raw); err != nil { // 空串合法（= 关闭）
		return err
	}
	a.mu.Lock()
	a.cfg.BackupProxy = raw
	if oczenKey != "\u0000unset\u0000" { // 哨兵：请求未携带该字段时不改动现有 key
		a.cfg.OczenAPIKey = strings.TrimSpace(oczenKey)
	}
	err := config.Save(a.cfg, a.cfgPath)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	if err := backuplink.Set(raw); err != nil { // 已校验过，这里不应再失败
		return err
	}
	if a.oczenSyncer != nil {
		a.oczenSyncer(a.cfg.OczenAPIKey)
	}
	if raw == "" {
		log.Printf("备用上游通道已关闭（所有请求直连；上游回地区拒绝将原样透传）")
	} else {
		log.Printf("备用上游通道已配置：%s（仅渠道命中地区拒绝时自动取用，不影响常规请求）", raw)
	}
	return nil
}

// validateFallbackRules 校验模型降级链规则集合：From/To 均须 channel/model 全路径、
// From 全局唯一、每链 To 至少一项、不含 From 自身（不自环）。原地 trim 并回填 To（去空项）。
//
// 为什么必须抽出来共用：`SetModelFallback`（整表保存）与新原子端点 `SetSceneRouting`
// （三场景 + 三条链一次落盘）必须是**同一份**判据。各写一份就会漂移成
// 「单独保存通过、原子保存拒绝」这类自相矛盾的文案（SR-001 §2 的 I2/I3 由机制层拦）。
func validateFallbackRules(rules []config.ModelFallbackRule) error {
	seen := map[string]bool{}
	for i := range rules {
		r := &rules[i]
		r.From = strings.TrimSpace(r.From)
		if strings.Index(r.From, "/") <= 0 {
			return fmt.Errorf("规则 %d 的主模型 %q 需为 channel/model 形式", i+1, r.From)
		}
		if seen[r.From] {
			return fmt.Errorf("主模型 %q 重复（每个主模型只允许一条规则）", r.From)
		}
		seen[r.From] = true
		tos := make([]string, 0, len(r.To))
		seenTo := make(map[string]bool, len(r.To))
		for _, to := range r.To {
			to = strings.TrimSpace(to)
			if to == "" {
				continue
			}
			if strings.Index(to, "/") <= 0 {
				return fmt.Errorf("主模型 %q 的备胎 %q 需为 channel/model 形式", r.From, to)
			}
			if to == r.From {
				return fmt.Errorf("主模型 %q 的备胎不能是它自己", r.From)
			}
			// 同一个备胎重复出现只保留首次（保持用户排的顺序）。
			// 为什么去重而不是报错（2026-10-29 审核发现的低危项）：重复项运行期只会让
			// `pickModelFallback` 对同一备胎多试一次（无效但不出错），而报错会让
			// 「从别处粘一串备胎名」这种高频操作直接失败。静默去重 + 保留首现语义最省事。
			if seenTo[to] {
				continue
			}
			seenTo[to] = true
			tos = append(tos, to)
		}
		if len(tos) == 0 {
			return fmt.Errorf("主模型 %q 的降级链为空（至少填一个备胎模型）", r.From)
		}
		r.To = tos
	}
	return nil
}

// SetCompatSyncer 注入 compat 热更新回调（由 main.go 注入，指向 gateway.SetCompat）。
func (a *App) SetCompatSyncer(fn func(defaultChannel string, maxTokensCap int, modelMap map[string]string)) {
	a.compatSyncer = fn
}
