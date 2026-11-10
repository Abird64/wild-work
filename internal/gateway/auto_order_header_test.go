// 自动选路的「降级全序」跨层传递（R167）。
//
// 背景：`/v1/responses` 与 `/v1/messages` 上收到 `model:"auto"` 时，本层必须先把
// auto 解析成具体 `<channel>/<model>` 才能交给内层（内层只认带前缀的全路径），
// 但一旦解析完，内层看到的就只是一个裸模型名——`isAutoModel` 恒假、`autoOrder` 为空，
// 于是「首选额度用完/模型下线」时内层只会去走静态降级链（按具体模型名配的 From），
// 对 auto 选出的模型一条都不命中 ⇒ 直接透传上游错误、永不降级。
//
// 修法：本层把那次决策的**全序**经内部头 `auto.OrderHeader` 带进内层。本文件锁两件事：
//  1. auto 请求确实带上了全序头（内容与 decision.Order 一致，逗号分隔）；
//  2. 非 auto 请求绝不带（否则会把一个跟本次请求无关的降级序列塞给内层）。
package gateway

import (
	"net/http"
	"sync"
	"testing"

	"wild-work/internal/auto"
)

// orderCaptureInner 假内层：记录收到的 auto 全序头，并回一轮最小合法响应。
// 与 scriptedHandler 分开是为了断言头本身，而不是模型名透传。
type orderCaptureInner struct {
	mu     sync.Mutex
	orders []string
	seen   []string
}

func (h *orderCaptureInner) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.orders = append(h.orders, r.Header.Get(auto.OrderHeader))
	h.seen = append(h.seen, r.URL.Path)
	h.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"up",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))
}

// TestAutoOrderHeaderReachesInner 自动选路请求：两个兼容端点都必须把全序带进内层。
func TestAutoOrderHeaderReachesInner(t *testing.T) {
	inner := &orderCaptureInner{}
	g, mux := newTestGateway(inner)
	order := []string{"kilo/dots-3-note-preview:free", "oczen/mimo-v2.6-flash-free", "kilo/cohere/north-mini-code:free"}
	g.SetAutoResolver(func(auto.Request) (string, []string, []string, bool) {
		return "kilo/dots-3-note-preview:free", order[:2], order, true
	})

	want := "kilo/dots-3-note-preview:free,oczen/mimo-v2.6-flash-free,kilo/cohere/north-mini-code:free"
	for _, path := range []string{"/v1/responses", "/v1/messages"} {
		body := `{"model":"auto","max_tokens":64,"input":"hi","messages":[{"role":"user","content":"hi"}]}`
		rec := post(mux, path, body)
		if rec.Code != 200 {
			t.Fatalf("%s status=%d body=%s", path, rec.Code, rec.Body.String())
		}
	}
	inner.mu.Lock()
	defer inner.mu.Unlock()
	if len(inner.orders) != 2 {
		t.Fatalf("内层应被调用 2 次，实际 %d 次（path=%v）", len(inner.orders), inner.seen)
	}
	for i, got := range inner.orders {
		if got != want {
			t.Errorf("第 %d 次调用 %s: %s = %q，want %q", i+1, inner.seen[i], auto.OrderHeader, got, want)
		}
	}
}

// TestAutoOrderHeaderAbsentForConcreteModel 显式点名模型（不经自动选路）时不得带头。
//
// 反例后果：带上一个无关的全序，内层会把「一个用户明确指定的模型」在 402 时
// 自动换成链上的另一个模型，等于静默改写用户的显式选择。
func TestAutoOrderHeaderAbsentForConcreteModel(t *testing.T) {
	inner := &orderCaptureInner{}
	g, mux := newTestGateway(inner)
	// 注入了解析器也不能对非 auto 请求生效
	g.SetAutoResolver(func(auto.Request) (string, []string, []string, bool) {
		return "kilo/x:free", nil, []string{"kilo/x:free", "glm/glm-4.6"}, true
	})
	rec := post(mux, "/v1/responses", `{"model":"workbuddy/glm-5.2","input":"hi"}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	inner.mu.Lock()
	defer inner.mu.Unlock()
	if len(inner.orders) != 1 {
		t.Fatalf("内层应被调用 1 次，实际 %d 次", len(inner.orders))
	}
	if got := inner.orders[0]; got != "" {
		t.Fatalf("非 auto 请求不得携带 %s，实际 %q", auto.OrderHeader, got)
	}
}

// TestAutoOrderHeaderSingleEntryForwarded 单条全序也照发。
//
// 语义边界：头表达的是「本次决策的全序」，长度 1 = 只有一个候选、没有任何备胎。
// 内层据此走完 order 后自然拿到空备胎并原样透传——不需要另开一个「无备胎」标志位，
// 少一个状态就少一处可能不一致。
func TestAutoOrderHeaderSingleEntryForwarded(t *testing.T) {
	inner := &orderCaptureInner{}
	g, mux := newTestGateway(inner)
	g.SetAutoResolver(func(auto.Request) (string, []string, []string, bool) {
		return "kilo/only:free", []string{"kilo/only:free"}, []string{"kilo/only:free"}, true
	})
	if rec := post(mux, "/v1/responses", `{"model":"auto","input":"hi"}`); rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	inner.mu.Lock()
	defer inner.mu.Unlock()
	if len(inner.orders) != 1 {
		t.Fatalf("内层应被调用 1 次，实际 %d 次", len(inner.orders))
	}
	if got := inner.orders[0]; got != "kilo/only:free" {
		t.Fatalf("%s = %q，want %q", auto.OrderHeader, got, "kilo/only:free")
	}
}
