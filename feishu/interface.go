package feishu

import (
	"context"

	"github.com/xujiahua/alertmanager-webhook-feishu/model"
)

type IBot interface {
	Send(context.Context, *model.WebhookMessage) error
}
