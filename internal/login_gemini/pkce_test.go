// pkce_test.go Antigravity 客户端的 PKCE / redirect / scope 三维回归（R135，2026-10-30）。
//
// 背景：`cloudcode-pa` 只认 Antigravity 自己的 OAuth 客户端。此前用 Gemini CLI 的客户端，
// 三条硬伤同时命中 —— scope 缺 `cclog`/`experimentsandconfigs`、客户端已被标记
// UNSUPPORTED_CLIENT、且无 PKCE。三者必须成对切换：只换 client 不补 scope 仍 403，
// 补了 scope 不加 PKCE 则被拒 `invalid_request: code_challenge`。
package login_gemini

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wild-work/internal/gemini"
)

// TestPKCEVerifierShape verifier/challenge 必须符合 RFC 7636 的形态与推导关系。
func TestPKCEVerifierShape(t *testing.T) {
	v, c, err := newPKCE()
	if err != nil {
		t.Fatalf("生成失败：%v", err)
	}
	if n := len(v); n < 43 || n > 128 {
		t.Fatalf("verifier 长度必须在 43~128，得 %d", n)
	}
	for _, r := range v + c {
		// base64url 无填充：只允许这些字符（含 '-' '_'，不含 '+' '/' '='）
		if !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_", r) {
			t.Fatalf("出现非法字符 %q（verifier=%s challenge=%s）", r, v, c)
		}
	}
	if v == c {
		t.Fatal("challenge 不得等于 verifier")
	}
	// 两次生成必须不同（随机性）
	v2, _, _ := newPKCE()
	if v == v2 {
		t.Fatal("verifier 必须随机")
	}
}

// TestAuthURLUsesAntigravityShape 授权 URL 三件套必须同源：scope 全集 + S256 + 127.0.0.1/callback。
func TestAuthURLUsesAntigravityShape(t *testing.T) {
	restore := gemini.SetBuiltinClientForTest("", "")
	t.Cleanup(restore) // 只是为了确保家族判定走内置路径，随后恢复
	_setFlavorAntigravity(t)

	_, challenge := mustPKCE(t)
	redirect := "http://127.0.0.1:51234/callback"
	raw := buildAuthURL("state123", redirect, challenge)

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("授权 URL 解析失败：%v", err)
	}
	q := u.Query()
	if q.Get("code_challenge") != challenge {
		t.Errorf("code_challenge 应为 %q，得 %q", challenge, q.Get("code_challenge"))
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Errorf("必须 S256，得 %q", q.Get("code_challenge_method"))
	}
	if got := q.Get("redirect_uri"); got != redirect {
		t.Errorf("redirect_uri 必须逐字一致，得 %q", got)
	}
	scope := q.Get("scope")
	for _, want := range []string{"cclog", "experimentsandconfigs", "cloud-platform"} {
		if !strings.Contains(scope, want) {
			t.Errorf("Antigravity 家族必须请求 %q（否则 Cloud Code 能力不全），实际 scope：%s", want, scope)
		}
	}
	if q.Get("access_type") != "offline" {
		t.Errorf("access_type 必须 offline（否则拿不到 refresh_token）")
	}
}

// TestAuthURLOmitsPKCEForCLIFlavor CLI 家族不得带 challenge（它历史上就没带）。
func TestAuthURLOmitsPKCEForCLIFlavor(t *testing.T) {
	_setFlavorCLI(t)
	raw := buildAuthURL("s", "http://localhost:8845/oauth-callback", "")
	q, _ := url.Parse(raw)
	vals := q.Query()
	if vals.Get("code_challenge") != "" || vals.Get("code_challenge_method") != "" {
		t.Fatalf("CLI 家族不该带 PKCE 参数：%s", raw)
	}
	if strings.Contains(vals.Get("scope"), "cclog") {
		t.Fatalf("CLI 家族不得请求内部 scope（会 403 restricted_client）：%s", vals.Get("scope"))
	}
}

// TestStatePersistsVerifier 换码要用的 verifier 必须落盘（进程重启后仍能完成登录）。
func TestStatePersistsVerifier(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "login-state.json")
	want := "verifier-abc"
	if err := writeState(p, state{State: "s", CodeVerifier: want, CreatedAt: 1}); err != nil {
		t.Fatalf("写 state 失败：%v", err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var back struct {
		CodeVerifier string `json:"codeVerifier"`
	}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.CodeVerifier != want {
		t.Fatalf("verifier 必须落盘，得 %q", back.CodeVerifier)
	}
	got, err := readState(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.CodeVerifier != want {
		t.Fatalf("读回必须一致，得 %q", got.CodeVerifier)
	}
}

func mustPKCE(t *testing.T) (string, string) {
	t.Helper()
	v, c, err := newPKCE()
	if err != nil {
		t.Fatal(err)
	}
	return v, c
}

// _setFlavorAntigravity / _setFlavorCLI 通过 env 控制家族：env 全套 ⇒ CLI；否则 ⇒ 内置 Antigravity。
func _setFlavorAntigravity(t *testing.T) {
	t.Helper()
	t.Setenv(gemini.EnvClientID, "")
	t.Setenv(gemini.EnvClientSecret, "")
	restore := gemini.SetBuiltinClientForTest(gemini.AntigravityClientID(), gemini.AntigravityClientSecret())
	t.Cleanup(restore)
	gemini.ResetOAuthClientCacheForTest()
}

func _setFlavorCLI(t *testing.T) {
	t.Helper()
	t.Setenv(gemini.EnvClientID, "123456789012-abcdefghijklmnopqrstuv.apps.googleusercontent.com")
	t.Setenv(gemini.EnvClientSecret, "GOCSPX-abcdefghijklmnopqrstuvwx")
	gemini.ResetOAuthClientCacheForTest()
	t.Cleanup(gemini.ResetOAuthClientCacheForTest)
}

var _ = base64.RawURLEncoding

// TestExchangeSendsCodeVerifier 换码表单必须回传 PKCE verifier（且与授权时同一个）。
//
// 这是 PKCE 的**成效点**：授权 URL 带 challenge、换码不带 verifier，上游必回
// `invalid_grant: Missing code verifier`，而面板只会显示「登录失败」。
// 单独测 authURL 抓不到这条（那只是「发了 challenge」，不等于「换码时配上了」）。
func TestExchangeSendsCodeVerifier(t *testing.T) {
	_setFlavorAntigravity(t)

	var gotForm url.Values
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotForm, _ = url.ParseQuery(string(raw))
		_, _ = io.WriteString(w, `{"access_token":"ya29.A","refresh_token":"1//R","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	userInfoSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"sub-1","email":"a@b.c"}`)
	}))
	defer userInfoSrv.Close()
	withEndpoints(t, tokenSrv.URL, userInfoSrv.URL)

	_, statePath, st := startLogin(t)
	if st.CodeVerifier == "" {
		t.Fatal("Antigravity 家族必须生成 verifier 并落盘")
	}
	_, redirPath := gemini.ActiveRedirect() // Antigravity 家族是 /callback，不能写死
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d%s?code=CODE-2&state=%s",
		st.CallbackPort, redirPath, st.State))
	if err != nil {
		t.Fatalf("回调请求失败：%v", err)
	}
	_ = resp.Body.Close()
	if _, err := Poll(NewClient(), statePath); err != nil {
		t.Fatalf("Poll 失败：%v", err)
	}
	if got := gotForm.Get("code_verifier"); got != st.CodeVerifier {
		t.Fatalf("换码必须回传授权时的 verifier，期望 %q，得 %q", st.CodeVerifier, got)
	}
}
