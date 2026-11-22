package policy

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// HideModel is text with the model and the provider a run used replaced by
// neutral words, for the review comment: a provider's error can name them
// ("Model not found: openai/my-model"), and the comment never does; the
// dashboard keeps the full text. "provider/model" and the model name become
// "the model"; each base URL and the provider name become "the provider".
// They go in that order, so the longer text is not left half replaced.
//
// A name is matched without regard to case and only as a whole word: a
// provider called "ai" doesn't cut "available". A run with no provider and no
// model recorded leaves text as it is.
func HideModel(text, provider, model string, baseURLs ...string) string {
	if provider == "" && model == "" {
		return text
	}
	if provider != "" && model != "" {
		text = replaceWord(text, provider+"/"+model, "the model")
	}
	text = replaceWord(text, model, "the model")
	for _, u := range baseURLs {
		text = replaceWord(text, strings.TrimRight(u, "/"), "the provider")
	}
	return replaceWord(text, provider, "the provider")
}

// ModelRef is a provider and a model, for HideModels.
type ModelRef struct{ Provider, Model string }

// HideModels is HideModel for the several models of a review panel: every
// "provider/model" first, then every model name, every base URL and every
// provider, each kind longest first, so a name that starts a longer one
// ("gpt-5" in "gpt-5-mini") never leaves it half replaced. A model with no
// provider and no name hides nothing.
func HideModels(text string, models []ModelRef, baseURLs []string) string {
	var pairs, names, provs []string
	for _, m := range models {
		if m.Provider != "" && m.Model != "" {
			pairs = append(pairs, m.Provider+"/"+m.Model)
		}
		names = append(names, m.Model)
		provs = append(provs, m.Provider)
	}
	urls := make([]string, 0, len(baseURLs))
	for _, u := range baseURLs {
		urls = append(urls, strings.TrimRight(u, "/"))
	}
	for _, kind := range []struct {
		names []string
		with  string
	}{{pairs, "the model"}, {names, "the model"}, {urls, "the provider"}, {provs, "the provider"}} {
		// Longest first; equal lengths in a fixed order.
		sorted := slices.Clone(kind.names)
		slices.SortStableFunc(sorted, func(a, b string) int { return len(b) - len(a) })
		for _, n := range slices.Compact(sorted) {
			text = replaceWord(text, n, kind.with)
		}
	}
	return text
}

// replaceWord is text with each occurrence of name, in any case, that no
// letter or digit touches replaced by with. An empty name replaces nothing.
func replaceWord(text, name, with string) string {
	if name == "" || len(text) < len(name) {
		return text
	}
	var b strings.Builder
	last := 0
	for i := 0; i+len(name) <= len(text); {
		if !strings.EqualFold(text[i:i+len(name)], name) || !wordEdge(text, i, i+len(name)) {
			i++
			continue
		}
		b.WriteString(text[last:i])
		b.WriteString(with)
		i += len(name)
		last = i
	}
	if last == 0 {
		return text
	}
	b.WriteString(text[last:])
	return b.String()
}

// wordEdge reports whether text[start:end] has no letter or digit right
// before or after it. A match that starts inside a multi-byte character isn't
// one.
func wordEdge(text string, start, end int) bool {
	if !utf8.RuneStart(text[start]) || (end < len(text) && !utf8.RuneStart(text[end])) {
		return false
	}
	if r, _ := utf8.DecodeLastRuneInString(text[:start]); start > 0 && isWordRune(r) {
		return false
	}
	if r, _ := utf8.DecodeRuneInString(text[end:]); end < len(text) && isWordRune(r) {
		return false
	}
	return true
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }
