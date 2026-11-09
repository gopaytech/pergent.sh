package engine

import (
	"testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// hideModel is how the engine tells the comment what to leave out; it needs
// no database. The base URL comes from the entry that has the run's provider
// and model.
func TestHideModel(t *testing.T) {
	e := &Engine{Config: &config.Config{LLM: config.LLM{Provider: "openai", Model: "gpt-example", BaseURL: "https://llm.example.com/v1"}}}
	str := func(s string) *string { return &s }
	const text = "Model not found: openai/gpt-example at https://llm.example.com/v1/chat"

	hide := e.hideModel(store.Run{Provider: str("openai"), Model: str("gpt-example")})
	if got, want := hide(text), "Model not found: the model at the provider/chat"; got != want {
		t.Errorf("hide(%q) = %q, want %q", text, got, want)
	}
	// A run of an older release recorded a model only.
	hide = e.hideModel(store.Run{Model: str("gpt-example")})
	if got := hide("no gpt-example"); got != "no the model" {
		t.Errorf("model only: %q", got)
	}
	if e.hideModel(store.Run{}) != nil {
		t.Error("a run with no recorded model must be left as it is")
	}
}
