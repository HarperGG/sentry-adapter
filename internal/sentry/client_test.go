package sentry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestGetFeedbackReadsEventWithoutRequestingAttachments(t *testing.T) {
	const eventID = "0123456789abcdef0123456789abcdef"
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sentry-token" {
			t.Errorf("Authorization = %q", got)
		}
		mu.Lock()
		paths = append(paths, r.URL.RequestURI())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/0/organizations/acme/issues/42/":
			_, _ = io.WriteString(w, `{"id":"42","title":"用户反馈","issueCategory":"FEEDBACK","project":{"id":"5","slug":"web"}}`)
		case "/api/0/organizations/acme/issues/42/events/oldest/":
			_, _ = io.WriteString(w, `{"eventID":"`+eventID+`","contexts":{"feedback":{"message":"保存时页面空白","type":"bug"}}}`)
		default:
			t.Errorf("unexpected request: %s", r.URL.RequestURI())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := New(server.URL, "sentry-token")
	if err != nil {
		t.Fatal(err)
	}
	feedback, err := client.GetFeedback(context.Background(), "acme", "42", "")
	if err != nil {
		t.Fatal(err)
	}
	if !feedback.IsFeedback || feedback.IssueID != "42" || feedback.EventID != eventID || feedback.Project != "web" ||
		feedback.Title != "用户反馈" || feedback.Message != "保存时页面空白" || feedback.Kind != "bug" {
		t.Fatalf("unexpected feedback: %+v", feedback)
	}
	if feedback.IssueURL != server.URL+"/organizations/acme/issues/feedback/?feedbackSlug=web%3A42&project=5" {
		t.Errorf("IssueURL = %q", feedback.IssueURL)
	}
	if len(feedback.Attachments) != 0 || feedback.AttachmentEventID != "" {
		t.Errorf("unexpected attachment metadata: %+v", feedback)
	}
	mu.Lock()
	defer mu.Unlock()
	wantPaths := []string{
		"/api/0/organizations/acme/issues/42/",
		"/api/0/organizations/acme/issues/42/events/oldest/",
	}
	if !reflect.DeepEqual(paths, wantPaths) {
		t.Errorf("requests = %v, want %v", paths, wantPaths)
	}
}

func TestGetFeedbackIgnoresNonFeedbackIssue(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/0/organizations/acme/issues/42/" {
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "42", "issueCategory": "error", "project": map[string]string{"slug": "web"},
		})
	}))
	defer server.Close()
	client, err := New(server.URL, "sentry-token")
	if err != nil {
		t.Fatal(err)
	}
	feedback, err := client.GetFeedback(context.Background(), "acme", "42", "")
	if err != nil {
		t.Fatal(err)
	}
	if feedback.IsFeedback || feedback.IssueID != "42" || feedback.Project != "web" {
		t.Fatalf("unexpected non-feedback result: %+v", feedback)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("requests = %d, want only the issue lookup", got)
	}
}

func TestGetFeedbackDoesNotFollowMetadataRedirect(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		destinationCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL+"/redirected", http.StatusFound)
	}))
	defer server.Close()
	client, err := New(server.URL, "sentry-token")
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.GetFeedback(context.Background(), "acme", "42", "")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusFound {
		t.Fatalf("redirect error = %v, want HTTP 302", err)
	}
	if got := destinationCalls.Load(); got != 0 {
		t.Errorf("metadata redirect target received %d requests", got)
	}
}

func TestGetFeedbackIgnoresAssociatedEventAttachments(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.RequestURI())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/0/organizations/acme/issues/42/":
			_, _ = io.WriteString(w, `{"id":"42","issueCategory":"FEEDBACK","project":{"slug":"web"}}`)
		case "/api/0/organizations/acme/issues/42/events/oldest/":
			_, _ = io.WriteString(w, `{"eventID":"feedback-event","contexts":{"feedback":{"message":"截图说明","type":"platform_bug","associated_event_id":"error-event"}}}`)
		default:
			t.Errorf("unexpected request: %s", r.URL.RequestURI())
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, "token")
	if err != nil {
		t.Fatal(err)
	}
	f, err := client.GetFeedback(context.Background(), "acme", "42", "")
	if err != nil {
		t.Fatal(err)
	}
	if !f.IsFeedback || f.EventID != "feedback-event" || f.Kind != "platform_bug" || f.Message != "截图说明" {
		t.Fatalf("unexpected feedback: %+v", f)
	}
	if f.AttachmentEventID != "" || len(f.Attachments) != 0 {
		t.Fatalf("associated event attachments should not be fetched: %+v", f)
	}
	mu.Lock()
	defer mu.Unlock()
	wantPaths := []string{
		"/api/0/organizations/acme/issues/42/",
		"/api/0/organizations/acme/issues/42/events/oldest/",
	}
	if !reflect.DeepEqual(paths, wantPaths) {
		t.Errorf("requests = %v, want %v", paths, wantPaths)
	}
}

func TestDownloadAttachmentAcceptsImageAndBoundsResponse(t *testing.T) {
	const attachmentPath = "/api/0/projects/acme/web/events/event1/attachments/7/"
	image := append([]byte("\x89PNG\r\n\x1a\n"), []byte("test image")...)
	var oversized atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != attachmentPath || r.URL.Query().Get("download") != "1" {
			t.Errorf("unexpected download URL: %s", r.URL.RequestURI())
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sentry-token" {
			t.Errorf("Authorization = %q", got)
		}
		if oversized.Load() {
			_, _ = w.Write(bytes.Repeat([]byte{'x'}, (5<<20)+1))
			return
		}
		_, _ = w.Write(image)
	}))
	defer server.Close()
	client, err := New(server.URL, "sentry-token")
	if err != nil {
		t.Fatal(err)
	}
	a := Attachment{ID: "7", Name: "screen.png", MimeType: "image/png", Size: int64(len(image))}
	got, err := client.DownloadAttachment(context.Background(), "acme", "web", "event1", a)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, image) {
		t.Errorf("downloaded image differs from response")
	}

	oversized.Store(true)
	_, err = client.DownloadAttachment(context.Background(), "acme", "web", "event1", a)
	if err == nil || !strings.Contains(err.Error(), "5 MiB") {
		t.Errorf("oversized download error = %v", err)
	}
}

func TestDownloadAttachmentRejectsUntrustedRedirectHost(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		destinationCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer destination.Close()
	location := strings.Replace(destination.URL, "127.0.0.1", "localhost", 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, location+"/stolen", http.StatusFound)
	}))
	defer server.Close()
	client, err := New(server.URL, "sentry-token")
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.DownloadAttachment(context.Background(), "acme", "web", "event1", Attachment{ID: "7"})
	if err == nil {
		t.Fatal("untrusted redirect unexpectedly succeeded")
	}
	if got := destinationCalls.Load(); got != 0 {
		t.Errorf("untrusted redirect target received %d requests", got)
	}
}
