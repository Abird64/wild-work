package app

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"wild-work/internal/config"
	"wild-work/internal/custom"
	"wild-work/internal/provider"
)

// customRename 一条渠道改名记录（R122）：面板编辑渠道标识时，随整表一起声明
// 「New 这个新标识是从 Old 这个旧标识改过来的」。
//
// 为什么必须显式传：渠道标识**就是** provider.Kind（cmd/wild-work/main.go 里
// `k := provider.Kind(cc.Name)`、state 文件与虚拟账号 UID 全部拼名字），而只对比新旧两张表
// 无法区分「改名」与「删一个 + 新增一个」——不声明就会踩到：脱敏回来的密钥按新名找不回、
// 真密钥被空串覆盖落盘。
type customRename struct {
	New string `json:"new"`
	Old string `json:"old"`
}

// SetCustomChannels 整表替换自定义渠道配置（R66）。
//
// api_key 脱敏回写处理（同 oczen key）：面板回显的是脱敏值。用户没改动过某个渠道的 key 时，
// 提交回来的仍是脱敏串——若原样落盘就把真实密钥换成了废值。故：提交值是脱敏形态
// （isMaskedKey）则视为未修改，从现有配置按渠道名找回真实密钥；改名过的渠道另按旧名找回。
func (a *App) SetCustomChannels(channels []custom.ChannelConfig, renames ...customRename) error {
	_, err := a.setCustomChannels(channels, renames)
	return err
}

// setCustomChannels 同 SetCustomChannels，额外返回**真正生效**的改名条数（面板要如实告知用户
// 引用配置被联动了几处；非法/无效的改名声明会被丢弃，不能拿提交数当成绩）。
func (a *App) setCustomChannels(channels []custom.ChannelConfig, renames []customRename) (int, error) {
	seen := map[string]bool{}
	for i := range channels {
		if isMaskedKey(channels[i].APIKey) { // 脱敏形态 = 未改动，保留现值
			channels[i].APIKey = ""
		}
		if err := channels[i].Validate(); err != nil {
			return 0, err
		}
		if seen[channels[i].Name] {
			return 0, fmt.Errorf("重复的渠道标识 %q", channels[i].Name)
		}
		seen[channels[i].Name] = true
	}
	a.mu.Lock()
	// 找回被提交为脱敏形态的密钥（按渠道名匹配现值）
	prev := map[string]string{}
	prevNames := map[string]bool{}
	for _, cc := range a.cfg.CustomChannels {
		prev[strings.ToLower(cc.Name)] = cc.APIKey
		prevNames[strings.ToLower(cc.Name)] = true
	}
	// 改名映射（新→旧）：密钥找回与后续级联都靠它；非法项直接丢（旧名不存在或新旧相同不算改名）
	ren := map[string]string{}
	for _, r := range renames {
		nw, od := strings.ToLower(strings.TrimSpace(r.New)), strings.ToLower(strings.TrimSpace(r.Old))
		if nw == "" || od == "" || nw == od || !prevNames[od] || prevNames[nw] {
			continue
		}
		ren[nw] = od
	}
	for i := range channels {
		if channels[i].APIKey != "" {
			continue
		}
		k := prev[channels[i].Name]
		if k == "" { // 改名后按新名查不到 ⇒ 必须回退查旧名，否则真密钥被空串覆盖并落盘
			if od, ok := ren[channels[i].Name]; ok {
				k = prev[od]
			}
		}
		if k != "" {
			channels[i].APIKey = k
		}
	}
	compatTouched := 0
	if len(ren) > 0 {
		compatTouched = renameCustomRefs(a.cfg, ren)
	}
	a.cfg.CustomChannels = channels
	err := config.Save(a.cfg, a.cfgPath)
	syncer := a.customSyncer
	fbSyncer, efSyncer := a.fbSyncer, a.efSyncer
	fbRules, efforts := a.cfg.ModelFallback, a.cfg.ReasoningEfforts
	a.mu.Unlock()
	if err != nil {
		return 0, fmt.Errorf("保存配置失败：%w", err)
	}
	// 改名连带改写了降级链/思考强度映射，而这两张表在 server.Handler 里是**另一份内存副本**：
	// 只改 cfg 不推 syncer，热路径会继续拿旧 key 查（要到面板重存一次才能修好）。
	if len(ren) > 0 {
		if fbSyncer != nil {
			fbSyncer(fbRules)
		}
		if efSyncer != nil {
			efSyncer(efforts)
		}
	}
	for nw, od := range ren {
		renameCustomState(a.stateDir(), od, nw) // 旧 state 文件留 .old，不硬删
		log.Printf("自定义渠道标识已改名：%s → %s（引用配置联动 %d 处）", od, nw, compatTouched)
	}
	if syncer != nil {
		syncer(channels)
	}
	log.Printf("自定义渠道配置已更新：共 %d 个渠道", len(channels))
	return len(ren), nil
}

// renameCustomRefs 把以 `旧标识/` 为键或值前缀的引用配置改写成新标识（R122）：
// compat.model_map / compat.default_channel / model_fallback / reasoning_efforts。
//
// 为什么必须联动：这几类配置**只校形状不校渠道存在性**，改名后旧前缀不会报错、只会静默
// 失效（用户看到「配置还在、就是不起作用」）。返回改写条数，供面板如实告知。持 a.mu 调用。
func renameCustomRefs(cfg *config.Config, ren map[string]string) int {
	// ren 的方向是「新→旧」（密钥找回靠它），而引用配置里出现的是**旧名** ⇒ 必须先倒转。
	// 同一个旧名被声明改成两个新名是脏数据（无法判断哪个是真），跳过它，不猜。
	cnt := map[string]int{}
	for _, od := range ren {
		cnt[od]++
	}
	to := make(map[string]string, len(ren))
	for nw, od := range ren {
		if cnt[od] == 1 {
			to[od] = nw
		}
	}
	swap := func(s string) string {
		if i := strings.Index(s, "/"); i > 0 {
			if nw, ok := to[strings.ToLower(s[:i])]; ok {
				return nw + s[i:]
			}
		}
		return s
	}
	n := 0
	for k, v := range cfg.Compat.ModelMap {
		if nv := swap(v); nv != v {
			cfg.Compat.ModelMap[k] = nv
			n++
		}
	}
	if od := strings.ToLower(strings.TrimSpace(cfg.Compat.DefaultChannel)); od != "" {
		if nw, ok := to[od]; ok {
			cfg.Compat.DefaultChannel = nw
			n++
		}
	}
	for i := range cfg.ModelFallback {
		if v := swap(cfg.ModelFallback[i].From); v != cfg.ModelFallback[i].From {
			cfg.ModelFallback[i].From, n = v, n+1
		}
		for j := range cfg.ModelFallback[i].To {
			if v := swap(cfg.ModelFallback[i].To[j]); v != cfg.ModelFallback[i].To[j] {
				cfg.ModelFallback[i].To[j], n = v, n+1
			}
		}
	}
	for k, v := range cfg.ReasoningEfforts {
		if nk := swap(k); nk != k {
			delete(cfg.ReasoningEfforts, k)
			cfg.ReasoningEfforts[nk] = v
			n++
		}
	}
	return n
}

// renameCustomState 把 state-custom-<旧>.json 改名留档（照 migrateStateFiles 的先例：只 rename
// 不删除，便于回滚）。新标识的池子从零开始，不搬旧快照——旧文件里的 UID 是 `custom-<旧>`，
// 原样搬到新文件会把一个对不上号的账号条目弄进新池子里。
func renameCustomState(dir, oldName, newName string) {
	old := filepath.Join(dir, "state-custom-"+oldName+".json")
	if !fileExists(old) {
		return
	}
	if err := os.Rename(old, old+".old"); err != nil {
		log.Printf("迁移自定义渠道状态文件失败（%s）：%v", oldName, err)
	}
}

// resolveCustomKey 按渠道名取真实 api_key（供脱敏回显时的测试与保存路径使用）。
// 名称为空（面板新增渠道时只填了 URL/Key）则返回空串，交由调用方自行处理。
func (a *App) resolveCustomKey(name string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, cc := range a.cfg.CustomChannels {
		if strings.EqualFold(cc.Name, name) {
			return cc.APIKey
		}
	}
	return ""
}

// customChannelFeesMeta 返回渠道名 -> [接入点, 脱敏 key]，只给费率页用（R119）。
// 这里给的必须是 maskKey 后的脱敏串：费率页会被面板反复拉，批量发明文等于
// 把所有第三方 key 长期暴露在浏览器内存/抓包里。
func (a *App) customChannelFeesMeta() map[string][2]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string][2]string, len(a.cfg.CustomChannels))
	for _, cc := range a.cfg.CustomChannels {
		out[cc.Name] = [2]string{cc.BaseURL, maskKey(cc.APIKey)}
	}
	return out
}

// maskCustomChannels 返回配置的深拷贝，并将 api_key 脱敏（长头 + **** + 短尾，见 maskKey）。
// 脱敏串里的 **** 就是「未改动」标记，供 SetCustomChannels / 测试端点识别。
func maskCustomChannels(in []custom.ChannelConfig) []custom.ChannelConfig {
	out := make([]custom.ChannelConfig, 0, len(in))
	for _, cc := range in {
		if cc.APIKey != "" {
			cc.APIKey = maskKey(cc.APIKey)
		}
		if cc.Models != nil {
			cc.Models = append([]custom.ModelConfig(nil), cc.Models...)
		}
		out = append(out, cc)
	}
	return out
}

// SetCustomChannelsSyncer 注入自定义渠道热更新回调。
func (a *App) SetCustomChannelsSyncer(fn func(channels []custom.ChannelConfig)) {
	a.customSyncer = fn
}

// UpdateCustomRuntimes 热更新 app 持有的自定义渠道运行时（R66）。
// 只碰 runtimes 表（不读 cfg），故用 runtimesMu 而非 a.mu。
func (a *App) UpdateCustomRuntimes(customRts map[provider.Kind]*Runtime) {
	a.runtimesMu.Lock()
	defer a.runtimesMu.Unlock()
	for k, rt := range a.runtimes {
		if rt != nil && rt.Custom {
			delete(a.runtimes, k)
		}
	}
	for k, rt := range customRts {
		rt.Custom = true
		a.runtimes[k] = rt
	}
}
