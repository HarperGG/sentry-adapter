package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestTokenProbeUsesPrivateGatewayShapeAndRedactsSecrets(t *testing.T) {
	const appID = "private-app-id"
	const secret = "private-app-secret"
	const token = "private-returned-token"
	var calls atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/gateway/appToken" {
			t.Errorf("unexpected token request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected Content-Type: %q", r.Header.Get("Content-Type"))
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode token request: %v", err)
		}
		if body["appId"] != appID || body["appSecret"] != secret || len(body) != 2 {
			t.Errorf("unexpected token request shape")
		}
		_, _ = io.WriteString(w, `{"result":{"appToken":"`+token+`","expire":1800}}`)
	}))
	defer upstream.Close()
	p := probe{cfg: settings{baseURL: upstream.URL, tokenURL: upstream.URL + "/gateway/appToken", appID: appID, appSecret: secret}, client: upstream.Client()}
	local := httptest.NewServer(p.handler())
	defer local.Close()
	resp, err := http.Post(local.URL+"/api/check/token", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	response, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("local status = %d, body = %s", resp.StatusCode, response)
	}
	var output diagnostic
	if err := json.Unmarshal(response, &output); err != nil {
		t.Fatal(err)
	}
	if output.GatewayStatus != 200 || !output.TokenReceived || output.ExpiresInSeconds != 1800 || output.Category != "ok" || calls.Load() != 1 {
		t.Fatalf("unexpected probe result: %+v, calls: %d", output, calls.Load())
	}
	for _, sensitive := range []string{appID, secret, token} {
		if strings.Contains(string(response), sensitive) {
			t.Fatalf("sensitive value appeared in diagnostic response")
		}
	}
}

func TestTaskReadSendsBearerAndTenantHeadersWithoutReturningDetails(t *testing.T) {
	const token = "top-secret-token"
	const taskID = "task_123"
	var calls atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/gateway/appToken":
			_, _ = io.WriteString(w, `{"appToken":"`+token+`","expire":600}`)
		case "/gateway/v3/task/query":
			if r.Method != http.MethodGet || r.URL.Query().Get("taskId") != taskID || r.Header.Get("Authorization") != "Bearer "+token ||
				r.Header.Get("X-Operator-Id") != "operator-1" || r.Header.Get("X-Tenant-Id") != "tenant-1" || r.Header.Get("X-Tenant-Type") != "organization" {
				t.Errorf("task request is missing method, query, or authentication headers")
			}
			_, _ = io.WriteString(w, `{"result":[{"id":"`+taskID+`","content":"private task contents"}]}`)
		default:
			t.Errorf("unexpected request path: %s", r.URL.Path)
		}
	}))
	defer upstream.Close()
	p := probe{cfg: settings{baseURL: upstream.URL, tokenURL: upstream.URL + "/gateway/appToken", appID: "app", appSecret: "secret", tenantID: "tenant-1", operatorID: "operator-1"}, client: upstream.Client()}
	local := httptest.NewServer(p.handler())
	defer local.Close()
	resp, err := http.Post(local.URL+"/api/check/task-read", "application/json", strings.NewReader(`{"taskId":"`+taskID+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	response, _ := io.ReadAll(resp.Body)
	var output diagnostic
	if err := json.Unmarshal(response, &output); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || output.GatewayStatus != 200 || !output.TokenReceived || output.TaskStatus != 200 || output.TaskCategory != "ok" || calls.Load() != 2 {
		t.Fatalf("unexpected task probe result: %+v, calls: %d", output, calls.Load())
	}
	if strings.Contains(string(response), token) || strings.Contains(string(response), "private task contents") {
		t.Fatal("token or task details appeared in diagnostic response")
	}
}

func TestTaskReadVerifiesResultContainsRequestedID(t *testing.T) {
	for _, tt := range []struct {
		name, response, want string
	}{
		{"empty array", `{"result":[]}`, "task_not_found"},
		{"mismatched id", `{"result":[{"id":"different-task"}]}`, "task_not_found"},
		{"scalar result", `{"result":"task1"}`, "invalid_task_response"},
		{"object without id", `{"result":{"content":"private"}}`, "invalid_task_response"},
		{"private object variant", `{"result":{"id":"task1"}}`, "ok"},
		{"deprecated taskId", `{"result":[{"taskId":"task1"}]}`, "ok"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/gateway/appToken" {
					_, _ = io.WriteString(w, `{"appToken":"token","expire":600}`)
					return
				}
				_, _ = io.WriteString(w, tt.response)
			}))
			defer upstream.Close()
			p := probe{cfg: settings{baseURL: upstream.URL, tokenURL: upstream.URL + "/gateway/appToken", appID: "app", appSecret: "secret", tenantID: "tenant", operatorID: "operator"}, client: upstream.Client()}
			result := p.checkTaskRead(t.Context(), "task1")
			if result.TaskStatus != 200 || result.TaskCategory != tt.want {
				t.Fatalf("taskCategory=%q, want %q (gatewayStatus=%d, taskStatus=%d)", result.TaskCategory, tt.want, result.GatewayStatus, result.TaskStatus)
			}
		})
	}
}

func TestTaskReadDoesNotMistakeBusinessErrorForSuccess(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/gateway/appToken" {
			_, _ = io.WriteString(w, `{"appToken":"token","expire":600}`)
			return
		}
		_, _ = io.WriteString(w, `{"code":403,"errorMessage":"permission denied with private details","result":null}`)
	}))
	defer upstream.Close()
	p := probe{cfg: settings{baseURL: upstream.URL, tokenURL: upstream.URL + "/gateway/appToken", appID: "app", appSecret: "secret", tenantID: "tenant", operatorID: "operator"}, client: upstream.Client()}
	local := httptest.NewServer(p.handler())
	defer local.Close()
	resp, err := http.Post(local.URL+"/api/check/task-read", "application/json", strings.NewReader(`{"taskId":"task1"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var output diagnostic
	if err := json.Unmarshal(body, &output); err != nil {
		t.Fatal(err)
	}
	if output.TaskStatus != 200 || output.TaskCategory != "api_error" || strings.Contains(string(body), "private details") {
		t.Fatalf("business error was not safely classified: %+v", output)
	}
}

func TestTokenForbiddenAndRedirectAreSanitized(t *testing.T) {
	for _, tt := range []struct {
		name, category string
		status         int
	}{
		{"forbidden", "forbidden", http.StatusForbidden},
		{"redirect", "redirect_blocked", http.StatusFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if tt.status == http.StatusFound {
					w.Header().Set("Location", "/other")
				}
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, "the gateway body contains secret-value")
			}))
			defer upstream.Close()
			client := upstream.Client()
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			p := probe{cfg: settings{tokenURL: upstream.URL + "/gateway/appToken", appID: "app", appSecret: "secret-value"}, client: client}
			local := httptest.NewServer(p.handler())
			defer local.Close()
			resp, err := http.Post(local.URL+"/api/check/token", "application/json", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			var output diagnostic
			if err := json.Unmarshal(body, &output); err != nil {
				t.Fatal(err)
			}
			if output.GatewayStatus != tt.status || output.Category != tt.category || output.TokenReceived || calls.Load() != 1 {
				t.Fatalf("unexpected result: %+v, calls=%d", output, calls.Load())
			}
			if strings.Contains(string(body), "secret-value") {
				t.Fatal("gateway body leaked")
			}
		})
	}
}

func TestTokenBusinessErrorDoesNotReturnToken(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":403,"errorMessage":"private error","result":{"appToken":"secret-token","expire":600}}`)
	}))
	defer upstream.Close()
	p := probe{cfg: settings{tokenURL: upstream.URL, appID: "app", appSecret: "secret"}, client: upstream.Client()}
	result, token := p.checkToken(t.Context())
	if result.GatewayStatus != 200 || result.Category != "api_error" || result.TokenReceived || token != "" {
		t.Fatalf("unexpected token business-error result: %+v", result)
	}
}

func TestCrossOriginAndBadTaskIDNeverCallGateway(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer upstream.Close()
	p := probe{cfg: settings{tokenURL: upstream.URL, appID: "app", appSecret: "secret"}, client: upstream.Client()}
	local := httptest.NewServer(p.handler())
	defer local.Close()
	req, _ := http.NewRequest(http.MethodPost, local.URL+"/api/check/token", nil)
	req.Header.Set("Origin", "https://unrelated.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin request returned %d", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodPost, local.URL+"/api/check/token", nil)
	req.Host = "rebound.example"
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-loopback Host returned %d", resp.StatusCode)
	}
	resp, err = http.Post(local.URL+"/api/check/task-read", "application/json", strings.NewReader(`{"taskId":"../bad"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || calls.Load() != 0 {
		t.Fatalf("invalid task ID reached upstream; status=%d calls=%d", resp.StatusCode, calls.Load())
	}
}

func TestConfigFilesWithOSOverrideAndLoopbackEnforcement(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	credFile := filepath.Join(dir, "credentials.env")
	if err := os.WriteFile(envFile, []byte("TEAMBITION_BASE_URL=https://teambition.example\nTEAMBITION_TENANT_ID=file-tenant\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credFile, []byte("TEAMBITION_APP_ID=file-app\nTEAMBITION_APP_SECRET=file-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEAMBITION_APP_ID", "os-app")
	for _, key := range []string{"TEAMBITION_BASE_URL", "TEAMBITION_TOKEN_URL", "TEAMBITION_APP_SECRET", "TEAMBITION_TENANT_ID", "TEAMBITION_OPERATOR_ID"} {
		// Ensure this test is deterministic even on a machine with deployment env vars.
		old, existed := os.LookupEnv(key)
		if existed {
			t.Cleanup(func() { _ = os.Setenv(key, old) })
		} else {
			t.Cleanup(func() { _ = os.Unsetenv(key) })
		}
		_ = os.Unsetenv(key)
	}
	cfg, err := loadSettings(envFile, credFile)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.appID != "os-app" || cfg.appSecret != "file-secret" || cfg.tokenURL != "https://teambition.example/gateway/appToken" || cfg.tenantID != "file-tenant" {
		t.Fatalf("configuration precedence or defaults are wrong")
	}
	for _, addr := range []string{"0.0.0.0:8790", ":8790", "192.168.1.2:8790", "example.com:8790"} {
		if err := validateLoopback(addr); err == nil {
			t.Errorf("accepted non-loopback address %q", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:8790", "localhost:8790", "[::1]:8790"} {
		if err := validateLoopback(addr); err != nil {
			t.Errorf("rejected loopback address %q: %v", addr, err)
		}
	}
}

func TestEmbeddedSwaggerIsServedLocally(t *testing.T) {
	p := probe{}
	local := httptest.NewServer(p.handler())
	defer local.Close()
	for _, check := range []struct{ path, contains string }{
		{"/swagger/", "SwaggerUIBundle"},
		{"/swagger/swagger-ui-bundle.js", "SwaggerUIBundle"},
		{"/swagger/swagger-ui.css", ".swagger-ui"},
		{"/openapi.yaml", "/api/check/token"},
	} {
		resp, err := http.Get(local.URL + check.path)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(data), check.contains) {
			t.Errorf("GET %s: status=%d, embedded asset missing expected content", check.path, resp.StatusCode)
		}
	}
	resp, err := http.Get(local.URL + "/api/check/token")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET token check returned %d, want 405", resp.StatusCode)
	}
}
