package opsearch

import (
	"regexp"
	"strings"
	"unicode"
)

var camelBoundary = regexp.MustCompile(`([a-z0-9])([A-Z])`)

var stopWords = map[string]bool{
	"a": true, "an": true, "the": true, "to": true, "of": true, "for": true, "in": true, "on": true, "my": true,
	"me": true, "i": true, "and": true, "or": true, "with": true, "from": true, "by": true, "is": true, "am": true,
	"all": true, "how": true, "do": true, "can": true, "that": true, "this": true, "it": true, "at": true, "as": true,
}

// synonyms widen a query word to the words the API descriptions use for the same thing.
var synonyms = map[string][]string{
	"mail":         {"gmail", "message"},
	"email":        {"gmail", "message"},
	"inbox":        {"gmail", "message"},
	"event":        {"calendar"},
	"meeting":      {"calendar", "event"},
	"appointment":  {"calendar", "event"},
	"folder":       {"drive", "file"},
	"upload":       {"drive", "file", "create"},
	"download":     {"get", "media"},
	"share":        {"permission", "create"},
	"doc":          {"docs", "document"},
	"document":     {"docs"},
	"room":         {"chat", "space"},
	"conversation": {"chat", "space"},
	"channel":      {"chat", "space"},
	"remove":       {"delete"},
	"search":       {"list"},
	"find":         {"list"},
	"read":         {"get"},
	"show":         {"get", "list"},
	"new":          {"create", "insert"},
	"add":          {"create", "insert"},
	"create":       {"insert"},
	"free":         {"freebusy"},
	"busy":         {"freebusy"},
	"post":         {"create"},
	"edit":         {"update", "patch", "batchupdate"},
	"change":       {"update", "patch"},
}

// softWords are words people use for a whole app ("email" for Gmail); matching them literally in a
// description says little, so they count for less than the words they stand for.
var softWords = map[string]bool{"mail": true, "email": true, "inbox": true}

// tokenize lowercases text, splits camelCase and punctuation, drops stop words and stems.
func tokenize(text string) []string {
	text = camelBoundary.ReplaceAllString(text, "$1 $2")
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if stopWords[f] {
			continue
		}
		out = append(out, stem(f))
	}
	return out
}

// stem folds plurals and common endings so "messages" and "message", "deleting" and "delete" meet.
func stem(w string) string {
	switch {
	case len(w) > 5 && strings.HasSuffix(w, "ing"):
		w = w[:len(w)-3]
	case len(w) > 4 && strings.HasSuffix(w, "ed"):
		w = w[:len(w)-2]
	case len(w) > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss"):
		w = w[:len(w)-1]
	}
	if len(w) > 4 && strings.HasSuffix(w, "e") {
		w = w[:len(w)-1]
	}
	return w
}

// queryTerms is the stemmed query with weight 1, plus its synonyms at a lower weight.
func queryTerms(query string) map[string]float64 {
	terms := map[string]float64{}
	for _, t := range tokenize(query) {
		terms[t] = 1
		if softWords[t] {
			terms[t] = synonymWeight
		}
	}
	for _, f := range strings.FieldsFunc(strings.ToLower(query), func(r rune) bool { return !unicode.IsLetter(r) }) {
		for _, s := range synonyms[f] {
			if st := stem(s); terms[st] == 0 {
				terms[st] = synonymWeight
				if softWords[f] {
					terms[st] = softSynonymWeight
				}
			}
		}
	}
	return terms
}
