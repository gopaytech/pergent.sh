package policy

import (
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/result"
)

func TestHideModel(t *testing.T) {
	const gateway = "https://llm-gateway.example.com/v1"
	for name, tc := range map[string]struct {
		text, provider, model string
		urls                  []string
		want                  string
	}{
		"provider error naming both": {
			"opencode reported UnknownError: Model not found: openai/my-model.", "openai", "my-model", nil,
			"opencode reported UnknownError: Model not found: the model.",
		},
		"model alone": {"model my-model is overloaded", "openai", "my-model", nil, "model the model is overloaded"},
		"provider alone": {
			"OpenAI returned 500", "openai", "my-model", nil, "the provider returned 500",
		},
		"base URL before the provider in it": {
			"POST https://api.openai.com/v1/chat failed", "openai", "m", []string{"https://api.openai.com/v1/"},
			"POST the provider/chat failed",
		},
		"base URL of a gateway": {"dial " + gateway + ": refused", "custom", "m", []string{gateway}, "dial the provider: refused"},
		"case and repeats":      {"MY-MODEL, my-model", "p", "My-Model", nil, "the model, the model"},
		"whole word only":       {"the available cases", "ai", "x", nil, "the available cases"},
		"neighbours that are letters": {
			"gpt-4o is not gpt-4", "openai", "gpt-4", nil, "gpt-4o is not the model",
		},
		"nothing recorded":     {"Model not found: openai/my-model.", "", "", []string{gateway}, "Model not found: openai/my-model."},
		"only a model":         {"no such my-model here", "", "my-model", nil, "no such the model here"},
		"text without a name":  {"timed out after 5m", "openai", "my-model", nil, "timed out after 5m"},
		"multi-byte neighbour": {"é-model é", "p", "model", nil, "é-the model é"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := HideModel(tc.text, tc.provider, tc.model, tc.urls...); got != tc.want {
				t.Errorf("HideModel(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

// Every way a failure reaches the comment goes through Hide, and the other
// text is untouched.
func TestDecideHidesTheModelInFailures(t *testing.T) {
	hide := func(s string) string { return HideModel(s, "openai", "my-model") }
	const leak = "Model not found: openai/my-model"
	failing := completed(result.Skill{Name: "a", Verdict: "failed", Failures: []result.Failure{{Files: []string{"x.go"}, Message: "opencode reported UnknownError: " + leak + "."}}})
	// A failed verification is told in a note as well.
	noted := skill("a", "no_blocking_issues", "x.go")
	noted.Notes = []string{"Verification failed: " + leak + "."}
	for name, o := range map[string]Outcome{
		"batch failure": {Result: failing},
		"note":          {Result: completed(noted)},
		"run failure":   {FailureReason: "the Job said: " + leak},
		"error message": {Result: &result.Result{SchemaVersion: 1, Status: "error", Error: &result.Error{Code: "x", Message: leak}}},
		"error comment": {Result: &result.Result{SchemaVersion: 1, Status: "error", Error: &result.Error{Code: "x", Message: "m"}, Comment: result.Comment{Markdown: Marker + "\n" + leak}}},
	} {
		t.Run(name, func(t *testing.T) {
			plain := Decide(o, config.FailureStatusFailed)
			if !strings.Contains(plain.Comment, leak) {
				t.Fatalf("the fixture's comment doesn't carry the text to hide:\n%s", plain.Comment)
			}
			o.Hide = hide
			d := Decide(o, config.FailureStatusFailed)
			if strings.Contains(d.Comment, "openai") || strings.Contains(d.Comment, "my-model") {
				t.Errorf("the comment names the model or provider:\n%s", d.Comment)
			}
			if !strings.Contains(d.Comment, "Model not found: the model") {
				t.Errorf("the comment lost the rest of the text:\n%s", d.Comment)
			}
			if d.Kind != plain.Kind || d.Status != plain.Status || d.Description != plain.Description {
				t.Errorf("the decision changed: %+v, was %+v", d, plain)
			}
		})
	}
}
