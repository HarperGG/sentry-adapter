package sentry

import (
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

type Client struct {
	base            *url.URL
	token           string
	http            *http.Client
	attachmentHosts map[string]bool
}

type APIError struct{ Status int }

func (e *APIError) Error() string   { return fmt.Sprintf("Sentry API returned HTTP %d", e.Status) }
func (e *APIError) Retryable() bool { return e.Status == 404 || e.Status == 429 || e.Status >= 500 }

type Attachment struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	MimeType string `json:"mimetype"`
	Size     int64  `json:"size"`
}

type Feedback struct {
	IsFeedback        bool
	IssueID           string
	EventID           string
	AttachmentEventID string
	Project           string
	Title             string
	Message           string
	Kind              string
	IssueURL          string
	Attachments       []Attachment
}

func New(baseURL, token string, attachmentRedirectHosts ...string) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return nil, errors.New("invalid Sentry base URL")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	hosts := map[string]bool{strings.ToLower(u.Hostname()): true}
	for _, host := range attachmentRedirectHosts {
		hosts[strings.ToLower(strings.TrimSpace(host))] = true
	}
	// Metadata requests never need to leave the configured Sentry API origin.
	// Attachment downloads use a separate client with an explicit redirect policy.
	metadataHTTP := &http.Client{Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	return &Client{base: u, token: token, http: metadataHTTP, attachmentHosts: hosts}, nil
}

type issueResponse struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	Permalink     string `json:"permalink"`
	IssueCategory string `json:"issueCategory"`
	Project       struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
	} `json:"project"`
}

type eventResponse struct {
	ID       string                     `json:"id"`
	EventID  string                     `json:"eventID"`
	Contexts map[string]json.RawMessage `json:"contexts"`
	Tags     []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	} `json:"tags"`
}

func (c *Client) GetFeedback(ctx context.Context, org, issueID, preferredEventID string) (Feedback, error) {
	var issue issueResponse
	if err := c.getJSON(ctx, "/api/0/organizations/"+url.PathEscape(org)+"/issues/"+url.PathEscape(issueID)+"/", &issue); err != nil {
		return Feedback{}, err
	}
	if !strings.EqualFold(issue.IssueCategory, "feedback") {
		return Feedback{IssueID: issueID, Project: issue.Project.Slug}, nil
	}
	eventID := preferredEventID
	if eventID == "" {
		eventID = "oldest"
	}
	var event eventResponse
	if err := c.getJSON(ctx, "/api/0/organizations/"+url.PathEscape(org)+"/issues/"+url.PathEscape(issueID)+"/events/"+url.PathEscape(eventID)+"/", &event); err != nil {
		return Feedback{}, err
	}
	if event.EventID != "" {
		eventID = event.EventID
	} else if event.ID != "" {
		eventID = event.ID
	}
	var details struct {
		Message      string `json:"message"`
		FeedbackType string `json:"feedback_type"`
		Type         string `json:"type"`
	}
	if raw, ok := event.Contexts["feedback"]; ok {
		_ = json.Unmarshal(raw, &details)
	}
	kind := strings.TrimSpace(details.FeedbackType)
	if kind == "" {
		for _, tag := range event.Tags {
			if tag.Key == "feedback_type" || tag.Key == "feedback.type" {
				kind = strings.TrimSpace(tag.Value)
				break
			}
		}
	}
	// Context "type" normally identifies the Sentry context itself ("feedback"),
	// not the application's feedback category. Keep legacy category values only.
	if kind == "" && !strings.EqualFold(strings.TrimSpace(details.Type), "feedback") {
		kind = strings.TrimSpace(details.Type)
	}
	f := Feedback{
		IsFeedback: true,
		IssueID:    issueID, EventID: eventID, Project: issue.Project.Slug,
		Title: issue.Title, Message: details.Message, Kind: kind,
		IssueURL: c.feedbackURL(org, issueID, issue.Project.Slug, issue.Project.ID, issue.Permalink),
	}
	if eventID == "" || eventID == "oldest" {
		return Feedback{}, errors.New("Sentry event has no ID")
	}
	return f, nil
}

func (c *Client) feedbackURL(org, issueID, projectSlug, projectID, permalink string) string {
	u := *c.base
	basePath := strings.TrimRight(c.base.Path, "/") + "/organizations/" + url.PathEscape(org)
	if projectSlug != "" && projectID != "" {
		// Match the feedback detail route used by this Sentry deployment.
		u.Path = basePath + "/issues/feedback/"
		u.RawQuery = url.Values{
			"feedbackSlug": {projectSlug + ":" + issueID},
			"project":      {projectID},
		}.Encode()
		return u.String()
	}
	if permalink != "" {
		p, err := url.Parse(permalink)
		if err == nil && p.IsAbs() && p.Scheme == c.base.Scheme && strings.EqualFold(p.Host, c.base.Host) && p.User == nil {
			return p.String()
		}
	}
	u.Path = basePath + "/issues/" + url.PathEscape(issueID) + "/"
	u.RawQuery = ""
	return u.String()
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	u := *c.base
	u.Path = strings.TrimRight(c.base.Path, "/") + strings.SplitN(path, "?", 2)[0]
	if i := strings.IndexByte(path, '?'); i >= 0 {
		u.RawQuery = path[i+1:]
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &APIError{Status: resp.StatusCode}
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(out)
}

// DownloadAttachment returns bounded image bytes. Sentry may redirect a
// download to object storage; only explicitly allowed HTTPS hosts are followed.
func (c *Client) DownloadAttachment(ctx context.Context, org, project, eventID string, a Attachment) ([]byte, error) {
	path := "/api/0/projects/" + url.PathEscape(org) + "/" + url.PathEscape(project) +
		"/events/" + url.PathEscape(eventID) + "/attachments/" + url.PathEscape(a.ID) + "/"
	u := *c.base
	u.Path = strings.TrimRight(c.base.Path, "/") + path
	u.RawQuery = "download=1"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(next *http.Request, via []*http.Request) error {
		if len(via) >= 3 || next.URL.User != nil || !c.attachmentHosts[strings.ToLower(next.URL.Hostname())] ||
			(next.URL.Scheme != "https" && !(next.URL.Host == c.base.Host && c.base.Scheme == "http")) {
			return errors.New("attachment redirect host is not allowed")
		}
		if !strings.EqualFold(next.URL.Host, c.base.Host) {
			next.Header.Del("Authorization")
		}
		return nil
	}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("Sentry attachment download failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &APIError{Status: resp.StatusCode}
	}
	if a.Size > 5<<20 || resp.ContentLength > 5<<20 {
		return nil, errors.New("screenshot exceeds 5 MiB limit")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (5<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 5<<20 {
		return nil, errors.New("screenshot exceeds 5 MiB limit")
	}
	actual := http.DetectContentType(data)
	if !(strings.HasPrefix(actual, "image/png") || strings.HasPrefix(actual, "image/jpeg") ||
		strings.HasPrefix(actual, "image/gif") || strings.HasPrefix(actual, "image/webp")) {
		return nil, errors.New("downloaded attachment is not a supported image")
	}
	return data, nil
}
