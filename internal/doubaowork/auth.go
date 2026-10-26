package doubaowork

import "wild-work/internal/auth"

// 本渠道同样是「零凭据落盘」：cookie 在客户端浏览器里，auth 只承载池内标识。
//
// 与 doubao 分成两个虚拟账号（而非共用）的原因：两者是**不同的产品形态**，模型维度、
// 协议、冷却状态都该各自独立——共用一条 uid 会让两个渠道的惩罚互相牵连。
const (
	ClientUID  = "doubaowork-client"
	ClientName = "豆包工作（本机客户端·工作任务）"
	clientTok  = "doubaowork-client-local"
)

// ClientAuth 返回本渠道唯一的账号标识。
func ClientAuth() *auth.Auth {
	return &auth.Auth{
		Kind:        string(Kind),
		AccessToken: clientTok,
		UID:         ClientUID,
		Nickname:    ClientName,
		Domain:      "www.doubao.com",
	}
}
