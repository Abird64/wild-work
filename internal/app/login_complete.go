package app

import (
	"log"
	"path/filepath"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/login"
	logincodearts "wild-work/internal/login_codearts"
	logingemini "wild-work/internal/login_gemini"
	loginlobsterai "wild-work/internal/login_lobsterai"
	loginloomy "wild-work/internal/login_loomy"
	loginminimax "wild-work/internal/login_minimax"
	loginqoder "wild-work/internal/login_qoder"
	loginqodercn "wild-work/internal/login_qodercn"
	loginqodercom "wild-work/internal/login_qodercom"
	loginqwenwork "wild-work/internal/login_qwenwork"
	loginraccoon "wild-work/internal/login_raccoon"
	logintrae "wild-work/internal/login_trae"
	loginwbai "wild-work/internal/login_wbai"
	loginzcode "wild-work/internal/login_zcode"
	"wild-work/internal/provider"
	"wild-work/internal/qoder"
	"wild-work/internal/qodercn"
	"wild-work/internal/qodercom"
	"wild-work/internal/qwenwork"
	"wild-work/internal/raccoon"
)

func (a *App) completeLogin(r login.Result) {
	log.Printf("workbuddy 登录成功 uid=%s nickname=%s expires_in=%d refresh_token=%t", r.UID, r.Nickname, r.ExpiresIn, r.RefreshToken != "")
	fp, err := login.SaveAuth(a.cfg.AuthDir, r)
	if err != nil {
		log.Printf("workbuddy 登录保存凭证失败 uid=%s err=%v", r.UID, err)
		return
	}
	log.Printf("workbuddy 登录凭证已保存 uid=%s file=%s", r.UID, filepath.Base(fp))
	a.reloadAccounts()
	a.finishLogin()
	a.afterAccountAdded(provider.WorkBuddy)
	a.safeGo(func() {
		rt := a.runtime(provider.WorkBuddy)
		if rt == nil || rt.Scheduler == nil {
			return
		}
		if res, err := rt.Scheduler.CheckinAccount(r.UID); err != nil {
			log.Printf("新账号签到失败 %s: %v", r.Nickname, err)
		} else {
			log.Printf("新账号签到完成 %s：%s", r.Nickname, res.Msg)
		}
		// 积分兑底：签到路径失败（余额查询失败/签到异常）时 pool 内 credits 仍为 0，
		// 独立刷一次保证新账号首屏正确（幂等：多刷无害）。
		if _, err := a.RefreshCredits(r.UID); err != nil {
			log.Printf("workbuddy 新账号积分兑底失败 %s: %v", r.Nickname, err)
		}
	})
}

// completeWbaiLogin 国际版登录成功：写 auth 文件、重载账号池、异步拉取初始积分。
// 不触发签到（国际版无签到活动）；但必须拉一次余额，
// 否则新账号在面板上显示 0 积分，需用户手动点「刷新积分」才正确。
func (a *App) completeWbaiLogin(r loginwbai.Result) {
	log.Printf("workbuddyai 登录成功 uid=%s nickname=%s expires_in=%d refresh_token=%t",
		r.UID, r.Nickname, r.ExpiresIn, r.RefreshToken != "")
	if r.UID == "" {
		log.Printf("workbuddyai 登录失败：响应缺 uid")
		a.finishLogin()
		return
	}
	fp, err := loginwbai.SaveAuth(a.cfg.AuthDir, r)
	if err != nil {
		log.Printf("workbuddyai 登录保存凭证失败 uid=%s err=%v", r.UID, err)
		a.finishLogin()
		return
	}
	log.Printf("workbuddyai 登录凭证已保存 uid=%s file=%s", r.UID, filepath.Base(fp))
	a.reloadAccounts()
	a.finishLogin()
	a.afterAccountAdded(provider.WorkBuddyAI)
	// 新账号首次拉取余额（异步，不阻塞登录流程）
	a.safeGo(func() {
		if remain, err := a.RefreshCredits(r.UID); err != nil {
			log.Printf("workbuddyai 新账号积分获取失败 %s: %v", r.Nickname, err)
		} else {
			log.Printf("workbuddyai 新账号积分获取完成 %s: %d", r.Nickname, remain)
		}
	})
}

func (a *App) completeTraeLogin(r logintrae.Result) {
	log.Printf("traework 登录成功 uid=%s nickname=%s expires_at=%d refresh_token=%t", r.UID, r.Nickname, r.ExpiresAt, r.RefreshToken != "")
	fp, err := logintrae.SaveAuth(a.cfg.AuthDir, r)
	if err != nil {
		log.Printf("traework 登录保存凭证失败 uid=%s err=%v", r.UID, err)
		return
	}
	log.Printf("traework 登录凭证已保存 uid=%s file=%s", r.UID, filepath.Base(fp))
	a.reloadAccounts()
	a.finishLogin()
	a.afterAccountAdded(provider.TraeWork)
	a.safeGo(func() {
		rt := a.runtime(provider.TraeWork)
		if rt == nil || rt.Scheduler == nil {
			return
		}
		if res, err := rt.Scheduler.CheckinAccount(r.UID); err != nil {
			log.Printf("TraeWork 新账号签到失败 %s: %v", r.Nickname, err)
		} else {
			log.Printf("TraeWork 新账号签到完成 %s：%s", r.Nickname, res.Msg)
		}
		// 积分兑底（对齐 workbuddy）：签到路径失败时 pool 内 credits 仍为 0
		if _, err := a.RefreshCredits(r.UID); err != nil {
			log.Printf("TraeWork 新账号积分兑底失败 %s: %v", r.Nickname, err)
		}
	})
}

// completeQoderLogin 登录成功：生成机器指纹、写 auth 文件、重载账号池。
// Qoder 当前无签到活动，不做首次签到。
func (a *App) completeQoderLogin(r loginqoder.Result) {
	log.Printf("qoder 登录成功 uid=%s nickname=%s expires_in=%d refresh_token=%t", r.UID, r.Nickname, r.ExpiresIn, r.RefreshToken != "")
	au := &auth.Auth{Kind: "qoder", AccessToken: r.AccessToken, RefreshToken: r.RefreshToken, UID: r.UID, Nickname: r.Nickname}
	qoder.EnsureFingerprint(au)
	fp, err := loginqoder.SaveAuth(a.cfg.AuthDir, r, au.MachineID, au.MachineToken, au.MachineType)
	if err != nil {
		log.Printf("qoder 登录保存凭证失败 uid=%s err=%v", r.UID, err)
		return
	}
	log.Printf("qoder 登录凭证已保存 uid=%s file=%s", r.UID, filepath.Base(fp))
	a.reloadAccounts()
	a.finishLogin()
	a.afterAccountAdded(provider.Qoder)
	// 新账号首次拉取余额（对齐 workbuddyai/qwenwork：qoder 登录后无签到流程，
	// 不主动刷则面板显示 0 直到下个 auto-refresh 周期）
	a.safeGo(func() {
		if remain, err := a.RefreshCredits(r.UID); err != nil {
			log.Printf("qoder 新账号积分获取失败 %s: %v", r.Nickname, err)
		} else {
			log.Printf("qoder 新账号积分获取完成 %s: %d", r.Nickname, remain)
		}
	})
}

// completeQoderCNLogin 登录成功：生成机器指纹、写 auth 文件、重载账号池、首次签到。
func (a *App) completeQoderCNLogin(r loginqodercn.Result) {
	log.Printf("qodercn 登录成功 uid=%s nickname=%s expires_in=%d refresh_token=%t", r.UID, r.Nickname, r.ExpiresIn, r.RefreshToken != "")
	au := &auth.Auth{Kind: "qodercn", AccessToken: r.AccessToken, RefreshToken: r.RefreshToken, UID: r.UID, Nickname: r.Nickname}
	qodercn.EnsureFingerprint(au)
	fp, err := loginqodercn.SaveAuth(a.cfg.AuthDir, r, au.MachineID, au.MachineToken, au.MachineType)
	if err != nil {
		log.Printf("qodercn 登录保存凭证失败 uid=%s err=%v", r.UID, err)
		return
	}
	au.FilePath = fp // 回填落盘路径，后续 SaveAtomic（显示名回填）依赖它
	log.Printf("qodercn 登录凭证已保存 uid=%s file=%s", r.UID, filepath.Base(fp))
	a.reloadAccounts()
	a.finishLogin()
	a.afterAccountAdded(provider.QoderCN)
	// 新账号首次拉取余额 + 实测 userType + 回填显示名。
	// 设备流响应里无 nickname，显示名（邮箱/昵称）只能从 userinfo 拿：
	// 拉到后写回 au.Nickname 并 SaveAtomic 落盘，面板即显示真实用户名。
	a.safeGo(func() {
		if rt := a.runtime(provider.QoderCN); rt != nil {
			if cu, ok := rt.Upstream.(*qodercn.Client); ok {
				if name, _, err := cu.FetchUserInfo(au); err != nil {
					log.Printf("qodercn 新账号 userType 实测失败 %s: %v", r.UID, err)
				} else if name != "" && au.Nickname == "" {
					au.Nickname = name
					if err := au.SaveAtomic(); err != nil {
						log.Printf("qodercn 显示名回写失败 %s: %v", r.UID, err)
					} else {
						log.Printf("qodercn 显示名已回填 %s: %s", r.UID, name)
						a.reloadAccounts() // 池内 nickname 刷新，面板即时可见
					}
				}
			}
		}
		if remain, err := a.RefreshCredits(r.UID); err != nil {
			log.Printf("qodercn 新账号积分获取失败 %s: %v", r.Nickname, err)
		} else {
			log.Printf("qodercn 新账号积分获取完成 %s: %d", r.Nickname, remain)
		}
	})
}

// completeQoderCOMLogin 登录成功：生成机器指纹、写 auth 文件、重载账号池、首次签到（仅 campaigns）。
func (a *App) completeQoderCOMLogin(r loginqodercom.Result) {
	log.Printf("qodercom 登录成功 uid=%s nickname=%s expires_in=%d refresh_token=%t", r.UID, r.Nickname, r.ExpiresIn, r.RefreshToken != "")
	au := &auth.Auth{Kind: "qodercom", AccessToken: r.AccessToken, RefreshToken: r.RefreshToken, UID: r.UID, Nickname: r.Nickname}
	qodercom.EnsureFingerprint(au)
	fp, err := loginqodercom.SaveAuth(a.cfg.AuthDir, r, au.MachineID, au.MachineToken, au.MachineType)
	if err != nil {
		log.Printf("qodercom 登录保存凭证失败 uid=%s err=%v", r.UID, err)
		return
	}
	au.FilePath = fp // 回填落盘路径，后续 SaveAtomic（显示名回填）依赖它
	log.Printf("qodercom 登录凭证已保存 uid=%s file=%s", r.UID, filepath.Base(fp))
	a.reloadAccounts()
	a.finishLogin()
	a.afterAccountAdded(provider.QoderCOM)
	// 新账号首次拉取余额 + 实测 userType + 回填显示名。
	// 设备流响应里无 nickname，显示名（邮箱/昵称）只能从 userinfo 拿：
	// 拉到后写回 au.Nickname 并 SaveAtomic 落盘，面板即显示真实用户名。
	a.safeGo(func() {
		if rt := a.runtime(provider.QoderCOM); rt != nil {
			if cu, ok := rt.Upstream.(*qodercom.Client); ok {
				if name, _, err := cu.FetchUserInfo(au); err != nil {
					log.Printf("qodercom 新账号 userType 实测失败 %s: %v", r.UID, err)
				} else if name != "" && au.Nickname == "" {
					au.Nickname = name
					if err := au.SaveAtomic(); err != nil {
						log.Printf("qodercom 显示名回写失败 %s: %v", r.UID, err)
					} else {
						log.Printf("qodercom 显示名已回填 %s: %s", r.UID, name)
						a.reloadAccounts() // 池内 nickname 刷新，面板即时可见
					}
				}
			}
		}
		if remain, err := a.RefreshCredits(r.UID); err != nil {
			log.Printf("qodercom 新账号积分获取失败 %s: %v", r.Nickname, err)
		} else {
			log.Printf("qodercom 新账号积分获取完成 %s: %d", r.Nickname, remain)
		}
	})
}

// completeQwenWorkLogin 登录成功：写 auth 文件、重载账号池。
// 千问办公无签到活动（每日积分服务端被动发放），不做首次签到。
func (a *App) completeQwenWorkLogin(r loginqwenwork.Result) {
	log.Printf("qwenwork 登录成功 uid=%s nickname=%s expires_in=%d refresh_token=%t",
		r.UID, r.Nickname, r.ExpiresIn, r.RefreshToken != "")
	fp, err := loginqwenwork.SaveAuth(a.cfg.AuthDir, r)
	if err != nil {
		log.Printf("qwenwork 登录保存凭证失败 uid=%s err=%v", r.UID, err)
		return
	}
	log.Printf("qwenwork 登录凭证已保存 uid=%s file=%s", r.UID, filepath.Base(fp))
	a.reloadAccounts()
	a.finishLogin()
	a.afterAccountAdded(provider.QwenWork)
	// 首屏初始化（顺序敏感，串行执行）：
	// 1) 刷新余额 —— 若 Poll 兑换的首个 token 无效（实测 OAuth 兑换 token 调 /user/info
	//    会 401 invalid-credential），refreshIfSessionDead 会自动换新 token；
	// 2) 之后再用有效 token 拉昵称兑底（Poll 兑换的 JWT 实测无 username 字段，
	//    refresh 后的 device_token 才有），写回 auth 文件，否则面板显示 hex uid。
	a.safeGo(func() {
		var nickname = r.Nickname
		if remain, err := a.RefreshCredits(r.UID); err != nil {
			log.Printf("qwenwork 新账号积分获取失败 %s: %v", r.Nickname, err)
		} else {
			log.Printf("qwenwork 新账号积分获取完成 %s: %d", r.Nickname, remain)
		}
		if nickname == "" {
			rt := a.runtime(provider.QwenWork)
			if rt == nil || rt.Upstream == nil {
				return
			}
			au := rt.Pool.AuthByUID(r.UID)
			if au == nil {
				return
			}
			qw, ok := rt.Upstream.(*qwenwork.Client)
			if !ok {
				return
			}
			if nick, nerr := qw.FetchNickname(au); nerr == nil && nick != "" {
				nickname = nick
				au.Nickname = nick // AuthByUID 返回池内指针，改字段即生效；SaveAtomic 自带锁
				_ = au.SaveAtomic()
				log.Printf("qwenwork 昵称兑底成功 uid=%s nickname=%s", r.UID, nick)
			} else if nerr != nil {
				log.Printf("qwenwork 昵称兑底失败 uid=%s err=%v", r.UID, nerr)
			}
		}
	})
}

// completeMinimaxLogin MiniMax 设备码登录成功后的收尾。
//
// 刻意**不**像 trae 登录那样顺带签到：MiniMax 是 7 天签到面板，用户在面板点一次即可，
// 少发一次上游请求（AGENTS.md §0.1「验证上游行为最小化请求数」的同源纪律）。
// completeLobsterAILogin LobsterAI 本机回调登录成功后的收尾。
//
// 与 minimax 同样**不**登录即签到：签到是面板上的显式动作，少发一次上游请求
// （AGENTS.md §0.1 的同源纪律）。
// completeZCodeLogin ZCode 设备授权登录成功后的收尾。
//
// 与 minimax / lobsterai 同样**不**登录即签到：ZCode 的积分领取依赖阿里云验证码
// （headful 浏览器），本仓不实现；签到走面板手动或干脆不做，少发一次上游请求。
// completeGeminiLogin Gemini loopback OAuth 登录成功后的收尾。
//
// 与其他渠道一致**不**登录即签到：Gemini 本渠道就没有签到（DailyCheckin 返回「不支持」）。
// completeCodeArtsLogin CodeArts PKCE OAuth 登录成功后的收尾。
//
// 本渠道**有**每日签到（三步、走 HMAC 签名，渠道包实现了 CheckinReporter/CheckinGranter），
// 但与其他渠道一致**不**在登录时顺手签到：定时任务会在签到时间自行领取，少发一次上游请求。
func (a *App) completeCodeArtsLogin(r logincodearts.Result) {
	// 带上用户在登录弹窗里填的「账号标识」：上游不提供稳定身份字段（实测），只能靠它区分账号
	// ——留空 = 覆盖该渠道已有条目（同一账号重新登录）；填写 = 作为独立账号并存。
	// 取完即清，避免污染下一次登录。
	a.muLogin.Lock()
	r.Label = a.loginLabel
	a.loginLabel = ""
	a.muLogin.Unlock()
	log.Printf("codearts 登录成功 uid=%s nickname=%s label=%q expires_at=%d refresh_token=%t", r.UID, r.Nickname, r.Label, r.ExpiresAt, r.RefreshToken != "")
	fp, err := logincodearts.SaveAuth(a.cfg.AuthDir, r)
	if err != nil {
		log.Printf("codearts 登录保存凭证失败 uid=%s err=%v", r.UID, err)
		return
	}
	log.Printf("codearts 登录凭证已保存 uid=%s file=%s", r.UID, filepath.Base(fp))
	a.reloadAccounts()
	a.finishLogin()
	a.afterAccountAdded(provider.CodeArts)
}

func (a *App) completeGeminiLogin(r logingemini.Result) {
	log.Printf("gemini 登录成功 uid=%s nickname=%s expires_at=%d refresh_token=%t", r.UID, r.Nickname, r.ExpiresAt, r.RefreshToken != "")
	fp, err := logingemini.SaveAuth(a.cfg.AuthDir, r)
	if err != nil {
		log.Printf("gemini 登录保存凭证失败 uid=%s err=%v", r.UID, err)
		return
	}
	log.Printf("gemini 登录凭证已保存 uid=%s file=%s", r.UID, filepath.Base(fp))
	a.reloadAccounts()
	a.finishLogin()
	a.afterAccountAdded(provider.Gemini)
}

func (a *App) completeZCodeLogin(r loginzcode.Result) {
	log.Printf("zcode 登录成功 uid=%s nickname=%s expires_at=%d refresh_token=%t", r.UID, r.Nickname, r.ExpiresAt, r.RefreshToken != "")
	fp, err := loginzcode.SaveAuth(a.cfg.AuthDir, r)
	if err != nil {
		log.Printf("zcode 登录保存凭证失败 uid=%s err=%v", r.UID, err)
		return
	}
	log.Printf("zcode 登录凭证已保存 uid=%s file=%s", r.UID, filepath.Base(fp))
	a.reloadAccounts()
	a.finishLogin()
	a.afterAccountAdded(provider.ZCode)
}

func (a *App) completeLobsterAILogin(r loginlobsterai.Result) {
	log.Printf("lobsterai 登录成功 uid=%s nickname=%s expires_at=%d refresh_token=%t", r.UID, r.Nickname, r.ExpiresAt, r.RefreshToken != "")
	fp, err := loginlobsterai.SaveAuth(a.cfg.AuthDir, r)
	if err != nil {
		log.Printf("lobsterai 登录保存凭证失败 uid=%s err=%v", r.UID, err)
		return
	}
	log.Printf("lobsterai 登录凭证已保存 uid=%s file=%s", r.UID, filepath.Base(fp))
	a.reloadAccounts()
	a.finishLogin()
	a.afterAccountAdded(provider.LobsterAI)
}

func (a *App) completeMinimaxLogin(r loginminimax.Result) {
	log.Printf("minimax 登录成功 uid=%s nickname=%s expires_at=%d refresh_token=%t", r.UID, r.Nickname, r.ExpiresAt, r.RefreshToken != "")
	fp, err := loginminimax.SaveAuth(a.cfg.AuthDir, r)
	if err != nil {
		log.Printf("minimax 登录保存凭证失败 uid=%s err=%v", r.UID, err)
		return
	}
	log.Printf("minimax 登录凭证已保存 uid=%s file=%s", r.UID, filepath.Base(fp))
	a.reloadAccounts()
	a.finishLogin()
	a.afterAccountAdded(provider.Minimax)
}

// completeRaccoonLogin 小浣熊网页版登录成功：把捕获并换取的 token 落成本工具凭据。
// 凭据形态与「从本机客户端导入」完全一致（两者拿到的是同一个上游账号）：
// uid 取 access_token 的 JWT name claim，domain/apiHost 对齐 internal/raccoon 的调用前缀。
// access_token 约 1h，靠 refresh_token 续期；签到走 setting_info 触发每日积分落账
// （见 internal/raccoon/checkin.go），故登录后立即签到一次，让当日积分马上落账。
func (a *App) completeRaccoonLogin(r loginraccoon.Result) {
	// uid 提取统一走 raccoon.JWTUID：旧版 JWT 取 name，新版（2026-10-15 上游迁站后）
	// 标准 OIDC 无 name claim，回落 sub。与客户会话自愈、导入路径同源，避免多处各自漂移。
	uid := raccoon.JWTUID(r.AccessToken)
	if uid == "" {
		uid = "default"
	}
	expiresAt := r.ExpiresAt
	if expiresAt <= 0 {
		expiresAt = time.Now().Add(time.Hour).Unix() // 实测 access_token 寿命约 1h
	}
	log.Printf("raccoon 登录成功 uid=%s org=%s refresh_token=%t", uid, r.OfficeOrgName, r.RefreshToken != "")
	doc := authDoc{
		Auth: authSection{
			AccessToken:  r.AccessToken,
			RefreshToken: r.RefreshToken,
			ExpiresAt:    expiresAt,
			Domain:       "/api/web/llm/v2",
			ApiHost:      raccoon.MainSite,
		},
		Account: accountSection{
			UID:          uid,
			EnterpriseID: r.OfficeIdentity,
			Nickname:     uid,
		},
	}
	fp, err := a.writeAuthFile("raccoon", uid, doc)
	if err != nil {
		log.Printf("raccoon 登录保存凭证失败 uid=%s err=%v", uid, err)
		return
	}
	log.Printf("raccoon 登录凭证已保存 uid=%s file=%s", uid, filepath.Base(fp))
	a.reloadAccounts()
	a.finishLogin()
	a.afterAccountAdded(provider.Raccoon)
	// 首次签到（对齐 workbuddy/traework）：setting_info 触发当日每日积分落账，
	// 不签到的话新账号要等下一个签到时刻才会把当天该给的 300 分记上。
	a.safeGo(func() { a.firstCheckinRaccoon(uid) })
}

// firstCheckinRaccoon 新账号首次签到 + 积分兜底（幂等：已领过时上游返回空 popups）。
func (a *App) firstCheckinRaccoon(uid string) {
	rt := a.runtime(provider.Raccoon)
	if rt == nil || rt.Scheduler == nil {
		return
	}
	if res, err := rt.Scheduler.CheckinAccount(uid); err != nil {
		log.Printf("raccoon 新账号签到失败 %s: %v", uid, err)
	} else {
		log.Printf("raccoon 新账号签到完成 %s：%s", uid, res.Msg)
	}
	// 积分兜底：签到路径失败（余额查询失败/签到异常）时 pool 内 credits 仍为 0，
	// 独立刷一次保证新账号首屏正确（幂等：多刷无害）。
	if _, err := a.RefreshCredits(uid); err != nil {
		log.Printf("raccoon 新账号积分兜底失败 %s: %v", uid, err)
	}
}

// completeLogin 登录成功：写 auth 文件、重载账号池，异步签到。
// completeLoomyLogin Loomy 浏览器登录（短信验证码）成功后的收尾。
//
// 凭据形态与「从本机客户端导入」（import_local.go 的 importLoomy）**完全一致** ——
// 两者拿到的是同一个上游 session，只是来源不同：
//   - uid 优先取上游 userid，缺失时由 login_loomy 从前缀派生（账号池需要稳定去重标识）；
//   - domain/apiHost 对齐 internal/loomy 的调用前缀；
//   - refreshToken 留空：Loomy **没有任何 refresh 端点**，session 约 14 天到期后
//     只能重新登录（或重新从客户端导入）。留空还能让 scheduler 跳过必然失败的保活。
//
// 刻意**不**做登录即签到：internal/loomy 没有签到端点（见 noExplicitCheckin 的说明），
// 硬加一次请求只会产生一条无意义的失败流水。
func (a *App) completeLoomyLogin(r loginloomy.Result) {
	uid := strings.TrimSpace(r.UID)
	if uid == "" {
		// login_loomy 已保证非空（上游缺 userid 时会派生）；这里是防御，
		// 避免把半成品凭据写成 loomy-default.json 污染账号池。
		log.Printf("loomy 登录失败：缺少账号标识（上游未下发 userid）")
		a.finishLogin()
		return
	}
	// LogFields 只含 uid / 脱敏手机号 / 到期时刻，**不含 token**（零 token 落盘红线）。
	log.Printf("loomy 登录成功 %s", r.LogFields())
	doc := authDoc{
		Auth: authSection{
			AccessToken: r.AccessToken,
			// 留空：Loomy 无 refresh 端点（与 importLoomy 同口径）。
			RefreshToken: "",
			ExpiresAt:    r.ExpiresAt,
			Domain:       r.Domain,
			ApiHost:      r.ApiHost,
		},
		Account: accountSection{
			UID:      uid,
			Nickname: r.Nickname,
		},
	}
	fp, err := a.writeAuthFile("loomy", uid, doc)
	if err != nil {
		log.Printf("loomy 登录失败：写凭据出错 uid=%s err=%v", uid, err)
		a.finishLogin()
		return
	}
	log.Printf("loomy 登录成功，凭据已落盘 uid=%s file=%s", uid, filepath.Base(fp))
	a.reloadAccounts()
	a.finishLogin()
	a.afterAccountAdded(provider.Loomy)
	// 登录后立即领一次宠物打工奖励：此时凭据刚拿到，若该账号今天在官方客户端已有
	// 有效对话轮次，这一步就能把已达档位的奖励领掉，不必等到下一个签到时刻。
	// 与 raccoon 同款（firstCheckinRaccoon）：**走调度器**，领到的积分才会进流水账；
	// 失败只记日志，不影响「登录成功」这个主结果。
	a.safeGo(func() { a.firstCheckinLoomy(uid) })
}

// firstCheckinLoomy 新账号首次签到（领宠物打工奖励）+ 积分兜底。
//
// 与 firstCheckinRaccoon 同形：走调度器的 CheckinAccount，因而领到的积分会经
// Ledger.RecordCheckinEarn 进流水；随后独立刷一次余额，保证新账号首屏数字正确。
// 幂等：未达档位时上游返回 granted=false，不会重复发放。
func (a *App) firstCheckinLoomy(uid string) {
	rt := a.runtime(provider.Loomy)
	if rt == nil || rt.Scheduler == nil {
		return
	}
	if res, err := rt.Scheduler.CheckinAccount(uid); err != nil {
		log.Printf("loomy 新账号签到失败 %s: %v", uid, err)
	} else {
		log.Printf("loomy 新账号签到完成 %s：%s", uid, res.Msg)
	}
	// 积分兜底：签到路径失败（余额查询失败/未达档位）时 pool 内 credits 仍为 0，
	// 独立刷一次保证新账号首屏正确（幂等：多刷无害；Loomy 已在积分自动刷新名单里）。
	if _, err := a.RefreshCredits(uid); err != nil {
		log.Printf("loomy 新账号积分兜底失败 %s: %v", uid, err)
	}
}

// afterAccountAdded 新账号就绪后的统一收尾：刷新模型列表与费率。
// 原因：模型列表与费率都按「已接入账号」填充，新渠道/新账号加入后，
// 若不主动失效缓存，两处都要等 TTL（1h / 1h）才更新，面板上显示不完整。
// 故每个渠道登录成功后统一调用。
func (a *App) afterAccountAdded(kind provider.Kind) {
	// 1) 失效模型缓存后【必须】立即重建——否则其它渠道的模型缓存被误杀：
	//    InvalidateModels 清空的是【全部渠道】的缓存，而 RefreshPricing 只拉费率不拉模型，
	//    若只失效不重建，qodercn/qodercom 等无静态兑底渠道会从面板整组消失，
	//    直到下一个 30min ticker（生产 2026-09-28 实测：登录 qwenwork 后 qoder 双渠道消失 28min）。
	if a.handler != nil {
		a.handler.InvalidateModels()
		a.refreshChannelModels()
	}
	// 2) 立即刷新费率为后台任务，不阻塞登录流程
	a.safeGo(func() {
		a.RefreshPricing()
		log.Printf("account added platform=%s: models+pricing refreshed", kind)
	})
}
