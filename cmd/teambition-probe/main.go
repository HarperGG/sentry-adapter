// teambition-probe is a local, read-only connectivity diagnostic. It never
// returns app tokens, credentials, or Teambition task contents to the browser.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

//go:embed ui/*
var embeddedUI embed.FS

const requestTimeout = 20 * time.Second

type settings struct {
	baseURL, tokenURL, appID, appSecret string
	tenantID, operatorID                string
}

type diagnostic struct {
	GatewayStatus    int    `json:"gatewayStatus"`
	TokenReceived    bool   `json:"tokenReceived"`
	ExpiresInSeconds int64  `json:"expiresInSeconds"`
	Category         string `json:"category"`
	DurationMs       int64  `json:"durationMs"`
	TaskStatus       int    `json:"taskStatus,omitempty"`
	TaskCategory     string `json:"taskCategory,omitempty"`
}

type probe struct {
	cfg    settings
	client *http.Client
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("teambition-probe", flag.ContinueOnError)
	flags.SetOutput(stderr)
	envFile := flags.String("env-file", ".env", "nonsecret Teambition configuration file")
	credentialsFile := flags.String("credentials-file", "config/credentials.env", "local credential file")
	listen := flags.String("listen", "127.0.0.1:8790", "loopback listen address")
	once := flags.Bool("once", false, "check appToken once and exit")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "unexpected arguments")
		return 2
	}
	cfg, err := loadSettings(*envFile, *credentialsFile)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	p := probe{cfg: cfg, client: newHTTPClient()}
	if *once {
		result, _ := p.checkToken(context.Background())
		fmt.Fprintf(stdout, "gatewayStatus=%d category=%s expiresInSeconds=%d\n", result.GatewayStatus, result.Category, result.ExpiresInSeconds)
		if result.Category == "ok" {
			return 0
		}
		return 1
	}
	if err := validateLoopback(*listen); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	server := &http.Server{
		Addr: *listen, Handler: p.handler(), ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout: 5 * time.Second, WriteTimeout: 45 * time.Second,
		IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10,
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(stderr, "could not start loopback diagnostic server")
		return 1
	}
	fmt.Fprintf(stdout, "Teambition diagnostic: http://%s/swagger/\n", listener.Addr().String())
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(stderr, "diagnostic server stopped unexpectedly")
		return 1
	}
	return 0
}

func loadSettings(envFile, credentialsFile string) (settings, error) {
	values := make(map[string]string)
	for _, item := range []struct{ path, optionalDefault string }{{envFile, ".env"}, {credentialsFile, "config/credentials.env"}} {
		if err := readEnvFile(item.path, values); err != nil {
			if !(errors.Is(err, os.ErrNotExist) && item.path == item.optionalDefault) {
				return settings{}, fmt.Errorf("could not read configuration file %q", item.path)
			}
		}
	}
	for _, key := range []string{"TEAMBITION_BASE_URL", "TEAMBITION_TOKEN_URL", "TEAMBITION_APP_ID", "TEAMBITION_APP_SECRET", "TEAMBITION_TENANT_ID", "TEAMBITION_OPERATOR_ID"} {
		if value, ok := os.LookupEnv(key); ok {
			values[key] = value
		}
	}
	baseURL := values["TEAMBITION_BASE_URL"]
	if baseURL == "" {
		baseURL = "https://teambition.gwm.cn"
	}
	base, err := url.Parse(baseURL)
	if err != nil || !safeHTTPSURL(base) || base.Path != "" && base.Path != "/" {
		return settings{}, errors.New("TEAMBITION_BASE_URL must be an HTTPS origin")
	}
	tokenURL := values["TEAMBITION_TOKEN_URL"]
	if tokenURL == "" {
		u := *base
		u.Path = "/gateway/appToken"
		tokenURL = u.String()
	}
	token, err := url.Parse(tokenURL)
	if err != nil || !safeHTTPSURL(token) {
		return settings{}, errors.New("TEAMBITION_TOKEN_URL must be an absolute HTTPS URL")
	}
	for _, key := range []string{"TEAMBITION_APP_ID", "TEAMBITION_APP_SECRET"} {
		if strings.TrimSpace(values[key]) == "" {
			return settings{}, fmt.Errorf("%s is required", key)
		}
	}
	return settings{
		baseURL: strings.TrimRight(baseURL, "/"), tokenURL: tokenURL,
		appID: values["TEAMBITION_APP_ID"], appSecret: values["TEAMBITION_APP_SECRET"],
		tenantID: values["TEAMBITION_TENANT_ID"], operatorID: values["TEAMBITION_OPERATOR_ID"],
	}, nil
}

func safeHTTPSURL(u *url.URL) bool {
	return u != nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil &&
		u.Opaque == "" && u.RawQuery == "" && u.Fragment == ""
}

func readEnvFile(path string, values map[string]string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 1<<20+1))
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return errors.New("configuration file too large")
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return errors.New("invalid configuration line")
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '\'' && value[len(value)-1] == '\'' || value[0] == '"' && value[len(value)-1] == '"') {
			value = value[1 : len(value)-1]
		}
		values[key] = value
	}
	return nil
}

func validateLoopback(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return errors.New("-listen must include a loopback host and port")
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("-listen must bind only to localhost or a loopback IP address")
	}
	return nil
}

func newHTTPClient() *http.Client {
	return &http.Client{Timeout: requestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

func (p probe) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/check/token", func(w http.ResponseWriter, r *http.Request) {
		var first [1]byte
		if n, err := http.MaxBytesReader(w, r.Body, 1).Read(first[:]); n != 0 || err != io.EOF {
			writeJSON(w, http.StatusBadRequest, map[string]string{"category": "body_not_allowed"})
			return
		}
		result, _ := p.checkToken(r.Context())
		writeJSON(w, http.StatusOK, result)
	})
	mux.HandleFunc("POST /api/check/task-read", func(w http.ResponseWriter, r *http.Request) {
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"category": "content_type_required"})
			return
		}
		var input struct {
			TaskID string `json:"taskId"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil || decoder.Decode(&struct{}{}) != io.EOF || !validTaskID(input.TaskID) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"category": "invalid_task_id"})
			return
		}
		result := p.checkTaskRead(r.Context(), input.TaskID)
		writeJSON(w, http.StatusOK, result)
	})
	mux.HandleFunc("GET /openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		data, err := embeddedUI.ReadFile("ui/openapi.yaml")
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		_, _ = w.Write(data)
	})
	if static, err := fs.Sub(embeddedUI, "ui"); err == nil {
		mux.Handle("GET /swagger/", http.StripPrefix("/swagger/", http.FileServer(http.FS(static))))
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/swagger/", http.StatusFound)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if !loopbackRequestHost(r.Host) {
			writeJSON(w, http.StatusForbidden, map[string]string{"category": "host_forbidden"})
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"category": "cross_origin_forbidden"})
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func loopbackRequestHost(hostPort string) bool {
	host, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		host = hostPort
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func sameOrigin(origin string, r *http.Request) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return u.Scheme == scheme && strings.EqualFold(u.Host, r.Host)
}

func validTaskID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_') {
			return false
		}
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (p probe) checkToken(ctx context.Context) (diagnostic, string) {
	start := time.Now()
	result := diagnostic{Category: "network_error"}
	body, _ := json.Marshal(struct {
		AppID     string `json:"appId"`
		AppSecret string `json:"appSecret"`
	}{p.cfg.appID, p.cfg.appSecret})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.tokenURL, bytes.NewReader(body))
	if err != nil {
		result.Category = "invalid_configuration"
		result.DurationMs = time.Since(start).Milliseconds()
		return result, ""
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		result.Category = classifyNetworkError(err)
		result.DurationMs = time.Since(start).Milliseconds()
		return result, ""
	}
	defer resp.Body.Close()
	result.GatewayStatus = resp.StatusCode
	result.Category = classifyHTTPStatus(resp.StatusCode)
	if result.Category != "ok" {
		result.DurationMs = time.Since(start).Milliseconds()
		return result, ""
	}
	var output struct {
		AppToken     string          `json:"appToken"`
		Expire       int64           `json:"expire"`
		Code         json.RawMessage `json:"code"`
		ErrorMessage string          `json:"errorMessage"`
		Result       struct {
			AppToken string `json:"appToken"`
			Expire   int64  `json:"expire"`
		} `json:"result"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&output); err != nil {
		result.Category = "invalid_token_response"
		result.DurationMs = time.Since(start).Milliseconds()
		return result, ""
	}
	if output.ErrorMessage != "" || !successfulAPICode(output.Code) {
		result.Category = "api_error"
		result.DurationMs = time.Since(start).Milliseconds()
		return result, ""
	}
	token, ttl := output.AppToken, output.Expire
	if token == "" {
		token, ttl = output.Result.AppToken, output.Result.Expire
	}
	if token == "" || ttl <= 0 {
		result.Category = "invalid_token_response"
		result.DurationMs = time.Since(start).Milliseconds()
		return result, ""
	}
	result.TokenReceived = true
	result.ExpiresInSeconds = ttl
	result.DurationMs = time.Since(start).Milliseconds()
	return result, token
}

func (p probe) checkTaskRead(ctx context.Context, taskID string) diagnostic {
	start := time.Now()
	result, token := p.checkToken(ctx)
	result.TaskCategory = "not_attempted"
	if token == "" {
		result.DurationMs = time.Since(start).Milliseconds()
		return result
	}
	if p.cfg.tenantID == "" || p.cfg.operatorID == "" {
		result.TaskCategory = "missing_tenant_or_operator"
		result.DurationMs = time.Since(start).Milliseconds()
		return result
	}
	u, _ := url.Parse(p.cfg.baseURL)
	u.Path = "/gateway/v3/task/query"
	u.RawQuery = url.Values{"taskId": {taskID}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		result.TaskCategory = "invalid_configuration"
		result.DurationMs = time.Since(start).Milliseconds()
		return result
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("x-operator-id", p.cfg.operatorID)
	req.Header.Set("X-Tenant-Id", p.cfg.tenantID)
	req.Header.Set("X-Tenant-Type", "organization")
	resp, err := p.client.Do(req)
	if err != nil {
		result.TaskCategory = classifyNetworkError(err)
		result.DurationMs = time.Since(start).Milliseconds()
		return result
	}
	defer resp.Body.Close()
	result.TaskStatus = resp.StatusCode
	result.TaskCategory = classifyHTTPStatus(resp.StatusCode)
	if result.TaskCategory == "ok" {
		var output struct {
			Code         json.RawMessage `json:"code"`
			ErrorMessage string          `json:"errorMessage"`
			Result       json.RawMessage `json:"result"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, (1<<20)+1)).Decode(&output); err != nil {
			result.TaskCategory = "invalid_task_response"
		} else if output.ErrorMessage != "" || !successfulAPICode(output.Code) {
			result.TaskCategory = "api_error"
		} else {
			result.TaskCategory = classifyTaskResult(output.Result, taskID)
		}
	}
	result.DurationMs = time.Since(start).Milliseconds()
	return result
}

// A successful HTTP status only proves that the gateway responded. The task
// lookup is successful when the response actually contains the requested ID.
func classifyTaskResult(raw json.RawMessage, requestedID string) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return "invalid_task_response"
	}
	var items []json.RawMessage
	switch raw[0] {
	case '[':
		if err := json.Unmarshal(raw, &items); err != nil {
			return "invalid_task_response"
		}
		if len(items) == 0 {
			return "task_not_found"
		}
	case '{':
		items = []json.RawMessage{raw}
	default:
		return "invalid_task_response"
	}
	matched := false
	for _, item := range items {
		item = bytes.TrimSpace(item)
		if len(item) == 0 || item[0] != '{' {
			return "invalid_task_response"
		}
		var task struct {
			ID     string `json:"id"`
			TaskID string `json:"taskId"`
		}
		if err := json.Unmarshal(item, &task); err != nil {
			return "invalid_task_response"
		}
		if task.ID == "" && task.TaskID == "" {
			return "invalid_task_response"
		}
		if task.ID == requestedID || task.TaskID == requestedID {
			matched = true
		}
	}
	if matched {
		return "ok"
	}
	return "task_not_found"
}

func successfulAPICode(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return true
	}
	var code float64
	if err := json.Unmarshal(raw, &code); err != nil {
		return false
	}
	return code == 0 || code == 200
}

func classifyHTTPStatus(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "ok"
	case status >= 300 && status < 400:
		return "redirect_blocked"
	case status == 400:
		return "bad_request"
	case status == 401:
		return "unauthorized"
	case status == 403:
		return "forbidden"
	case status == 404:
		return "not_found"
	case status == 429:
		return "rate_limited"
	case status >= 500:
		return "server_error"
	default:
		return "http_error"
	}
}

func classifyNetworkError(err error) string {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() || errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns_error"
	}
	var unknownAuthority x509.UnknownAuthorityError
	var invalidCertificate x509.CertificateInvalidError
	var invalidHostname x509.HostnameError
	var tlsHeader *tls.RecordHeaderError
	if errors.As(err, &unknownAuthority) || errors.As(err, &invalidCertificate) || errors.As(err, &invalidHostname) || errors.As(err, &tlsHeader) {
		return "tls_error"
	}
	return "network_error"
}
