package main

import (
	"io"
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/inputs"
)

func TestParseArgs(t *testing.T) {
	if got, err := parseArgs([]string{"serve"}, io.Discard); err != nil || got != "/etc/pergent-sh/config.yaml" {
		t.Errorf("serve = %q, %v; want the default path", got, err)
	}
	if got, err := parseArgs([]string{"serve", "--config", "/tmp/c.yaml"}, io.Discard); err != nil || got != "/tmp/c.yaml" {
		t.Errorf("serve --config = %q, %v", got, err)
	}
	for _, args := range [][]string{nil, {"run"}, {"serve", "extra"}, {"serve", "--nope"}} {
		if _, err := parseArgs(args, io.Discard); err == nil {
			t.Errorf("parseArgs(%q) succeeded, want an error", args)
		}
	}
}

func TestCheckSkills(t *testing.T) {
	cfg := &config.Config{SkillsDir: "/skills", Repositories: []config.Repository{
		{GitLab: "main", Project: "acme/widgets", Skills: []string{"security"}},
		{GitLab: "main", Project: "acme/gadgets"},
	}}
	if err := checkSkills(cfg, inputs.Library{"security": nil}); err != nil {
		t.Errorf("checkSkills = %v, want nil", err)
	}
	err := checkSkills(cfg, inputs.Library{"style": nil})
	if err == nil || !strings.Contains(err.Error(), `skill "security"`) || !strings.Contains(err.Error(), "acme/widgets") {
		t.Errorf("checkSkills = %v, want an error naming the skill and repository", err)
	}
}
