package message

import "time"

type TargetType string

const (
	TargetC2C     TargetType = "c2c"
	TargetGroup   TargetType = "group"
	TargetChannel TargetType = "channel"
)

type PushRequest struct {
	RequestID      string     `json:"request_id"`
	IdempotencyKey string     `json:"idempotency_key"`
	TargetType     TargetType `json:"target_type"`
	TargetID       string     `json:"target_id"`
	Content        string     `json:"content"`
}

type PushResult struct {
	RequestID string    `json:"request_id"`
	MessageID string    `json:"message_id"`
	Timestamp time.Time `json:"timestamp"`
}

type DeliveryState string

const (
	StateQueued  DeliveryState = "queued"
	StateSending DeliveryState = "sending"
	StateSuccess DeliveryState = "success"
	StateFailed  DeliveryState = "failed"
)

type DeliveryStatus struct {
	RequestID string        `json:"request_id"`
	State     DeliveryState `json:"state"`
	Error     string        `json:"error,omitempty"`
	Result    *PushResult   `json:"result,omitempty"`
	UpdatedAt time.Time     `json:"updated_at"`
}
