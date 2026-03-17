package qqbot

import (
	"context"
	"log/slog"
	"os"

	"github.com/sky22333/qqbot/config"
	"github.com/sky22333/qqbot/internal/notifier"
)

type Client struct {
	notifier *notifier.Notifier
}

func New(cfg Config) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	n, err := notifier.New(cfg, logger)
	if err != nil {
		return nil, err
	}
	return &Client{notifier: n}, nil
}

func NewFromConfigFile(path string) (*Client, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	return New(cfg)
}

func (c *Client) Send(ctx context.Context, req PushRequest) (PushResult, error) {
	return c.notifier.Send(ctx, req)
}

func (c *Client) Enqueue(ctx context.Context, req PushRequest) (string, error) {
	return c.notifier.Enqueue(ctx, req)
}

func (c *Client) GetStatus(requestID string) (DeliveryStatus, bool) {
	return c.notifier.GetStatus(requestID)
}

func (c *Client) Close() {
	c.notifier.Close()
}
