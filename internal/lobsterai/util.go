// 小工具：JSON 取值、UUID、HTTP 收发、时间解析。
//
// 从 client.go 拆出（结构重排，零行为变更）。
package lobsterai

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"wild-work/internal/auth"
)

// firstPresent 按给定键序取第一个**存在**的整数值（宽松兼容字符串数字）。
func firstPresent(obj map[string]json.RawMessage, keys ...string) int64 {
	for _, k := range keys {
		raw, ok := obj[k]
		if !ok || len(raw) == 0 || string(raw) == "null" {
			continue
		}
		return intOfRaw(raw)
	}
	return 0
}

// stringList 把 JSON 数组解析成字符串切片（形状不符时给 nil）。
func stringList(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil
	}
	return list
}

// contains 判定字符串切片是否含某值（大小写敏感：动作名是协议里的字面量）。
func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// newUUID4 生成 RFC 4122 v4 UUID 字符串（签到幂等键，协议 §7.1）。
//
// 幂等键由**客户端**生成：上游用它去重，重试时必须换新键才代表「新的一次尝试」，
// 而同一次尝试内重试应复用同一键（本实现了没有重试，故每次调用生成一个）。
func newUUID4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant RFC 4122
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// ---------------------------------------------------------------------------
// provider.Upstream：错误分类（协议 §5.1）
// ---------------------------------------------------------------------------

// getJSON 发一个带 Bearer 的 GET（业务端点：积分 / 签到），只带 4 个基础头。
func (c *Client) getJSON(url, token string) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	c.authHeaders(req, token, "application/json", false)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, raw, nil
}

// postJSON 发一个 JSON POST。
//
// token 为空时不带 Authorization：exchange / refresh 都是这种形态（协议 §2.1）——
// 凭据在请求体里，带上 Authorization 反而是上游没见过的形态。
func (c *Client) postJSON(url, token string, body []byte, withClientMeta bool) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	if token == "" {
		credentialHeaders(req)
	} else {
		c.authHeaders(req, token, "application/json", withClientMeta)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, raw, nil
}

// tokenOf 取账号的 access_token 快照（锁内读，防与续期写回竞争）。
func tokenOf(a *auth.Auth) string {
	if a == nil {
		return ""
	}
	return strings.TrimSpace(a.AccessTokenValue())
}

// extraOf 取 auth.Extra 里的扩展凭据（nil 安全）。
func extraOf(a *auth.Auth, key string) string {
	if a == nil {
		return ""
	}
	return strings.TrimSpace(a.Extra[key])
}

// decodeObject 把 JSON 解成「键 → 原始值」。
func decodeObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, errors.New("字段缺失")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errors.New("不是 JSON 对象")
	}
	return m, nil
}

// businessObject 取业务字段所在的对象：有 `data` 对象就用它，否则用顶层
// （与 minimax 同款策略：信封与平铺两种形态都要能吃下）。
func businessObject(top map[string]json.RawMessage) map[string]json.RawMessage {
	if raw, ok := top["data"]; ok {
		if obj, err := decodeObject(raw); err == nil && len(obj) > 0 {
			return obj
		}
	}
	return top
}

// strOfRaw 取 JSON 字符串（非字符串给空串）。
func strOfRaw(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// intOfRaw 宽松取整数：数字与数字字符串都认（上游的积分数实测给字符串）。
// 小数按截断取整（面板是整数口径）。
func intOfRaw(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return int64(f)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if g, perr := strconv.ParseFloat(strings.TrimSpace(s), 64); perr == nil {
			return int64(g)
		}
	}
	return 0
}

// boolOfRaw 宽松取布尔：JSON 布尔与 "true"/"false" 字符串都认。
func boolOfRaw(raw json.RawMessage) (bool, bool) {
	if len(raw) == 0 {
		return false, false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b, true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true":
			return true, true
		case "false":
			return false, true
		}
	}
	return false, false
}

// snippet 截一段响应体用于错误信息（排障线索，不含凭据）。
func snippet(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 200 {
		s = s[:200]
	}
	if s == "" {
		return "空响应"
	}
	return s
}

// expireLoc 本渠道的墙钟口径（与 provider 内部同值，固定 UTC+8）。
var expireLoc = time.FixedZone("UTC+8", 8*60*60)
