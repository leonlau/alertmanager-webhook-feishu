package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v2"
)

type Mention struct {
	All      bool     `yaml:"all"`
	OpenIDs  []string `yaml:"open_ids"`
	Rotation string   `yaml:"rotation"`
}

// Template 2 options:
// nil -> default.tmpl,default_alert.tmpl
// CustomPath -> path/to/tmpl/file
type Template struct {
	CustomPath string `yaml:"custom_path"`
}

type Bot struct {
	// Bot Webhook URL
	Webhook     string            `yaml:"url"`
	Mention     *Mention          `yaml:"mention"`
	Template    *Template         `yaml:"template"`
	TitlePrefix string            `yaml:"title_prefix"`
	MetaData    map[string]string `yaml:"metadata"`
}

type Config struct {
	Bots map[string]*Bot `yaml:"bots"`
}

func Load(filename string) (*Config, error) {
	var conf Config
	bs, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	err = yaml.Unmarshal(bs, &conf)
	if err != nil {
		return nil, err
	}

	if err := conf.Validate(); err != nil {
		return nil, err
	}

	return &conf, nil
}

// Validate 检查配置合法性：webhook URL 必须为 https 且 host 非空。
// 启动时尽早失败，避免运行时才暴露。
func (c *Config) Validate() error {
	if len(c.Bots) == 0 {
		return errors.New("no bots configured")
	}
	for name, bot := range c.Bots {
		if err := validateWebhook(bot.Webhook); err != nil {
			return fmt.Errorf("bot %q: %w", name, err)
		}
	}
	return nil
}

func validateWebhook(raw string) error {
	if raw == "" {
		return errors.New("empty webhook url")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid webhook url: %w", err)
	}
	if u.Host == "" {
		return fmt.Errorf("webhook url missing host: %q", raw)
	}
	// 飞书 webhook 要求 https
	if !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("webhook url must use https, got %q", u.Scheme)
	}
	return nil
}
