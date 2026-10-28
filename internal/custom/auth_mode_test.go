package custom

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAuthHeader 锁住两种鉴权头的**逐字形态**。
//
// 为什么值得逐字锁：华为云 APIG 系网关对头形状零容忍——发 `Bearer` 回
// APIG.0303「Authorization header is missing」，发 Basic 但 key 少了冒号回
// APIG.1009「AppKey or AppSecret is invalid」。两者的报错文案都指向「你没鉴权」，
// 于是「姿态改错了」在用户侧看起来和「密钥填错了」一模一样，只能靠单测守住形状。
func TestAuthHeader(t *testing.T) {
	cases := []struct {
		name string
		mode AuthMode
		key  string
		want string
	}{
		{"空密钥不设头", AuthBearer, "", ""},
		{"空密钥 basic 也不设头", AuthBasic, "", ""},
		{"默认 bearer", AuthBearer, "sk-abc", "Bearer sk-abc"},
		// 留空 = 旧配置（AuthMode 零值），必须仍走 Bearer，否则升级即全渠道鉴权失效
		{"零值即 bearer", "", "sk-abc", "Bearer sk-abc"},
		{"basic 编码 app_key:app_secret", AuthBasic, "23715d77:fc1ee421",
			"Basic " + base64.StdEncoding.EncodeToString([]byte("23715d77:fc1ee421"))},
	}
	for _, c := range cases {
		if got := authHeader(c.mode, c.key); got != c.want {
			t.Errorf("%s: authHeader(%q,%q) = %q, want %q", c.name, c.mode, c.key, got, c.want)
		}
	}
	// 定点核验一次真值形态，避免上面那条断言自己把 base64 算错还互相印证
	if got := authHeader(AuthBasic, "app:sec"); got != "Basic YXBwOnNlYw==" {
		t.Fatalf("basic 头的 base64 形态不对：%q", got)
	}
}

// TestValidateAuthMode 校验规整与拦阻：未知值归 Bearer、basic 缺冒号要报错。
func TestValidateAuthMode(t *testing.T) {
	// 1) 旧配置（无 auth 字段）零迁移：读出来仍是 Bearer 口径
	old := ChannelConfig{Name: "legacy", BaseURL: "https://api.x.com/v1", APIKey: "sk-1"}
	if err := old.Validate(); err != nil {
		t.Fatalf("旧配置应仍可用：%v", err)
	}
	if old.Auth != "" {
		t.Fatalf("旧配置的 auth 应保持空（= bearer），实为 %q", old.Auth)
	}
	// 2) 非法值一律归 Bearer，不得原样落盘（否则前端读到第三种形态）
	weird := ChannelConfig{Name: "w", BaseURL: "https://api.x.com/v1", APIKey: "k", Auth: "Basic"}
	if err := weird.Validate(); err != nil {
		t.Fatalf("未知 auth 值不应报错：%v", err)
	}
	if weird.Auth != "" {
		t.Fatalf("未知 auth 值应归空（= bearer），实为 %q", weird.Auth)
	}
	if got := authHeader(weird.Auth, weird.APIKey); got != "Bearer k" {
		t.Fatalf("被规整的渠道应发 Bearer，实为 %q", got)
	}
	// 3) basic 缺冒号必须在 Validate 就挡住（不能等上游回一句「鉴权无效」）
	noColon := ChannelConfig{Name: "b", BaseURL: "https://api.x.com/v1", APIKey: "just-a-key", Auth: AuthBasic}
	if err := noColon.Validate(); err == nil {
		t.Fatal("basic 但 api_key 无冒号应报错")
	}
	// 4) 冒号在中间/密钥自身含冒号都应放行（不对切分做额外假设）
	withColon := ChannelConfig{Name: "b2", BaseURL: "https://api.x.com/v1", APIKey: "app:sec:extra", Auth: AuthBasic}
	if err := withColon.Validate(); err != nil {
		t.Fatalf("含冒号的 basic 密钥应通过：%v", err)
	}
}

// TestBasicAuthReachesUpstream 端到端：basic 渠道的**探测**与**对话**都必须带 Basic 头。
//
// 这里的重点是「同一条链路」：旧缺陷形态是探针走 Bearer、真调用走 Basic（或反过来），
// 表现为探测成功却对话 401。两个入口各断言一次，任一处退回写死 Bearer 都会变红。
func TestBasicAuthReachesUpstream(t *testing.T) {
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("app:sec"))
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path+" "+r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
		case "/v2/chat/completions":
			_, _ = w.Write([]byte(`{"id":"c","choices":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := New(ChannelConfig{Name: "oa", BaseURL: srv.URL + "/v2", APIKey: "app:sec", Auth: AuthBasic})
	// 入口 ①：面板「识别端点 / 拉模型」走的 Discover
	if _, err := Discover(c.Config()); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	// 入口 ②：真实对话
	if _, _, _, err := c.ChatStream(nil, []byte(`{"model":"m1","stream":false}`)); err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	for _, s := range seen {
		path, auth := s[:strings.IndexByte(s, ' ')], s[strings.IndexByte(s, ' ')+1:]
		if path == "/v2/chat/completions" && auth != want {
			t.Fatalf("对话请求的鉴权头不对：%q，want %q", auth, want)
		}
		if path == "/v2/models" && auth != want {
			t.Fatalf("探测请求的鉴权头不对：%q，want %q", auth, want)
		}
	}
	var sawChat, sawModels bool
	for _, s := range seen {
		sawChat = sawChat || strings.HasPrefix(s, "/v2/chat/completions ")
		sawModels = sawModels || strings.HasPrefix(s, "/v2/models ")
	}
	if !sawChat || !sawModels {
		t.Fatalf("两个入口没有都走到：%v", seen)
	}
}

// TestBearerUnaffectedByAuthField 守住向后兼容：其余字段不变时 bearer 渠道行为一字不差。
func TestBearerUnaffectedByAuthField(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
	}))
	defer srv.Close()

	// 显式 bearer 与空 auth（旧配置）必须产生同一条头
	for _, mode := range []AuthMode{AuthBearer, ""} {
		c := New(ChannelConfig{Name: "t", BaseURL: srv.URL, APIKey: "sk-abc", Auth: mode})
		if _, err := c.TestConnection(); err != nil {
			t.Fatalf("TestConnection(%q): %v", mode, err)
		}
		if got != "Bearer sk-abc" {
			t.Fatalf("auth=%q 时应发 Bearer，实为 %q", mode, got)
		}
	}
}
