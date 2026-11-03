package app

import (
	"path/filepath"
	"sort"
	"strings"
	"time"

	"wild-work/internal/backuplink"
	"wild-work/internal/config"
	"wild-work/internal/custom"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// AccountView 面板展示的账号（脱敏）。
type AccountView struct {
	UID      string `json:"uid"`
	Group    string `json:"group"` // workbuddy | workbuddyai | traework | qoder
	Nickname string `json:"nickname"`
	// Credits 本工具可消耗的积分余额（pool 路由依据）。
	Credits int64 `json:"credits"`
	// ExpiringCredits 可消耗余额中 24h 内（到期日≤明天）到期的部分，仅面板展示。
	ExpiringCredits int64 `json:"expiring_credits,omitempty"`
	// UnusableCredits 账号名下有、但本工具用不了的积分（如 TraeWork ep=1 专用池），
	// 仅面板展示；0 表示该渠道不区分或没有此类额度。
	UnusableCredits int64 `json:"unusable_credits"`
	// CreditsStale 余额口径不可信（旧版 state 或尚未完成首次成功刷新），UI 显示「待刷新」。
	CreditsStale bool `json:"credits_stale,omitempty"`
	// CreditsNA 积分概念不适用（如 OpenCodeZen 匿名通道），UI 显示「不适用」
	// 而非 0，并隐藏刷新积分与明细入口。
	CreditsNA bool `json:"credits_na,omitempty"`
	// CreditsUnit 该账号额度的计量单位：空 = 积分（credits），`"token"` = Token 计数。
	// 前端据此把它渲染成「Token 计数」面板（文案/单位/合计口径都随之切换），
	// 且**不并入积分合计**——即「与积分分开统计和显示」（用户 2026-10-17 的要求）。
	CreditsUnit string `json:"credits_unit,omitempty"`
	// QuotaWindows 百分比配额窗口（credits_unit="percent" 时才有值）。
	//
	// 为什么必须随 state 下发而不是让前端逐个拉详情：账号卡片是首屏渲染，
	// 每卡一次异步请求会把「30 个账号」变成 30 个并行请求；而窗口数据本来就在
	// 积分刷新的同一次上游响应里（见 creditSnapshot），顺手存下来成本几乎为零。
	QuotaWindows []QuotaWindowView `json:"quota_windows,omitempty"`
	Cooling      bool              `json:"cooling"`
	Until        string            `json:"until"`
	// UntilTS 解冻时刻的 unix 秒（0 = 无）。面板用它实时算「剩 X 时 X 分」——
	// until 文案是 "01-02 15:04"（24h 制），跨零点的次日 07:07 极易被误读成
	// 「19 点被显示成 7 点」；带相对时长后（issue：24h 制统一）歧义即消除。
	UntilTS  int64  `json:"until_ts,omitempty"`
	Reason   string `json:"reason"`
	Disabled bool   `json:"disabled"`
	// ModelCooling 模型级冷却（model → 解冻时刻）；仅该账号该模型限流，账号整体仍可用
	// （429/6004 上游按模型计）。面板据此区分「整号冷却」与「个别模型限流」。
	ModelCooling   map[string]string `json:"model_cooling,omitempty"`
	ErrCount       int               `json:"err_count"`
	LastCheckinOK  bool              `json:"last_checkin_ok"`
	LastCheckinAt  string            `json:"last_checkin_at"`
	LastCheckinMsg string            `json:"last_checkin_msg"`
	// SessionDead 登录态失效（401/登录已失效）：面板显示「需重新登录」+ 重新登录入口（#134）。
	SessionDead       bool   `json:"session_dead,omitempty"`
	SessionDeadReason string `json:"session_dead_reason,omitempty"`
	// RefreshTokenMissing 凭据缺 refresh_token —— 目前只有 OfficeAce 会这样：
	// 客户端导入只拿得到「不过期的网关 Basic」，而积分/签到要用的临时凭据（约 2h）
	// 必须靠 refresh_token 换 ⇒ 缺它时积分/签到**永久不可用**。它与 SessionDead 不同：
	// 不是「被上游拒了」，而是本地根本没这条凭据，故不会自愈，只能重新登录/重新导入补齐。
	// 面板据此显示**持久**徽标（原先只在导入那一刻弹一次提示文字，事后无从复查）。
	RefreshTokenMissing       bool   `json:"refresh_token_missing,omitempty"`
	RefreshTokenMissingReason string `json:"refresh_token_missing_reason,omitempty"`
}

// QuotaWindowView 百分比配额窗口的面板形状。
type QuotaWindowView struct {
	Name    string `json:"name"`
	Percent int    `json:"percent"`
	// ResetAt 窗口重置时刻（RFC3339，空 = 上游未给）。
	ResetAt string `json:"reset_at,omitempty"`
}

// State Web UI 初始数据。
type State struct {
	Accounts       []AccountView `json:"accounts"`
	CheckinTimes   []string      `json:"checkin_times"`
	KeepaliveHours []int         `json:"keepalive_hours"`
	ListenHost     string        `json:"listen_host"`
	ListenPort     int           `json:"listen_port"`
	APIKey         string        `json:"api_key"`

	// AdminPassSet 是否已配置管理密码（不回显密码本身）；
	// AuthEnabled = 面板鉴权已生效（密码非空）；
	// AuthSession = 当前浏览器 cookie 对应的口令指纹（已登录时非空且等于服务端口令指纹）。
	// 前端靠 AuthSession 判断登录态：HttpOnly cookie 不可读，但指纹可回显。
	AdminPassSet bool   `json:"admin_pass_set"`
	AuthEnabled  bool   `json:"auth_enabled"`
	AuthSession  string `json:"auth_session"`
	AuthRequired bool   `json:"auth_required"` // 当前监听地址是否必须配置密码（非环回）

	LoginBusy   bool   `json:"login_busy"`
	LoginError  string `json:"login_error"` // 上次登录终态错误（非空时面板提示失败而非「登录完成」）
	NextCheckin string `json:"next_checkin"`
	Version     string `json:"version"`
	Autostart   bool   `json:"autostart"`
	Running     bool   `json:"running"`

	// LanIP 非环回出口 IP（如 192.168.x.x），供面板在监听 0.0.0.0 时提示局域网可用的 API 地址。
	// 取不到（无网络/全部环回）时为空串。
	LanIP string `json:"lan_ip"`

	// Compat 模型名路由配置（只读，保存走 POST /api/config/compat）
	Compat struct {
		DefaultChannel string            `json:"default_channel"`
		MaxTokensCap   int               `json:"max_tokens_cap"`
		ModelMap       map[string]string `json:"model_map"`
		Channels       []string          `json:"channels"` // 可用渠道列表（供 UI 下拉）
	} `json:"compat"`

	// BackupProxy 全局备用上游通道（只读，保存走 POST /api/config/proxy）。
	// 注意：它**不参与常规请求**，仅当渠道自行判定上游按地区拒绝时在同请求内重放一次（R57）。
	BackupProxy string `json:"backup_proxy"`

	// BackupProxyActive 备用通道是否已生效（与 BackupProxy 非空等价，分开回显
	// 是为了让面板能区分「地址合法但进程内未生效」这种理论上不应存在的状态）。
	BackupProxyActive bool `json:"backup_proxy_active"`

	// OczenAPIKey OpenCodeZen 自定义 API key（只读脱敏回显：sk-xxx…尾4位；保存走同端点）。
	OczenAPIKey string `json:"oczen_api_key"`

	// BodyLog 请求体摘要诊断是否开启（只读，保存走 POST /api/config/body_log）。
	BodyLog bool `json:"body_log"`

	// LoopGuard 思考链死循环检测（发癫拦截）是否开启（只读，保存走 POST /api/config/loop_guard）。
	LoopGuard bool `json:"loop_guard"`

	// ModelFallback 模型降级链（保存走 POST /api/config/model_fallback）。
	ModelFallback []config.ModelFallbackRule `json:"model_fallback"`

	// ReasoningEfforts 思考强度注入映射（键=模型 ID 或 `*` 全局默认；保存走 POST /api/config/reasoning_efforts）。
	ReasoningEfforts map[string]string `json:"reasoning_efforts"`

	// CustomChannels 自定义第三方渠道列表（保存走 POST /api/config/custom_channels，R66）。
	CustomChannels []custom.ChannelConfig `json:"custom_channels"`
}

// GetState 返回面板初始数据。
func (a *App) GetState() State {
	a.mu.Lock()
	pass, listen := a.cfg.AdminPass, a.cfg.Listen
	a.mu.Unlock()
	authOn := strings.TrimSpace(pass) != ""
	brief := passBrief(pass)
	session := ""
	if authOn {
		session = brief // 仅当请求通过 authGuard 时才会走到这里（无有效 cookie 已被 401 拦截）
	}
	st := State{
		CheckinTimes:   a.checkinTimes(),
		KeepaliveHours: a.keepaliveHours(),
		ListenHost:     listen.Host,
		ListenPort:     listen.Port,
		APIKey:         a.cfg.APIKey,
		AdminPassSet:   authOn,
		AuthEnabled:    authOn,
		AuthSession:    session,
		AuthRequired:   requireRemoteAuth(listen),
		LoginBusy:      a.LoginBusy(),
		LoginError:     a.peekLoginError(),
		NextCheckin:    fmtTime(a.nextFire()),
		Version:        Version,
		Autostart:      a.AutostartEnabled(),
		Running:        a.ServerRunning(),
		LanIP:          lanIP(),
		LoopGuard:      a.cfg.Features.LoopGuard,
	}
	st.ModelFallback = a.cfg.ModelFallback
	st.ReasoningEfforts = a.cfg.ReasoningEfforts
	if st.ReasoningEfforts == nil {
		st.ReasoningEfforts = map[string]string{}
	}
	st.Compat.DefaultChannel = a.cfg.Compat.DefaultChannel
	st.Compat.MaxTokensCap = a.cfg.Compat.MaxTokensCap
	st.Compat.ModelMap = a.cfg.Compat.ModelMap
	if st.Compat.ModelMap == nil {
		st.Compat.ModelMap = map[string]string{}
	}
	for _, rt := range a.runtimesSnapshot() {
		if rt != nil {
			st.Compat.Channels = append(st.Compat.Channels, rt.Kind.String())
		}
	}
	sort.Strings(st.Compat.Channels)
	st.BackupProxy = a.cfg.BackupProxy
	st.BackupProxyActive = backuplink.Enabled()
	// oczen key 脱敏回显（与自定义渠道同一套 maskKey，面板侧靠 **** 识别未改动）
	st.OczenAPIKey = maskKey(a.cfg.OczenAPIKey)
	st.BodyLog = a.cfg.BodyLog
	// 自定义渠道路由的 api_key 同样脱敏回显（不把上游密钥明文送进面板载荷）。
	// 返回深拷贝而非配置引用，避免调用方意外改写配置。
	st.CustomChannels = maskCustomChannels(a.cfg.CustomChannels)
	st.Accounts = a.accountViews()
	return st
}

func (a *App) accountViews() []AccountView {
	statuses := a.allStatuses()
	out := make([]AccountView, 0, len(statuses))
	for _, s := range statuses {
		group := a.accountGroup(s.UID)
		missingRT, missingRTWhy := a.refreshTokenMissing(s.UID)
		out = append(out, AccountView{
			UID:             s.UID,
			Group:           group,
			Nickname:        s.Nickname,
			Credits:         s.Credits,
			ExpiringCredits: s.ExpiringCredits,
			UnusableCredits: s.UnusableCredits,
			CreditsStale:    s.CreditsStale,
			// 匿名与自定义渠道无积分：前端据此显示「不适用」
			CreditsNA:                 a.creditsNAKind(group),
			CreditsUnit:               a.creditsUnit(group),
			QuotaWindows:              quotaWindowViews(a.quotaWindows(group, s.UID)),
			Cooling:                   s.Cooling,
			Until:                     fmtTime(s.Until),
			UntilTS:                   unixOrZero(s.Until),
			Reason:                    s.Reason,
			Disabled:                  s.Disabled,
			ModelCooling:              fmtTimeMap(s.ModelCooling),
			ErrCount:                  s.ErrCount,
			LastCheckinOK:             s.LastCheckinOK,
			LastCheckinAt:             fmtTime(s.LastCheckinAt),
			LastCheckinMsg:            s.LastCheckinMsg,
			SessionDead:               s.SessionDead,
			SessionDeadReason:         s.SessionDeadReason,
			RefreshTokenMissing:       missingRT,
			RefreshTokenMissingReason: missingRTWhy,
		})
	}
	// 面板固定渠道序展示：OpenCodeZen → WorkBuddyCN → WorkBuddyAI → QoderCN → QoderCOM → TraeWork → 千问办公
	sortChannelsByOrder(out, func(v AccountView) provider.Kind { return provider.Kind(v.Group) })
	return out
}

// accountGroup 返回账号所属分组（workbuddy/traework）。
func (a *App) accountGroup(uid string) string {
	_, au := a.findRuntimeAuth(uid)
	if au != nil {
		if au.Kind != "" {
			return au.Kind
		}
		if au.FilePath != "" && strings.HasPrefix(filepath.Base(au.FilePath), "trae-") {
			return "traework"
		}
		if au.FilePath != "" && strings.HasPrefix(filepath.Base(au.FilePath), "qoder") {
			return "qoder"
		}
		if au.FilePath != "" && strings.HasPrefix(filepath.Base(au.FilePath), "workbuddyai-") {
			return "workbuddyai"
		}
	}
	return "workbuddy"
}

// refreshTokenMissing 判定该账号的凭据是否**缺 refresh_token**（面板持久徽标用，改进2）。
//
// 目前只有 OfficeAce 存在这种形态：其凭据分两层——网关 Basic（不过期，用于对话）与
// 临时 AK/SK（约 2h，用于积分/签到）。客户端导入只拿得到前者，后者必须由 refresh_token
// 经 DPoP 换取 ⇒ **没有 refresh_token 就没有积分/签到**，且不会自愈（不像 401 失效那样
// 能靠 refresh+重试恢复），只能「重新登录」或「先在客户端登录该账号再重新导入」补齐。
//
// 判定放在这里（而非各渠道自查）的原因：这是**账号卡片**要展示的状态，与 SessionDead
// 同源同层；只有明确「凭据模型依赖 refresh_token」的渠道才判定，避免给 Cline/oczen
// 这类无 refresh 概念的渠道误挂徽标（它们本来就不写 RefreshToken）。
//
// 返回 (是否缺失, 面板提示文案)。文案带**补救路径**，因为用户看到徽标后最想知道的就是
// 「我该做什么」；两条路径都实测可用（2026-10-17：浏览器登录补齐、客户端重登后重导入补齐）。
//
// 两种「等效于没有 refresh_token」的形态都要报（2026-10-18 独立复核补第二种）：
//   - 字段为空：本地根本没这条凭据；
//   - 字段非空但**已在死名单**：该 token 被上游明确拒过（STS5.1806/1807 "has been used"），
//     `internal/officeace/refresh.go` 会直接短路、`ensureCredential` 不会被调用 ⇒ 续期
//     **必然失败**。只判空会让这种账号在面板上看起来完全正常（复核用探针实证）。
func (a *App) refreshTokenMissing(uid string) (bool, string) {
	_, au := a.findRuntimeAuth(uid)
	if au == nil || au.Kind != string(provider.OfficeAce) {
		return false, ""
	}
	rt := strings.TrimSpace(au.RefreshTokenValue())
	if rt == "" {
		return true, "缺 refresh_token：积分/签到不可用（本工具不会自动恢复）。" +
			"补救一：先在 OfficeAce 客户端登录该账号，再回面板「从客户端导入」；" +
			"补救二：直接在本工具用「浏览器登录」重新添加该账号。"
	}
	// 只读一次 Extra（持锁快照，避免与续期写路径并发读 map）。
	au.RLock()
	dead := au.Extra["dead_tokens"]
	au.RUnlock()
	if csvContains(dead, rt) {
		return true, "refresh_token 已被上游拒绝（本工具已拉黑、不再重放）：积分/签到不可用，需重新登录。" +
			"补救一：先在 OfficeAce 客户端登录该账号，再回面板「从客户端导入」；" +
			"补救二：直接在本工具用「浏览器登录」重新添加该账号。"
	}
	return false, ""
}

func (a *App) allStatuses() []pool.Status {
	out := []pool.Status{}
	for _, rt := range a.runtimesSnapshot() {
		// 别名渠道（TraeCode）复用同一 Pool：计入会导致每个账号重复一条（issue #36）。
		if rt != nil && rt.Pool != nil && !rt.Alias {
			out = append(out, rt.Pool.List()...)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out
}

// channelOrder 渠道固定展示顺序（面板账号卡片/费率表/设置代理列表统一用此序）。
// 旧 Qoder 已下线不列；未出现在表中的渠道排最后（按字母序）。
var channelOrder = map[provider.Kind]int{
	provider.Oczen: 0, provider.WorkBuddy: 1, provider.WorkBuddyAI: 2,
	provider.QoderCN: 3, provider.QoderCOM: 4, provider.TraeWork: 5, provider.QwenWork: 6,
	provider.GLM:       7,
	provider.OfficeAce: 8,
	provider.Kilo:      9, // 免密免费池：排序上紧随 oczen 之后（同属无账号渠道）
	provider.Minimax:   10,
	provider.LobsterAI: 11,
	provider.ZCode:     12,
	provider.Gemini:    13,
	provider.CodeArts:  14,
}

// quotaWindows 取某账号的百分比窗口快照（非百分比渠道返回 nil）。
//
// 只对 credits_unit="percent" 的渠道读：其他渠道的 pool 里即便残留空切片也不该下发，
// 免得前端要额外判「这个渠道到底支不支持百分比」。
func (a *App) quotaWindows(group, uid string) []pool.QuotaWindow {
	if a.creditsUnit(group) != "percent" {
		return nil
	}
	a.runtimesMu.RLock()
	rt := a.runtimes[provider.Kind(group)]
	a.runtimesMu.RUnlock()
	if rt == nil || rt.Pool == nil {
		return nil
	}
	return rt.Pool.QuotaWindows(uid)
}

// quotaWindowViews 把 pool 形状转成面板形状（时间统一成 RFC3339，空值留空）。
func quotaWindowViews(ws []pool.QuotaWindow) []QuotaWindowView {
	if len(ws) == 0 {
		return nil
	}
	out := make([]QuotaWindowView, 0, len(ws))
	for _, w := range ws {
		v := QuotaWindowView{Name: w.Name, Percent: w.Percent}
		if !w.ResetAt.IsZero() {
			v.ResetAt = w.ResetAt.Format(time.RFC3339)
		}
		out = append(out, v)
	}
	return out
}
