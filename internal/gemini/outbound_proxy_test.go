// outbound_proxy_test.go Gemini 出站出口（直连 / 备用上游通道）回归测试。
//
// 现场驱动（2026-10-30 用户实测）：本渠道端点全在 Google 域（accounts.google.com /
// oauth2.googleapis.com / cloudcode-pa.googleapis.com），国内直连**必然超时**；
// 而本包此前既没有 backuplink 取用、Transport 也没有 Proxy 字段 ⇒ 用户配好别的渠道的
// 备用通道后，Gemini 仍然一条都发不出去，症状只有一行 `dial tcp …: i/o timeout`。
//
// 判据层级：httptest 假上游 + 假代理（**零真实请求**，遵 §0.1）。
package gemini

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"wild-work/internal/backuplink"
)

// TestOutboundDirectWhenNoBackup 未配备用通道 ⇒ 原样直连（零行为变化）。
func TestOutboundDirectWhenNoBackup(t *testing.T) {
	_ = backuplink.Set("")
	direct := &http.Client{}
	if got := Outbound(direct); got != direct {
		t.Fatalf("未配备用通道时必须原样返回直连 client，得 %#v", got)
	}
}

// TestOutboundUsesBackupWhenConfigured 配备用通道 ⇒ 取用绑定该出口的 client。
//
// 断言两件事：① 返回的 client **不是**直连那个；② 它的 Transport 里 Proxy 指向该通道。
func TestOutboundUsesBackupWhenConfigured(t *testing.T) {
	px := "socks5://127.0.0.1:1080"
	if err := backuplink.Set(px); err != nil {
		t.Fatalf("配置备用通道失败：%v", err)
	}
	t.Cleanup(func() { _ = backuplink.Set("") })

	direct := &http.Client{Transport: newTransport()}
	got := Outbound(direct)
	if got == direct {
		t.Fatal("已配备用通道时不得返回直连 client")
	}
	tr, ok := got.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("备用通道 client 的 Transport 应是 *http.Transport，得 %T", got.Transport)
	}
	if tr.Proxy == nil {
		t.Fatal("备用通道 client 必须设置 Proxy")
	}
	req, _ := http.NewRequest(http.MethodPost, "https://cloudcode-pa.googleapis.com/v1internal:streamGenerateContent", nil)
	u, err := tr.Proxy(req)
	if err != nil || u == nil {
		t.Fatalf("Proxy 取不到出口：u=%v err=%v", u, err)
	}
	if u.Scheme != "socks5" || u.Host != "127.0.0.1:1080" {
		t.Fatalf("出口指向错误：%s", u)
	}
}

// TestChatStreamGoesThroughBackupExit 端到端：配了备用通道后，推理请求**真的**经代理出站。
//
// 这条是本缺陷的主断言 —— 旧实现（完全直连）在这里必然打到假上游本体
// （假上游会给 `via-direct`），从而变红。
func TestChatStreamGoesThroughBackupExit(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"via-direct\"}]}}]}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(up.Close)

	// 假代理把请求转给假上游的 /proxy-hit，并在内容里标注出口。
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"via-proxy\"}]}}]}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(proxySrv.Close)
	if err := backuplink.Set(proxySrv.URL); err != nil {
		t.Fatalf("配置备用通道失败：%v", err)
	}
	t.Cleanup(func() { _ = backuplink.Set("") })

	c := NewWithBase(up.URL)
	body := []byte(`{"model":"gemini/gemini-3.8-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	rc, status, _, err := c.ChatStream(testAuth(), body)
	if err != nil {
		t.Fatalf("ChatStream 失败：%v", err)
	}
	defer rc.Close()
	if status != http.StatusOK {
		t.Fatalf("状态码应为 200，得 %d", status)
	}
	got, _ := io.ReadAll(rc)
	if !strings.Contains(string(got), "via-proxy") {
		t.Fatalf("配了备用通道就必须经代理出站，实际响应：%s", string(got))
	}
}

// TestPostFormGoesThroughBackupExit 令牌端点（续期）同一出口。
//
// 为什么单独测：`postForm` 是另一条发送点（刷新 access_token），漏了它就会出现
// 「对话能过、token 一到期就再也刷不动」的半接线形态。
func TestPostFormGoesThroughBackupExit(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"ya29.DIRECT","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(up.Close)

	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"ya29.VIA_PROXY","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(proxySrv.Close)
	if err := backuplink.Set(proxySrv.URL); err != nil {
		t.Fatalf("配置备用通道失败：%v", err)
	}
	t.Cleanup(func() { _ = backuplink.Set("") })

	c := NewWithBase(up.URL)
	c.TokenURL = up.URL + "/token"
	status, raw, err := c.postForm(c.TokenURL, url.Values{"grant_type": {"refresh_token"}})
	if err != nil {
		t.Fatalf("postForm 失败：%v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("状态码应为 200，得 %d", status)
	}
	if !strings.Contains(string(raw), "VIA_PROXY") {
		t.Fatalf("令牌端点必须同源走备用出口，实际：%s", string(raw))
	}
}

// TestPostJSONGoesThroughBackupExit 配额/探测端点同一出口。
func TestPostJSONGoesThroughBackupExit(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"currentTier":{"id":"free-tier"},"cloudaicompanionProject":"proj-direct"}`)
	}))
	t.Cleanup(up.Close)

	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"currentTier":{"id":"free-tier"},"cloudaicompanionProject":"proj-proxy"}`)
	}))
	t.Cleanup(proxySrv.Close)
	if err := backuplink.Set(proxySrv.URL); err != nil {
		t.Fatalf("配置备用通道失败：%v", err)
	}
	t.Cleanup(func() { _ = backuplink.Set("") })

	c := NewWithBase(up.URL)
	status, raw, err := c.postJSON(c.sandboxHost()+EpLoadCodeAssist, "ya29.FAKE", []byte(LoadCodeAssistBody))
	if err != nil {
		t.Fatalf("postJSON 失败：%v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("状态码应为 200，得 %d", status)
	}
	if !strings.Contains(string(raw), "proj-proxy") {
		t.Fatalf("配额/探测端点必须同源走备用出口，实际：%s", string(raw))
	}
}

// TestOutboundNonTransportFallsBackToConservativeExit 直连 client 的 Transport 被换成
// 自定义 RoundTripper（测试注入的常见形态）时，仍必须能取到备用出口，不得静默退回直连。
func TestOutboundNonTransportFallsBackToConservativeExit(t *testing.T) {
	px := "http://127.0.0.1:8888"
	if err := backuplink.Set(px); err != nil {
		t.Fatalf("配置备用通道失败：%v", err)
	}
	t.Cleanup(func() { _ = backuplink.Set("") })

	direct := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, net.ErrClosed
	})}
	got := Outbound(direct)
	if got == direct {
		t.Fatal("自定义 RoundTripper 时也不得退回直连")
	}
	tr, ok := got.Transport.(*http.Transport)
	if !ok || tr.Proxy == nil {
		t.Fatalf("应退回带 Proxy 的保守底座，得 %T", got.Transport)
	}
}

// roundTripperFunc 把函数适配成 http.RoundTripper。
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
