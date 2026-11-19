package agentos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const AnalysisTimeout = 12 * time.Minute

type Client struct {
	baseURL string
	appID   string
	userID  string
	token   string
	http    *http.Client
}

type APIError struct {
	Status int
}

func (e *APIError) Error() string { return fmt.Sprintf("AgentOS returned HTTP %d", e.Status) }
func (e *APIError) Retryable() bool {
	return e.Status == http.StatusRequestTimeout || e.Status == http.StatusTooManyRequests || e.Status >= 500
}

func New(baseURL, appID, userID, bearerToken string) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid AgentOS base URL")
	}
	if strings.TrimSpace(appID) == "" || strings.TrimSpace(userID) == "" {
		return nil, errors.New("AgentOS app ID and user ID are required")
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"), appID: appID, userID: userID, token: bearerToken,
		http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}, nil
}

func (c *Client) post(ctx context.Context, path string, body any, accept string) (*http.Response, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", accept)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, &APIError{Status: resp.StatusCode}
	}
	return resp, nil
}

// Analyze runs each feedback issue in its own session; sessions never share report context.
func (c *Client) Analyze(ctx context.Context, issueID, issueURL, eventID string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, AnalysisTimeout)
	defer cancel()
	sessionID, err := c.createSession(ctx)
	if err != nil {
		return nil, fmt.Errorf("create AgentOS session: %w", err)
	}
	message := "请分析 Sentry 用户反馈 Issue " + issueID + "。"
	if issueURL != "" {
		message += "\n反馈链接：" + issueURL
	}
	if eventID != "" {
		message += "\n反馈关联的 Event ID：" + eventID
	}
	message += "\n根据反馈链接和 Sentry 返回的所属项目，调用 Sentry 工具读取指定 Issue 及对应 Event，按应用约定输出研判和结构化报告。"
	resp, err := c.post(ctx, "/chat_stream", map[string]string{
		"agentId": c.appID, "userId": c.userID, "sessionId": sessionID, "message": message,
	}, "text/event-stream")
	if err != nil {
		return nil, fmt.Errorf("start AgentOS analysis: %w", err)
	}
	defer resp.Body.Close()
	return readReportStream(resp.Body)
}

func (c *Client) createSession(ctx context.Context) (string, error) {
	resp, err := c.post(ctx, "/create_session", map[string]string{"agentId": c.appID, "userId": c.userID}, "application/json")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var result struct {
		Code string `json:"code"`
		Data struct {
			SessionID string `json:"sessionId"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return "", fmt.Errorf("invalid AgentOS session response: %w", err)
	}
	if result.Code != "0000" {
		return "", errors.New("AgentOS rejected session creation")
	}
	if result.Data.SessionID == "" {
		return "", errors.New("AgentOS session response missing sessionId")
	}
	return result.Data.SessionID, nil
}
