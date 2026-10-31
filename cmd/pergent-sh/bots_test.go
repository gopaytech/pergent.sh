package main

import (
	"slices"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/engine"
	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/syncmap"
)

type namedForge struct {
	forge.Forge
	user, mention string
}

func (f namedForge) Bot() forge.Identity { return forge.Identity{ID: 1, Username: f.user} }
func (f namedForge) Mention() string     { return f.mention }

type namedApprover struct {
	forge.Approver
	user string
}

func (a namedApprover) Bot() forge.Identity { return forge.Identity{ID: 2, Username: a.user} }

func TestCommandBots(t *testing.T) {
	platforms := syncmap.New(map[string]engine.Platform{
		"gh": {Forge: namedForge{user: "pergent[bot]", mention: "pergent"}, Approver: namedApprover{user: "pergent-approve[bot]"}},
		"gl": {Forge: namedForge{user: "reviewer", mention: "reviewer"}},
	})
	bots := commandBots(platforms)

	mention, ignore, ok := bots("gh")
	if !ok || mention != "pergent" || !slices.Equal(ignore, []string{"pergent[bot]", "pergent-approve[bot]"}) {
		t.Errorf("gh = %q %q %v", mention, ignore, ok)
	}
	mention, ignore, ok = bots("gl")
	if !ok || mention != "reviewer" || !slices.Equal(ignore, []string{"reviewer"}) {
		t.Errorf("gl = %q %q %v", mention, ignore, ok)
	}
	if _, _, ok := bots("missing"); ok {
		t.Error("an instance that isn't connected has bots")
	}
}
