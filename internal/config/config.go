package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

type Config struct {
	ListenAddr                     string
	DatabaseURL                    string
	WebhookSecret                  string
	SentryBaseURL                  string
	SentryToken                    string
	SentryOrg                      string
	SentryProject                  string
	SentryAttachmentRedirectHosts  []string
	TeambitionMode                 string
	TeambitionAuthMode             string
	TeambitionTokenURL             string
	TeambitionBaseURL              string
	TeambitionTaskPath             string
	TeambitionUploadTokenPath      string
	TeambitionAttachmentFieldID    string
	TeambitionUploadAllowedOrigins []string
	TeambitionAppID                string
	TeambitionAppSecret            string
	TeambitionTenantID             string
	TeambitionOperatorID           string
	TeambitionProjectID            string
	TeambitionScenarioID           string
	TeambitionCustomFieldID        string
	TeambitionCustomFieldOptionID  string
	TeambitionCustomFieldTitle     string
	TeambitionTasklistID           string
	TeambitionStageID              string
	TeambitionStatusID             string
	TeambitionExecutorID           string
	TeambitionBugTypeID            string
	TeambitionFeatureTypeID        string
}

func Load() (Config, error) {
	teambitionBaseURL := os.Getenv("TEAMBITION_BASE_URL")
	teambitionTokenURL := os.Getenv("TEAMBITION_TOKEN_URL")
	if teambitionTokenURL == "" && teambitionBaseURL != "" {
		teambitionTokenURL = strings.TrimRight(teambitionBaseURL, "/") + "/gateway/appToken"
	}
	c := Config{
		ListenAddr:                     fallback(os.Getenv("LISTEN_ADDR"), ":8787"),
		DatabaseURL:                    "",
		SentryBaseURL:                  fallback(os.Getenv("SENTRY_BASE_URL"), "https://sentry.gwm-adas.com"),
		SentryOrg:                      fallback(os.Getenv("SENTRY_ORG"), "sentry"),
		SentryProject:                  fallback(os.Getenv("SENTRY_PROJECT"), "anno-seg-test"),
		SentryAttachmentRedirectHosts:  splitHosts(os.Getenv("SENTRY_ATTACHMENT_REDIRECT_HOSTS")),
		TeambitionMode:                 fallback(os.Getenv("TEAMBITION_MODE"), "mock"),
		TeambitionAuthMode:             fallback(os.Getenv("TEAMBITION_AUTH_MODE"), "app_token"),
		TeambitionTokenURL:             teambitionTokenURL,
		TeambitionBaseURL:              teambitionBaseURL,
		TeambitionTaskPath:             fallback(os.Getenv("TEAMBITION_TASK_PATH"), "/gateway/v3/task/create"),
		TeambitionUploadTokenPath:      fallback(os.Getenv("TEAMBITION_UPLOAD_TOKEN_PATH"), "/gateway/v3/awos/upload-token"),
		TeambitionAttachmentFieldID:    os.Getenv("TEAMBITION_ATTACHMENT_FIELD_ID"),
		TeambitionUploadAllowedOrigins: splitHosts(os.Getenv("TEAMBITION_UPLOAD_ALLOWED_ORIGINS")),
		TeambitionAppID:                os.Getenv("TEAMBITION_APP_ID"),
		TeambitionTenantID:             os.Getenv("TEAMBITION_TENANT_ID"),
		TeambitionOperatorID:           os.Getenv("TEAMBITION_OPERATOR_ID"),
		TeambitionProjectID:            os.Getenv("TEAMBITION_PROJECT_ID"),
		TeambitionScenarioID:           os.Getenv("TEAMBITION_SCENARIO_ID"),
		TeambitionCustomFieldID:        os.Getenv("TEAMBITION_CUSTOM_FIELD_ID"),
		TeambitionCustomFieldOptionID:  os.Getenv("TEAMBITION_CUSTOM_FIELD_OPTION_ID"),
		TeambitionCustomFieldTitle:     os.Getenv("TEAMBITION_CUSTOM_FIELD_OPTION_TITLE"),
		TeambitionTasklistID:           os.Getenv("TEAMBITION_TASKLIST_ID"),
		TeambitionStageID:              os.Getenv("TEAMBITION_STAGE_ID"),
		TeambitionStatusID:             os.Getenv("TEAMBITION_TASKFLOWSTATUS_ID"),
		TeambitionExecutorID:           os.Getenv("TEAMBITION_EXECUTOR_ID"),
		TeambitionBugTypeID:            os.Getenv("TEAMBITION_BUG_TYPE_ID"),
		TeambitionFeatureTypeID:        os.Getenv("TEAMBITION_FEATURE_TYPE_ID"),
	}
	if c.TeambitionScenarioID != "" {
		c.TeambitionBugTypeID = c.TeambitionScenarioID
		c.TeambitionFeatureTypeID = c.TeambitionScenarioID
	}
	var err error
	if c.DatabaseURL, err = secret("DATABASE_URL"); err != nil {
		return c, err
	}
	if c.WebhookSecret, err = secret("SENTRY_WEBHOOK_SECRET"); err != nil {
		return c, err
	}
	if c.SentryToken, err = secret("SENTRY_AUTH_TOKEN"); err != nil {
		return c, err
	}
	if c.TeambitionAppSecret, err = secret("TEAMBITION_APP_SECRET"); err != nil {
		return c, err
	}
	for name, value := range map[string]string{
		"DATABASE_URL":          c.DatabaseURL,
		"SENTRY_WEBHOOK_SECRET": c.WebhookSecret,
		"SENTRY_AUTH_TOKEN":     c.SentryToken,
	} {
		if strings.TrimSpace(value) == "" {
			return c, fmt.Errorf("%s is required", name)
		}
	}
	if c.TeambitionMode != "mock" && c.TeambitionMode != "real" {
		return c, errors.New("TEAMBITION_MODE must be mock or real")
	}
	if c.TeambitionMode == "real" {
		if c.TeambitionAuthMode != "app_token" && c.TeambitionAuthMode != "local_jwt" {
			return c, errors.New("TEAMBITION_AUTH_MODE must be app_token or local_jwt")
		}
		for name, value := range map[string]string{
			"TEAMBITION_BASE_URL":    c.TeambitionBaseURL,
			"TEAMBITION_APP_ID":      c.TeambitionAppID,
			"TEAMBITION_APP_SECRET":  c.TeambitionAppSecret,
			"TEAMBITION_TENANT_ID":   c.TeambitionTenantID,
			"TEAMBITION_OPERATOR_ID": c.TeambitionOperatorID,
			"TEAMBITION_PROJECT_ID":  c.TeambitionProjectID,
		} {
			if strings.TrimSpace(value) == "" {
				return c, fmt.Errorf("%s is required in real mode", name)
			}
		}
	}
	return c, nil
}

func fallback(value, defaultValue string) string {
	if value == "" {
		return defaultValue
	}
	return value
}

func splitHosts(value string) []string {
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if h := strings.TrimSpace(part); h != "" {
			out = append(out, h)
		}
	}
	return out
}

func secret(name string) (string, error) {
	value := os.Getenv(name)
	path := os.Getenv(name + "_FILE")
	if value != "" && path != "" {
		return "", fmt.Errorf("set only one of %s and %s_FILE", name, name)
	}
	if path == "" {
		return strings.TrimSpace(value), nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s_FILE: %w", name, err)
	}
	return strings.TrimSpace(string(b)), nil
}
