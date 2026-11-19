package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestRCAConfiguration(t *testing.T) {
	for name, value := range map[string]string{
		"TEAMBITION_MODE": "mock", "DATABASE_URL": "postgres://example.invalid/adapter",
		"SENTRY_WEBHOOK_SECRET": "test-secret", "SENTRY_AUTH_TOKEN": "test-token",
		"DATABASE_URL_FILE": "", "SENTRY_WEBHOOK_SECRET_FILE": "", "SENTRY_AUTH_TOKEN_FILE": "",
		"RCA_ENABLED": "", "RCA_ALLOWED_PROJECTS": "", "AGENTOS_APP_ID": "", "AGENTOS_USER_ID": "", "AGENTOS_BASE_URL": "",
		"AGENTOS_BEARER_TOKEN": "", "AGENTOS_BEARER_TOKEN_FILE": "",
		"DINGTALK_WEBHOOK_URL": "", "DINGTALK_WEBHOOK_URL_FILE": "",
	} {
		t.Setenv(name, value)
	}
	if cfg, err := Load(); err != nil || cfg.RCAEnabled {
		t.Fatalf("expected legacy configuration to run with RCA disabled, err=%v", err)
	}
	t.Setenv("RCA_ENABLED", "enabled")
	if _, err := Load(); err == nil {
		t.Fatal("accepted an invalid RCA switch")
	}
	t.Setenv("RCA_ENABLED", "true")
	if _, err := Load(); err == nil {
		t.Fatal("accepted enabled RCA without a webhook")
	}
	path := filepath.Join(t.TempDir(), "robot")
	const robot = "https://example.invalid/robot"
	if err := os.WriteFile(path, []byte(robot+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DINGTALK_WEBHOOK_URL_FILE", path)
	cfg, err := Load()
	if err != nil || !cfg.RCAEnabled || cfg.DingTalkWebhookURL != robot || cfg.AgentOSAppID != "200004" || cfg.AgentOSUserID != "sentry-adapter" {
		t.Fatalf("could not enable RCA independently of Teambition in mock mode, err=%v", err)
	}
	if !slices.Equal(cfg.RCAAllowedProjects, []string{"anno-seg-test"}) {
		t.Fatalf("default RCA rollout = %v, want only anno-seg-test", cfg.RCAAllowedProjects)
	}
	t.Setenv("RCA_ALLOWED_PROJECTS", "anno-seg-test, anno-dev")
	cfg, err = Load()
	if err != nil || !slices.Equal(cfg.RCAAllowedProjects, []string{"anno-seg-test", "anno-dev"}) {
		t.Fatalf("explicit project rollout = %v, err=%v", cfg.RCAAllowedProjects, err)
	}
	t.Setenv("RCA_ALLOWED_PROJECTS", "*")
	cfg, err = Load()
	if err != nil || len(cfg.RCAAllowedProjects) != 0 {
		t.Fatalf("explicit all-project rollout = %v, err=%v", cfg.RCAAllowedProjects, err)
	}
	for _, invalid := range []string{"anno-seg-test,*", ",", "anno-*"} {
		t.Setenv("RCA_ALLOWED_PROJECTS", invalid)
		if _, err := Load(); err == nil {
			t.Errorf("accepted invalid rollout %q", invalid)
		}
	}
}

func TestLoadUsesEnvironmentForSentryProjectAndTeambitionCredentials(t *testing.T) {
	for name, value := range map[string]string{
		"DATABASE_URL":                         "postgres://example.invalid/adapter",
		"DATABASE_URL_FILE":                    "",
		"SENTRY_WEBHOOK_SECRET":                "test-webhook-secret",
		"SENTRY_WEBHOOK_SECRET_FILE":           "",
		"SENTRY_AUTH_TOKEN":                    "test-sentry-token",
		"SENTRY_AUTH_TOKEN_FILE":               "",
		"SENTRY_ORG":                           "example-org",
		"SENTRY_PROJECT":                       "custom-feedback-project",
		"FEEDBACK_ROUTING_FILE":                "/etc/adapter/routing.json",
		"TEAMBITION_MODE":                      "real",
		"TEAMBITION_AUTH_MODE":                 "app_token",
		"TEAMBITION_TOKEN_URL":                 "",
		"TEAMBITION_BASE_URL":                  "https://example.invalid",
		"TEAMBITION_APP_ID":                    "test-client-id",
		"TEAMBITION_APP_SECRET":                "test-client-secret",
		"TEAMBITION_APP_SECRET_FILE":           "",
		"TEAMBITION_TENANT_ID":                 "test-tenant",
		"TEAMBITION_OPERATOR_ID":               "test-operator",
		"TEAMBITION_PROJECT_ID":                "test-project",
		"TEAMBITION_SCENARIO_ID":               "shared-scenario",
		"TEAMBITION_BUG_TYPE_ID":               "old-bug-scenario",
		"TEAMBITION_FEATURE_TYPE_ID":           "old-feature-scenario",
		"TEAMBITION_CUSTOM_FIELD_ID":           "category-field",
		"TEAMBITION_CUSTOM_FIELD_OPTION_ID":    "category-option",
		"TEAMBITION_CUSTOM_FIELD_OPTION_TITLE": "标注平台",
	} {
		t.Setenv(name, value)
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.SentryOrg != "example-org" || c.SentryProject != "custom-feedback-project" {
		t.Errorf("Sentry target = %q/%q", c.SentryOrg, c.SentryProject)
	}
	if c.FeedbackRoutingFile != "/etc/adapter/routing.json" {
		t.Errorf("routing config path = %q", c.FeedbackRoutingFile)
	}
	if c.TeambitionAppID != "test-client-id" || c.TeambitionAppSecret != "test-client-secret" {
		t.Error("Teambition credentials were not loaded from environment")
	}
	if c.TeambitionAuthMode != "app_token" || c.TeambitionTokenURL != "https://example.invalid/gateway/appToken" {
		t.Error("Teambition online token configuration was not loaded")
	}
	if c.TeambitionBugTypeID != "shared-scenario" || c.TeambitionFeatureTypeID != "shared-scenario" {
		t.Error("shared Teambition scenario did not override the separate scenario IDs")
	}
	if c.TeambitionCustomFieldID != "category-field" || c.TeambitionCustomFieldOptionID != "category-option" || c.TeambitionCustomFieldTitle != "标注平台" {
		t.Error("Teambition fixed custom field configuration was not loaded")
	}
}

func TestLoadRejectsUnknownTeambitionAuthMode(t *testing.T) {
	for name, value := range map[string]string{
		"DATABASE_URL":               "postgres://example.invalid/adapter",
		"DATABASE_URL_FILE":          "",
		"SENTRY_WEBHOOK_SECRET":      "test-webhook-secret",
		"SENTRY_WEBHOOK_SECRET_FILE": "",
		"SENTRY_AUTH_TOKEN":          "test-sentry-token",
		"SENTRY_AUTH_TOKEN_FILE":     "",
		"TEAMBITION_MODE":            "real",
		"TEAMBITION_AUTH_MODE":       "invalid",
	} {
		t.Setenv(name, value)
	}
	if _, err := Load(); err == nil {
		t.Fatal("expected invalid Teambition auth mode to be rejected")
	}
}
