package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

const testSecret = "test-signing-secret"

func signed(body string) string {
	mac := hmac.New(sha256.New, []byte(testSecret))
	_, _ = mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

func TestParseIssueCreated(t *testing.T) {
	body := `{"action":"created","organization":{"slug":"org-a"},"project":"top-level","data":{"issue":{"id":12345,"issueCategory":"FEEDBACK","project":{"id":9,"slug":"app-a"},"permalink":"https://attacker.invalid/ignore"}}}`
	d, accepted, err := Parse([]byte(body), "issue", signed(body), testSecret)
	if err != nil || !accepted {
		t.Fatalf("Parse() = %+v, %v, %v", d, accepted, err)
	}
	if d.Organization != "org-a" || d.Project != "app-a" || d.IssueID != "12345" || d.EventID != "" || d.Resource != "issue" || d.Action != "created" || d.DedupKey != "issue:12345" {
		t.Fatalf("unexpected delivery: %+v", d)
	}
}

func TestParseRejectsInvalidSignatureBeforeJSON(t *testing.T) {
	body := []byte(`{not-json}`)
	_, accepted, err := Parse(body, "issue", strings.Repeat("0", 64), testSecret)
	if accepted || !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("Parse() = accepted %v, err %v; want invalid signature", accepted, err)
	}
	_, accepted, err = Parse(body, "issue", "not-hex", testSecret)
	if accepted || !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("Parse() = accepted %v, err %v; want invalid signature", accepted, err)
	}
}

func TestParseRequiresSecret(t *testing.T) {
	_, accepted, err := Parse([]byte(`{}`), "issue", "", "")
	if accepted || !errors.Is(err, ErrMissingSecret) {
		t.Fatalf("Parse() = accepted %v, err %v; want missing secret", accepted, err)
	}
}

func TestParseFiltersUnrelatedDeliveries(t *testing.T) {
	tests := []struct {
		name     string
		resource string
		body     string
	}{
		{"resource", "comment", `{"action":"created","data":{"issue":{"id":"1","issueCategory":"feedback"}}}`},
		{"action", "issue", `{"action":"resolved","data":{"issue":{"id":"1","issueCategory":"feedback"}}}`},
		{"other category", "issue", `{"action":"created","data":{"issue":{"id":"1","issueCategory":"error"}}}`},
		{"missing category", "issue", `{"action":"created","data":{"issue":{"id":"1"}}}`},
		{"other alert category", "event_alert", `{"action":"triggered","data":{"event":{"issue_id":"1","event_id":"evt","issueCategory":"error"}}}`},
		{"conflicting alert categories", "event_alert", `{"action":"triggered","data":{"issue":{"issueCategory":"error"},"event":{"issue_id":"1","event_id":"evt","issueCategory":"feedback"}}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, accepted, err := Parse([]byte(tt.body), tt.resource, signed(tt.body), testSecret)
			if err != nil || accepted || d != (Delivery{}) {
				t.Fatalf("Parse() = %+v, %v, %v; want filtered", d, accepted, err)
			}
		})
	}
}

func TestParseEventAlert(t *testing.T) {
	body := `{"action":"triggered","organization":"org-a","project":{"slug":"app-a"},"data":{"event":{"issue_id":"42","event_id":"evt-99"}}}`
	d, accepted, err := Parse([]byte(body), "event_alert", signed(body), testSecret)
	if err != nil || !accepted {
		t.Fatalf("Parse() = %+v, %v, %v", d, accepted, err)
	}
	if d.Organization != "org-a" || d.Project != "app-a" || d.IssueID != "42" || d.EventID != "evt-99" || d.DedupKey != "issue:42" {
		t.Fatalf("unexpected delivery: %+v", d)
	}
}

func TestParseEventAlertRuleFallback(t *testing.T) {
	body := `{"action":"triggered","data":{"event":{"issue_id":42,"issueCategory":"feedback"},"alert_rule":{"id":88}}}`
	d, accepted, err := Parse([]byte(body), "event_alert", signed(body), testSecret)
	if err != nil || !accepted {
		t.Fatalf("Parse() = %+v, %v, %v", d, accepted, err)
	}
	if d.IssueID != "42" || d.EventID != "" || d.DedupKey != "issue:42" {
		t.Fatalf("unexpected delivery: %+v", d)
	}
}

func TestParseMalformedAndIncomplete(t *testing.T) {
	tests := []struct {
		name     string
		resource string
		body     string
	}{
		{"invalid JSON", "issue", `{not-json}`},
		{"missing issue ID", "issue", `{"action":"created","data":{"issue":{"issueCategory":"feedback"}}}`},
		{"missing alert issue ID", "event_alert", `{"action":"triggered","data":{"event":{"event_id":"evt"}}}`},
		{"invalid issue ID type", "issue", `{"action":"created","data":{"issue":{"id":{},"issueCategory":"feedback"}}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, accepted, err := Parse([]byte(tt.body), tt.resource, signed(tt.body), testSecret)
			if accepted || !errors.Is(err, ErrInvalidPayload) {
				t.Fatalf("Parse() = accepted %v, err %v; want invalid payload", accepted, err)
			}
		})
	}
}

func TestParseBoundsJSON(t *testing.T) {
	tooLarge := []byte(strings.Repeat("x", MaxBodyBytes+1))
	_, accepted, err := Parse(tooLarge, "issue", "", testSecret)
	if accepted || !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("Parse() = accepted %v, err %v; want body too large", accepted, err)
	}
}
