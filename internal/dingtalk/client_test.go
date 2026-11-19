package dingtalk

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const sentryOrigin = "https://sentry.example.com"
const feedbackLink = sentryOrigin + "/organizations/acme/issues/feedback/?feedbackSlug=annotation%3A42&project=5"

func TestSendUsesSanitizedMarkdown(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json; charset=utf-8" {
			t.Errorf("unexpected request method or content type: %s %s", r.Method, r.Header.Get("Content-Type"))
		}
		var body struct {
			robotMessage
			At json.RawMessage `json:"at"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if body.Type != "markdown" || !strings.Contains(body.Markdown.Title, keyword) || len(body.At) != 0 {
			t.Errorf("unexpected message type, title, or mentions: %+v", body)
		}
		for _, want := range []string{
			keyword, "**Issue** 42", `保存\*操作\*失败`, "**初步原因** 读取失败", "**分类** 平台异常",
			"**置信度** 75/100", "**下一步** 核对请求", `\[伪造链接\]`, "（凭据已隐藏）", "（链接已省略）",
			"[查看 Sentry 反馈](" + feedbackLink + ")",
		} {
			if !strings.Contains(body.Markdown.Text, want) {
				t.Errorf("message is missing %q: %s", want, body.Markdown.Text)
			}
		}
		for _, unwanted := range []string{"@", "https://untrusted.example", "test-credential", "\n# 伪造标题", "<img>"} {
			if strings.Contains(body.Markdown.Text, unwanted) {
				t.Errorf("unsanitized text %q in message: %s", unwanted, body.Markdown.Text)
			}
		}
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer server.Close()
	client := testClient(t, server)
	score := 75
	err := client.Send(context.Background(), Report{
		IssueID:         "42",
		Problem:         "保存*操作*失败\n# 伪造标题 @all <img>",
		Cause:           "读取失败，Authorization: Bearer test-credential",
		Category:        "平台异常",
		ConfidenceScore: &score,
		NextStep:        "核对请求 [伪造链接](https://untrusted.example/)",
		SentryURL:       feedbackLink,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
}

func TestSendResponseOutcomes(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		response string
		reject   bool
		retry    bool
	}{
		{"robot rejection", 200, `{"errcode":310000,"errmsg":"do not expose this text"}`, true, false},
		{"robot rate limit", 200, `{"errcode":130101}`, true, true},
		{"HTTP rejection", 403, ``, true, false},
		{"HTTP rate limit", 429, ``, true, true},
		{"gateway error", 502, `{"errcode":0}`, false, false},
		{"HTTP timeout", 408, ``, false, false},
		{"missing errcode", 200, `{"errmsg":"ok"}`, false, false},
		{"null errcode", 200, `{"errcode":null}`, false, false},
		{"string errcode", 200, `{"errcode":"0"}`, false, false},
		{"wrong errcode case", 200, `{"ERRCODE":0}`, false, false},
		{"malformed response", 200, `{"errcode":0`, false, false},
		{"trailing response data", 200, `{"errcode":0} {}`, false, false},
		{"empty response", 204, ``, false, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.response)
			}))
			defer server.Close()
			err := testClient(t, server).Send(context.Background(), Report{IssueID: "42", SentryURL: feedbackLink})
			var rejected *Rejected
			var uncertain *Uncertain
			if test.reject && !errors.As(err, &rejected) || !test.reject && !errors.As(err, &uncertain) {
				t.Fatalf("unexpected outcome (%T): %v", err, err)
			}
			if rejected != nil && rejected.Retryable() != test.retry {
				t.Errorf("retryable = %t, want %t", rejected.Retryable(), test.retry)
			}
			if strings.Contains(err.Error(), "do not expose this text") {
				t.Errorf("response details escaped into error: %v", err)
			}
		})
	}
}

func TestSendDoesNotForwardRedirectOrUseUnverifiedLink(t *testing.T) {
	var requests, forwarded atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		forwarded.Add(1)
		_, _ = io.WriteString(w, `{"errcode":0}`)
	}))
	defer target.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client := testClient(t, server)
	var rejected *Rejected
	if err := client.Send(context.Background(), Report{IssueID: "42", SentryURL: "https://untrusted.example/42/"}); !errors.As(err, &rejected) {
		t.Fatalf("unverified link outcome: %v", err)
	}
	if requests.Load() != 0 {
		t.Fatal("request sent for unverified link")
	}
	var uncertain *Uncertain
	if err := client.Send(context.Background(), Report{IssueID: "42", SentryURL: feedbackLink}); !errors.As(err, &uncertain) {
		t.Fatalf("redirect outcome: %v", err)
	}
	if requests.Load() != 1 || forwarded.Load() != 0 {
		t.Fatalf("webhook requests %d, forwarded %d", requests.Load(), forwarded.Load())
	}
}

func TestTransportErrorKeepsWebhookCredentialOutOfError(t *testing.T) {
	client, err := New("https://robot.example.com/send?access_token=test-webhook-credential", sentryOrigin)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = client.Send(ctx, Report{IssueID: "42", SentryURL: feedbackLink})
	var uncertain *Uncertain
	if !errors.As(err, &uncertain) || strings.Contains(err.Error(), "test-webhook-credential") {
		t.Fatalf("unredacted or unexpected error: %v", err)
	}
}

func testClient(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	client, err := New(server.URL+"/robot/send?access_token=test-token", sentryOrigin)
	if err != nil {
		t.Fatalf("create test client: %v", err)
	}
	client.http.Transport = server.Client().Transport
	return client
}
