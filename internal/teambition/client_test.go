package teambition

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func assertBearerJWT(t *testing.T, authorization, appID, secret string) {
	t.Helper()
	const prefix = "Bearer "
	if !strings.HasPrefix(authorization, prefix) {
		t.Fatal("missing Bearer JWT")
	}
	parts := strings.Split(strings.TrimPrefix(authorization, prefix), ".")
	if len(parts) != 3 {
		t.Fatal("Bearer token is not a three-part JWT")
	}
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode JWT header: %v", err)
	}
	var hdr map[string]string
	if err := json.Unmarshal(header, &hdr); err != nil || hdr["alg"] != "HS256" || hdr["typ"] != "JWT" {
		t.Fatalf("JWT header invalid: %v, %v", hdr, err)
	}
	claimsBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode JWT claims: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(claimsBytes, &claims); err != nil {
		t.Fatalf("decode JWT claims JSON: %v", err)
	}
	if len(claims) != 3 || claims["_appId"] != appID {
		t.Fatalf("JWT claims missing exact _appId field")
	}
	iat, iatOK := claims["iat"].(float64)
	exp, expOK := claims["exp"].(float64)
	if !iatOK || !expOK || exp-iat != 7200 {
		t.Fatalf("JWT iat/exp are not Unix seconds with two-hour lifetime")
	}
	gotSignature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode JWT signature: %v", err)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(gotSignature, mac.Sum(nil)) {
		t.Fatal("JWT signature is not HMAC-SHA256 with configured AppSecret")
	}
}

func TestJWTCacheRefreshBoundary(t *testing.T) {
	client, err := New(Config{
		BaseURL: "https://teambition.example", TaskPath: "/gateway/v3/task/create",
		AuthMode: "local_jwt", AppID: "app-id", AppSecret: "app-secret", BugTypeID: "bug", FeatureTypeID: "feature",
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0)
	client.now = func() time.Time { return now }
	first, err := client.accessToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertBearerJWT(t, "Bearer "+first, "app-id", "app-secret")
	now = now.Add(2*time.Hour - 101*time.Second)
	stillCached, err := client.accessToken(context.Background())
	if err != nil || stillCached != first {
		t.Fatal("JWT was not reused while more than 100 seconds remained")
	}
	now = now.Add(time.Second)
	refreshed, err := client.accessToken(context.Background())
	if err != nil || refreshed == first {
		t.Fatal("JWT was not refreshed when 100 seconds remained")
	}
	assertBearerJWT(t, "Bearer "+refreshed, "app-id", "app-secret")
}

func TestCreateStopsAfterUnauthorized(t *testing.T) {
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/gateway/v3/task/create" {
			t.Errorf("unexpected online token request: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		assertBearerJWT(t, r.Header.Get("Authorization"), "app-id", "app-secret")
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer api.Close()
	client, err := New(Config{
		BaseURL: api.URL, TaskPath: "/gateway/v3/task/create",
		AuthMode: "local_jwt", AppID: "app-id", AppSecret: "app-secret", ProjectID: "project",
		BugTypeID: "bug", FeatureTypeID: "feature",
	})
	if err != nil {
		t.Fatal(err)
	}
	id, err := client.Create(context.Background(), Card{Kind: "bug", Title: "Feedback", SourceIssueID: "1"})
	var permanent *Permanent
	if id != "" || !errors.As(err, &permanent) || calls.Load() != 1 {
		t.Fatalf("Create after 401 = ID %q, error %v, calls %d; want one request and permanent error", id, err, calls.Load())
	}
}

func TestUploadTicketStopsAfterUnauthorized(t *testing.T) {
	image := append([]byte("\x89PNG\r\n\x1a\n"), []byte("screenshot")...)
	var ticketCalls, taskCalls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v3/awos/upload-token":
			assertBearerJWT(t, r.Header.Get("Authorization"), "app-id", "app-secret")
			ticketCalls.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
		case "/v3/task/create":
			taskCalls.Add(1)
			_, _ = w.Write([]byte(`{"result":{"id":"unexpected"}}`))
		default:
			t.Errorf("unexpected online token request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	client, err := New(Config{
		BaseURL: api.URL, TaskPath: "/v3/task/create", UploadTokenPath: "/v3/awos/upload-token",
		AuthMode: "local_jwt", AppID: "app-id", AppSecret: "app-secret", ProjectID: "project",
		BugTypeID: "bug", FeatureTypeID: "feature", AttachmentFieldID: "file-field",
		UploadAllowedOrigins: []string{api.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Prepare(context.Background(), Card{Kind: "bug", Images: []Image{{Name: "screen.png", MimeType: "image/png", Data: image}}})
	var permanent *Permanent
	if !errors.As(err, &permanent) || ticketCalls.Load() != 1 || taskCalls.Load() != 0 {
		t.Fatalf("Prepare after 401 = error %v, ticket calls %d, task calls %d; want one ticket request and permanent error", err, ticketCalls.Load(), taskCalls.Load())
	}
}

func TestCreateSignsJWTLocallyAndReusesItForTwoTasks(t *testing.T) {
	type taskRequest struct {
		ProjectID             string   `json:"projectId"`
		Content               string   `json:"content"`
		Note                  string   `json:"note"`
		InvolveMembers        []string `json:"involveMembers"`
		ObjectType            string   `json:"objectType"`
		ScenarioFieldConfigID string   `json:"scenariofieldconfigId"`
		CustomFields          []struct {
			CfID  string `json:"cfId"`
			Value []struct {
				ID    string `json:"id"`
				Title string `json:"title"`
			} `json:"value"`
		} `json:"customfields"`
	}
	var taskCalls atomic.Int32
	var mu sync.Mutex
	var tasks []taskRequest
	var tokens []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("method/content type = %s/%q", r.Method, r.Header.Get("Content-Type"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v3/task/create":
			call := taskCalls.Add(1)
			authorization := r.Header.Get("Authorization")
			assertBearerJWT(t, authorization, "app-id", "app-secret")
			if got := r.Header.Get("X-Tenant-Id"); got != "tenant-id" {
				t.Errorf("X-Tenant-Id = %q", got)
			}
			if got := r.Header.Get("X-Tenant-Type"); got != "organization" {
				t.Errorf("X-Tenant-Type = %q", got)
			}
			if got := r.Header.Get("X-Operator-Id"); got != "operator-id" {
				t.Errorf("X-Operator-Id = %q", got)
			}
			var payload map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("decode task request: %v", err)
			}
			if _, exists := payload["noteRenderMode"]; exists {
				t.Error("task request included unsupported noteRenderMode")
			}
			encoded, _ := json.Marshal(payload)
			var task taskRequest
			if err := json.Unmarshal(encoded, &task); err != nil {
				t.Errorf("decode task payload: %v", err)
			}
			mu.Lock()
			tasks = append(tasks, task)
			tokens = append(tokens, authorization)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]string{"id": "task-" + string(rune('0'+call))}})
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := New(Config{
		BaseURL: server.URL, TaskPath: "/v3/task/create",
		AuthMode: "local_jwt", AppID: "app-id", AppSecret: "app-secret", TenantID: "tenant-id", OperatorID: "operator-id",
		ProjectID: "project-id", BugTypeID: "bug-type", FeatureTypeID: "feature-type",
		CustomFieldID: "platform-field", CustomFieldOptionID: "platform-option", CustomFieldOptionTitle: "标注平台",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		card Card
		want string
	}{
		{Card{SourceIssueID: "sentry-1", Title: "保存失败", Description: "点击保存后空白", Kind: "bug"}, "task-1"},
		{Card{SourceIssueID: "sentry-2", Title: "支持导出", Description: "希望支持 CSV", Kind: "feature"}, "task-2"},
	} {
		id, err := client.Create(context.Background(), tc.card)
		if err != nil || id != tc.want {
			t.Fatalf("Create(%q) = %q, %v; want %q", tc.card.Kind, id, err, tc.want)
		}
	}
	if got := taskCalls.Load(); got != 2 {
		t.Errorf("task create calls = %d, want 2", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(tasks) != 2 {
		t.Fatalf("task payload count = %d", len(tasks))
	}
	if tokens[0] != tokens[1] {
		t.Error("local JWT was not cached across two create requests")
	}
	for i, wantType := range []string{"bug-type", "feature-type"} {
		if tasks[i].ProjectID != "project-id" || tasks[i].ObjectType != "task" ||
			len(tasks[i].InvolveMembers) != 1 || tasks[i].InvolveMembers[0] != "operator-id" ||
			tasks[i].ScenarioFieldConfigID != wantType {
			t.Errorf("task %d payload = %+v", i, tasks[i])
		}
		if len(tasks[i].CustomFields) != 1 || tasks[i].CustomFields[0].CfID != "platform-field" ||
			len(tasks[i].CustomFields[0].Value) != 1 || tasks[i].CustomFields[0].Value[0].ID != "platform-option" ||
			tasks[i].CustomFields[0].Value[0].Title != "标注平台" {
			t.Errorf("task %d custom fields = %+v", i, tasks[i].CustomFields)
		}
	}
	if tasks[0].Content != "保存失败" || tasks[0].Note != "点击保存后空白" {
		t.Errorf("bug payload = %+v", tasks[0])
	}
	if tasks[1].Content != "支持导出" || tasks[1].Note != "希望支持 CSV" {
		t.Errorf("feature payload = %+v", tasks[1])
	}
}

func TestPrepareWithoutImagesSignsJWTBeforeTask(t *testing.T) {
	var taskCalls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v3/task/create":
			taskCalls.Add(1)
			assertBearerJWT(t, r.Header.Get("Authorization"), "app-id", "app-secret")
			_, _ = w.Write([]byte(`{"result":{"id":"task-1"}}`))
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	client, err := New(Config{
		BaseURL: api.URL, TaskPath: "/v3/task/create",
		AuthMode: "local_jwt", AppID: "app-id", AppSecret: "app-secret", TenantID: "tenant-id", OperatorID: "operator-id",
		ProjectID: "project-id", BugTypeID: "bug-type", FeatureTypeID: "feature-type",
	})
	if err != nil {
		t.Fatal(err)
	}
	card, err := client.Prepare(context.Background(), Card{SourceIssueID: "sentry-1", Title: "保存失败", Kind: "bug"})
	if err != nil {
		t.Fatal(err)
	}
	if client.token == "" || taskCalls.Load() != 0 {
		t.Fatalf("Prepare should cache a JWT locally without creating a task")
	}
	id, err := client.Create(context.Background(), card)
	if err != nil || id != "task-1" {
		t.Fatalf("Create(prepared) = %q, %v", id, err)
	}
	if taskCalls.Load() != 1 {
		t.Errorf("task calls = %d; want 1", taskCalls.Load())
	}
}

func TestCreateWithScreenshotFailsClosedBeforeNetworkRequest(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	client, err := New(Config{
		BaseURL: server.URL, TaskPath: "/v3/task/create",
		AuthMode: "local_jwt", AppID: "app-id", AppSecret: "app-secret", TenantID: "tenant-id", OperatorID: "operator-id",
		ProjectID: "project-id", BugTypeID: "bug-type", FeatureTypeID: "feature-type",
	})
	if err != nil {
		t.Fatal(err)
	}
	id, err := client.Create(context.Background(), Card{
		SourceIssueID: "sentry-1", Title: "保存失败", Kind: "bug",
		Images: []Image{{Name: "screen.png", MimeType: "image/png", Data: []byte("image")}},
	})
	var permanent *Permanent
	if id != "" || !errors.As(err, &permanent) || !errors.Is(err, ErrImagesUnsupported) {
		t.Fatalf("Create with screenshot = %q, %v; want permanent unsupported-image error", id, err)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("network requests = %d, want none", got)
	}
}

func TestPrepareUploadsScreenshotBeforeCreate(t *testing.T) {
	image := append([]byte("\x89PNG\r\n\x1a\n"), []byte("screenshot bytes")...)
	var uploadCalls atomic.Int32
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uploadCalls.Add(1)
		if r.Method != http.MethodPut || r.URL.RequestURI() != "/upload?signature=private" {
			t.Errorf("upload request = %s %s", r.Method, r.URL.RequestURI())
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("storage upload received Authorization header")
		}
		if got := r.Header.Get("Content-Type"); got != "image/png" {
			t.Errorf("storage Content-Type = %q", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(body, image) {
			t.Errorf("uploaded image differs: read error %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer storage.Close()

	var ticketCalls, taskCalls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v3/awos/upload-token":
			ticketCalls.Add(1)
			assertBearerJWT(t, r.Header.Get("Authorization"), "app-id", "app-secret")
			if got := r.Header.Get("X-Tenant-Id"); got != "tenant-id" {
				t.Errorf("ticket X-Tenant-Id = %q", got)
			}
			if got := r.Header.Get("X-Operator-Id"); got != "operator-id" {
				t.Errorf("ticket X-Operator-Id = %q", got)
			}
			var body struct {
				Scope, FileType, FileName, Category string
				FileSize                            int
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode upload token request: %v", err)
			}
			if body.Scope != "project:project-id/task/attachment" || body.FileType != "image/png" ||
				body.FileName != "screen.png" || body.FileSize != len(image) || body.Category != "attachment" {
				t.Errorf("upload token request = %+v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]string{
				"token": "file-token-1", "uploadUrl": storage.URL + "/upload?signature=private",
			}})
		case "/v3/task/create":
			taskCalls.Add(1)
			assertBearerJWT(t, r.Header.Get("Authorization"), "app-id", "app-secret")
			var body struct {
				CustomFields []struct {
					CfID  string `json:"cfId"`
					Value []struct {
						ID         string `json:"id"`
						Title      string `json:"title"`
						MetaString string `json:"metaString"`
					} `json:"value"`
				} `json:"customfields"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode task create request: %v", err)
			}
			if len(body.CustomFields) != 2 || body.CustomFields[0].CfID != "platform-field" ||
				len(body.CustomFields[0].Value) != 1 || body.CustomFields[0].Value[0].ID != "platform-option" ||
				body.CustomFields[0].Value[0].Title != "标注平台" || body.CustomFields[1].CfID != "attachment-field" ||
				len(body.CustomFields[1].Value) != 1 {
				t.Errorf("task customfields = %+v", body.CustomFields)
			} else {
				value := body.CustomFields[1].Value[0]
				var meta map[string]string
				if err := json.Unmarshal([]byte(value.MetaString), &meta); err != nil || value.Title != "screen.png" || meta["fileToken"] != "file-token-1" {
					t.Errorf("attachment field value = %+v, parsed meta = %+v, error = %v", value, meta, err)
				}
			}
			_, _ = w.Write([]byte(`{"result":{"id":"task-1"}}`))
		default:
			t.Errorf("unexpected API request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	client, err := New(Config{
		BaseURL: api.URL, TaskPath: "/v3/task/create", UploadTokenPath: "/v3/awos/upload-token",
		AuthMode: "local_jwt", AppID: "app-id", AppSecret: "app-secret", TenantID: "tenant-id", OperatorID: "operator-id",
		ProjectID: "project-id", BugTypeID: "bug-type", FeatureTypeID: "feature-type", AttachmentFieldID: "attachment-field",
		CustomFieldID: "platform-field", CustomFieldOptionID: "platform-option", CustomFieldOptionTitle: "标注平台",
		UploadAllowedOrigins: []string{storage.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	card := Card{SourceIssueID: "sentry-1", Title: "保存失败", Kind: "bug", Images: []Image{{
		Name: "C:\\uploads\\screen\x00.png", MimeType: "image/png", Data: image,
	}}}
	prepared, err := client.Prepare(context.Background(), card)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err = client.Prepare(context.Background(), prepared)
	if err != nil {
		t.Fatalf("Prepare(prepared) = %v", err)
	}
	if got := taskCalls.Load(); got != 0 {
		t.Fatalf("task created during Prepare: %d calls", got)
	}
	id, err := client.Create(context.Background(), prepared)
	if err != nil || id != "task-1" {
		t.Fatalf("Create(prepared) = %q, %v", id, err)
	}
	if ticketCalls.Load() != 1 || uploadCalls.Load() != 1 || taskCalls.Load() != 1 {
		t.Errorf("calls: ticket=%d PUT=%d task=%d; want all 1",
			ticketCalls.Load(), uploadCalls.Load(), taskCalls.Load())
	}
	if id, err := client.Create(context.Background(), card); err != nil || id != "task-1" {
		t.Fatalf("Create(unprepared) = %q, %v", id, err)
	}
	if ticketCalls.Load() != 2 || uploadCalls.Load() != 2 || taskCalls.Load() != 2 {
		t.Errorf("direct Create calls: ticket=%d PUT=%d task=%d; want all 2",
			ticketCalls.Load(), uploadCalls.Load(), taskCalls.Load())
	}
}

func TestPrepareRejectsUntrustedUploadOriginWithoutCreatingTask(t *testing.T) {
	image := append([]byte("\x89PNG\r\n\x1a\n"), []byte("screenshot")...)
	var taskCalls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v3/awos/upload-token":
			assertBearerJWT(t, r.Header.Get("Authorization"), "app-id", "app-secret")
			_, _ = w.Write([]byte(`{"result":{"token":"file-token","uploadUrl":"https://untrusted.example/upload?signature=secret"}}`))
		case "/v3/task/create":
			taskCalls.Add(1)
			_, _ = w.Write([]byte(`{"result":{"id":"unexpected"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	client, err := New(Config{
		BaseURL: api.URL, TaskPath: "/v3/task/create", UploadTokenPath: "/v3/awos/upload-token",
		AuthMode: "local_jwt", AppID: "app-id", AppSecret: "app-secret", TenantID: "tenant-id", OperatorID: "operator-id",
		ProjectID: "project-id", BugTypeID: "bug-type", FeatureTypeID: "feature-type", AttachmentFieldID: "attachment-field",
		UploadAllowedOrigins: []string{api.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Create(context.Background(), Card{SourceIssueID: "sentry-1", Title: "保存失败", Kind: "bug",
		Images: []Image{{Name: "screen.png", MimeType: "image/png", Data: image}},
	})
	var permanent *Permanent
	if !errors.As(err, &permanent) || strings.Contains(err.Error(), "signature=secret") {
		t.Fatalf("untrusted origin error = %v; want safe permanent error", err)
	}
	if got := taskCalls.Load(); got != 0 {
		t.Errorf("task create calls = %d, want 0", got)
	}
}

func TestPrepareRejectsInvalidImageBeforeNetworkRequest(t *testing.T) {
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer api.Close()
	client, err := New(Config{
		BaseURL: api.URL, TaskPath: "/v3/task/create", UploadTokenPath: "/v3/awos/upload-token",
		AuthMode: "local_jwt", AppID: "app-id", AppSecret: "app-secret", TenantID: "tenant-id", OperatorID: "operator-id",
		ProjectID: "project-id", BugTypeID: "bug-type", FeatureTypeID: "feature-type", AttachmentFieldID: "attachment-field",
		UploadAllowedOrigins: []string{api.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		image Image
	}{
		{"too large", Image{Name: "large.png", MimeType: "image/png", Data: bytes.Repeat([]byte{'x'}, (5<<20)+1)}},
		{"non-image MIME", Image{Name: "file.txt", MimeType: "text/plain", Data: []byte("text")}},
		{"wrong bytes", Image{Name: "fake.png", MimeType: "image/png", Data: []byte("not a PNG")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.Prepare(context.Background(), Card{Kind: "bug", Images: []Image{tc.image}})
			var permanent *Permanent
			if !errors.As(err, &permanent) {
				t.Errorf("Prepare error = %v; want permanent validation error", err)
			}
		})
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("network requests = %d, want 0", got)
	}
}

func TestUploadPutDoesNotFollowRedirect(t *testing.T) {
	image := append([]byte("\x89PNG\r\n\x1a\n"), []byte("screenshot")...)
	var redirectedCalls, taskCalls atomic.Int32
	redirected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectedCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer redirected.Close()
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirected.URL+"/redirected", http.StatusTemporaryRedirect)
	}))
	defer storage.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v3/awos/upload-token":
			assertBearerJWT(t, r.Header.Get("Authorization"), "app-id", "app-secret")
			_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]string{
				"token": "file-token", "uploadUrl": storage.URL + "/upload?signature=private",
			}})
		case "/v3/task/create":
			taskCalls.Add(1)
			_, _ = w.Write([]byte(`{"result":{"id":"unexpected"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	client, err := New(Config{
		BaseURL: api.URL, TaskPath: "/v3/task/create", UploadTokenPath: "/v3/awos/upload-token",
		AuthMode: "local_jwt", AppID: "app-id", AppSecret: "app-secret", TenantID: "tenant-id", OperatorID: "operator-id",
		ProjectID: "project-id", BugTypeID: "bug-type", FeatureTypeID: "feature-type", AttachmentFieldID: "attachment-field",
		UploadAllowedOrigins: []string{storage.URL, redirected.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Prepare(context.Background(), Card{Kind: "bug", Images: []Image{{Name: "screen.png", MimeType: "image/png", Data: image}}})
	if err == nil || strings.Contains(err.Error(), "signature=private") {
		t.Fatalf("redirected PUT error = %v; want safe failure", err)
	}
	if redirectedCalls.Load() != 0 || taskCalls.Load() != 0 {
		t.Errorf("redirect destination calls=%d, task calls=%d; want both 0", redirectedCalls.Load(), taskCalls.Load())
	}
}

func TestOnlineAppTokenExchangeAndRefresh(t *testing.T) {
	var tokenCalls, taskCalls atomic.Int32
	var now = time.Unix(1_700_000_000, 0)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/appToken":
			call := tokenCalls.Add(1)
			if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Accept") != "application/json" {
				t.Errorf("appToken method/headers = %s/%v", r.Method, r.Header)
			}
			if r.Header.Get("Authorization") != "" || r.Header.Get("X-Operator-Id") != "" {
				t.Error("appToken request included business authorization headers")
			}
			var credentials map[string]string
			if err := json.NewDecoder(r.Body).Decode(&credentials); err != nil {
				t.Errorf("decode appToken request: %v", err)
			}
			if len(credentials) != 2 || credentials["appId"] != "app-id" || credentials["appSecret"] != "app-secret" {
				t.Errorf("appToken request did not contain the expected credentials")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"appToken": "online-token-" + string(rune('0'+call)), "expire": 1800})
		case "/gateway/v3/task/create":
			call := taskCalls.Add(1)
			want := "Bearer online-token-1"
			if call == 3 {
				want = "Bearer online-token-2"
			}
			if got := r.Header.Get("Authorization"); got != want {
				t.Errorf("business bearer token = %q, want %q", got, want)
			}
			_, _ = w.Write([]byte(`{"result":{"id":"task-1"}}`))
		default:
			t.Errorf("unexpected URL path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := New(Config{
		BaseURL: server.URL, TaskPath: "/gateway/v3/task/create", TokenURL: server.URL + "/api/appToken",
		AppID: "app-id", AppSecret: "app-secret", BugTypeID: "bug", FeatureTypeID: "feature",
	})
	if err != nil {
		t.Fatal(err)
	}
	client.http.Transport = server.Client().Transport
	client.now = func() time.Time { return now }
	card := Card{Kind: "bug", Title: "feedback", SourceIssueID: "1"}
	for i := 0; i < 2; i++ {
		if _, err := client.Create(context.Background(), card); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}
	if tokenCalls.Load() != 1 || taskCalls.Load() != 2 {
		t.Fatalf("calls before expiry: token=%d task=%d; want 1/2", tokenCalls.Load(), taskCalls.Load())
	}
	now = now.Add(1700 * time.Second)
	if _, err := client.Create(context.Background(), card); err != nil {
		t.Fatal(err)
	}
	if tokenCalls.Load() != 2 || taskCalls.Load() != 3 {
		t.Fatalf("calls at refresh boundary: token=%d task=%d; want 2/3", tokenCalls.Load(), taskCalls.Load())
	}
}

func TestOnlineAppTokenNestedResponse(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"result":{"appToken":"nested-token","expire":1800}}`))
	}))
	defer server.Close()
	client, err := New(Config{
		BaseURL: server.URL, TaskPath: "/gateway/v3/task/create", AuthMode: "app_token", TokenURL: server.URL + "/api/appToken",
		AppID: "app-id", AppSecret: "app-secret", BugTypeID: "bug", FeatureTypeID: "feature",
	})
	if err != nil {
		t.Fatal(err)
	}
	client.http.Transport = server.Client().Transport
	token, err := client.accessToken(context.Background())
	if err != nil || token != "nested-token" {
		t.Fatalf("nested appToken exchange failed: token received=%t, error=%v", token != "", err)
	}
}

func TestOnlineAppTokenFailureClassificationAndRedaction(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		permanent bool
	}{
		{"forbidden", http.StatusForbidden, `{"message":"app-secret bad-token"}`, true},
		{"rate limited", http.StatusTooManyRequests, `{"message":"app-secret bad-token"}`, false},
		{"server error", http.StatusBadGateway, `{"message":"app-secret bad-token"}`, false},
		{"malformed success", http.StatusOK, `{"appToken":"bad-token"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client, err := New(Config{
				BaseURL: server.URL, TaskPath: "/gateway/v3/task/create", TokenURL: server.URL + "/api/appToken",
				AppID: "app-id", AppSecret: "app-secret", BugTypeID: "bug", FeatureTypeID: "feature",
			})
			if err != nil {
				t.Fatal(err)
			}
			client.http.Transport = server.Client().Transport
			_, err = client.accessToken(context.Background())
			var permanent *Permanent
			if err == nil || errors.As(err, &permanent) != tc.permanent {
				t.Fatalf("token error = %v, permanent=%t; want permanent=%t", err, permanent != nil, tc.permanent)
			}
			if strings.Contains(err.Error(), "app-secret") || strings.Contains(err.Error(), "bad-token") {
				t.Fatal("token error leaked credential or token response")
			}
		})
	}
}

func TestOnlineAppTokenDoesNotFollowRedirect(t *testing.T) {
	var redirectedCalls atomic.Int32
	redirected := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectedCalls.Add(1)
	}))
	defer redirected.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirected.URL+"/capture", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client, err := New(Config{
		BaseURL: server.URL, TaskPath: "/gateway/v3/task/create", TokenURL: server.URL + "/api/appToken",
		AppID: "app-id", AppSecret: "app-secret", BugTypeID: "bug", FeatureTypeID: "feature",
	})
	if err != nil {
		t.Fatal(err)
	}
	client.http.Transport = server.Client().Transport
	_, err = client.accessToken(context.Background())
	var permanent *Permanent
	if !errors.As(err, &permanent) || redirectedCalls.Load() != 0 {
		t.Fatalf("redirect result = %v, destination calls=%d; want permanent error and no destination call", err, redirectedCalls.Load())
	}
}

func TestOnlineAppTokenURLValidation(t *testing.T) {
	for _, raw := range []string{
		"http://open.teambition.com/api/appToken", "https://user:secret@open.teambition.com/api/appToken",
		"https://open.teambition.com/api/appToken?secret=x", "https://open.teambition.com/api/appToken#fragment",
	} {
		_, err := New(Config{
			BaseURL: "https://teambition.example", TokenURL: raw,
			AppID: "app-id", AppSecret: "app-secret", BugTypeID: "bug", FeatureTypeID: "feature",
		})
		if err == nil {
			t.Errorf("New accepted unsafe appToken URL")
		}
	}
}

func TestOnlineAppTokenDefaultsToPrivateGateway(t *testing.T) {
	client, err := New(Config{
		BaseURL: "https://teambition.example/tenant/path", AppID: "app-id", AppSecret: "app-secret",
		BugTypeID: "bug", FeatureTypeID: "feature",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := client.cfg.TokenURL, "https://teambition.example/gateway/appToken"; got != want {
		t.Errorf("default token URL = %q; want %q", got, want)
	}
}

func TestCustomFieldConfigurationMustBeComplete(t *testing.T) {
	_, err := New(Config{
		BaseURL: "https://teambition.example", AuthMode: "local_jwt",
		AppID: "app-id", AppSecret: "app-secret", BugTypeID: "bug", FeatureTypeID: "feature",
		CustomFieldID: "platform-field",
	})
	if err == nil {
		t.Fatal("accepted a partial custom field configuration")
	}
}
