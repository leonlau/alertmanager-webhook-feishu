package feishu

import (
	"context"

	"github.com/davecgh/go-spew/spew"
	"github.com/xujiahua/alertmanager-webhook-feishu/model"
)

type FakeBot struct {
}

func (f FakeBot) Send(_ context.Context, message *model.WebhookMessage) error {
	spew.Dump(message)
	return nil
}
