package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	"github.com/xujiahua/alertmanager-webhook-feishu/feishu"
	"github.com/xujiahua/alertmanager-webhook-feishu/model"
)

type Server struct {
	bots          map[string]feishu.IBot
	splitByStatus bool

	mu  sync.Mutex
	srv *http.Server // set by Start, consumed by Shutdown
}

// 业务指标
var (
	hookRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "awf_hook_requests_total",
			Help: "Total webhook requests received, by group and outcome.",
		},
		[]string{"group", "outcome"}, // outcome: success | send_error | parse_error | bad_group
	)
	hookLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "awf_hook_duration_seconds",
			Help:    "End-to-end webhook handler latency.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"group"},
	)
)

func init() {
	prometheus.MustRegister(hookRequests, hookLatency)
}

func New(bots map[string]feishu.IBot, splitByStatus bool) *Server {
	s := &Server{
		bots:          bots,
		splitByStatus: splitByStatus,
	}
	return s
}

// maxRequestBody 限制 webhook 请求体大小（1 MiB）。
// Alertmanager 告警 JSON 通常 < 100 KiB；超过则视为异常或攻击。
const maxRequestBody = 1 << 20

// recoverMiddleware 把 panic 转成 500 而不是直接断连。
// Alertmanager 看到 connection reset 会无限重试，panic 恢复可避免此雪崩。
func recoverMiddleware(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				logrus.Errorf("panic in handler %s %s: %v", r.Method, r.URL.Path, rec)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		h(w, r)
	}
}

func (s *Server) hook(w http.ResponseWriter, r *http.Request) {
	// 限制请求体大小，防止大包攻击
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)

	// get path param
	vars := mux.Vars(r)
	group := vars["group"]
	bot, ok := s.bots[group]
	if !ok {
		logrus.Errorf("group not found: %s", group)
		hookRequests.WithLabelValues(group, "bad_group").Inc()
		http.Error(w, "group not found", http.StatusBadRequest)
		return
	}

	timer := prometheus.NewTimer(hookLatency.WithLabelValues(group))
	defer timer.ObserveDuration()

	// get body param
	var alerts model.WebhookMessage
	err := json.NewDecoder(r.Body).Decode(&alerts)
	if err != nil {
		// 不向调用方回显内部错误细节
		logrus.Errorf("cannot parse content: %v", err)
		hookRequests.WithLabelValues(group, "parse_error").Inc()
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if logrus.IsLevelEnabled(logrus.DebugLevel) {
		spew.Dump(alerts)
	}

	// get query string
	meta := make(map[string]string)
	for key, values := range r.URL.Query() {
		meta[key] = strings.Join(values, ",")
	}
	// also include path param
	meta["group"] = group

	var alertsGroups []model.WebhookMessage
	if s.splitByStatus {
		alertsGroups = split(alerts)
	} else {
		alertsGroups = []model.WebhookMessage{alerts}
	}

	for _, alerts := range alertsGroups {
		alerts.Meta = meta
		err = bot.Send(r.Context(), &alerts)
		if err != nil {
			logrus.Errorf("cannot send alerts, %s", err)
			hookRequests.WithLabelValues(group, "send_error").Inc()
			// 不向调用方回显内部错误细节（可能是飞书 URL、敏感告警内容等）
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
	}
	hookRequests.WithLabelValues(group, "success").Inc()

	_, _ = fmt.Fprintf(w, "ok")
}

func split(alerts model.WebhookMessage) []model.WebhookMessage {
	var groups []model.WebhookMessage
	if len(alerts.Alerts.Firing()) != 0 {
		alertsClone := alerts
		alertsClone.Alerts = alerts.Alerts.Firing()
		groups = append(groups, alertsClone)
	}
	if len(alerts.Alerts.Resolved()) != 0 {
		alertsClone := alerts
		alertsClone.Alerts = alerts.Alerts.Resolved()
		groups = append(groups, alertsClone)
	}
	return groups
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	// 简单存活探针 + 配置校验
	if len(s.bots) == 0 {
		http.Error(w, "no bots configured", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) reload(w http.ResponseWriter, r *http.Request) {
	// TODO: 重新加载 config 并热替换 bots
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"reload not implemented"}`))
}

func (s *Server) Start(address string) error {
	r := mux.NewRouter()
	r.HandleFunc("/hook/{group}", recoverMiddleware(s.hook)).Methods("POST")

	// management etc...
	sr := r.PathPrefix("/-").Subrouter()
	sr.HandleFunc("/healthz", recoverMiddleware(s.health)).Methods("GET")
	sr.HandleFunc("/reload", recoverMiddleware(s.reload)).Methods("GET")

	// prometheus
	r.Handle("/metrics", promhttp.Handler()).Methods("GET")

	srv := &http.Server{
		Handler:      r,
		Addr:         address,
		WriteTimeout: 15 * time.Second,
		ReadTimeout:  15 * time.Second,
	}

	// 暴露给 Shutdown：cmd 层拿到信号后调用
	s.mu.Lock()
	s.srv = srv
	s.mu.Unlock()

	err := srv.ListenAndServe()
	// ListenAndServe 在 Shutdown 后会返回 http.ErrServerClosed，属于正常退出
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// Shutdown 优雅关闭：拒绝新请求，等待 in-flight handler 完成（最迟 ctx 到期）。
// 必须先调 Start，再调 Shutdown。
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	srv := s.srv
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}
