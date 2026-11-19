package sentry

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestGetErrorObservation(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	const value = "资源读取失败（private/object/path/schema.json）: HTTP status 404"
	const stage = "open.resources"
	const fromTags = `"tags":[{"key":"unknown","value":true},{"key":"failure_stage","value":"open.resources"}]`
	wantException := ErrorObservation{
		EventID: id, FailureStage: stage, ExceptionType: "ResourceError", ExceptionValue: "资源读取失败", ResourceFile: "schema.json", HTTPStatus: "404",
	}
	for _, tc := range []struct {
		name   string
		json   string
		status int
		want   *ErrorObservation
		fails  bool
	}{
		{"matching eventID and metadata", fmt.Sprintf(`{"id":"unrelated-id","eventID":%q,"type":"error","metadata":{"type":"ResourceError","value":%q},%s}`, id, value, fromTags), 200, &wantException, false},
		{"id and exception entries", fmt.Sprintf(`{"id":%q,"type":"error","entries":[{"type":"request","data":[]},{"type":"exception","data":{"values":[{"type":"Other","value":"cause"},{"type":"ResourceError","value":%q}]}}],%s}`, id, value, fromTags), 200, &wantException, false},
		{"ID mismatch even if id matches", fmt.Sprintf(`{"id":%q,"eventID":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","type":"error",%s}`, id, fromTags), 200, nil, true},
		{"matched ID but feedback event", fmt.Sprintf(`{"eventID":%q,"type":"feedback",%s}`, id, fromTags), 200, nil, true},
		{"wrong field types and private contents", fmt.Sprintf(`{"eventID":%q,"type":"error","user":{"username":"hidden"},"tags":[{"key":"failure_stage","value":true}],"entries":[{"type":"exception","data":{"values":[{"type":"bad name","value":"Authorization: Bearer hidden"}]}}]}`, id), 200, nil, false},
		{"forbidden", `{}`, 403, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.RequestURI() != "/api/0/projects/acme/web/events/"+id+"/" || r.Header.Get("Authorization") != "Bearer test-read" {
					t.Errorf("unexpected project event request: %s %s", r.Method, r.URL.RequestURI())
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.json)
			}))
			defer server.Close()
			client, err := New(server.URL, "test-read")
			if err != nil {
				t.Fatal(err)
			}
			got, err := client.GetErrorObservation(t.Context(), "acme", "web", id)
			if (err != nil) != tc.fails || (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Fatalf("observation=%+v err=%v, expected %+v with fails=%t", got, err, tc.want, tc.fails)
			}
		})
	}
}

func TestDiagnosticMessageOmitsSecretsAndDynamicPaths(t *testing.T) {
	for _, tc := range []struct{ raw, summary, file, status string }{
		{"配置请求失败（https://private.example/path/private-bucket/layout.cfg?signature=secret）: HTTP status 403", "配置请求失败", "layout.cfg", "403"},
		{"Bad response (/private/credentials.json): HTTP 400", "Bad response", "", "400"},
		{"Bad response (/object/67c00cff9fa4343e936a4e34d37cb885.json)", "Bad response", "", ""},
		{"Bad response (/private/other?redirect=/private/layout.cfg)", "Bad response", "", ""},
		{"Authorization: Bearer private-token", "", "", ""},
		{"Missing config for alice@example.test", "Missing config for", "", ""},
		{"Cannot load resource\nAuthorization: Bearer secret", "Cannot load resource", "", ""},
		{"Invalid password for alice", "", "", ""},
	} {
		summary, file, status := diagnosticMessage(tc.raw)
		if summary != tc.summary || file != tc.file || status != tc.status {
			t.Errorf("summary=%q resource=%q status=%q; want %q %q %q", summary, file, status, tc.summary, tc.file, tc.status)
		}
	}
	summary, _, _ := diagnosticMessage(strings.Repeat("失败", 200))
	if !utf8.ValidString(summary) || utf8.RuneCountInString(summary) != 181 {
		t.Errorf("expected a bounded UTF-8 summary: %q", summary)
	}
}
