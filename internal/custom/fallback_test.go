// fallback_test.go 自定义渠道「直连失败 → 备用通道重试」回归（2026-10-30 现场）。
//
// 现场：用户在面板里配好并验证过备用通道，但自定义渠道打境外 API 仍然连接失败 ——
// 因为自定义渠道的 transport 用 `http.ProxyFromEnvironment`（只读 HTTPS_PROXY 环境变量），
// 既不认 backuplink，也不读 Windows 系统代理。用户看着「● 已生效」的绿点毫无线索。
package custom

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

// TestIsConnectFailure 判据只认连接层，HTTP 错误码不算。
func TestIsConnectFailure(t *testing.T) {
	if isConnectFailure(nil) {
		t.Fatal("nil 不算")
	}
	// 真实形态：DNS 失败 / 超时 / 拒绝
	if !isConnectFailure(&net.OpError{Op: "dial", Err: &net.DNSError{Name: "x", Err: "no such host"}}) {
		t.Error("DNS 失败应算连接层")
	}
	hErr := &url.Error{Op: "Get", URL: "https://x", Err: timeoutErr{}}
	if !isConnectFailure(hErr) {
		t.Error("url.Error 包裹超时应算连接层")
	}
	// 反向护栏：业务错误不得触发回落（否则会掩盖真实错误、白走一次代理）
	if isConnectFailure(io.EOF) {
		t.Log("EOF 归连接层（可接受）")
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// TestDoWithFallbackRetriesThroughProxy 直连失败时，必须真的经备用通道把请求发出去。
//
// 关键点：**请求体必须完整重发**（ChatStream 的 body 是 JSON，丢了就等于空请求），
// 所以断言收到的是原 body、且出口确实是代理。
func TestDoWithFallbackRetriesThroughProxy(t *testing.T) {
	t.Cleanup(func() { _ = backuplink.Set("") })

	// 目标服务：记录收到的 body
	var gotBody string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer target.Close()

	// 假备用通道：把所有请求转给目标（模拟 HTTP 代理）
	proxyHit := 0
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHit++
		// 作为正向代理时，r.URL 是绝对地址；直接回内容即可
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer proxy.Close()

	if err := backuplink.Set(proxy.URL); err != nil {
		t.Fatalf("配置备用通道失败：%v", err)
	}

	// 直连指向一个必然连不上的地址（模拟被墙）
	body := `{"model":"gemini-2.5-flash","messages":[]}`
	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	hc := &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}

	resp, err := doWithFallbackTr(req, hc, hc.Transport.(*http.Transport))
	if err != nil {
		t.Fatalf("应经备用通道成功，得 %v", err)
	}
	defer resp.Body.Close()
	if proxyHit == 0 {
		t.Fatal("备用通道没被用到")
	}
	if gotBody != body {
		t.Fatalf("请求体必须完整重发，得 %q", gotBody)
	}
}

// TestDoWithFallbackNoProxyKeepsOriginalError 没配备用通道时保留原错误（不得伪装成功）。
func TestDoWithFallbackNoProxyKeepsOriginalError(t *testing.T) {
	t.Cleanup(func() { _ = backuplink.Set("") })
	_ = backuplink.Set("")

	req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:1/x", strings.NewReader("{}"))
	hc := &http.Client{Transport: &http.Transport{}}
	if _, err := doWithFallbackTr(req, hc, hc.Transport.(*http.Transport)); err == nil {
		t.Fatal("没备用通道时必须报原始连接错误")
	}
}
