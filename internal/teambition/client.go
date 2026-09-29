package teambition

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Card is the adapter's internal model, independent of Teambition's JSON schema.
type Card struct {
	SourceIssueID string
	Title         string
	Description   string
	Kind          string // bug or feature
	Images        []Image
	preparedFiles []preparedFile
	preparedFor   string
}

type preparedFile struct {
	name  string
	token string
}

type Image struct {
	Name     string
	MimeType string
	Data     []byte
}

type Creator interface {
	Prepare(context.Context, Card) (Card, error)
	Create(context.Context, Card) (string, error)
}

// Uncertain means the request may have created a task. The worker must not
// automatically send a second create request.
type Uncertain struct{ Cause error }

func (e *Uncertain) Error() string {
	return "Teambition create outcome is uncertain: " + e.Cause.Error()
}
func (e *Uncertain) Unwrap() error { return e.Cause }

type Permanent struct{ Cause error }

func (e *Permanent) Error() string { return e.Cause.Error() }
func (e *Permanent) Unwrap() error { return e.Cause }

var ErrImagesUnsupported = errors.New("Teambition image upload protocol is not configured; card was not created")

type Config struct {
	BaseURL, TaskPath, UploadTokenPath        string
	AuthMode, TokenURL                        string
	AppID, AppSecret                          string
	TenantID, OperatorID, ProjectID           string
	TasklistID, StageID, StatusID, ExecutorID string
	BugTypeID, FeatureTypeID                  string
	CustomFieldID, CustomFieldOptionID        string
	CustomFieldOptionTitle                    string
	AttachmentFieldID                         string
	UploadAllowedOrigins                      []string
}

type Client struct {
	cfg       Config
	http      *http.Client
	mu        sync.Mutex
	token     string
	expiresAt time.Time
	now       func() time.Time
}

func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return nil, errors.New("invalid Teambition base URL")
	}
	if cfg.BugTypeID == "" || cfg.FeatureTypeID == "" {
		return nil, errors.New("both Teambition bug and feature scenario IDs are required")
	}
	if (cfg.CustomFieldID != "" || cfg.CustomFieldOptionID != "" || cfg.CustomFieldOptionTitle != "") &&
		(cfg.CustomFieldID == "" || cfg.CustomFieldOptionID == "" || cfg.CustomFieldOptionTitle == "") {
		return nil, errors.New("Teambition custom field ID, option ID, and option title must be configured together")
	}
	if cfg.AppID == "" || cfg.AppSecret == "" {
		return nil, errors.New("Teambition AppID and AppSecret are required")
	}
	if cfg.AuthMode == "" {
		cfg.AuthMode = "app_token"
	}
	switch cfg.AuthMode {
	case "app_token":
		if cfg.TokenURL == "" {
			tokenURL := *u
			tokenURL.Path = "/gateway/appToken"
			tokenURL.RawPath = ""
			tokenURL.RawQuery = ""
			tokenURL.Fragment = ""
			cfg.TokenURL = tokenURL.String()
		}
		if !validTokenURL(cfg.TokenURL) {
			return nil, errors.New("Teambition appToken URL must be an absolute HTTPS URL without credentials, query, or fragment")
		}
	case "local_jwt":
	default:
		return nil, errors.New("invalid Teambition authentication mode")
	}
	return &Client{cfg: cfg, http: &http.Client{
		Timeout:       20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, now: time.Now}, nil
}

func (c *Client) Create(ctx context.Context, card Card) (string, error) {
	if len(card.Images) > 0 && len(card.preparedFiles) == 0 {
		var err error
		card, err = c.Prepare(ctx, card)
		if err != nil {
			return "", err
		}
	}
	if len(card.preparedFiles) > 0 && (len(card.preparedFiles) != len(card.Images) || card.preparedFor != c.cfg.ProjectID) {
		return "", &Permanent{errors.New("prepared Teambition images do not match this card and project")}
	}
	scenarioID := c.cfg.BugTypeID
	if card.Kind == "feature" {
		scenarioID = c.cfg.FeatureTypeID
	} else if card.Kind != "bug" {
		return "", &Permanent{fmt.Errorf("unsupported feedback kind %q", card.Kind)}
	}
	token, err := c.accessToken(ctx)
	if err != nil {
		return "", err
	}
	request := map[string]any{
		"projectId":             c.cfg.ProjectID,
		"content":               card.Title,
		"note":                  card.Description,
		"involveMembers":        []string{c.cfg.OperatorID},
		"objectType":            "task",
		"scenariofieldconfigId": scenarioID,
	}
	for key, value := range map[string]string{
		"tasklistId": c.cfg.TasklistID, "stageId": c.cfg.StageID,
		"taskflowstatusId": c.cfg.StatusID, "executorId": c.cfg.ExecutorID,
	} {
		if value != "" {
			request[key] = value
		}
	}
	customFields := make([]any, 0, 2)
	if c.cfg.CustomFieldID != "" {
		customFields = append(customFields, map[string]any{
			"cfId": c.cfg.CustomFieldID,
			"value": []any{map[string]string{
				"id": c.cfg.CustomFieldOptionID, "title": c.cfg.CustomFieldOptionTitle,
			}},
		})
	}
	if len(card.preparedFiles) > 0 {
		values := make([]map[string]string, 0, len(card.preparedFiles))
		for _, file := range card.preparedFiles {
			meta, _ := json.Marshal(map[string]string{"fileToken": file.token})
			values = append(values, map[string]string{"title": file.name, "metaString": string(meta)})
		}
		customFields = append(customFields, map[string]any{"cfId": c.cfg.AttachmentFieldID, "value": values})
	}
	if len(customFields) > 0 {
		request["customfields"] = customFields
	}
	body, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	return c.postTask(ctx, body, token)
}

// Prepare uploads screenshot bytes before the task create request. If it fails,
// retrying is safe because no task has been created yet.
func (c *Client) Prepare(ctx context.Context, card Card) (Card, error) {
	if card.Kind != "bug" && card.Kind != "feature" {
		return Card{}, &Permanent{fmt.Errorf("unsupported feedback kind %q", card.Kind)}
	}
	if len(card.Images) == 0 {
		_, err := c.accessToken(ctx)
		return card, err
	}
	if len(card.preparedFiles) > 0 {
		if len(card.preparedFiles) != len(card.Images) || card.preparedFor != c.cfg.ProjectID {
			return Card{}, &Permanent{errors.New("prepared Teambition images do not match this card and project")}
		}
		return card, nil
	}
	if c.cfg.AttachmentFieldID == "" || c.cfg.UploadTokenPath == "" || c.cfg.ProjectID == "" || len(c.cfg.UploadAllowedOrigins) == 0 {
		return Card{}, &Permanent{ErrImagesUnsupported}
	}
	allowed, err := uploadOrigins(c.cfg.UploadAllowedOrigins)
	if err != nil {
		return Card{}, &Permanent{err}
	}
	type validatedImage struct {
		name, mimeType string
		data           []byte
	}
	images := make([]validatedImage, 0, len(card.Images))
	for _, image := range card.Images {
		mimeType, err := imageMIME(image)
		if err != nil {
			return Card{}, &Permanent{err}
		}
		images = append(images, validatedImage{safeImageName(image.Name, mimeType), mimeType, image.Data})
	}
	token, err := c.accessToken(ctx)
	if err != nil {
		return Card{}, err
	}
	files := make([]preparedFile, 0, len(images))
	for _, image := range images {
		ticket, err := c.requestUploadTicket(ctx, image.name, image.mimeType, len(image.data), token)
		if err != nil {
			return Card{}, err
		}
		if err := c.putImage(ctx, ticket.uploadURL, image.mimeType, image.data, allowed); err != nil {
			return Card{}, err
		}
		files = append(files, preparedFile{name: image.name, token: ticket.fileToken})
	}
	card.preparedFiles = files
	card.preparedFor = c.cfg.ProjectID
	return card, nil
}

type uploadTicket struct {
	fileToken string
	uploadURL string
}

func (c *Client) requestUploadTicket(ctx context.Context, name, mimeType string, size int, token string) (uploadTicket, error) {
	u, err := c.endpoint(c.cfg.UploadTokenPath)
	if err != nil {
		return uploadTicket{}, &Permanent{err}
	}
	body, _ := json.Marshal(map[string]any{
		"scope":    "project:" + c.cfg.ProjectID + "/task/attachment",
		"fileSize": size, "fileType": mimeType, "fileName": name, "category": "attachment",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return uploadTicket{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	setBusinessHeaders(req, c.cfg, token)
	resp, err := c.http.Do(req)
	if err != nil {
		return uploadTicket{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("Teambition upload token returned HTTP %d", resp.StatusCode)
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized ||
			resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound ||
			(resp.StatusCode >= 300 && resp.StatusCode < 400) {
			return uploadTicket{}, &Permanent{err}
		}
		return uploadTicket{}, err
	}
	var out struct {
		Result struct {
			Token     string `json:"token"`
			UploadURL string `json:"uploadUrl"`
		} `json:"result"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return uploadTicket{}, errors.New("decode Teambition upload token response failed")
	}
	if out.Result.Token == "" || out.Result.UploadURL == "" {
		return uploadTicket{}, errors.New("Teambition upload token response is incomplete")
	}
	return uploadTicket{fileToken: out.Result.Token, uploadURL: out.Result.UploadURL}, nil
}

func (c *Client) putImage(ctx context.Context, rawURL, mimeType string, data []byte, allowed map[string]bool) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Opaque != "" || u.User != nil || u.Hostname() == "" || u.Fragment != "" || !allowed[strings.ToLower(u.Scheme+"://"+u.Host)] {
		return &Permanent{errors.New("Teambition upload URL origin is not allowed")}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), bytes.NewReader(data))
	if err != nil {
		return &Permanent{errors.New("invalid Teambition upload URL")}
	}
	req.Header.Set("Content-Type", mimeType)
	client := &http.Client{Timeout: c.http.Timeout, Transport: c.http.Transport, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("Teambition image upload request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Teambition image upload returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func uploadOrigins(values []string) (map[string]bool, error) {
	allowed := make(map[string]bool, len(values))
	for _, value := range values {
		u, err := url.Parse(strings.TrimSpace(value))
		if err != nil || u.Opaque != "" || u.User != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") ||
			(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return nil, errors.New("invalid Teambition upload allowed origin")
		}
		allowed[strings.ToLower(u.Scheme+"://"+u.Host)] = true
	}
	return allowed, nil
}

func imageMIME(image Image) (string, error) {
	if len(image.Data) == 0 || len(image.Data) > 5<<20 {
		return "", errors.New("Teambition screenshot must be between 1 byte and 5 MiB")
	}
	mimeType, _, err := mime.ParseMediaType(image.MimeType)
	if err != nil {
		return "", errors.New("invalid screenshot MIME type")
	}
	mimeType = strings.ToLower(mimeType)
	if mimeType == "image/jpg" {
		mimeType = "image/jpeg"
	}
	switch mimeType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return "", errors.New("unsupported screenshot MIME type")
	}
	if http.DetectContentType(image.Data) != mimeType {
		return "", errors.New("screenshot MIME type does not match image bytes")
	}
	return mimeType, nil
}

func safeImageName(name, mimeType string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '/' || r == '\\' {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name != "" && name != "." && name != ".." {
		return name
	}
	ext := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp"}[mimeType]
	return "screenshot" + ext
}

func setBusinessHeaders(req *http.Request, cfg Config, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Tenant-Id", cfg.TenantID)
	req.Header.Set("X-Tenant-Type", "organization")
	req.Header.Set("x-operator-id", cfg.OperatorID)
}

func (c *Client) postTask(ctx context.Context, body []byte, token string) (string, error) {
	u, err := c.endpoint(c.cfg.TaskPath)
	if err != nil {
		return "", &Permanent{err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return "", &Permanent{err}
	}
	req.Header.Set("Content-Type", "application/json")
	setBusinessHeaders(req, c.cfg, token)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", &Uncertain{err}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return "", &Permanent{errors.New("Teambition rejected app token")}
	}
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
		return "", &Permanent{fmt.Errorf("Teambition create returned HTTP %d", resp.StatusCode)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &Uncertain{fmt.Errorf("Teambition create returned HTTP %d", resp.StatusCode)}
	}
	var out struct {
		Result struct {
			ID     string `json:"id"`
			TaskID string `json:"taskId"`
		} `json:"result"`
		Code         *float64 `json:"code"`
		ErrorMessage string   `json:"errorMessage"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", &Uncertain{fmt.Errorf("decode Teambition response: %w", err)}
	}
	id := out.Result.TaskID
	if id == "" {
		id = out.Result.ID
	}
	if id == "" || out.ErrorMessage != "" || (out.Code != nil && *out.Code != 0 && *out.Code != 200) {
		return "", &Uncertain{errors.New("Teambition did not confirm a task ID")}
	}
	return id, nil
}

// accessToken caches the online appToken, or signs the enterprise JWT when
// explicitly configured for the private gateway's local JWT mode.
func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.token != "" && c.expiresAt.Sub(now) > 100*time.Second {
		return c.token, nil
	}
	if c.cfg.AuthMode == "app_token" {
		return c.exchangeAppToken(ctx)
	}
	return c.signLocalJWT(now)
}

func (c *Client) exchangeAppToken(ctx context.Context) (string, error) {
	body, _ := json.Marshal(struct {
		AppID     string `json:"appId"`
		AppSecret string `json:"appSecret"`
	}{AppID: c.cfg.AppID, AppSecret: c.cfg.AppSecret})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.TokenURL, bytes.NewReader(body))
	if err != nil {
		return "", &Permanent{errors.New("invalid Teambition appToken request")}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", errors.New("Teambition appToken request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		statusErr := fmt.Errorf("Teambition appToken returned HTTP %d", resp.StatusCode)
		if (resp.StatusCode >= 300 && resp.StatusCode < 400) ||
			(resp.StatusCode >= 400 && resp.StatusCode < 500 &&
				resp.StatusCode != http.StatusRequestTimeout && resp.StatusCode != http.StatusTooManyRequests) {
			return "", &Permanent{statusErr}
		}
		return "", statusErr
	}
	var out struct {
		AppToken string `json:"appToken"`
		Expire   int64  `json:"expire"`
		Result   struct {
			AppToken string `json:"appToken"`
			Expire   int64  `json:"expire"`
		} `json:"result"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", &Permanent{errors.New("invalid Teambition appToken response")}
	}
	token, expire := out.AppToken, out.Expire
	if token == "" {
		token, expire = out.Result.AppToken, out.Result.Expire
	}
	if token == "" || expire <= 0 {
		return "", &Permanent{errors.New("incomplete Teambition appToken response")}
	}
	// Bound a server-supplied lifetime so a malformed response cannot keep a
	// token cached indefinitely (or overflow time.Duration).
	if expire > 24*60*60 {
		expire = 24 * 60 * 60
	}
	c.token = token
	c.expiresAt = c.now().Add(time.Duration(expire) * time.Second)
	return token, nil
}

func (c *Client) signLocalJWT(now time.Time) (string, error) {
	// Unix timestamps are required by JWT. Taking one clock reading keeps iat
	// and exp in sync at a second boundary.
	iat := now.Unix()
	payload, err := json.Marshal(struct {
		Exp   int64  `json:"exp"`
		Iat   int64  `json:"iat"`
		AppID string `json:"_appId"`
	}{Exp: iat + int64((2 * time.Hour).Seconds()), Iat: iat, AppID: c.cfg.AppID})
	if err != nil {
		return "", err
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	claims := base64.RawURLEncoding.EncodeToString(payload)
	message := header + "." + claims
	mac := hmac.New(sha256.New, []byte(c.cfg.AppSecret))
	_, _ = mac.Write([]byte(message))
	c.token = message + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	c.expiresAt = time.Unix(iat+int64((2*time.Hour).Seconds()), 0)
	return c.token, nil
}

func validTokenURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil &&
		u.Opaque == "" && u.RawQuery == "" && u.Fragment == ""
}

func (c *Client) endpoint(path string) (string, error) {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return "", errors.New("Teambition API path must be absolute")
	}
	u, err := url.Parse(c.cfg.BaseURL)
	if err != nil {
		return "", err
	}
	u.Path = path
	u.RawQuery = ""
	return u.String(), nil
}

type Mock struct {
	mu    sync.Mutex
	cards map[string]Card
}

func NewMock() *Mock                                               { return &Mock{cards: make(map[string]Card)} }
func (m *Mock) Prepare(_ context.Context, card Card) (Card, error) { return card, nil }
func (m *Mock) Create(_ context.Context, card Card) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cards[card.SourceIssueID] = card
	return "mock-" + card.SourceIssueID, nil
}
func (m *Mock) Get(issueID string) (Card, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	card, ok := m.cards[issueID]
	return card, ok
}
