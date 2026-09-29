package config

import "testing"

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
