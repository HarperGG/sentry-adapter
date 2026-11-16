// Package routing resolves business feedback categories and single task owners
// from deployment configuration, independently of Teambition's task schema.
package routing

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

var identifierPattern = regexp.MustCompile(`^[a-fA-F0-9]{24}$`)

var feedbackTypes = [...]string{"platform_bug", "data_issue", "suggestion", "other"}

type fileConfig struct {
	FeedbackTags              map[string]string `json:"feedbackTags"`
	EnvironmentTags           map[string]string `json:"environmentTags,omitempty"`
	CategoryOwners            map[string]string `json:"categoryOwners,omitempty"`
	DefaultPlatformExecutorID string            `json:"defaultPlatformExecutorId"`
	Owners                    []ownerConfig     `json:"owners"`
	SuggestionOwners          []ownerConfig     `json:"suggestionOwners,omitempty"`
}

type ownerConfig struct {
	Name       string   `json:"name"`
	ExecutorID string   `json:"executorId"`
	TaskTypes  []string `json:"taskTypes"`
}

// Rules is compiled once at startup. Resolve only reads these maps, so a config
// change takes effect after restarting the adapter, never midway through a job.
type Rules struct {
	feedbackTags              map[string]string
	environmentTags           map[string]string
	categoryOwners            map[string]string
	defaultPlatformExecutorID string
	owners                    map[string]string
	suggestionOwners          map[string]string
}

type Decision struct {
	FeedbackType     string
	TagID            string
	EnvironmentTagID string
	ExecutorID       string
	ReviewReason     string
}

func Load(path string) (*Rules, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open feedback routing config: %w", err)
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var config fileConfig
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("decode feedback routing config: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("feedback routing config must contain exactly one JSON object")
	}
	return compile(config)
}

func compile(config fileConfig) (*Rules, error) {
	tags, err := validateTags(config.FeedbackTags)
	if err != nil {
		return nil, err
	}
	environmentTags, err := validateEnvironmentTags(config.EnvironmentTags)
	if err != nil {
		return nil, err
	}
	categoryOwners, err := validateCategoryOwners(config.CategoryOwners)
	if err != nil {
		return nil, err
	}
	defaultPlatformExecutorID := strings.TrimSpace(config.DefaultPlatformExecutorID)
	if !identifierPattern.MatchString(defaultPlatformExecutorID) {
		return nil, errors.New("defaultPlatformExecutorId must be one 24-character hexadecimal user ID")
	}
	owners := make(map[string]string)
	for index, owner := range config.Owners {
		if err := addOwner(owners, owner); err != nil {
			return nil, fmt.Errorf("feedback owner %d: %w", index+1, err)
		}
	}
	suggestionOwners := make(map[string]string)
	for index, owner := range config.SuggestionOwners {
		if err := addOwner(suggestionOwners, owner); err != nil {
			return nil, fmt.Errorf("suggestion owner %d: %w", index+1, err)
		}
	}
	return &Rules{
		feedbackTags: tags, environmentTags: environmentTags, categoryOwners: categoryOwners,
		defaultPlatformExecutorID: defaultPlatformExecutorID,
		owners:                    owners, suggestionOwners: suggestionOwners,
	}, nil
}

func validateEnvironmentTags(input map[string]string) (map[string]string, error) {
	tags := make(map[string]string, len(input))
	for cloudServer, value := range input {
		key := strings.ToUpper(strings.TrimSpace(cloudServer))
		if key == "" {
			return nil, errors.New("environmentTags must not contain an empty cloud server")
		}
		if key != "TC" && key != "HS" {
			return nil, fmt.Errorf("environmentTags.%s is unsupported; only TC and HS cloud servers are supported", cloudServer)
		}
		if _, exists := tags[key]; exists {
			return nil, fmt.Errorf("cloud server %q has more than one tag entry", cloudServer)
		}
		id := strings.TrimSpace(value)
		if !identifierPattern.MatchString(id) {
			return nil, fmt.Errorf("environmentTags.%s must be a 24-character hexadecimal tag ID", cloudServer)
		}
		tags[key] = id
	}
	return tags, nil
}

func validateCategoryOwners(input map[string]string) (map[string]string, error) {
	owners := make(map[string]string, len(input))
	for kind, value := range input {
		if kind != "data_issue" && kind != "suggestion" {
			return nil, fmt.Errorf("categoryOwners.%s is unsupported; only data_issue and suggestion allow fixed owners", kind)
		}
		id := strings.TrimSpace(value)
		if !identifierPattern.MatchString(id) {
			return nil, fmt.Errorf("categoryOwners.%s must be one 24-character hexadecimal user ID", kind)
		}
		owners[kind] = id
	}
	return owners, nil
}

func validateTags(input map[string]string) (map[string]string, error) {
	if len(input) != len(feedbackTypes) {
		return nil, errors.New("feedbackTags must define exactly platform_bug, data_issue, suggestion, and other")
	}
	tags := make(map[string]string, len(feedbackTypes))
	seenIDs := make(map[string]bool, len(feedbackTypes))
	for _, kind := range feedbackTypes {
		id := strings.TrimSpace(input[kind])
		if !identifierPattern.MatchString(id) {
			return nil, fmt.Errorf("feedbackTags.%s must be a 24-character hexadecimal ID", kind)
		}
		if seenIDs[strings.ToLower(id)] {
			return nil, errors.New("each feedback category must have a distinct tag ID")
		}
		seenIDs[strings.ToLower(id)] = true
		tags[kind] = id
	}
	return tags, nil
}

func addOwner(owners map[string]string, owner ownerConfig) error {
	if strings.TrimSpace(owner.Name) == "" {
		return errors.New("name is required")
	}
	id := strings.TrimSpace(owner.ExecutorID)
	if !identifierPattern.MatchString(id) {
		return errors.New("executorId must be one 24-character hexadecimal user ID")
	}
	if len(owner.TaskTypes) == 0 {
		return errors.New("taskTypes must not be empty")
	}
	for _, taskType := range owner.TaskTypes {
		key := normalize(taskType)
		if key == "" {
			return errors.New("taskTypes must not contain empty values")
		}
		if _, exists := owners[key]; exists {
			return fmt.Errorf("task type %q has more than one owner entry", taskType)
		}
		owners[key] = id
	}
	return nil
}

func (r *Rules) Resolve(feedbackType, taskType, cloudServer string) Decision {
	kind, reason := normalizeFeedbackType(feedbackType)
	decision := Decision{FeedbackType: kind, ReviewReason: reason}
	if r == nil {
		decision.ReviewReason = "feedback routing configuration is unavailable"
		return decision
	}
	decision.TagID = r.feedbackTags[kind]
	decision.EnvironmentTagID = r.environmentTags[strings.ToUpper(strings.TrimSpace(cloudServer))]
	// Suggestion ownership is separate from platform ownership. Its fixed
	// category owner handles unknown task types and missing task metadata.
	if kind == "data_issue" || kind == "suggestion" {
		if kind == "suggestion" {
			decision.ExecutorID = r.suggestionOwners[normalize(taskType)]
		}
		if decision.ExecutorID == "" {
			decision.ExecutorID = r.categoryOwners[kind]
		}
		if decision.ExecutorID == "" {
			decision.ReviewReason = "feedback category has no configured fixed owner; assignment requires manual triage"
		}
		return decision
	}
	if kind != "platform_bug" {
		return decision
	}
	key := normalize(taskType)
	decision.ExecutorID = r.owners[key]
	if decision.ExecutorID == "" {
		decision.ExecutorID = r.defaultPlatformExecutorID
	}
	return decision
}

func normalizeFeedbackType(value string) (string, string) {
	switch normalize(value) {
	case "platform_bug", "bug", "defect":
		return "platform_bug", ""
	case "data_issue", "other":
		return normalize(value), ""
	case "suggestion", "feature", "request":
		return "suggestion", ""
	case "feature_gap":
		return "other", "legacy feature_gap requires manual triage"
	case "":
		return "other", "feedback type is missing; manual triage required"
	default:
		return "other", "feedback type is unknown; manual triage required"
	}
}

func normalize(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}
