// Package webhook verifies and parses Sentry webhook deliveries. It does not
// trust URLs or resource details supplied by the webhook for outbound requests.
package webhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const MaxBodyBytes = 1 << 20

var (
	ErrMissingSecret    = errors.New("webhook: missing signing secret")
	ErrInvalidSignature = errors.New("webhook: invalid signature")
	ErrBodyTooLarge     = errors.New("webhook: body too large")
	ErrInvalidPayload   = errors.New("webhook: invalid payload")
)

// Delivery contains only stable identifiers needed by the asynchronous worker.
// Issue category must still be checked against the Sentry API before task creation.
type Delivery struct {
	Organization string
	Project      string
	IssueID      string
	EventID      string
	Resource     string
	Action       string
	DedupKey     string
}

// Parse verifies the HMAC-SHA256 signature over the raw request body before
// parsing it. accepted=false means the signed webhook is unrelated to feedback.
// The caller must independently limit the HTTP request body before reading it.
func Parse(body []byte, resourceHeader, signature, secret string) (delivery Delivery, accepted bool, err error) {
	if secret == "" {
		return Delivery{}, false, ErrMissingSecret
	}
	if len(body) > MaxBodyBytes {
		return Delivery{}, false, ErrBodyTooLarge
	}
	provided, decodeErr := hex.DecodeString(strings.TrimSpace(signature))
	if decodeErr != nil || len(provided) != sha256.Size {
		return Delivery{}, false, ErrInvalidSignature
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	if !hmac.Equal(mac.Sum(nil), provided) {
		return Delivery{}, false, ErrInvalidSignature
	}

	resource := strings.ToLower(strings.TrimSpace(resourceHeader))
	if resource != "issue" && resource != "event_alert" {
		return Delivery{}, false, nil
	}

	var payload envelope
	if err := json.Unmarshal(body, &payload); err != nil {
		return Delivery{}, false, fmt.Errorf("%w: %v", ErrInvalidPayload, err)
	}
	action := strings.ToLower(strings.TrimSpace(payload.Action))
	delivery = Delivery{
		Organization: parseRef(payload.Organization),
		Project:      parseRef(payload.Project),
		Resource:     resource,
		Action:       action,
	}

	switch resource {
	case "issue":
		if action != "created" || !isFeedback(payload.Data.Issue.IssueCategory) {
			return Delivery{}, false, nil
		}
		delivery.IssueID = string(payload.Data.Issue.ID)
		if delivery.IssueID == "" {
			return Delivery{}, false, fmt.Errorf("%w: issue.created missing data.issue.id", ErrInvalidPayload)
		}
		if project := parseRef(payload.Data.Issue.Project); project != "" {
			delivery.Project = project
		}
		delivery.DedupKey = "issue:" + delivery.IssueID
		return delivery, true, nil
	case "event_alert":
		if action != "triggered" {
			return Delivery{}, false, nil
		}
		// Sentry alert deliveries may omit the issue category. In that case the
		// worker must fetch the issue and confirm its category before writing.
		eventCategory := strings.TrimSpace(payload.Data.Event.IssueCategory)
		issueCategory := strings.TrimSpace(payload.Data.Issue.IssueCategory)
		if (eventCategory != "" && !isFeedback(eventCategory)) ||
			(issueCategory != "" && !isFeedback(issueCategory)) {
			return Delivery{}, false, nil
		}
		delivery.IssueID = string(payload.Data.Event.IssueID)
		if delivery.IssueID == "" {
			return Delivery{}, false, fmt.Errorf("%w: event_alert.triggered missing data.event.issue_id", ErrInvalidPayload)
		}
		delivery.EventID = string(payload.Data.Event.EventID)
		if delivery.EventID == "" {
			delivery.EventID = string(payload.Data.Event.ID)
		}
		if project := parseRef(payload.Data.Event.Project); project != "" {
			delivery.Project = project
		}
		// One Teambition card per Sentry issue even if both issue and issue-alert
		// delivery paths are enabled later.
		delivery.DedupKey = "issue:" + delivery.IssueID
		return delivery, true, nil
	}
	return Delivery{}, false, nil
}

func isFeedback(category string) bool {
	return strings.EqualFold(strings.TrimSpace(category), "feedback")
}

// identifier accepts the string and integer forms used by Sentry IDs, while
// rejecting objects, negative numbers and fractional values.
type identifier string

func (id *identifier) UnmarshalJSON(raw []byte) error {
	if bytes.Equal(raw, []byte("null")) {
		*id = ""
		return nil
	}
	if len(raw) == 0 {
		return errors.New("empty identifier")
	}
	if raw[0] == '"' {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		*id = identifier(strings.TrimSpace(value))
		return nil
	}
	for _, digit := range raw {
		if digit < '0' || digit > '9' {
			return errors.New("identifier must be a string or nonnegative integer")
		}
	}
	*id = identifier(raw)
	return nil
}

type envelope struct {
	Action       string          `json:"action"`
	Organization json.RawMessage `json:"organization"`
	Project      json.RawMessage `json:"project"`
	Data         struct {
		Issue struct {
			ID            identifier      `json:"id"`
			IssueCategory string          `json:"issueCategory"`
			Project       json.RawMessage `json:"project"`
		} `json:"issue"`
		Event struct {
			ID            identifier      `json:"id"`
			EventID       identifier      `json:"event_id"`
			IssueID       identifier      `json:"issue_id"`
			IssueCategory string          `json:"issueCategory"`
			Project       json.RawMessage `json:"project"`
		} `json:"event"`
		AlertRule struct {
			ID identifier `json:"id"`
		} `json:"alert_rule"`
		Rule struct {
			ID identifier `json:"id"`
		} `json:"rule"`
	} `json:"data"`
}

func parseRef(raw json.RawMessage) string {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var name string
	if err := json.Unmarshal(raw, &name); err == nil {
		return strings.TrimSpace(name)
	}
	var ref struct {
		ID   identifier `json:"id"`
		Slug string     `json:"slug"`
	}
	if err := json.Unmarshal(raw, &ref); err == nil {
		if ref.Slug != "" {
			return strings.TrimSpace(ref.Slug)
		}
		return string(ref.ID)
	}
	var id identifier
	if err := json.Unmarshal(raw, &id); err == nil {
		return string(id)
	}
	return ""
}
