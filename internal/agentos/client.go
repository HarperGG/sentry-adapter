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
func (c *Client) Analyze(ctx context.Context, issueID, issueURL, eventID, feedbackType string) (json.RawMessage, error) {
	message, err := analysisMessage(issueID, issueURL, eventID, feedbackType)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, AnalysisTimeout)
	defer cancel()
	sessionID, err := c.createSession(ctx)
	if err != nil {
		return nil, fmt.Errorf("create AgentOS session: %w", err)
	}
	resp, err := c.post(ctx, "/chat_stream", map[string]string{
		"agentId": c.appID, "userId": c.userID, "sessionId": sessionID, "message": message,
	}, "text/event-stream")
	if err != nil {
		return nil, fmt.Errorf("start AgentOS analysis: %w", err)
	}
	defer resp.Body.Close()
	return readReportStream(resp.Body, feedbackType)
}

func analysisMessage(issueID, issueURL, eventID, feedbackType string) (string, error) {
	var task string
	switch feedbackType {
	case "platform_bug":
		task = "执行平台异常研判，沿用应用的 root_cause 和置信度规则。区分反馈人自述、已观察到的事实和仍待验证的原因。"
	case "data_issue":
		task = "整理数据现象。在 feedback_summary.description 写现象，在 feedback_summary.impact 写已知批次、数据集、视角、帧范围或影响范围；标明来自用户描述还是已独立核实，未知范围明确写待核实。缺少原始数据对照时不推断故障机制。"
	case "other":
		task = "做反馈分流。在 feedback_summary.description 概括诉求；triage.suggested_category 仅选 platform_bug、data_issue、suggestion、other，triage.reason 说明归类依据。无法区分时保留 other，给出能帮助分流的一个问题。疑似需求建议到此结束，不展开需求分析。"
	default:
		return "", errors.New("unsupported feedback type for analysis")
	}
	message := "请分析 Sentry 用户反馈 Issue " + issueID + "。\nfeedback_type=" + feedbackType
	if issueURL != "" {
		message += "\n反馈链接：" + issueURL
	}
	if eventID != "" {
		message += "\n反馈事件本身的 Event ID：" + eventID
	}
	message += "\n使用 Sentry 工具读取指定 Issue 和反馈 Event。单独的关联错误仅从反馈关联字段核实；关联 Trace 或 Replay 只在与现象有关时用作证据。\n" + task
	if feedbackType != "platform_bug" {
		message += "\n在最终 JSON 顶层原样写 feedback_type；此类型可省略 root_cause。保留 analysis_status、evidence（observation、reference）和 recommendations.verification_steps，第一项给出具体核查或分流动作。"
	}
	return message, nil
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
