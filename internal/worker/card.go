package worker

import (
	"strings"

	"sentry-adapter/internal/routing"
	"sentry-adapter/internal/sentry"
	"sentry-adapter/internal/teambition"
)

func cardFromFeedback(f sentry.Feedback, decision routing.Decision) teambition.Card {
	description := strings.TrimSpace(f.Message) + "\n\n" + strings.TrimSpace(f.IssueURL)
	username := strings.Join(strings.Fields(f.Username), " ")
	if username != "" {
		description += "\n\n用户名：" + username
	}
	return teambition.Card{
		SourceIssueID: f.IssueID,
		Title:         title(f.TaskType, f.DatasetID, f.Message),
		Description:   description,
		FeedbackType:  decision.FeedbackType,
		TagIDs:        []string{decision.TagID},
		ExecutorID:    decision.ExecutorID,
	}
}
