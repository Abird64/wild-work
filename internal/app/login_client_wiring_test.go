package app

import (
	"net/http"
	"testing"

	"wild-work/internal/backuplink"
	"wild-work/internal/provider"
)

// TestGeminiLoginClientUsesBackupExit 登录链必须走备用上游通道（2026-10-30 现场缺陷，R133）。
//
// **缺陷形态**（用户实测：授权页正常、点完同意后 30 秒报「网络不可达」）：
// `loginFlows[provider.Gemini]` 没登记 `clientFactory` ⇒ `newLoginClient` 退回
// `login.NewClient()`（裸直连），于是**换码请求完全没经过代理**，日志是
// `dial tcp 108.177.98.95:443 …`（直连 Google IP）。授权页在浏览器里打开当然没事，
// 所以表象是「能登录、但登录完就失败」，且错误文案指的是网络——用户只会去怀疑代理。
//
// 这条用例断言**接线本身**（而不是 NewClient 的内部实现）：即使 `login_gemini.NewClient`
// 改对了，只要表里漏登记就必红。
func TestGeminiLoginClientUsesBackupExit(t *testing.T) {
	t.Cleanup(func() { _ = backuplink.Set("") })
	if err := backuplink.Set("http://127.0.0.1:7890"); err != nil {
		t.Fatalf("配置备用通道失败：%v", err)
	}

	f, ok := loginFlows[provider.Gemini]
	if !ok {
		t.Fatal("loginFlows 未登记 gemini")
	}
	if f.clientFactory == nil {
		t.Fatal("gemini 必须显式登记 clientFactory：缺省会退回裸直连（R133 现场缺陷）")
	}
	c := newLoginClient(provider.Gemini)
	if c == nil {
		t.Fatal("newLoginClient 返回 nil")
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr.Proxy == nil {
		t.Fatalf("登录 client 必须带 Proxy（Translport=%T）", c.Transport)
	}
	req, _ := http.NewRequest(http.MethodPost, "https://oauth2.googleapis.com/token", nil)
	u, err := tr.Proxy(req)
	if err != nil || u == nil || u.Host != "127.0.0.1:7890" {
		t.Fatalf("登录出口指向错误：u=%v err=%v", u, err)
	}
}

// TestGeminiLoginPollTimeoutCoversFlowTTL app 的轮询上限不得短于本包的 flowTTL。
//
// 若 app 用通用 5m 先到期，`login_gemini.Poll` 那条**带传输失败原因**的超时文案
// 永远没机会出现，用户只能看到一个没有任何线索的「登录超时」。
func TestGeminiLoginPollTimeoutCoversFlowTTL(t *testing.T) {
	f := loginFlows[provider.Gemini]
	if f.pollTimeout == nil {
		t.Fatal("gemini 必须登记 pollTimeout（否则 app 用通用 5m，早于 flowTTL）")
	}
	if got := f.pollTimeout(); got <= loginTimeout {
		t.Fatalf("pollTimeout(%v) 必须长于通用 loginTimeout(%v)", got, loginTimeout)
	}
}
