// outbound_proxy_test.go 登录链路的出站出口回归（2026-10-30 现场）。
//
// 现场症状：点「＋Gemini」→ 浏览器授权页打开、用户点同意 → 浏览器跳回本机回调 →
// **面板永远停在「授权中」**，5 分钟后超时。根因是换码请求（oauth2.googleapis.com）
// 与取身份请求（www.googleapis.com）走的是裸 `&http.Client{}` 直连，
// 在国内对 Google 必然超时（本机实测 25s 全 timeout）。
//
// 判据层级：httptest 假上游 + 假代理，零真实请求（§0.1）。
package login_gemini

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"wild-work/internal/backuplink"
	"wild-work/internal/gemini"
)

// TestNewClientUsesBackupExit 配了备用通道 ⇒ 登录 client 必须经它出站。
func TestNewClientUsesBackupExit(t *testing.T) {
	if err := backuplink.Set("http://127.0.0.1:8888"); err != nil {
		t.Fatalf("配置备用通道失败：%v", err)
	}
	t.Cleanup(func() { _ = backuplink.Set("") })

	c := NewClient()
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr.Proxy == nil {
		t.Fatalf("登录 client 必须带 Proxy（得 Transport=%T）", c.Transport)
	}
	req, _ := http.NewRequest(http.MethodPost, "https://oauth2.googleapis.com/token", nil)
	u, err := tr.Proxy(req)
	if err != nil || u == nil || u.Host != "127.0.0.1:8888" {
		t.Fatalf("出口指向错误：u=%v err=%v", u, err)
	}
}

// TestNewClientDirectWhenNoBackup 未配通道 ⇒ 直连（零行为变化）。
func TestNewClientDirectWhenNoBackup(t *testing.T) {
	_ = backuplink.Set("")
	c := NewClient()
	if tr, ok := c.Transport.(*http.Transport); ok && tr.Proxy != nil {
		if req, _ := http.NewRequest(http.MethodPost, "https://oauth2.googleapis.com/token", nil); func() bool {
			u, err := tr.Proxy(req)
			return err == nil && u != nil
		}() {
			t.Fatal("未配备用通道时不应设置出口代理")
		}
	}
}

// TestExchangeGoesThroughBackupExit 端到端：换码请求真的经代理发出。
//
// 这条是主断言：旧实现（直连）在这里会打到假上游本体，拿到 DIRECT 那份 token。
func TestExchangeGoesThroughBackupExit(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"ya29.DIRECT","refresh_token":"1//D","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(up.Close)

	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"ya29.PROXY","refresh_token":"1//P","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(proxySrv.Close)
	if err := backuplink.Set(proxySrv.URL); err != nil {
		t.Fatalf("配置备用通道失败：%v", err)
	}
	t.Cleanup(func() { _ = backuplink.Set("") })

	// 把两个 Google 端点都指到假上游（与既有测试同款注入手法）。
	oldToken, oldUser := tokenEndpoint, userInfoEndpoint
	tokenEndpoint = up.URL + "/token"
	userInfoEndpoint = up.URL + "/userinfo"
	t.Cleanup(func() { tokenEndpoint, userInfoEndpoint = oldToken, oldUser })

	st := state{State: "S", RedirectURI: "http://localhost:8845/oauth-callback", CallbackPort: 8845}
	res, _, err := exchange(NewClient(), st, "CODE")
	if err != nil {
		t.Fatalf("换码失败：%v", err)
	}
	if res.AccessToken != "ya29.PROXY" {
		t.Fatalf("登录换码必须经备用通道出站，实际 token=%q", res.AccessToken)
	}
}

// TestOutboundIsSharedWithDataPlane 登录链与数据面必须同源（同一份判据）。
//
// 守护目标：两边各判一次必然漂移成「登录走代理、对话直连」或反之，
// 症状是「登录成功但一对话就 502」，排查成本极高（R108 同族教训）。
func TestOutboundIsSharedWithDataPlane(t *testing.T) {
	if err := backuplink.Set("http://127.0.0.1:9999"); err != nil {
		t.Fatalf("配置备用通道失败：%v", err)
	}
	t.Cleanup(func() { _ = backuplink.Set("") })

	loginClient := NewClient()
	dataClient := gemini.Outbound(&http.Client{})
	lt, ok1 := loginClient.Transport.(*http.Transport)
	dt, ok2 := dataClient.Transport.(*http.Transport)
	if !ok1 || !ok2 {
		t.Fatalf("两侧 Transport 类型异常：login=%T data=%T", loginClient.Transport, dataClient.Transport)
	}
	req, _ := http.NewRequest(http.MethodPost, "https://cloudcode-pa.googleapis.com/x", nil)
	lu, err1 := lt.Proxy(req)
	du, err2 := dt.Proxy(req)
	if err1 != nil || err2 != nil || lu == nil || du == nil || lu.String() != du.String() {
		t.Fatalf("登录链与数据面出口不一致：login=%v data=%v", lu, du)
	}
}
