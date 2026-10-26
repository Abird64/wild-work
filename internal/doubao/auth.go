package doubao

import "wild-work/internal/auth"

// 本渠道的虚拟账号标识。
//
// # 为什么是「虚拟」账号而不是真实账号
//
// 豆包的凭据**不在本工具手里**：登录态是客户端浏览器里的 cookie（含 httpOnly 的
// sessionid），所有上游调用都通过 doubaocdp.CDP 在页面上下文里完成，cookie 由浏览器自动携带。
// 因此本工具既拿不到、也不需要保存任何 token。
//
// 这是全部 21 个渠道里唯一「零凭据落盘」的渠道——比 R0.2「日志/面板绝不输出 token」
// 更强一层：连 auth 文件里都没有可泄露的东西。
//
// 代价：pool 需要一个条目来承载冷却/统计，所以这里造一个稳定标识。
// UID 固定为 `doubao-client`：同一台机器上换登录账号后 UID 不变，
// 快照/流水会延续（对单账号渠道这是想要的行为，不会把历史切碎）。
const (
	ClientUID   = "doubao-client"
	ClientName  = "豆包工作（本机客户端）"
	clientToken = "doubao-client-local" // 仅作占位，不参与任何上游鉴权
)

// ClientAuth 返回本渠道唯一的账号标识。
//
// 与 oczen/kilo 的 AnonymousAuth 同形（R16 的「无账号渠道」装配方式），
// 但语义不同：这不是匿名通道，而是「凭据在别处（客户端里）」的本地通道。
func ClientAuth() *auth.Auth {
	return &auth.Auth{
		Kind:        string(Kind),
		AccessToken: clientToken,
		UID:         ClientUID,
		Nickname:    ClientName,
		Domain:      "www.doubao.com",
	}
}
