// transport_retry_test.go 传输层失败的重试回归（2026-11-03 用户实测驱动）。
//
// 现场：gemini 对话间歇 502
//
//	Post "https://daily-cloudcode-pa.sandbox.googleapis.com/v1internal:streamGenerateContent?alt=sse": EOF
//
// app.log 里同类错误全部指向**本地代理**（FlClash 127.0.0.1:7890），原文有两种：
// `write tcp 127.0.0.1:…->127.0.0.1:7890: wsasend: An existing connection was forcibly closed`
// 与 `EOF`。实测排除了两个更「合理」的猜测：
//  1. **不是空闲连接复用** —— 失败前的间隔有 4s / 6s，而 242s 的间隔反而成功（无相关性）；
//  2. **不是「上游提前拒绝导致写 body 失败」** —— 22KB body + 坏签名直连上游，
//     两个端点都返回**干净的 400**（`Corrupted thought signature.`），不是断连。
//
// 处置：全部端点传输层失败时，整轮重试一次（换新连接吸收瞬时故障）。
// 安全性：此处尚未向客户端写出任何字节（返回的是 reader），重试对外不可见。
package gemini

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// flakyDialErr 模拟代理断连：错误信息与现场同构（EOF）。
var errProxyEOF = errors.New("Post \"…\": EOF")

// TestChatStreamRetriesTransportFailure 第一次传输层失败、第二次成功 → 整体成功。
func TestChatStreamRetriesTransportFailure(t *testing.T) {
	var calls int32
	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse(
			`{"candidates":[{"content":{"parts":[{"text":"好了"}]}}]}`,
			`{"candidates":[{"finishReason":"STOP"}]}`,
		)))
	}))
	defer inner.Close()

	// 第一次请求直接返回传输层错误（模拟代理掐连接），之后正常转给假上游。
	c := NewWithBase(inner.URL)
	c.StreamHTTP = &http.Client{Transport: &flakyTransport{
		failFirst: &calls,
		base:      http.DefaultTransport,
	}}
	rc, status, body, err := c.ChatStream(testAuth(), []byte(envelopeInput))
	if err != nil {
		t.Fatalf("应吸收掉一次传输层失败，得 err=%v body=%s", err, body)
	}
	if status != http.StatusOK {
		t.Fatalf("status=%d", status)
	}
	defer rc.Close()
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("应恰好请求 2 次（失败 1 + 成功 1），得 %d", n)
	}
}

// TestChatStreamTransportRetryIsBounded 连续失败不得无限重试：最多多打一轮。
func TestChatStreamTransportRetryIsBounded(t *testing.T) {
	var calls int32
	c := NewWithBase("http://127.0.0.1:1") // 必然连不上
	c.StreamHTTP = &http.Client{Transport: &flakyTransport{
		failAlways: true,
		failFirst:  &calls,
		base:       http.DefaultTransport,
	}}
	_, _, _, err := c.ChatStream(testAuth(), []byte(envelopeInput))
	if err == nil {
		t.Fatal("全部端点失败时必须返回错误")
	}
	// NewWithBase 把全部端点统一成**一个**基址 ⇒ 1 端点 × (原轮 + 1 重试) = 2 次。
	// 这条断言守的是「有上限」：不断言具体端点基数（那是 endpoints() 的职责，
	// 由 TestEndpointsRotation 覆盖），只要求它**不无限重试**。
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("应重试恰好一轮（共 2 次），得 %d", n)
	}
}

// TestChatStreamUpstream400IsNotRetried 上游明确回 400 时**不得**当成网络故障重试。
//
// 这条是重试逻辑的爆炸半径护栏：把「上游拒绝」误判成「网络抖动」会让每个错误请求
// 都多打一次上游，既浪费配额也让真正的错误被掩盖。
func TestChatStreamUpstream400IsNotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":400,"message":"Corrupted thought signature.","status":"INVALID_ARGUMENT"}}`))
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL)
	_, status, body, err := c.ChatStream(testAuth(), []byte(envelopeInput))
	if err != nil {
		t.Fatalf("400 应原样透传而不是变成传输层错误：%v", err)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("status=%d", status)
	}
	if !strings.Contains(string(body), "Corrupted thought signature") {
		t.Fatalf("上游原文应透传：%s", body)
	}
	// 单端点上**只有一次**请求（不是 2 次）：没触发传输层重试
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("400 不该触发重试，请求次数 %d", n)
	}
}

// flakyTransport 前 N 次返回传输层错误，之后转给真实 Transport。
type flakyTransport struct {
	failFirst  *int32
	failAlways bool
	// failOnHost 只对这个 Host 模拟失败（用于构造「端点间失败类型不一致」）。
	failOnHost string
	base       http.RoundTripper
}

func (f *flakyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	n := atomic.AddInt32(f.failFirst, 1)
	if f.failAlways {
		return nil, errProxyEOF
	}
	if f.failOnHost != "" && r.URL.Host == f.failOnHost {
		return nil, errProxyEOF
	}
	if f.failOnHost == "" && n == 1 {
		// 未指定 host 时：只有**第一次**失败（用来验证「重试一次即可恢复」）
		return nil, errProxyEOF
	}
	return f.base.RoundTrip(r)
}

// TestChatStreamMixedFailureDoesNotFullRetry 端点间失败类型不一致时**不得**整轮重试。
//
// 这条守的是重试判据的精确性：只有「**全部**端点都是传输层失败」才整轮重来。
// 若第一个端点回 500（上游故障）而第二个端点传输层失败，说明上游确实有响应 ——
// 此时整轮重试等于把「上游明确回错」也当成网络抖动，会放大每个错误请求的上游压力。
func TestChatStreamMixedFailureDoesNotFullRetry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":500,"message":"boom"}}`))
	}))
	defer srv.Close()

	// 手工构造两个端点：第一个假上游回 500，第二个连不上。
	c := New()
	c.Daily = srv.URL
	c.Sandbox = "http://127.0.0.1:1"
	c.StreamHTTP = &http.Client{Transport: &flakyTransport{
		// 只有连 127.0.0.1:1 的那次失败；到假上游的那次放行
		failOnHost: "127.0.0.1:1",
		failFirst:  &calls,
		base:       http.DefaultTransport,
	}}
	_, status, body, _ := c.ChatStream(testAuth(), []byte(envelopeInput))
	if status != http.StatusInternalServerError {
		t.Fatalf("混合失败时上游的 500 应原样透传，得 status=%d body=%s", status, body)
	}
	// 500 的端点只被访问 1 次（没有整轮重试）
	if !strings.Contains(string(body), "boom") {
		t.Fatalf("上游原文应透传：%s", body)
	}
}
