package specimport

import (
	"regexp"
	"strings"
)

const (
	summaryLimit = 200
	paramLimit   = 160
)

var (
	markdownLink = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	whitespace   = regexp.MustCompile(`\s+`)
)

// firstSentence is the first sentence of Markdown prose as one plain line of at most limit runes.
func firstSentence(markdown string, limit int) string {
	text := whitespace.ReplaceAllString(markdownLink.ReplaceAllString(markdown, "$1"), " ")
	text = strings.TrimSpace(text)
	text = leadingSentences(text)
	if runes := []rune(text); len(runes) > limit {
		text = strings.TrimSpace(string(runes[:limit-1])) + "…"
	}
	return text
}

// fieldLabels are the one-word prefixes Google puts on field descriptions before the sentence that says something.
var fieldLabels = map[string]bool{"Optional.": true, "Required.": true, "Output only.": true, "Immutable.": true, "Identifier.": true, "Input only.": true}

// leadingSentences cuts text after its first sentence, keeping any label sentences in front of it.
func leadingSentences(text string) string {
	end := 0
	for {
		i := strings.Index(text[end:], ". ")
		if i < 0 {
			return text
		}
		end += i + 1
		if !fieldLabels[strings.TrimSpace(text[labelStart(text, end):end])] {
			return text[:end]
		}
		end++
	}
}

func labelStart(text string, end int) int {
	if i := strings.LastIndex(text[:end-1], ". "); i >= 0 {
		return i + 2
	}
	return 0
}
