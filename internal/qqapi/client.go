package qqapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sky22333/qqbot/config"
	"github.com/sky22333/qqbot/message"
)

type APIError struct {
	StatusCode int    `json:"status_code"`
	Code       int    `json:"code"`
	Message    string `json:"message"`
	TraceID    string `json:"trace_id"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("qq api error status=%d code=%d message=%s trace=%s", e.StatusCode, e.Code, e.Message, e.TraceID)
}

func (e *APIError) Temporary() bool {
	if e.StatusCode == http.StatusTooManyRequests {
		return true
	}
	return e.StatusCode >= 500
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   any    `json:"expires_in"`
}

type messageResp struct {
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
}

type gatewayResp struct {
	URL string `json:"url"`
}

type apiErrPayload struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type Client struct {
	cfg        config.Config
	httpClient *http.Client

	mu       sync.RWMutex
	token    string
	expires  time.Time
	appID    string
	secret   string
	tokenURL string
	baseURL  string
}

func NewClient(cfg config.Config) (*Client, error) {
	timeout, err := cfg.RequestTimeout()
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		MaxIdleConns:        128,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     90 * time.Second,
	}
	return &Client{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout:   timeout,
			Transport: transport,
		},
		appID:    cfg.QQBot.AppID,
		secret:   cfg.QQBot.ClientSecret,
		tokenURL: strings.TrimRight(cfg.QQBot.TokenURL, "/"),
		baseURL:  strings.TrimRight(cfg.QQBot.APIBase, "/"),
	}, nil
}

func (c *Client) Send(ctx context.Context, req message.PushRequest) (message.PushResult, error) {
	token, err := c.getToken(ctx)
	if err != nil {
		return message.PushResult{}, err
	}
	res, err := c.sendWithToken(ctx, token, req)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnauthorized {
			c.invalidateToken()
			token, tokenErr := c.getToken(ctx)
			if tokenErr != nil {
				return message.PushResult{}, tokenErr
			}
			return c.sendWithToken(ctx, token, req)
		}
		return message.PushResult{}, err
	}
	return res, nil
}

func (c *Client) AccessToken(ctx context.Context) (string, error) {
	return c.getToken(ctx)
}

func (c *Client) GatewayURL(ctx context.Context) (string, error) {
	token, err := c.getToken(ctx)
	if err != nil {
		return "", err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/gateway", nil)
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Authorization", "QQBot "+token)
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("获取 gateway 失败: status=%d body=%s", resp.StatusCode, string(raw))
	}
	data := gatewayResp{}
	if err := json.Unmarshal(raw, &data); err != nil {
		return "", err
	}
	if strings.TrimSpace(data.URL) == "" {
		return "", errors.New("gateway url 为空")
	}
	return data.URL, nil
}

func (c *Client) sendWithToken(ctx context.Context, token string, req message.PushRequest) (message.PushResult, error) {
	url, body, err := c.buildRequest(req)
	if err != nil {
		return message.PushResult{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return message.PushResult{}, err
	}
	httpReq.Header.Set("Authorization", "QQBot "+token)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return message.PushResult{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return message.PushResult{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		apiErr := apiErrPayload{}
		_ = json.Unmarshal(raw, &apiErr)
		return message.PushResult{}, &APIError{
			StatusCode: resp.StatusCode,
			Code:       apiErr.Code,
			Message:    apiErr.Message,
			TraceID:    resp.Header.Get("x-tps-trace-id"),
		}
	}
	out := messageResp{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return message.PushResult{}, err
	}
	return message.PushResult{
		RequestID: req.RequestID,
		MessageID: out.ID,
		Timestamp: parseTime(out.Timestamp),
	}, nil
}

func (c *Client) buildRequest(req message.PushRequest) (string, []byte, error) {
	if strings.TrimSpace(req.TargetID) == "" {
		return "", nil, errors.New("target_id 不能为空")
	}
	if strings.TrimSpace(req.Content) == "" {
		return "", nil, errors.New("content 不能为空")
	}
	payload := map[string]any{
		"content":  req.Content,
		"msg_type": 0,
	}
	if c.cfg.QQBot.Markdown {
		payload = map[string]any{
			"markdown": map[string]any{"content": req.Content},
			"msg_type": 2,
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", nil, err
	}
	switch req.TargetType {
	case message.TargetC2C:
		return fmt.Sprintf("%s/v2/users/%s/messages", c.baseURL, req.TargetID), body, nil
	case message.TargetGroup:
		return fmt.Sprintf("%s/v2/groups/%s/messages", c.baseURL, req.TargetID), body, nil
	case message.TargetChannel:
		return fmt.Sprintf("%s/channels/%s/messages", c.baseURL, req.TargetID), body, nil
	default:
		return "", nil, fmt.Errorf("不支持的 target_type: %s", req.TargetType)
	}
}

func (c *Client) getToken(ctx context.Context) (string, error) {
	c.mu.RLock()
	if c.token != "" && time.Until(c.expires) > 5*time.Minute {
		token := c.token
		c.mu.RUnlock()
		return token, nil
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Until(c.expires) > 5*time.Minute {
		return c.token, nil
	}

	reqBody, _ := json.Marshal(map[string]string{
		"appId":        c.appID,
		"clientSecret": c.secret,
	})
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, bytes.NewReader(reqBody))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("获取 token 失败: status=%d body=%s", resp.StatusCode, string(raw))
	}
	tokenData := tokenResponse{}
	if err := json.Unmarshal(raw, &tokenData); err != nil {
		return "", err
	}
	if tokenData.AccessToken == "" {
		return "", errors.New("token 为空")
	}
	expSec := parseExpiresIn(tokenData.ExpiresIn)
	if expSec <= 0 {
		expSec = 7200
	}
	c.token = tokenData.AccessToken
	c.expires = time.Now().Add(time.Duration(expSec) * time.Second)
	return c.token, nil
}

func parseExpiresIn(raw any) int64 {
	switch v := raw.(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case json.Number:
		n, err := v.Int64()
		if err == nil {
			return n
		}
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return 0
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err == nil {
			return n
		}
	}
	return 0
}

func (c *Client) invalidateToken() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = ""
	c.expires = time.Time{}
}

func parseTime(raw string) time.Time {
	if raw == "" {
		return time.Now()
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t
	}
	if t, err := time.Parse("2006-01-02T15:04:05Z07:00", raw); err == nil {
		return t
	}
	return time.Now()
}
