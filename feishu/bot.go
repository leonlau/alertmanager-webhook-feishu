package feishu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"
	"time"

	"github.com/icza/gox/stringsx"
	"github.com/sirupsen/logrus"
	"github.com/xujiahua/alertmanager-webhook-feishu/config"
	"github.com/xujiahua/alertmanager-webhook-feishu/feishu/rotate"
	"github.com/xujiahua/alertmanager-webhook-feishu/model"
	"github.com/xujiahua/alertmanager-webhook-feishu/tmpl"
)

type Bot struct {
	webhook     string
	openIDs     []string
	rotator     *rotate.MentionRotator
	sdk         *Sdk
	tpl         *template.Template
	alertTpl    *template.Template
	titlePrefix string
	metadata    map[string]string
}

func New(bot *config.Bot) (*Bot, error) {
	openIDs, err := getOpenIDs(bot.Mention)
	if err != nil {
		return nil, err
	}

	var rotator *rotate.MentionRotator
	if bot.Mention != nil && bot.Mention.Rotation != "" && len(openIDs) > 1 {
		rotator, err = rotate.New(bot.Mention.Rotation, openIDs)
		if err != nil {
			return nil, err
		}
	}

	// template
	tpl, alertTpl, err := getTemplates(bot.Template)
	if err != nil {
		return nil, err
	}

	return &Bot{
		webhook:     bot.Webhook,
		rotator:     rotator,
		openIDs:     openIDs,
		sdk:         NewSDK(),
		tpl:         tpl,
		alertTpl:    alertTpl,
		titlePrefix: bot.TitlePrefix,
		metadata:    bot.MetaData,
	}, nil
}

// getOpenIDs resolves mention targets.
// Use `mention.open_ids` (or `mention.all: true`) to address users.
func getOpenIDs(mention *config.Mention) ([]string, error) {
	if mention == nil {
		return nil, nil
	}
	if mention.All {
		return []string{"all"}, nil
	}
	return mention.OpenIDs, nil
}

func getTemplates(tmplConf *config.Template) (*template.Template, *template.Template, error) {
	if tmplConf != nil && tmplConf.CustomPath != "" {
		t, err := tmpl.GetCustomTemplate(tmplConf.CustomPath)
		if err != nil {
			return nil, nil, err
		}
		return t, nil, nil
	}

	// by default, use two tmpls, one is for alert
	dt, err := tmpl.GetEmbedTemplate("default.tmpl")
	if err != nil {
		return nil, nil, err
	}

	dat, err := tmpl.GetEmbedTemplate("default_alert.tmpl")
	if err != nil {
		return nil, nil, err
	}

	return dt, dat, nil
}

func (b Bot) Send(ctx context.Context, alerts *model.WebhookMessage) error {
	// attach @xxx
	if b.rotator != nil {
		alerts.OpenIDs = b.rotator.Rotate(time.Now())
	} else {
		alerts.OpenIDs = b.openIDs
	}
	// title prefix
	alerts.TitlePrefix = b.titlePrefix

	// merge metadata
	alerts.Meta = mergeMap(alerts.Meta, b.metadata)

	// prepare data
	err := b.preprocessAlerts(alerts)
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	err = b.tpl.Execute(&buf, alerts)
	if err != nil {
		return err
	}
	if logrus.IsLevelEnabled(logrus.DebugLevel) {
		if d, err := beautifyJSON(buf.String()); err != nil {
			logrus.WithError(err).Debug("beautify rendered card failed; dumping raw body")
			logrus.Debug(buf.String())
		} else {
			logrus.Debug(d)
		}
	}

	return b.sdk.WebhookV2(ctx, b.webhook, &buf)
}

// mergeMap 返回一个新 map：left 中已有的 key 保持不变，right 中仅补充 left 没有的 key。
// 不修改入参 map，避免对调用方共享状态的隐式耦合（splitByStatus 多迭代
// 或将来并发扩展时尤其重要）。
// 语义：left 优先（与历史行为一致），right 用于补缺。
// 特殊：两个 nil 都传 → 返回 nil（保持历史行为）。
func mergeMap(left, right map[string]string) map[string]string {
	if left == nil && right == nil {
		return nil
	}
	out := make(map[string]string, len(left)+len(right))
	for k, v := range left {
		out[k] = v
	}
	for k, v := range right {
		if _, ok := out[k]; !ok {
			out[k] = v
		}
	}
	return out
}

// field description may contain double quote, non printable chars
func fixDescription(s string) string {
	// feishu fix: clean non printable char
	s = stringsx.Clean(s)
	// feishu fix: unescape a string
	s = fmt.Sprintf("%#v", s)
	// remove prefix and suffix double quote, means we just unescape inner text
	s = strings.TrimPrefix(s, "\"")
	s = strings.TrimSuffix(s, "\"")
	return s
}

func (b Bot) preprocessAlerts(alerts *model.WebhookMessage) error {
	if b.alertTpl == nil {
		return nil
	}

	// preprocess using alert template
	for _, alert := range alerts.Alerts.Firing() {
		var buf bytes.Buffer
		if _, ok := alert.Annotations["description"]; ok {
			alert.Annotations["description"] = fixDescription(alert.Annotations["description"])
		}
		err := b.alertTpl.Execute(&buf, alert)
		if err != nil {
			return err
		}
		res := strings.ReplaceAll(buf.String(), "\n", "\\n")
		alerts.FiringAlerts = append(alerts.FiringAlerts, res)
	}
	for _, alert := range alerts.Alerts.Resolved() {
		var buf bytes.Buffer
		if _, ok := alert.Annotations["description"]; ok {
			alert.Annotations["description"] = fixDescription(alert.Annotations["description"])
		}
		err := b.alertTpl.Execute(&buf, alert)
		if err != nil {
			return err
		}
		res := strings.ReplaceAll(buf.String(), "\n", "\\n")
		alerts.ResolvedAlerts = append(alerts.ResolvedAlerts, res)
	}

	return nil
}

func beautifyJSON(raw string) (string, error) {
	data := make(map[string]interface{})
	err := json.Unmarshal([]byte(raw), &data)
	if err != nil {
		return "", err
	}
	d, err := json.MarshalIndent(data, "", "\t")
	if err != nil {
		return "", err
	}
	return string(d), nil
}
