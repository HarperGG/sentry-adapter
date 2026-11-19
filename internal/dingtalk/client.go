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
	IssueID           string
	FeedbackType      string
	TaskType          string
	DatasetID         string
	AnalysisStatus    string
	Problem           string
	Cause             string
	Category          string
	Confidence        string
	ConfidenceScore   *int
	Evidence          string
	Impact            string
	SuggestedCategory string
	TriageReason      string
	NextStep          string
	SentryURL         string
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
	content, err := renderReport(report, link)
	if err != nil {
		return err
	}
	body, err := json.Marshal(robotMessage{Type: "markdown", Markdown: content})
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

func renderReport(report Report, link *url.URL) (markdown, error) {
	var subject string
	var details []string
	switch report.FeedbackType {
	case "", "platform_bug": // Reports created before the category rollout were platform bugs.
		subject = "平台异常"
		confidence := "未评分"
		if score := report.ConfidenceScore; score != nil {
			if *score < 0 || *score > 100 {
				return markdown{}, &Rejected{reason: "confidence score outside 0–100"}
			}
			confidence = fmt.Sprintf("%d/100", *score)
			if report.Confidence != "" {
				confidence = markdownText(report.Confidence, "未知") + "（" + confidence + "）"
			}
		}
		details = []string{
			"- **初步原因** " + reportText(report.Cause, "待核实"),
			"- **分类** " + reportText(report.Category, "无法判定"),
			"- **置信度** " + confidence,
		}
	case "data_issue":
		subject = "数据问题"
		details = []string{"- **影响范围** " + reportText(report.Impact, "待核实")}
	case "other":
		subject = "其他反馈分流"
		details = []string{
			"- **建议分类** " + suggestedCategoryLabel(report.SuggestedCategory),
			"- **分流依据** " + reportText(report.TriageReason, "待核实"),
		}
	default:
		return markdown{}, &Rejected{reason: "unsupported feedback type"}
	}

	title := keyword + "｜" + subject
	if task := markdownText(report.TaskType, ""); task != "" {
		title += "｜" + task
	}
	if dataset := markdownText(report.DatasetID, ""); dataset != "" {
		title += "｜数据集 " + dataset
	}
	lines := []string{"### " + title, "", "- **Issue** " + markdownText(report.IssueID, "未提供")}
	if status := markdownText(report.AnalysisStatus, ""); status != "" {
		lines = append(lines, "- **分析状态** "+status)
	}
	lines = append(lines, "- **问题** "+reportText(report.Problem, "未提供"))
	lines = append(lines, details...)
	lines = append(lines,
		"- **证据** "+reportText(report.Evidence, "见原始反馈"),
		"- **下一步** "+reportText(report.NextStep, "待进一步核实"),
		"", "[查看 Sentry 反馈]("+markdownLink.Replace(link.String())+")",
	)
	return markdown{Title: title, Text: strings.Join(lines, "\n")}, nil
}

func suggestedCategoryLabel(value string) string {
	switch value {
	case "platform_bug":
		return "平台异常"
	case "data_issue":
		return "数据问题"
	case "suggestion":
		return "需求建议"
	case "other":
		return "其他"
	default:
		return "待人工分流"
	}
}

// Summaries sometimes copy a whole list of internal IDs from the Sentry context.
// Keep the actionable dataset number and replace a contiguous list only once.
const (
	refValueSep = `\s*(?:[=:：]\s*)?`
	taskRef     = `\b(?:clips?_?id|data_?pool_?id|requirement(?:_?id)?|replay(?:_?id)?)\b` + refValueSep +
		`(?:[0-9a-f]{4,}(?:…+|\.{3})[0-9a-f]*|[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}|[0-9a-f]{8,})`
	datasetRef = `\bdataset_?id\b` + refValueSep + `([0-9]+)\b`
	namedRef   = `(?:` + taskRef + `|` + datasetRef + `)`
)

var (
	reportRefList = regexp.MustCompile(`(?i)` + namedRef + `(?:\s*[,，、]\s*` + namedRef + `)*`)
	reportDataset = regexp.MustCompile(`(?i)` + datasetRef)
)

func reportText(value, fallback string) string {
	value = reportRefList.ReplaceAllStringFunc(sanitizedText(value), func(list string) string {
		if matches := reportDataset.FindAllStringSubmatch(list, -1); len(matches) != 0 {
			ids := make([]string, 0, len(matches))
			for _, match := range matches {
				ids = append(ids, match[1])
			}
			return "数据集 " + strings.Join(ids, "、")
		}
		if strings.HasPrefix(strings.ToLower(list), "replay") {
			return "该回放"
		}
		return "该任务"
	})
	return escapeField(value, fallback)
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
	return escapeField(sanitizedText(value), fallback)
}

func sanitizedText(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return -1
		}
		return r
	}, value)
	value = secretValue.ReplaceAllString(value, "（凭据已隐藏）")
	return inlineURL.ReplaceAllString(value, "（链接已省略）")
}

func escapeField(value, fallback string) string {
	if value == "" {
		return fallback
	}
	if runes := []rune(value); len(runes) > maxFieldRunes {
		value = string(runes[:maxFieldRunes]) + "…"
	}
	return escapeText.Replace(value)
}
