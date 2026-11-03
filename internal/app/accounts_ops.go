package app

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/custom"
	"wild-work/internal/provider"
	"wild-work/internal/scheduler"
)

func (a *App) findRuntimeAuth(uid string) (*Runtime, *auth.Auth) {
	for _, rt := range a.runtimesSnapshot() {
		// 跳过别名渠道：map 遍历无序，若命中 TraeCode 会拿到错误 upstream（function=solo_agent），
		// 且 accountGroup 会误报 traecode；账号级操作一律归主渠道（TraeWork）。
		if rt == nil || rt.Pool == nil || rt.Alias {
			continue
		}
		if au := rt.Pool.AuthByUID(uid); au != nil {
			return rt, au
		}
	}
	return nil, nil
}

// noExplicitCheckin 不支持显式签到（手动按钮）的渠道。
// WorkBuddy 国际版改为定时自动对话保活并领取日活奖励（详见 workbuddyai.DailyCheckin），
// 无需用户手动触发，故也不提供手动签到入口；
// 千问办公无签到活动且每日积分服务端被动发放，无需领取/保活。
// QoderCN/QoderCOM 已实现 campaigns 签到，支持手动按钮（上游同一套机制）。
// 智谱清言已实现 activity-api 每日签到，支持手动按钮。
// 小浣熊已实现 setting_info 触发每日积分落账（见 internal/raccoon/checkin.go，R65），支持手动按钮。
// MonkeyCode 已实现签到（POST {console}/api/v1/users/wallet/checkin，见 internal/monkeycode/checkin.go），
// 支持手动按钮并参与批量签到 —— 故**不在**本名单内（2026-10-17 起，此前误列为「无签到」）。
// Loomy 已实现签到（宠物每日打工奖励，POST /api/v1/pet-work/rewards，0~1000 分），
// 支持手动按钮并参与批量签到 —— 故**不在**本名单内（2026-10-17 接线完成）。
// ⚠️ 它的判据是该账号当日**有效对话轮次**（由上游计算，见 internal/loomy/checkin.go 文件头）：
// 本工具是 API 网关、不产生轮次归属，故未在官方客户端用过该账号时，领取结果是
// 「未达档位」（可重试 error），**不是**「无活动」——这是如实状态，不是缺陷。
// Cline 走官方 API Key：无签到活动、Key 不过期 ⇒ 既不需手动签到也不需保活。
// OpenCodeZen 匿名通道无账号概念，既无签到也无积分。
// Kilo 免费池同为免密渠道：无账号、无签到、无积分。
// 自定义渠道无签到活动（R66）；内建名单固定，故抽成无状态的包级函数（可脱离 App 断言），
// 方法体仅补上「是否自定义渠道」这一动态判定。
func noExplicitCheckin(k provider.Kind) bool {
	return k == provider.WorkBuddyAI || k == provider.QwenWork || k == provider.Oczen || k == provider.Kilo || k == provider.ZCode || k == provider.Gemini || k == provider.Cline
}

func (a *App) noExplicitCheckinKind(k provider.Kind) bool {
	return noExplicitCheckin(k) || a.isCustomChannel(k.String())
}

// CheckinAccount 单个账号立即签到。
func (a *App) CheckinAccount(uid string) (scheduler.CheckinResult, error) {
	rt, _ := a.findRuntimeAuth(uid)
	if rt == nil || rt.Scheduler == nil {
		return scheduler.CheckinResult{}, fmt.Errorf("unknown account %s", uid)
	}
	if a.noExplicitCheckinKind(rt.Kind) {
		if rt.Kind == provider.WorkBuddyAI {
			return scheduler.CheckinResult{}, fmt.Errorf("workbuddyai 渠道无需手动签到，已定时自动领取日活奖励")
		}
		return scheduler.CheckinResult{}, fmt.Errorf("%s 渠道不支持手动签到", rt.Kind)
	}
	res, err := rt.Scheduler.CheckinAccount(uid)
	if err != nil {
		log.Printf("checkin failed uid=%s err=%v", uid, err)
		return res, err
	}
	log.Printf("checkin uid=%s ok=%t retryable=%t msg=%s remain=%d has_remain=%t", uid, res.OK, res.Retryable, res.Msg, res.Remain, res.HasRemain)
	return res, nil
}

// CheckinAll 全部账号立即签到。
func (a *App) CheckinAll() []scheduler.CheckinResult {
	results := make([]scheduler.CheckinResult, 0)
	for _, rt := range a.runtimesSnapshot() {
		// 别名渠道复用同一 Pool 且无独立 Scheduler：计入会重复签到同一批账号。
		if rt == nil || rt.Pool == nil || rt.Scheduler == nil || rt.Alias {
			continue
		}
		if a.noExplicitCheckinKind(rt.Kind) { // 无签到活动渠道，跳过
			continue
		}
		for _, st := range rt.Pool.List() {
			if st.Disabled {
				results = append(results, scheduler.CheckinResult{UID: st.UID, Msg: "账号已禁用"})
				continue
			}
			res, err := rt.Scheduler.CheckinAccount(st.UID)
			if err != nil {
				res = scheduler.CheckinResult{UID: st.UID, Msg: err.Error()}
			}
			results = append(results, res)
		}
	}
	ok := 0
	for _, r := range results {
		if r.OK {
			ok++
		}
	}
	log.Printf("批量签到完成：total=%d ok=%d failed=%d", len(results), ok, len(results)-ok)
	return results
}

// RemoveAccount 删除账号（auth 文件 + 内存池）。
// oczen / kilo 是例外：免密渠道的虚拟账号不可删除（需求约束），返回明确错误而非默默失败。
func (a *App) RemoveAccount(uid string) error {
	rt, au := a.findRuntimeAuth(uid)
	if rt == nil || au == nil {
		return fmt.Errorf("unknown account %s", uid)
	}
	if rt.Kind == provider.Oczen {
		return fmt.Errorf("OpenCodeZen 匿名通道账号不可删除")
	}
	if rt.Kind == provider.Kilo {
		return fmt.Errorf("Kilo 免费池账号不可删除")
	}
	if rt.Custom {
		var next []custom.ChannelConfig
		for _, cc := range a.cfg.CustomChannels {
			if !strings.EqualFold(cc.Name, rt.Kind.String()) {
				next = append(next, cc)
			}
		}
		return a.SetCustomChannels(next)
	}
	if au.FilePath != "" {
		_ = os.Remove(au.FilePath)
	}
	rt.Pool.Remove(uid)
	log.Printf("已删除账号 %s", shortUID(uid))
	return nil
}

// DisableAccount 停用/启用账号。
// oczen / kilo 的虚拟账号不可停用：停用后无法从面板恢复，而这两条渠道各自只有一个
// 虚拟账号，停用等于禁用整条渠道。
func (a *App) DisableAccount(uid string, disabled bool) error {
	rt, _ := a.findRuntimeAuth(uid)
	if rt == nil || rt.Pool == nil {
		return fmt.Errorf("unknown account %s", uid)
	}
	if rt.Kind == provider.Oczen {
		return fmt.Errorf("OpenCodeZen 匿名通道账号不可停用/启用")
	}
	if rt.Kind == provider.Kilo {
		return fmt.Errorf("Kilo 免费池账号不可停用/启用")
	}
	// R83 起自定义渠道没有 enabled 概念（要么在列表里=启用，要么被删除），
	// 故不再走「停用即写配置」的特例；落到下面常规的 pool.SetDisabled。
	// 效果差异：面板重启后自定义渠道的停用状态不再持久化 —— 这正是删除该功能的本意。
	rt.Pool.SetDisabled(uid, disabled)
	if disabled {
		log.Printf("已停用账号 %s", shortUID(uid))
	} else {
		log.Printf("已启用账号 %s", shortUID(uid))
	}
	return nil
}

// SetAccountNickname 修改账号显示名：写回 auth 文件 + 池内即时生效。
func (a *App) SetAccountNickname(uid, nickname string) error {
	nickname = strings.TrimSpace(nickname)
	if nickname == "" {
		return fmt.Errorf("显示名不能为空")
	}
	if len([]rune(nickname)) > 60 {
		return fmt.Errorf("显示名过长（最多 60 字符）")
	}
	rt, au := a.findRuntimeAuth(uid)
	if rt == nil || au == nil {
		return fmt.Errorf("unknown account %s", uid)
	}
	if rt.Kind == provider.Oczen {
		return fmt.Errorf("OpenCodeZen 匿名通道账号不可改名")
	}
	if rt.Kind == provider.Kilo {
		return fmt.Errorf("Kilo 免费池账号不可改名")
	}
	if au.FilePath == "" {
		return fmt.Errorf("账号 %s 无凭证文件，不可改名", uid)
	}
	a.mu.Lock()
	au.Nickname = nickname // 池内 AuthByUID 返回同一指针，改字段即生效
	err := au.SaveAtomic()
	a.mu.Unlock()
	if err != nil {
		return fmt.Errorf("保存显示名失败：%v", err)
	}
	log.Printf("账号显示名已更新 uid=%s nickname=%s", shortUID(uid), nickname)
	return nil
}

// reloadAccounts 用 auths 目录最新文件对齐账号池。
// 注意：oczen 与 kilo 不在此列——两者的虚拟账号都没有凭证文件，
// SyncToDir 会因「目录扫描不到」而把它从池里剔除，故只能由 main 装配时注入一次。
func (a *App) reloadAccounts() {
	if rt := a.runtime(provider.WorkBuddy); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadWorkBuddyDir(a.cfg.AuthDir, a.cfg.Region)
		if err != nil {
			log.Printf("reload workbuddy accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	if rt := a.runtime(provider.TraeWork); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadTraeDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload traework accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	if rt := a.runtime(provider.Qoder); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadQoderDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload qoder accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	if rt := a.runtime(provider.QoderCN); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadQoderCNDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload qodercn accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	if rt := a.runtime(provider.QoderCOM); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadQoderCOMDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload qodercom accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	if rt := a.runtime(provider.WorkBuddyAI); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadWorkBuddyAiDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload workbuddyai accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	if rt := a.runtime(provider.Minimax); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadMinimaxDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload minimax accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	if rt := a.runtime(provider.LobsterAI); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadLobsterAIDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload lobsterai accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	if rt := a.runtime(provider.ZCode); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadZCodeDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload zcode accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	if rt := a.runtime(provider.Gemini); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadGeminiDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload gemini accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	if rt := a.runtime(provider.CodeArts); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadCodeArtsDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload codearts accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	if rt := a.runtime(provider.GLM); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadGLMDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload glm accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	if rt := a.runtime(provider.QwenWork); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadQwenWorkDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload qwenwork accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	if rt := a.runtime(provider.MonkeyCode); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadMonkeyCodeDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload monkeycode accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	if rt := a.runtime(provider.Raccoon); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadRaccoonDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload raccoon accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	if rt := a.runtime(provider.Loomy); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadLoomyDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload loomy accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	// Cline：一把官方 API Key = 一个账号，凭据由面板「粘贴 API Key」写入。
	if rt := a.runtime(provider.Cline); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadClineDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload cline accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	// OfficeAce：华为云果办，凭据由面板「浏览器登录」或「从客户端导入」写入 auths/。
	// 两条添加路径（OAuth onDone / importOfficeAce）都依赖本分支把新账号同步进池，
	// 缺了它账号要等 daemon 重启才出现（R34/R41 家族的「声明了却没接线」）。
	if rt := a.runtime(provider.OfficeAce); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadOfficeAceDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload officeace accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
}

func (a *App) firstRuntime() *Runtime {
	// oczen 排在末位：它无调度器活动，不应成为「签到时间/下次签到」的展示来源。
	// 顺序即展示优先级：新渠道追加在 Oczen 之前（Oczen 恒末位）。
	for _, k := range []provider.Kind{provider.WorkBuddy, provider.WorkBuddyAI, provider.TraeWork, provider.Qoder, provider.QoderCN, provider.QoderCOM, provider.QwenWork, provider.GLM, provider.MonkeyCode, provider.Raccoon, provider.Loomy, provider.Minimax, provider.LobsterAI, provider.ZCode, provider.Gemini, provider.CodeArts, provider.Kilo, provider.Oczen} {
		if rt := a.runtime(k); rt != nil {
			return rt
		}
	}
	return nil
}

func (a *App) checkinTimes() []string {
	if rt := a.firstRuntime(); rt != nil && rt.Scheduler != nil {
		return rt.Scheduler.CheckinTimes()
	}
	return nil
}

func (a *App) keepaliveHours() []int {
	if rt := a.firstRuntime(); rt != nil && rt.Scheduler != nil {
		return rt.Scheduler.KeepaliveHours()
	}
	return nil
}

func (a *App) nextFire() time.Time {
	if rt := a.firstRuntime(); rt != nil && rt.Scheduler != nil {
		return rt.Scheduler.NextFire()
	}
	return time.Time{}
}

// creditsNAKind 判定该渠道的「积分」概念是否不适用（面板显示「不适用」而不是 0）。
//
// 两种成因不同，但 UI 处理一致：
//   - oczen / kilo：免密免费池，根本没有账号/余额概念；
//   - cline：上游**确无余额端点**（/credits /usage /billing /quota /me 等 12 个候选实测全 404），
//     属于「查不到」。把这种情况显示成 0 会被误读成「额度已用光」，故同样走 NA。
//   - 自定义渠道（R66）：无积分与额度接口，同样走 NA。
//
// 注意：**ZCode 不走这里**（尽管它的额度也不是积分）——它有真实的 token 桶要展示，
// 走 creditsUnit="token" 的「Token 计数」面板，而不是「不适用」（见 creditsUnit）。
func (a *App) creditsNAKind(group string) bool {
	if group == provider.Oczen.String() || group == provider.Kilo.String() ||
		group == provider.Cline.String() {
		return true
	}
	return a.isCustomChannel(group)
}

// creditsUnit 该渠道「额度」的计量单位：空串 = 积分（credits，默认），`"token"` = Token 计数。
//
// 为什么做成渠道级：面板的**卡片与汇总条**按渠道渲染，它们只知道渠道名，需要先知道
// 「这个渠道的数字是什么单位」才能选对文案与合计口径（token 绝不并入积分合计）；
// 条目级的单位见 provider.ResourceItem.Unit（弹层逐条显示用）。
//
// ⚠️ 这是**单一来源**：前端不维护渠道名单，全靠本函数下发的 credits_unit 字段 ——
// 以后新增以 token 计量的渠道，只在这里加一行即可（用户 2026-10-17 的明确要求）。
func (a *App) creditsUnit(group string) string {
	if group == provider.ZCode.String() {
		return "token"
	}
	// 百分比配额（2026-10-30 用户实测）：这类渠道的额度是「窗口剩余百分比」，
	// `UserResource` 恒返回 0 ⇒ 面板只能显示「0 积分」，看着像额度耗尽，
	// 实际可能满格。标成 "percent" 让卡片改显示窗口百分比（数据见 quota_windows）。
	// 百分比配额（2026-10-30 用户实测）：这类渠道的额度是「窗口剩余百分比」，
	// `UserResource` 恒返回 0 ⇒ 面板只能显示「0 积分」，看着像额度耗尽，
	// 实际可能满格。标成 "percent" 让卡片改显示窗口百分比（数据见 quota_windows）。
	//
	// 豆包两条渠道共用同一个客户端的同一份额度（同一个账号、同一个接口）：
	// 5 小时窗口 / 7 天窗口 / 活动赠送额度，全部是 used_percent 形态。
	if group == provider.Gemini.String() ||
		group == provider.Doubao.String() || group == provider.Doubaowork.String() {
		return "percent"
	}
	return "" // 积分（credits）
}

// isCustomChannel 报告该分组名是否是自定义渠道。先查运行时表（热更新后的权威态），
// 再回退查配置（供应器未装配/已停用的渠道）。
// 两把锁分开取：runtimesMu 只管运行时表，a.mu 只管 cfg——不可与外层已持 a.mu 的调用嵌套。
func (a *App) isCustomChannel(group string) bool {
	a.runtimesMu.RLock()
	rt := a.runtimes[provider.Kind(group)]
	a.runtimesMu.RUnlock()
	if rt != nil && rt.Custom {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, cc := range a.cfg.CustomChannels {
		if strings.EqualFold(cc.Name, group) {
			return true
		}
	}
	return false
}
