package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/sirupsen/logrus"
)

// httpClientTimeout 限制飞书单次响应时间，避免慢端点耗尽 handler 协程。
const httpClientTimeout = 10 * time.Second

type Sdk struct {
	client http.Client
}

func NewSDK() *Sdk {
	return &Sdk{client: http.Client{Timeout: httpClientTimeout}}
}

// wired response:
// response of success
//{
//    "Extra": null,
//    "StatusCode": 0,
//    "StatusMessage": "success"
//}
// response of failure
//{
//    "code": 99991300,
//    "msg": "invalid request body: not json, invalid character '\n' in string literal"
//}

// 重试配置
const (
	maxAttempts          = 3
	codeFrequencyLimited = 11232 // 飞书频控错误码
)

// backoff 计算第 N 次重试前的等待时间。
// 频控时退避翻倍：2s/4s/6s；普通失败：1s/2s/3s。
func backoff(attempt int, isFrequencyLimit bool) time.Duration {
	if isFrequencyLimit {
		return time.Duration(attempt+1) * 2 * time.Second
	}
	return time.Duration(attempt+1) * time.Second
}

// sleepCtx 与 time.Sleep 等价，但会在 ctx 被取消时立刻返回。
// 返回 true 表示正常睡完，false 表示被 ctx 打断。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// truncate 截断字符串用于日志，避免过长响应刷屏。
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// WebhookV2 发送飞书卡片消息，失败时自动重试。
//
// 重试条件：
//   - 网络错误（DNS / 连接失败 / 超时）
//   - HTTP 状态码非 200
//   - 业务 code == 11232（飞书频控）
//
// 不重试：HTTP 200 + code != 0 且非 11232（业务参数错误，无重试意义）。
//
// ctx 可用于打断飞行中的请求与重试退避，常传 r.Context() 或 shutdownCtx。
func (s Sdk) WebhookV2(ctx context.Context, webhook string, body io.Reader) error {
	// 重试需要多次读取 body，先缓存为字节数组
	bodyBytes, err := io.ReadAll(body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}

	var (
		lastErr    error
		lastStatus int
		lastCode   int
		lastMsg    string
	)

	for attempt := 0; attempt < maxAttempts; attempt++ {
		// 每次循环开头都检查 ctx，及时退出
		if ctx.Err() != nil {
			return fmt.Errorf("feishu webhook canceled: %w", ctx.Err())
		}

		if attempt > 0 {
			logrus.Warnf("feishu webhook retry, attempt %d/%d", attempt+1, maxAttempts)
		}

		req, err := http.NewRequestWithContext(ctx, "POST", webhook, bytes.NewReader(bodyBytes))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := s.client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("feishu webhook canceled: %w", ctx.Err())
			}
			lastErr = err
			logrus.Warnf("feishu webhook network error: %v", err)
			if !sleepCtx(ctx, backoff(attempt, false)) {
				return fmt.Errorf("feishu webhook canceled during backoff: %w", ctx.Err())
			}
			continue
		}

		respBody, readErr := io.ReadAll(resp.Body)
		if closeErr := resp.Body.Close(); closeErr != nil {
			logrus.Debugf("feishu webhook response body close: %v", closeErr)
		}
		if readErr != nil {
			// 读 body 失败：视同传输错误，按非 200 路径重试
			lastStatus = resp.StatusCode
			lastErr = fmt.Errorf("read response body (status=%d): %w", resp.StatusCode, readErr)
			logrus.Warnf("feishu webhook read body error: %v", lastErr)
			if !sleepCtx(ctx, backoff(attempt, false)) {
				return fmt.Errorf("feishu webhook canceled during backoff: %w", ctx.Err())
			}
			continue
		}

		var result struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		}
		if unmarshalErr := json.Unmarshal(respBody, &result); unmarshalErr != nil {
			// 响应不是合法 JSON（HTML 错误页、空 body、网关异常等），
			// 不能假定 code==0 就是成功，按非 200 路径重试
			lastStatus = resp.StatusCode
			lastErr = fmt.Errorf("unmarshal response (status=%d, body=%q): %w",
				resp.StatusCode, truncate(string(respBody), 200), unmarshalErr)
			logrus.Warnf("feishu webhook unmarshal error: %v", lastErr)
			if !sleepCtx(ctx, backoff(attempt, false)) {
				return fmt.Errorf("feishu webhook canceled during backoff: %w", ctx.Err())
			}
			continue
		}
		lastStatus = resp.StatusCode
		lastCode = result.Code
		lastMsg = result.Msg

		if resp.StatusCode == http.StatusOK && result.Code == 0 {
			logrus.Debug("feishu webhook send success")
			return nil
		}

		logrus.WithFields(logrus.Fields{
			"attempt": attempt + 1,
			"max":     maxAttempts,
			"status":  resp.StatusCode,
			"code":    result.Code,
			// 截断飞书返回的 msg：可能含告警原文、runbook URL、敏感 label 等。
			"msg": truncate(result.Msg, 200),
		}).Warn("feishu webhook failed")

		// 触发重试：非 200 或频控
		if resp.StatusCode != http.StatusOK || result.Code == codeFrequencyLimited {
			isFreqLimit := result.Code == codeFrequencyLimited
			if isFreqLimit {
				logrus.Infof("feishu frequency limited (code=%d), retry after %s",
					codeFrequencyLimited, backoff(attempt, true))
			}
			if !sleepCtx(ctx, backoff(attempt, isFreqLimit)) {
				return fmt.Errorf("feishu webhook canceled during backoff: %w", ctx.Err())
			}
			continue
		}

		// 200 + code != 0 非 11232：业务错误，立即返回
		return fmt.Errorf("code: %d, err: %s", result.Code, result.Msg)
	}

	// 用尽重试次数
	if lastErr != nil {
		return fmt.Errorf("max attempts (%d) reached, last network error: %w", maxAttempts, lastErr)
	}
	return fmt.Errorf("max attempts (%d) reached, last status: %d, code: %d, msg: %s",
		maxAttempts, lastStatus, lastCode, truncate(lastMsg, 200))
}