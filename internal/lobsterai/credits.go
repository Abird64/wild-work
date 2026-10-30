// 积分与资源明细：UserResource / UserResourceDetail 与 creditItems 解析。
//
// 从 client.go 拆出（结构重排，零行为变更）。
package lobsterai

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// UserResource 查询可消耗积分余额（pool 路由口径）。
func (c *Client) UserResource(a *auth.Auth) (int64, error) {
	remain, _, err := c.creditSummary(a)
	return remain, err
}

// UserResourceDetail 返回余额与明细条目。
//
// 明细直接映射上游 creditItems[]：包名优先 label（实测「每日登录奖励」）、回退 type、
// 再回退「积分包」；到期时间取 expiresAt（ISO8601）并归一到 UTC+8 墙钟的日期粒度
// （不变式 18：ResourceItem.ExpireAt 是 YYYY-MM-DD；上游没给就留空串，前端隐藏整列）。
func (c *Client) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	return c.creditSummary(a)
}

// creditSummary 单次请求同时产出总额与明细（口径同源，避免两处各算一遍）。
func (c *Client) creditSummary(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	tok := tokenOf(a)
	if tok == "" {
		return 0, nil, &provider.Error{Kind: provider.ErrSessionDead, Msg: "lobsterai：账号没有 access_token"}
	}
	// 只发 4 个基础头（协议 §2.1：签到/余额不带客户端能力头）。
	status, raw, err := c.getJSON(c.base()+EpProfileSummary, tok)
	if err != nil {
		return 0, nil, err
	}
	if e := c.businessErr(status, raw); e != nil {
		return 0, nil, e
	}
	top, perr := decodeObject(raw)
	if perr != nil {
		return 0, nil, fmt.Errorf("lobsterai：积分响应解析失败：%w", perr)
	}
	if code, msg, ok := envelopeCode(top); ok && code != 0 {
		return 0, nil, c.envelopeError(status, code, msg, raw)
	}
	data := businessObject(top)
	// 总额：data.totalCreditsRemaining，**负数 clamp 到 0**（协议 §7.2）。
	total := intOfRaw(data["totalCreditsRemaining"])
	if total < 0 {
		total = 0
	}
	items := creditItems(data["creditItems"])
	if total == 0 && len(items) == 0 {
		// 协议 §7.2 的空判据：**「总额 0 且无明细」必须与「余额真的为 0」区分**。
		// wild-work 的通道是 error（面板据此显示「查不到」而不是 0；creditSnapshot
		// 也不会把 0 写进 pool 快照与 ledger 差分）。
		//
		// 为什么用 ErrPassthrough：它在本仓的语义是「请求级、不罚号、不轮转」，
		// 恰好是这里需要的（这不是会话失效，也不是账号故障）。**未确认**：
		// 参考实现同样无法区分这两种情形，只是它选择返回 null；本仓没有 null 通道。
		return 0, nil, &provider.Error{Kind: provider.ErrPassthrough, Status: status,
			Msg: "lobsterai：积分接口未给出可解释的数据（总额 0 且无明细）"}
	}
	return total, items, nil
}

// creditPackage 明细条目的中间形态（面值要按组二次聚合，故先收集再产出）。
type creditPackage struct {
	name   string
	remain int64
}

// creditItems 解析 data.creditItems[]（协议 §7.2）。
//
// 面值推断：服务端**没有**面值字段，协议的做法是取「同组（同 label/type）有效包剩余量
// 的最大值」当面值 —— 于是 Used = 面值 − 剩余量。这是**展示口径**，不是真实历史消耗
// （协议未确认上游有 per-package 的发放总量）。
func creditItems(raw json.RawMessage) []provider.ResourceItem {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		// 非数组 ⇒ 视作「无明细」而不是失败（与 minimax 同口径：缺字段 ≠ 查询失败）。
		return nil
	}
	pkgs := make([]creditPackage, 0, len(list))
	expires := make([]string, 0, len(list))
	face := make(map[string]int64, len(list))
	for _, it := range list {
		obj, err := decodeObject(it)
		if err != nil {
			continue // 坏条目只损失自己
		}
		name := strings.TrimSpace(strOfRaw(obj["label"]))
		if name == "" {
			name = strings.TrimSpace(strOfRaw(obj["type"]))
		}
		if name == "" {
			name = "积分包"
		}
		// creditsRemaining 实测可能给字符串（"100.00"），故用宽松取数（协议 §7.2）。
		remain := intOfRaw(obj["creditsRemaining"])
		if remain < 0 {
			remain = 0
		}
		pkgs = append(pkgs, creditPackage{name: name, remain: remain})
		expires = append(expires, dateOfISO(strOfRaw(obj["expiresAt"])))
		if remain > face[name] {
			face[name] = remain
		}
	}
	out := make([]provider.ResourceItem, 0, len(pkgs))
	for i, p := range pkgs {
		total := face[p.name]
		used := total - p.remain
		if used < 0 {
			used = 0
		}
		out = append(out, provider.ResourceItem{
			Name:   p.name,
			Total:  total,
			Used:   used,
			Remain: p.remain,
			// ExpireAt 为空串 = 上游未下发（前端隐藏「有效期」列，不得用零值冒充）。
			ExpireAt: expires[i],
			// Usable 恒 true：这些包就是本工具消耗的额度池（协议没有专用池概念）。
			Usable: true,
			// Key 留空：上游条目没有稳定 ID 字段（协议 §7.2 的字段表只有
			// creditsRemaining/type/label/expiresAt），按 ResourceItem 约定退回
			// Name 作伪键。已知限制：同名不同到期日的条目在 ledger 差分里会并成一条。
		})
	}
	return out
}

// dateOfISO 把上游 ISO8601 的到期时间归一到 UTC+8 墙钟的 **RFC3339 精确时刻**。
// 解析不出（缺失 / 非 ISO）→ 空串：**不得**用零值时间冒充「永不过期」。
//
// 历史：旧实现截到日期粒度（不变式 18 时代的写法），丢了时分秒。面板按
// 「N 小时内到期」分桶需要精确时刻（2026-10-19 实测）。
//
// 注：lobsterai 的 ledger 伪键用条目名（Key 留空 ⇒ itemKey 退回 Name），
// 不含日期，故本改动零抖动。
func dateOfISO(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// 带时区偏移的形态：按偏移解析后归一到 UTC+8 输出
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.In(expireLoc).Format(time.RFC3339)
	}
	// 无时区后缀的形态：按 **UTC+8 墙钟**解析（上游是国内服务，下发的是本地墙钟）。
	// 旧实现用 time.Parse 会按 UTC 解析，"2026-10-31 12:00:00" 变成 20:00+08:00，
	// 整整偏 8 小时——精确到时刻后这个偏差会直接算错「剩几小时」（2026-10-19 修）。
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, expireLoc); err == nil {
			return t.Format(time.RFC3339)
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// provider.Upstream：签到（协议 §7.1 三步流程）
// ---------------------------------------------------------------------------
