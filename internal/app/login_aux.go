package app

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"wild-work/internal/auth"
	"wild-work/internal/cline"
	loginglm "wild-work/internal/login_glm"
	"wild-work/internal/officeace"
	"wild-work/internal/provider"
)

// SubmitGLMToken 提交用户粘贴的智谱清言 refresh_token：验证 → 落盘 → 重载账号池。
//
// 这是智谱清言的**手工兜底**入口（自动路径见 StartGLMAutoLogin）。
// 返回保存后的 uid 供面板定位新账号。
func (a *App) SubmitGLMToken(refreshToken string) (string, error) {
	r, err := loginglm.ValidateAndSave(a.cfg.AuthDir, refreshToken)
	if err != nil {
		return "", err
	}
	log.Printf("glm 账号已添加（手工）uid=%s nickname=%s", r.UID, r.Nickname)
	a.reloadAccounts()
	a.afterAccountAdded(provider.GLM)
	return r.UID, nil
}

// SubmitClineKey 提交用户粘贴的 Cline 官方 API Key（sk_…）：验证 → 落盘 → 重载账号池。
//
// 验证只调 GET /api/v1/api-keys —— **零 token 消耗**，避免「添加账号即烧额度」。
// uid 取该端点中 key_prefix 与本 Key 相符那条的 apk- id（实测 key_prefix = Key 前 10 字符）。
//
// 第二个返回值是**告警**（非错误，账号照样保存）：官方配额字段名是 `…UsageCostUSDPerUser`，
// 即上限按 Cline **账号**计 ⇒ 同一账号的多把 Key 共享一份额度。
// 由于 api-keys 返回的是本账号**全部** Key，可据此发现「两把 Key 同属一个账号」并提醒
// 用户去注册独立账号，而不是误以为多 Key 等于多额度。
func (a *App) SubmitClineKey(apiKey string) (string, string, error) {
	key := strings.TrimSpace(apiKey)
	res, err := cline.ValidateKey(key)
	if err != nil {
		return "", "", err
	}

	// 同账号检测：新 Key 所在账号的 Key 列表 ∩ 已入库账号的 Key 前缀
	warn := ""
	if existing, lerr := auth.LoadClineDir(a.cfg.AuthDir); lerr == nil {
		family := append([]string{res.KeyPrefix}, res.SiblingKeys...)
		for _, e := range existing {
			p := e.AccessTokenValue()
			if len(p) > cline.KeyPrefixLen {
				p = p[:cline.KeyPrefixLen]
			}
			if p == "" {
				continue
			}
			for _, kp := range family {
				if kp == p {
					warn = fmt.Sprintf("这把 Key 与已添加的账号 %s 同属一个 Cline 账号，共享同一份额度"+
						"（官方按账号计），不会增加可用额度。请注册独立 Cline 账号并各自建 Key。", e.UID)
					break
				}
			}
			if warn != "" {
				break
			}
		}
	}

	name := res.AccountName
	if name == "" {
		name = "API Key " + res.KeyPrefix
	}
	_ = os.MkdirAll(a.cfg.AuthDir, 0o755)
	ca := &auth.Auth{
		Kind:        "cline",
		UID:         res.UID,
		Nickname:    name,
		AccessToken: key,
		// Key 无过期概念：ExpiresAt 必须远期，0 会让 NeedsRefresh 恒真（不变量 21 同款陷阱）
		ExpiresAt: cline.FarFutureExpiry(),
		FilePath:  filepath.Join(a.cfg.AuthDir, "cline-"+res.UID+".json"),
	}
	if err := ca.SaveAtomic(); err != nil {
		return "", "", fmt.Errorf("保存 Cline 凭据失败：%w", err)
	}
	// 日志零 token（不变量 2）：只打 uid 与展示名，绝不打完整 Key
	log.Printf("cline 账号已添加（粘贴 Key）uid=%s name=%s 同账号告警=%v", res.UID, name, warn != "")
	a.reloadAccounts()
	a.afterAccountAdded(provider.Cline)
	return res.UID, warn, nil
}

// StartGLMAutoLogin 发起智谱清言自动登录：拉起独立 profile 的浏览器，
// 用户在真实浏览器里登录，后台经 CDP 自动捕获 Cookie。
//
// 返回后立即进入 pending；前端轮询 GetGLMAutoLoginStatus 获知结果。
func (a *App) StartGLMAutoLogin() error {
	a.muGLMLogin.Lock()
	defer a.muGLMLogin.Unlock()
	if a.glmLogin != nil {
		if st, _ := a.glmLogin.Status(); st == "pending" {
			return errors.New("已有智谱清言登录流程进行中，请先在浏览器完成登录或取消")
		}
	}
	sess, err := loginglm.AutoLogin(a.cfg.AuthDir, func(r loginglm.Result) {
		// 落盘成功后重载账号池（在后台 goroutine 里执行）
		a.reloadAccounts()
		a.afterAccountAdded(provider.GLM)
		log.Printf("glm 自动登录完成，账号池已重载 uid=%s", r.UID)
	})
	if err != nil {
		return err
	}
	a.glmLogin = sess
	return nil
}

// GetGLMAutoLoginStatus 查询自动登录状态。
// 返回 status（idle/pending/success/failed/cancelled）、错误信息与成功时的 uid。
func (a *App) GetGLMAutoLoginStatus() map[string]any {
	a.muGLMLogin.Lock()
	sess := a.glmLogin
	a.muGLMLogin.Unlock()
	if sess == nil {
		return map[string]any{"status": "idle"}
	}
	status, err := sess.Status()
	out := map[string]any{"status": status}
	if err != nil {
		out["error"] = err.Error()
	}
	if r, ok := sess.Result(); ok {
		out["uid"] = r.UID
		out["nickname"] = r.Nickname
	}
	return out
}

// CancelGLMAutoLogin 取消自动登录并关闭浏览器。
func (a *App) CancelGLMAutoLogin() error {
	a.muGLMLogin.Lock()
	sess := a.glmLogin
	a.muGLMLogin.Unlock()
	if sess == nil {
		return errors.New("没有进行中的智谱清言登录")
	}
	sess.Cancel()
	return nil
}

// officeAceLoginStatus OfficeAce 自助登录状态（**单一来源**：前端读的字段名以此为准）。
//
// 字段契约（app.js 的 startOfficeAcePoll 依赖它）：
//
//	status: pending | exchanging | done | failed | expired | canceled | idle
//	可选 error / uid / nickname / warning
//
// 历史：这里原先返回 map[string]any，字段名靠手写字符串维护 —— 改一个拼写前端就静默读不到
// （表现为「永远转圈」）。改成结构体后由 json 标签约束，并有静态断言测试锁死。
type officeAceLoginStatus struct {
	Status   string `json:"status"`
	Error    string `json:"error,omitempty"`
	UID      string `json:"uid,omitempty"`
	Nickname string `json:"nickname,omitempty"`
	AuthURL  string `json:"auth_url,omitempty"`
	Warning  string `json:"warning,omitempty"`
}

// oaLoginActive 该状态是否表示「流程进行中」（此时不允许再起一条）。
func oaLoginActive(st string) bool { return st == "pending" || st == "exchanging" }

// StartOfficeAceLogin 发起 OfficeAce 自助登录（华为云 OAuth 授权码 + PKCE + DPoP）。
// 返回待打开的授权 URL；后台轮询完成登录并落盘。前端轮询 GetOfficeAceLoginStatus。
//
// 不在服务端开浏览器（#143）：授权页由前端 window.open 在「用户正在使用的设备」打开
// —— 捕获走云端轮询 state→code，与授权页在哪台设备打开无关；服务端弹窗对局域网用户
// 不可见，反而造成「点了没反应」的困惑。前端有「复制链接」兜底。
//
// 并发约定（2026-10-17 修）：只有「进行中」的会话才拒绝新请求；已进入终态的旧会话
// 必须**先清空再解锁**，否则并发的状态轮询会读到上一轮的 done/failed，把用户新点的
// 「新增账号」直接顶成旧终态（表现为「点了没反应」）。
func (a *App) StartOfficeAceLogin() (string, error) {
	// ① 短临界区：判定是否已有进行中的会话，并占住「正在发起」位。
	a.muOALogin.Lock()
	if a.oaLogin != nil {
		if st, _ := a.oaLogin.Status(); oaLoginActive(st) {
			url := a.oaLogin.URL()
			a.muOALogin.Unlock()
			return url, errors.New("已有 OfficeAce 登录流程进行中，请先完成或取消")
		}
		// 上一轮已到终态：清空，让本次新建的会话立刻成为唯一可见状态。
		// 刻意在上锁期间清空 —— 否则并发的 GetOfficeAceLoginStatus 会读到旧会话。
		a.oaLogin = nil
	}
	if a.oaStarting {
		a.muOALogin.Unlock()
		return "", errors.New("OfficeAce 登录正在发起中，请稍候")
	}
	a.oaStarting = true
	a.muOALogin.Unlock()

	// ② 网络部分必须在锁外：StartLogin 先要向云端申请 state（实测约 1s），持锁调用会把
	// 并发到达的状态轮询全部卡住。
	sess, err := officeAceStartLogin(a.cfg.AuthDir, func(r officeace.LoginResult) {
		a.reloadAccounts()
		a.afterAccountAdded(provider.OfficeAce)
		log.Printf("officeace 自助登录完成，账号池已重载 uid=%s", r.UID)
	})

	// ③ 收尾临界区：无论成败都要清占位并释放（历史缺陷正是成功路径漏了解锁）。
	a.muOALogin.Lock()
	a.oaStarting = false
	if err == nil {
		a.oaLogin = sess
	}
	a.muOALogin.Unlock()
	if err != nil {
		return "", err
	}
	// 不在服务端开浏览器：授权页由前端 window.open 在「用户正在使用的设备」打开
	//（捕获走云端轮询 state→code，与授权页在哪台设备打开无关；服务端弹窗对
	// 局域网用户不可见，反而造成「点了没反应」的困惑。前端有复制链接兜底。
	return sess.URL(), nil
}

// officeAceStartLogin 是 officeace.StartLogin 的注入点：真实实现要打华为云，单测无法走完；
// 用零值 *officeace.LoginSession 替身即可验证「成功返回后 muOALogin 必须已释放」这个不变量。
var officeAceStartLogin = officeace.StartLogin

// GetOfficeAceLoginStatus 查询 OfficeAce 自助登录状态。
// GetOfficeAceLoginStatus 查询 OfficeAce 自助登录状态。
func (a *App) GetOfficeAceLoginStatus() officeAceLoginStatus {
	a.muOALogin.Lock()
	sess := a.oaLogin
	a.muOALogin.Unlock()
	if sess == nil {
		return officeAceLoginStatus{Status: "idle"}
	}
	status, err := sess.Status()
	out := officeAceLoginStatus{Status: status}
	if status == "" {
		// 会话刚建立、run() 还没写首个状态：按「进行中」上报，避免前端误判成空闲。
		out.Status = "pending"
	}
	if err != nil {
		out.Error = err.Error()
	}
	if r, ok := sess.Result(); ok {
		out.UID = r.UID
		out.Nickname = r.Nickname
	}
	return out
}

// CancelOfficeAceLogin 取消 OfficeAce 自助登录。
func (a *App) CancelOfficeAceLogin() error {
	a.muOALogin.Lock()
	sess := a.oaLogin
	a.muOALogin.Unlock()
	if sess == nil {
		return errors.New("没有进行中的 OfficeAce 登录")
	}
	sess.Cancel()
	return nil
}
