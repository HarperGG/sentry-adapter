package worker

import (
	"html"
	"regexp"
	"strings"
)

const (
	maxTitleRunes   = 100
	maxSummaryRunes = 60
)

var (
	descriptionMarker = regexp.MustCompile(`(?m)^\[问题描述\][ \t]*\r?$`)
	linkDefinition    = regexp.MustCompile(`(?m)^[ \t]{0,3}\[[^\]\r\n]+\]:[^\r\n]*`)
	bareResourceURL   = regexp.MustCompile(`(?i)(?:https?://|ftp://|cos://|s3://|www\.|blob:|data:|//)[^\s<>"'，。；！？、）】》]+`)
)

func title(taskType, datasetID, message string) string {
	summary := feedbackSummary(message)
	metadata := titleMetadata(taskType, "未知任务类型") + ": " + titleMetadata(datasetID, "未知数据集")
	// Reserve room for the actual problem even if malformed task metadata is
	// unusually long. The adapter's existing 100-rune title cap remains in force.
	metadata = truncateRunes(metadata, maxTitleRunes-len([]rune(summary))-len([]rune("【】 ")))
	return "【" + metadata + "】 " + summary
}

func titleMetadata(value, fallback string) string {
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return fallback
	}
	return value
}

func feedbackSummary(message string) string {
	if marker := descriptionMarker.FindStringIndex(message); marker != nil {
		message = message[marker[1]:]
	}
	message = html.UnescapeString(message)
	message = stripHTMLTags(message)
	message = linkDefinition.ReplaceAllString(message, " ")
	// Two passes also handle a screenshot wrapped in a Markdown link without
	// including its alt text or either resource address in the title.
	message = stripMarkdownLinks(stripMarkdownLinks(message))
	message = bareResourceURL.ReplaceAllString(message, " ")
	message = strings.Join(strings.Fields(message), " ")
	if message == "" {
		return "问题详情见反馈"
	}
	return truncateRunes(message, maxSummaryRunes)
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	if limit <= 1 {
		return "…"
	}
	return string(runes[:limit-1]) + "…"
}

// Quoted '>' characters belong to the tag's attributes, not its closing edge.
// A small scanner avoids leaking image alt/src attributes after removing tags.
func stripHTMLTags(value string) string {
	var result strings.Builder
	for index := 0; index < len(value); {
		if value[index] != '<' || !isHTMLTagStart(value, index) {
			result.WriteByte(value[index])
			index++
			continue
		}
		end := htmlTagEnd(value, index+1)
		if end < 0 {
			result.WriteString(value[index:])
			break
		}
		result.WriteByte(' ')
		index = end + 1
	}
	return result.String()
}

func isHTMLTagStart(value string, index int) bool {
	if index+1 >= len(value) {
		return false
	}
	next := value[index+1]
	return next == '/' || next == '!' || next >= 'a' && next <= 'z' || next >= 'A' && next <= 'Z'
}

func htmlTagEnd(value string, start int) int {
	var quote byte
	for index := start; index < len(value); index++ {
		current := value[index]
		if quote != 0 {
			if current == quote {
				quote = 0
			}
			continue
		}
		switch current {
		case '\'', '"':
			quote = current
		case '>':
			return index
		}
	}
	return -1
}

func stripMarkdownLinks(value string) string {
	var result strings.Builder
	pairs := markdownDelimiterPairs(value)
	for index := 0; index < len(value); {
		image := value[index] == '!' && index+1 < len(value) && value[index+1] == '['
		start := index
		if image {
			start++
		}
		if value[start] != '[' {
			result.WriteByte(value[index])
			index++
			continue
		}
		close, paired := pairs[start]
		if !paired {
			result.WriteString(value[index:])
			break
		}
		end := markdownLinkEnd(value, close+1, pairs)
		if image {
			result.WriteByte(' ')
		} else if end > close+1 {
			result.WriteString(value[start+1 : close])
		} else {
			result.WriteString(value[index : close+1])
		}
		index = end
	}
	return result.String()
}

func markdownLinkEnd(value string, start int, pairs map[int]int) int {
	if start >= len(value) {
		return start
	}
	if value[start] != '(' && value[start] != '[' {
		return start
	}
	close, paired := pairs[start]
	if !paired {
		return start
	}
	return close + 1
}

// Pair delimiters once instead of rescanning suffixes for each malformed link.
// A long sequence of unclosed "[text](" must remain linear in input length.
func markdownDelimiterPairs(value string) map[int]int {
	pairs := make(map[int]int)
	stacks := map[byte][]int{'[': nil, '(': nil}
	for index := 0; index < len(value); index++ {
		if value[index] == '\\' {
			index++
			continue
		}
		switch current := value[index]; current {
		case '[', '(':
			stacks[current] = append(stacks[current], index)
		case ']', ')':
			open := byte('[')
			if current == ')' {
				open = '('
			}
			stack := stacks[open]
			if len(stack) == 0 {
				continue
			}
			pairs[stack[len(stack)-1]] = index
			stacks[open] = stack[:len(stack)-1]
		}
	}
	return pairs
}
