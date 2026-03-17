package notifier

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/sky22333/qqbot/config"
	"github.com/sky22333/qqbot/internal/qqapi"
	"github.com/sky22333/qqbot/internal/targets"
	"github.com/sky22333/qqbot/message"
)

type idempotentRecord struct {
	result    message.PushResult
	expiresAt time.Time
}

type statusRecord struct {
	status    message.DeliveryStatus
	expiresAt time.Time
}

type job struct {
	req message.PushRequest
}

type Notifier struct {
	cfg    config.Config
	logger *slog.Logger
	client *qqapi.Client

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	queue chan job

	mu         sync.RWMutex
	idempotent map[string]idempotentRecord
	statuses   map[string]statusRecord
	targets    *targets.Store
}

func New(cfg config.Config, logger *slog.Logger) (*Notifier, error) {
	client, err := qqapi.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	n := &Notifier{
		cfg:        cfg,
		logger:     logger,
		client:     client,
		ctx:        ctx,
		cancel:     cancel,
		queue:      make(chan job, cfg.Dispatch.QueueSize),
		idempotent: make(map[string]idempotentRecord),
		statuses:   make(map[string]statusRecord),
	}
	n.startWorkers()
	n.startCleanup()
	return n, nil
}

func (n *Notifier) SetTargetStore(store *targets.Store) {
	n.targets = store
}

func (n *Notifier) Close() {
	n.cancel()
	n.wg.Wait()
}

func (n *Notifier) Enqueue(ctx context.Context, req message.PushRequest) (string, error) {
	req = n.fillTargetIfMissing(req)
	if err := validateRequest(req); err != nil {
		return "", err
	}
	if req.RequestID == "" {
		req.RequestID = newRequestID()
	}
	n.setStatus(message.DeliveryStatus{
		RequestID: req.RequestID,
		State:     message.StateQueued,
		UpdatedAt: time.Now(),
	})

	timeout, _ := n.cfg.EnqueueTimeout()
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-n.ctx.Done():
		return "", errors.New("notifier 已关闭")
	case <-timer.C:
		n.setStatus(message.DeliveryStatus{
			RequestID: req.RequestID,
			State:     message.StateFailed,
			Error:     "入队超时",
			UpdatedAt: time.Now(),
		})
		return "", errors.New("入队超时")
	case n.queue <- job{req: req}:
		return req.RequestID, nil
	}
}

func (n *Notifier) Send(ctx context.Context, req message.PushRequest) (message.PushResult, error) {
	req = n.fillTargetIfMissing(req)
	if err := validateRequest(req); err != nil {
		return message.PushResult{}, err
	}
	if req.RequestID == "" {
		req.RequestID = newRequestID()
	}
	if req.IdempotencyKey != "" {
		if result, ok := n.getIdempotent(req.IdempotencyKey); ok {
			n.setStatus(message.DeliveryStatus{
				RequestID: req.RequestID,
				State:     message.StateSuccess,
				Result:    &result,
				UpdatedAt: time.Now(),
			})
			return result, nil
		}
	}
	n.setStatus(message.DeliveryStatus{
		RequestID: req.RequestID,
		State:     message.StateSending,
		UpdatedAt: time.Now(),
	})
	result, err := n.sendWithRetry(ctx, req)
	if err != nil {
		n.setStatus(message.DeliveryStatus{
			RequestID: req.RequestID,
			State:     message.StateFailed,
			Error:     err.Error(),
			UpdatedAt: time.Now(),
		})
		return message.PushResult{}, err
	}
	if req.IdempotencyKey != "" {
		n.setIdempotent(req.IdempotencyKey, result)
	}
	n.setStatus(message.DeliveryStatus{
		RequestID: req.RequestID,
		State:     message.StateSuccess,
		Result:    &result,
		UpdatedAt: time.Now(),
	})
	return result, nil
}

func (n *Notifier) fillTargetIfMissing(req message.PushRequest) message.PushRequest {
	if strings.TrimSpace(req.TargetID) != "" || n.targets == nil {
		return req
	}
	req.TargetType = message.TargetType(strings.TrimSpace(string(req.TargetType)))
	if req.TargetType == "" {
		item, ok := n.targets.LatestAny()
		if !ok {
			return req
		}
		req.TargetType = item.TargetType
		req.TargetID = item.TargetID
		return req
	}
	item, ok := n.targets.Latest(req.TargetType)
	if !ok {
		return req
	}
	req.TargetID = item.TargetID
	return req
}

func (n *Notifier) GetStatus(requestID string) (message.DeliveryStatus, bool) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	record, ok := n.statuses[requestID]
	if !ok || time.Now().After(record.expiresAt) {
		return message.DeliveryStatus{}, false
	}
	return record.status, true
}

func (n *Notifier) startWorkers() {
	for i := 0; i < n.cfg.Dispatch.Workers; i++ {
		n.wg.Add(1)
		go func(workerID int) {
			defer n.wg.Done()
			for {
				select {
				case <-n.ctx.Done():
					return
				case task := <-n.queue:
					_, err := n.Send(n.ctx, task.req)
					if err != nil {
						n.logger.Error("消息发送失败", "worker", workerID, "request_id", task.req.RequestID, "error", err)
					}
				}
			}
		}(i + 1)
	}
}

func (n *Notifier) startCleanup() {
	interval := time.Duration(n.cfg.Runtime.CleanupIntervalSec) * time.Second
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-n.ctx.Done():
				return
			case <-ticker.C:
				now := time.Now()
				n.mu.Lock()
				for key, val := range n.idempotent {
					if now.After(val.expiresAt) {
						delete(n.idempotent, key)
					}
				}
				for key, val := range n.statuses {
					if now.After(val.expiresAt) {
						delete(n.statuses, key)
					}
				}
				n.mu.Unlock()
			}
		}
	}()
}

func (n *Notifier) sendWithRetry(ctx context.Context, req message.PushRequest) (message.PushResult, error) {
	backoff := time.Duration(n.cfg.Dispatch.RetryBackoffMS) * time.Millisecond
	max := n.cfg.Dispatch.RetryMax
	var lastErr error
	for attempt := 0; attempt <= max; attempt++ {
		result, err := n.client.Send(ctx, req)
		if err == nil {
			return result, nil
		}
		lastErr = err
		var apiErr *qqapi.APIError
		if errors.As(err, &apiErr) {
			if !apiErr.Temporary() {
				return message.PushResult{}, err
			}
		}
		if attempt == max {
			break
		}
		sleep := backoff * time.Duration(1<<attempt)
		timer := time.NewTimer(sleep)
		select {
		case <-ctx.Done():
			timer.Stop()
			return message.PushResult{}, ctx.Err()
		case <-timer.C:
		}
	}
	return message.PushResult{}, fmt.Errorf("重试后仍失败: %w", lastErr)
}

func (n *Notifier) setIdempotent(key string, result message.PushResult) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.idempotent[key] = idempotentRecord{
		result:    result,
		expiresAt: time.Now().Add(time.Duration(n.cfg.Runtime.IdempotencyTTLSeconds) * time.Second),
	}
}

func (n *Notifier) getIdempotent(key string) (message.PushResult, bool) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	record, ok := n.idempotent[key]
	if !ok || time.Now().After(record.expiresAt) {
		return message.PushResult{}, false
	}
	return record.result, true
}

func (n *Notifier) setStatus(status message.DeliveryStatus) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.statuses[status.RequestID] = statusRecord{
		status:    status,
		expiresAt: time.Now().Add(time.Duration(n.cfg.Runtime.StatusTTLSeconds) * time.Second),
	}
}

func newRequestID() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

func validateRequest(req message.PushRequest) error {
	if strings.TrimSpace(req.TargetID) == "" {
		return errors.New("target_id 不能为空")
	}
	if strings.TrimSpace(req.Content) == "" {
		return errors.New("content 不能为空")
	}
	if req.TargetType != message.TargetC2C &&
		req.TargetType != message.TargetGroup &&
		req.TargetType != message.TargetChannel {
		return errors.New("target_type 只支持 c2c/group/channel")
	}
	return nil
}
