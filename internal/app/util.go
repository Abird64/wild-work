package app

import (
	"net"
	"sort"
	"strings"
	"time"

	"wild-work/internal/provider"
)

// channelRank 渠道排序键：未登记的渠道一律返回同一个值（排在已登记渠道之后）。
//
// 注意：单独用本函数做比较**不能**决定未登记渠道之间的先后——它们的 rank 全相等，
// sort.SliceStable 会保持插入顺序。要「未登记渠道按字母序」必须走 channelLess。
func channelRank(k provider.Kind) int {
	if r, ok := channelOrder[k]; ok {
		return r
	}
	return 100
}

// channelLess 是渠道排序的**唯一**比较函数：
//  1. 已登记渠道按 channelOrder 的序号；
//  2. 未登记渠道排在所有已登记渠道之后，且彼此按渠道名（ASCII）升序——
//     channelRank 对它们恒返回同一值，只比 rank 会退化成插入顺序。
//
// 为什么专门写这一条：channelOrder 只登记了部分渠道（实测 15/23），
// 而 channelOrder 的注释长期写着「未出现在表中的渠道排最后（按字母序）」——
// 实际实现并不成立（全打平 ⇒ SliceStable 保持插入序）。这里让注释与行为对上。
func channelLess(a, b provider.Kind) bool {
	ra, rb := channelRank(a), channelRank(b)
	if ra != rb {
		return ra < rb
	}
	return a < b
}

// sortChannelsByOrder 按 channelOrder 固定顺序稳定排序渠道条目（泛型，复用于各展示处）。
func sortChannelsByOrder[T any](items []T, kindOf func(T) provider.Kind) {
	sort.SliceStable(items, func(i, j int) bool { return channelLess(kindOf(items[i]), kindOf(items[j])) })
}

// maskKey 脱敏形态：长头 + **** + 短尾（R121）。头取 12 个字符、尾取 4 个，但可见总量
// 不得超过原文一半（短 key 自动收缩头部，收到 4 字符以下就直接全遮）。
// 头部留长是硬需求：第三方 key 的**前缀**才是辨识度来源（百度千帆 bce-v3/ALTAK…、
// OpenAI sk-proj-…），只留 5 个字符根本看不出是哪家，不能假定密钥都形如 sk-…。
// 中间固定四个星（不泄漏长度），四星同时充当「面板未改动」标记（见 isMaskedKey）。
func maskKey(key string) string {
	r := []rune(strings.TrimSpace(key))
	if len(r) == 0 {
		return ""
	}
	if len(r) <= 12 {
		return maskedMark // 短到「露头 + 尾」就等于露大半原文，直接全遮
	}
	head, tail := 12, 4
	if head+tail > len(r)/2 {
		head = len(r)/2 - tail // 可见字符总量没有上限一半，短 key 自动收缩头部
	}
	if head < 4 {
		return maskedMark
	}
	return string(r[:head]) + maskedMark + string(r[len(r)-tail:])
}

// maskedMark 是脱敏串的中间标记，同时被当作「面板未改动过此字段」的记号（isMaskedKey）。
const maskedMark = "****"

// isMaskedKey 认一个字符串是不是脱敏形态（而不是用户真填的密钥）。
// 三种都认：新格式 ****、旧格式 …、更早的前端 … 占位——升级瞬间浏览器里可能还
// 渲染着旧脱敏串，当成真值落盘会把可用密钥洗成废值（这个判定同时活在 Go 与 JS 两侧）。
func isMaskedKey(s string) bool {
	return strings.Contains(s, maskedMark) || strings.Contains(s, "…") || strings.Contains(s, "••")
}

// csvContains 判断 CSV 串里是否含某个值（逐项 TrimSpace 后精确比较）。
// 与 officeace 内部 splitCSV/containsCSV 同口径；这里只需判定、无需解析成切片以外的东西。
func csvContains(csv, want string) bool {
	if csv == "" || want == "" {
		return false
	}
	for _, part := range strings.Split(csv, ",") {
		if strings.TrimSpace(part) == want {
			return true
		}
	}
	return false
}

func normalizeMinutes(minutes []int) []int {
	seen := map[int]bool{}
	out := []int{}
	for _, m := range minutes {
		if m >= 0 && m < 24*60 && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	sort.Ints(out)
	return out
}

// lanIP 返回非环回的出站 IPv4/IPv6 地址（如 192.168.1.5），用于监听 0.0.0.0 时
// 面板提示局域网可用的 API 地址。UDP Dial 不实际发包，仅让内核选路由；
// 无网络/全部环回时返回空串，调用方自行兑底。
func lanIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:53")
	if err != nil {
		return ""
	}
	defer conn.Close()
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || addr.IP == nil || addr.IP.IsLoopback() {
		return ""
	}
	return addr.IP.String()
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("01-02 15:04")
}

// unixOrZero 解冻时刻的 unix 秒（零值返回 0）。统一 24 小时制补充通道：
// "01-02 15:04" 文案虽是 24h 制，但跨零点的次日清晨时刻（如 07:07）仍易被
// 误读为「12 小时制丢了上午/下午」；面板拿到 until_ts 后可实时算出
// 「剩 11 时 53 分」，绝对时刻 + 相对时长并列展示，歧义自然消除。
func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// fmtTimeMap 模型级冷却 map 的时间格式化（仅保留未过期条目；空 map 返回 nil 以便 omitempty）。
func fmtTimeMap(m map[string]time.Time) map[string]string {
	if len(m) == 0 {
		return nil
	}
	now := time.Now()
	out := make(map[string]string, len(m))
	for model, t := range m {
		if t.After(now) {
			out[model] = fmtTime(t)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func shortUID(uid string) string {
	if len(uid) <= 10 {
		return uid
	}
	return uid[:10] + "…"
}

func shortErr(err error) string {
	s := strings.TrimSpace(err.Error())
	if len(s) > 120 {
		return s[:120]
	}
	return s
}
