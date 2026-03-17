package collector

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sky22333/qqbot/config"
	"github.com/sky22333/qqbot/internal/qqapi"
	"github.com/sky22333/qqbot/internal/targets"
	"github.com/sky22333/qqbot/message"
)

const (
	intentPublicGuildMessages = 1 << 30
	intentDirectMessage       = 1 << 12
	intentGroupAndC2C         = 1 << 25
	intentGuildMembers        = 1 << 1
)

var intentLevels = []int{
	intentPublicGuildMessages | intentDirectMessage | intentGroupAndC2C,
	intentPublicGuildMessages | intentGroupAndC2C,
	intentPublicGuildMessages | intentGuildMembers,
}

type wsPayload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
	S  *int64          `json:"s,omitempty"`
	T  string          `json:"t,omitempty"`
}

type helloData struct {
	HeartbeatInterval int64 `json:"heartbeat_interval"`
}

type c2cMessageEvent struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Author  struct {
		UserOpenID string `json:"user_openid"`
	} `json:"author"`
}

type groupMessageEvent struct {
	ID          string `json:"id"`
	Content     string `json:"content"`
	GroupOpenID string `json:"group_openid"`
}

type Collector struct {
	cfg    config.Config
	logger *slog.Logger
	client *qqapi.Client
	store  *targets.Store

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func New(cfg config.Config, logger *slog.Logger, store *targets.Store) (*Collector, error) {
	client, err := qqapi.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Collector{
		cfg:    cfg,
		logger: logger,
		client: client,
		store:  store,
		ctx:    ctx,
		cancel: cancel,
	}, nil
}

func (c *Collector) Start() {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.runLoop()
	}()
}

func (c *Collector) Stop() {
	c.cancel()
	c.wg.Wait()
}

func (c *Collector) runLoop() {
	reconnectDelay, _ := c.cfg.CollectorReconnectDelay()
	if reconnectDelay <= 0 {
		reconnectDelay = 3 * time.Second
	}
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}
		if err := c.runOnce(); err != nil {
			if c.ctx.Err() != nil {
				return
			}
			c.logger.Error("collector 连接中断", "error", err)
		}
		timer := time.NewTimer(reconnectDelay)
		select {
		case <-c.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (c *Collector) runOnce() error {
	token, err := c.client.AccessToken(c.ctx)
	if err != nil {
		return err
	}
	gatewayURL, err := c.client.GatewayURL(c.ctx)
	if err != nil {
		return err
	}
	conn, _, err := websocket.DefaultDialer.DialContext(c.ctx, gatewayURL, nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	stopClose := make(chan struct{})
	go func() {
		select {
		case <-c.ctx.Done():
			_ = conn.Close()
		case <-stopClose:
		}
	}()
	defer close(stopClose)
	c.logger.Info("collector 已连接 gateway")

	var lastSeq atomic.Int64
	intentIndex := 0
	heartbeatStop := make(chan struct{})
	defer close(heartbeatStop)

	for {
		select {
		case <-c.ctx.Done():
			return nil
		default:
		}

		_, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		payload := wsPayload{}
		if err := json.Unmarshal(data, &payload); err != nil {
			continue
		}
		if payload.S != nil {
			lastSeq.Store(*payload.S)
		}

		switch payload.Op {
		case 10:
			hello := helloData{}
			if err := json.Unmarshal(payload.D, &hello); err == nil && hello.HeartbeatInterval > 0 {
				go startHeartbeat(c.ctx, conn, hello.HeartbeatInterval, &lastSeq, heartbeatStop)
			}
			if err := sendIdentify(conn, token, intentLevels[intentIndex]); err != nil {
				return err
			}
		case 9:
			if intentIndex < len(intentLevels)-1 {
				intentIndex++
			}
			if err := sendIdentify(conn, token, intentLevels[intentIndex]); err != nil {
				return err
			}
		case 0:
			c.handleDispatch(payload.T, payload.D)
		case 7:
			return errors.New("gateway 要求重连")
		}
	}
}

func (c *Collector) handleDispatch(eventType string, raw json.RawMessage) {
	switch strings.TrimSpace(eventType) {
	case "C2C_MESSAGE_CREATE":
		event := c2cMessageEvent{}
		if err := json.Unmarshal(raw, &event); err != nil {
			return
		}
		if event.Author.UserOpenID == "" {
			return
		}
		_ = c.store.Upsert(message.TargetC2C, event.Author.UserOpenID, event.ID, event.Content)
		c.logger.Info("采集到 c2c 目标", "target_id", event.Author.UserOpenID, "message_id", event.ID)
	case "GROUP_AT_MESSAGE_CREATE":
		event := groupMessageEvent{}
		if err := json.Unmarshal(raw, &event); err != nil {
			return
		}
		if event.GroupOpenID == "" {
			return
		}
		_ = c.store.Upsert(message.TargetGroup, event.GroupOpenID, event.ID, event.Content)
		c.logger.Info("采集到 group 目标", "target_id", event.GroupOpenID, "message_id", event.ID)
	}
}

func startHeartbeat(ctx context.Context, conn *websocket.Conn, intervalMS int64, seq *atomic.Int64, stop <-chan struct{}) {
	ticker := time.NewTicker(time.Duration(intervalMS) * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-ticker.C:
			payload := map[string]any{
				"op": 1,
				"d":  seq.Load(),
			}
			_ = conn.WriteJSON(payload)
		}
	}
}

func sendIdentify(conn *websocket.Conn, token string, intents int) error {
	payload := map[string]any{
		"op": 2,
		"d": map[string]any{
			"token":   "QQBot " + token,
			"intents": intents,
			"shard":   []int{0, 1},
		},
	}
	return conn.WriteJSON(payload)
}
