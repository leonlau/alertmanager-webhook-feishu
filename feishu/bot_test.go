package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"

	"github.com/davecgh/go-spew/spew"
	"github.com/prometheus/alertmanager/template"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"github.com/xujiahua/alertmanager-webhook-feishu/config"
	"github.com/xujiahua/alertmanager-webhook-feishu/model"
	"testing"
	"time"
)

func getConf() *config.Config {
	conf, err := config.Load("../config.example.yml")
	if err != nil {
		panic(err)
	}
	return conf
}

func getBotConf() *config.Bot {
	for _, bot := range getConf().Bots {
		if bot.Mention != nil {
			continue
		}
		return bot
	}
	panic("expect at least one")
}

func TestBot_Send(t *testing.T) {
	// 临时把日志级别调到 debug；用 t.Cleanup 恢复，避免污染其他测试
	prevLevel := logrus.GetLevel()
	logrus.SetLevel(logrus.DebugLevel)
	t.Cleanup(func() { logrus.SetLevel(prevLevel) })

	bot, err := New(getBotConf())
	require.Nil(t, err)
	bot.openIDs = []string{"ou_177f84317c6ee52630edf335d5f8a6fc", "ou_177f84317c6ee52630edf335d5f8a6fc"}
	bot.titlePrefix = "[SHANGHAI]"
	bot.metadata = map[string]string{
		"链接": "https://www.baidu.com",
	}

	// Replace the real feishu webhook with an in-process fake so the test
	// doesn't depend on a valid webhook token.
	var received []byte
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, `{"StatusCode":0,"StatusMessage":"success"}`)
	}))
	defer fake.Close()
	bot.webhook = fake.URL

	alerts := model.WebhookMessage{
		Data: newAlerts(),
		Meta: map[string]string{"group": "hello", "url": "www.baidu.com"},
	}
	err = bot.Send(context.Background(), &alerts)
	spew.Dump(err)
	require.Nil(t, err)
	require.NotEmpty(t, received, "fake feishu server should have received the payload")
}

// copyright: https://github.com/tomtom-international/alertmanager-webhook-logger/blob/master/main_test.go#L132
func newAlerts() template.Data {
	type Cat struct {
		Name  string
		BugMe string
	}
	bs, _ := json.Marshal(&Cat{
		Name: "cool cat",
	})
	bs, _ = json.Marshal(&Cat{
		Name:  "not cool cat",
		BugMe: string(bs),
	})
	return template.Data{
		Alerts: template.Alerts{
			template.Alert{
				Status: "firing",
				Annotations: map[string]string{
					"description": "26.09% throttling of CPU in namespace monitoring for container node-exporter in pod node-exporter-h5sjn" + string(bs),
					"runbook_url": "https://github.com/kubernetes-monitoring/kubernetes-mixin/tree/master/runbook.md#alert-name-cputhrottlinghigh",
					"summary":     "summary",
				},
				Labels:       map[string]string{"severity": "info", "m_key": "m_value"},
				StartsAt:     time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
				EndsAt:       time.Date(2000, 1, 1, 0, 0, 1, 0, time.UTC),
				GeneratorURL: "file://generatorUrl",
			},
			template.Alert{
				Annotations: map[string]string{
					"description": "\u001b26.09% throttling of CPU in namespace monitoring for container node-exporter in pod node-exporter-h5sjn",
				},
				Labels:   map[string]string{"l_key_warn": "l_value_warn"},
				Status:   "resolved",
				StartsAt: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
			},
		},
		CommonAnnotations: map[string]string{"ca_key": "ca_value"},
		CommonLabels:      map[string]string{"cl_key": "cl_value"},
		GroupLabels:       map[string]string{"gl_key": "gl_value"},
		ExternalURL:       "file://externalUrl",
		Receiver:          "test-receiver",
	}
}

func Test_mergeMap(t *testing.T) {
	type args struct {
		left  map[string]string
		right map[string]string
	}
	tests := []struct {
		name string
		args args
		want map[string]string
	}{
		{
			args: args{
				left: map[string]string{
					"a": "1",
					"b": "1",
				},
				right: map[string]string{
					"a": "2",
					"c": "2",
				},
			},
			want: map[string]string{
				"a": "1",
				"b": "1",
				"c": "2",
			},
		},
		{
			args: args{
				left:  nil,
				right: nil,
			},
			want: nil,
		},
		{
			args: args{
				left: nil,
				right: map[string]string{
					"a": "2",
					"c": "2",
				},
			},
			want: map[string]string{
				"a": "2",
				"c": "2",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mergeMap(tt.args.left, tt.args.right); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("mergeMap() = %v, want %v", got, tt.want)
			}
		})
	}
}
