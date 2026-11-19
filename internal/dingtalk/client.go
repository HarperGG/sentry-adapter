package dingtalk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
)

const (
	keyword          = "Sentry智能助手"
	maxFieldRunes    = 500
	maxResponseBytes = 64 << 10
	robotRateLimited = 130101
)

type Client struct {
	webhook    *url.URL
	sentryBase *url.URL
	http       *http.Client
}

type Report struct {
	IssueID         string
	Problem         string
	Cause           string
	Category        string
	ConfidenceScore *int
	NextStep        string
	SentryURL       string
}

// Rejected means this call definitively did not post a message.
type Rejected struct {
	reason    string
	retryable bool
}

func (e *Rejected) Error() string   { return "DingTalk notification rejected: " + e.reason }
func (e *Rejected) Retryable() bool { return e.retryable }

// Uncertain means the caller cannot tell whether DingTalk posted the message.
type Uncertain struct{ reason string }

func (e *Uncertain) Error() string { return "DingTalk notification outcome unknown: " + e.reason }

type robotMessage struct {
	Type     string   `json:"msgtype"`
	Markdown markdown `json:"markdown"`
}

type markdown struct {
	Title string `json:"title"`
	Text  string `json:"text"`
}

func New(webhookURL, sentryBaseURL string) (*Client, error) {
	webhook, err := url.Parse(strings.TrimSpace(webhookURL))
	if err != nil || !isAbsoluteWebURL(webhook) || webhook.Scheme != "https" || webhook.Fragment != "" {
		return nil, errors.New("invalid DingTalk HTTPS webhook URL")
	}
	sentryBase, err := url.Parse(strings.TrimSpace(sentryBaseURL))
	if err != nil || !isAbsoluteWebURL(sentryBase) {
		return nil, errors.New("invalid Sentry base URL for DingTalk links")
	}
	return &Client{
		webhook:    webhook,
		sentryBase: sentryBase,
		http: &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (c *Client) Send(ctx context.Context, report Report) error {
	if strings.TrimSpace(report.IssueID) == "" {
		return &Rejected{reason: "missing Sentry issue ID"}
	}
	link, err := url.Parse(strings.TrimSpace(report.SentryURL))
	if err != nil || !isAbsoluteWebURL(link) || link.Scheme != c.sentryBase.Scheme ||
		!strings.EqualFold(link.Host, c.sentryBase.Host) {
		return &Rejected{reason: "invalid Sentry issue link"}
	}
	confidence := "未评分"
	if report.ConfidenceScore != nil {
		score := *report.ConfidenceScore
		if score < 0 || score > 100 {
			return &Rejected{reason: "confidence score outside 0–100"}
		}
		confidence = fmt.Sprintf("%d/100", score)
	}
	message := robotMessage{Type: "markdown", Markdown: markdown{
		Title: keyword + "｜用户反馈初步分析",
		Text: strings.Join([]string{
			"### " + keyword + " · 用户反馈初步分析",
			"",
			"- **Issue** " + markdownText(report.IssueID, "未提供"),
			"- **问题** " + markdownText(report.Problem, "未提供"),
			"- **初步原因** " + markdownText(report.Cause, "证据不足"),
			"- **分类** " + markdownText(report.Category, "未分类"),
			"- **置信度** " + confidence,
			"- **下一步** " + markdownText(report.NextStep, "待进一步核实"),
			"",
			"[查看 Sentry 反馈](" + markdownLink.Replace(link.String()) + ")",
		}, "\n"),
	}}
	body, err := json.Marshal(message)
	if err != nil {
		return &Rejected{reason: "unable to encode notification"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.webhook.String(), bytes.NewReader(body))
	if err != nil {
		return &Rejected{reason: "unable to create request"}
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := c.http.Do(req)
	if err != nil {
		// HTTP client errors may include the webhook URL and its access token.
		return &Uncertain{reason: "request did not return a response"}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		reason := fmt.Sprintf("HTTP %d", resp.StatusCode)
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusRequestTimeout {
			return &Rejected{reason: reason, retryable: resp.StatusCode == http.StatusTooManyRequests}
		}
		return &Uncertain{reason: reason}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil || len(data) > maxResponseBytes {
		return &Uncertain{reason: "unable to read response"}
	}
	var reply map[string]json.RawMessage
	var errorCode *int
	if json.Unmarshal(data, &reply) != nil || json.Unmarshal(reply["errcode"], &errorCode) != nil || errorCode == nil {
		return &Uncertain{reason: "response has no valid errcode"}
	}
	if *errorCode != 0 {
		return &Rejected{reason: fmt.Sprintf("robot errcode %d", *errorCode), retryable: *errorCode == robotRateLimited}
	}
	return nil
}

func isAbsoluteWebURL(u *url.URL) bool {
	return u != nil && (u.Scheme == "https" || u.Scheme == "http") && u.Hostname() != "" &&
		u.User == nil && u.Opaque == ""
}

var (
	inlineURL    = regexp.MustCompile(`(?i)(?:[a-z][a-z0-9+.-]*://|www\.)[^\s<>\[\]{}]+`)
	secretValue  = regexp.MustCompile(`(?i)(?:\b(?:authorization|access[_-]?token|api[_-]?key|password|secret)\s*[:=]\s*(?:basic\s+|bearer\s+)?|\bbearer\s+)(?:"[^"]*"|'[^']*'|[^\s,;]+)`)
	markdownLink = strings.NewReplacer("(", "%28", ")", "%29", "<", "%3C", ">", "%3E", " ", "%20", "@", "%40")
	escapeText   = strings.NewReplacer(
		"\\", "\\\\", "`", "\\`", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]", "|", "\\|",
		"&", "&amp;", "<", "&lt;", ">", "&gt;", "@", "＠",
	)
)

func markdownText(value, fallback string) string {
	value = strings.Join(strings.Fields(value), " ")
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return -1
		}
		return r
	}, value)
	if value == "" {
		return fallback
	}
	value = secretValue.ReplaceAllString(value, "（凭据已隐藏）")
	value = inlineURL.ReplaceAllString(value, "（链接已省略）")
	if runes := []rune(value); len(runes) > maxFieldRunes {
		value = string(runes[:maxFieldRunes]) + "…"
	}
	return escapeText.Replace(value)
}
