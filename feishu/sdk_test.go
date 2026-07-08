package feishu

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// newTestSDK 返回带短超时的 SDK，避免测试被卡住。
func newTestSDK() *Sdk {
	return &Sdk{client: http.Client{Timeout: 2 * time.Second}}
}

// 调用一次 WebhookV2 并返回 err 与 attempt 计数。
func runOnce(s *Sdk, serverURL string) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	body := strings.NewReader(`{"test":"payload"}`)
	return s.WebhookV2(ctx, serverURL, body)
}

func TestWebhookV2_FirstTrySuccess(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"success"}`))
	}))
	defer srv.Close()

	err := runOnce(newTestSDK(), srv.URL)
	require.NoError(t, err)
	require.Equal(t, int32(1), atomic.LoadInt32(&calls))
}

func TestWebhookV2_Non200RetryThenSuccess(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 2 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`<html>oops</html>`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0,"msg":"success"}`))
	}))
	defer srv.Close()

	s := newTestSDK()
	// 把退避时间压短：单独测无法改 maxAttempts，但 1+2s 总计仍 < 10s 超时
	err := runOnce(s, srv.URL)
	require.NoError(t, err)
	require.Equal(t, int32(2), atomic.LoadInt32(&calls))
}

func TestWebhookV2_FrequencyLimitedRetryThenSuccess(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 2 {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"code":11232,"msg":"frequency limited"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))
	}))
	defer srv.Close()

	err := runOnce(newTestSDK(), srv.URL)
	require.NoError(t, err)
	require.Equal(t, int32(2), atomic.LoadInt32(&calls))
}

func TestWebhookV2_BusinessErrorNoRetry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		// 200 + code != 0 非 11232：业务错误，立即返回
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":99991300,"msg":"invalid body"}`))
	}))
	defer srv.Close()

	err := runOnce(newTestSDK(), srv.URL)
	require.Error(t, err)
	require.Contains(t, err.Error(), "99991300")
	require.Equal(t, int32(1), atomic.LoadInt32(&calls),
		"业务错误不应重试")
}

func TestWebhookV2_MaxAttemptsExhausted(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`<html>bad gateway</html>`))
	}))
	defer srv.Close()

	err := runOnce(newTestSDK(), srv.URL)
	require.Error(t, err)
	require.Contains(t, err.Error(), "max attempts")
	require.Equal(t, int32(maxAttempts), atomic.LoadInt32(&calls))
}

func TestWebhookV2_UnmarshalFailureRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 2 {
			// 200 但 body 是非 JSON → unmarshal 失败 → 应重试
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`<html>html error page</html>`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer srv.Close()

	err := runOnce(newTestSDK(), srv.URL)
	require.NoError(t, err)
	require.Equal(t, int32(2), atomic.LoadInt32(&calls),
		"非 JSON 响应必须重试，否则会误判成功")
}

func TestWebhookV2_ContextCancel(t *testing.T) {
	// server hang 住请求直到 client 关闭连接；
	// 用 select 让 handler 在 ctx 取消或 5s 后退出，避免与 Close 死锁。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	body := strings.NewReader(`{}`)
	err := (&Sdk{client: http.Client{Timeout: 5 * time.Second}}).
		WebhookV2(ctx, srv.URL, body)
	require.Error(t, err)
	require.Contains(t, err.Error(), "canceled")
}

func TestWebhookV2_ContextCancelDuringBackoff(t *testing.T) {
	// server 第一次失败后 ctx 取消 → 重试退避应被打断
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// 给第一次调用一点时间进入退避 sleep，然后取消
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := newTestSDK().WebhookV2(ctx, srv.URL, strings.NewReader(`{}`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "canceled")
	require.Equal(t, int32(1), atomic.LoadInt32(&calls),
		"ctx 取消后不应继续重试")
}

func TestWebhookV2_TruncatedMsgInError(t *testing.T) {
	longMsg := strings.Repeat("x", 500)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = fmt.Fprintf(w, `{"code":0,"msg":%q}`, longMsg)
	}))
	defer srv.Close()

	err := runOnce(newTestSDK(), srv.URL)
	require.Error(t, err)
	// 最终 error 中的 msg 字段被截断到 200 + "..."
	require.Contains(t, err.Error(), "...")
	require.NotContains(t, err.Error(), longMsg,
		"长 msg 必须被 truncate，避免日志/响应被淹没")
}

// 简易 helper：body 必须是 io.Reader 但 tests 里都用 strings.NewReader
var _ = io.Reader(nil)