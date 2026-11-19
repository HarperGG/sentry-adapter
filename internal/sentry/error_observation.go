package sentry

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"unicode"
)

// ErrorObservation is a small projection of a verified project error event.
// Request, user, breadcrumb and stack data never enter the Agent message.
type ErrorObservation struct {
	EventID        string `json:"event_id"`
	FailureStage   string `json:"failure_stage,omitempty"`
	ExceptionType  string `json:"exception_type,omitempty"`
	ExceptionValue string `json:"exception_value,omitempty"`
	ResourceFile   string `json:"resource_file,omitempty"`
	HTTPStatus     string `json:"http_status,omitempty"`
}

type exceptionValue struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

func (c *Client) GetErrorObservation(ctx context.Context, org, project, eventID string) (*ErrorObservation, error) {
	wantID := normalizeEventID(eventID)
	if org == "" || project == "" || wantID == "" || wantID == "00000000000000000000000000000000" {
		return nil, errors.New("invalid project error identity")
	}
	var event struct {
		ID       string         `json:"id"`
		EventID  string         `json:"eventID"`
		Type     string         `json:"type"`
		Metadata exceptionValue `json:"metadata"`
		Tags     []struct {
			Key   string          `json:"key"`
			Value json.RawMessage `json:"value"`
		} `json:"tags"`
		Entries []struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		} `json:"entries"`
	}
	path := "/api/0/projects/" + url.PathEscape(org) + "/" + url.PathEscape(project) + "/events/" + wantID + "/"
	if err := c.getJSON(ctx, path, &event); err != nil {
		return nil, err
	}
	gotID := event.EventID
	if gotID == "" {
		gotID = event.ID
	}
	if normalizeEventID(gotID) != wantID || event.Type != "error" {
		return nil, errors.New("Sentry did not return the requested error event")
	}
	observation := ErrorObservation{EventID: wantID}
	for _, tag := range event.Tags {
		var stage string
		if tag.Key == "failure_stage" && json.Unmarshal(tag.Value, &stage) == nil && diagnosticName.MatchString(stage) {
			observation.FailureStage = stage
			break
		}
	}
	exception := event.Metadata
	if exception.Type == "" || exception.Value == "" {
		for _, entry := range event.Entries {
			if entry.Type != "exception" {
				continue
			}
			var data struct{ Values []exceptionValue }
			if json.Unmarshal(entry.Data, &data) == nil && len(data.Values) > 0 {
				exception = data.Values[len(data.Values)-1]
				break
			}
		}
	}
	if diagnosticName.MatchString(exception.Type) {
		observation.ExceptionType = exception.Type
	}
	observation.ExceptionValue, observation.ResourceFile, observation.HTTPStatus = diagnosticMessage(exception.Value)
	if observation == (ErrorObservation{EventID: wantID}) {
		return nil, nil
	}
	return &observation, nil
}

var (
	diagnosticName  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_.-]{0,95}$`)
	statusInMessage = regexp.MustCompile(`(?i)\bHTTP(?:\s+(?:status|error))?[\s:]+([1-5][0-9]{2})\b`)
	// Only short alphabetic basenames of structured configuration resources
	// can survive a dynamic path. Skip generated, numbered or secret filenames.
	resourceInMessage = regexp.MustCompile(`(?i)(?:^|[/\\（(\s])([a-z]{2,16}(?:[-_][a-z]{2,12})?\.(?:cfg|json|ya?ml|xml|toml|ini|proto))(?:$|[?#）)\s,:，])`)
	privateFilename   = regexp.MustCompile(`(?i)auth|private|secret|token|key|cert|user|account|passwd|password|credential|session|env|signature`)
	privateMessage    = regexp.MustCompile(`(?i)authorization|bearer|basic\s+\S+|cookie|password|passwd|secret|token|api[_-]?key|signature|credential|\b(?:user(?:name)?|email|phone)\b|用户名|手机号|密码|凭据|密钥|[\w.+%-]+@[\w.-]+|[0-9]{7,}|[a-z0-9_-]{32,}`)
)

func diagnosticMessage(value string) (summary, filename, status string) {
	line, _, _ := strings.Cut(strings.TrimSpace(value), "\n")
	line, _, _ = strings.Cut(line, "\r")
	if m := statusInMessage.FindStringSubmatch(line); m != nil {
		status = m[1]
	}
	// Query parameters can contain credentials or unrelated resource names.
	pathPart, _, _ := strings.Cut(line, "?")
	if m := resourceInMessage.FindStringSubmatch(pathPart); m != nil && !privateFilename.MatchString(m[1]) {
		filename = m[1]
	}
	// Preserve the static leading description, excluding interpolated paths,
	// quoted values and trailing detail. Report the explicit HTTP code separately.
	if i := strings.IndexAny(line, "（([【{：:\"'`"); i >= 0 {
		line = line[:i]
	}
	words := strings.Fields(line)
	for i, word := range words {
		if strings.ContainsAny(word, "/\\?=@#<>") {
			words = words[:i]
			break
		}
	}
	summary = strings.Join(words, " ")
	if privateMessage.MatchString(summary) || strings.IndexFunc(summary, func(r rune) bool {
		return unicode.IsControl(r) || unicode.In(r, unicode.Cf)
	}) >= 0 {
		return "", filename, status
	}
	if runes := []rune(summary); len(runes) > 180 {
		summary = string(runes[:180]) + "…"
	}
	return summary, filename, status
}
