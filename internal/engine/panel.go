package engine

import (
	"context"
	"errors"
	"fmt"
)

// reasonInputsChanged fails a member whose dispatch would change what its run
// was dispatched with: a member never writes its run.
const reasonInputsChanged = "the review's inputs changed while the panel ran"

// errInputsChanged is how subset says it would write to a member's run.
var errInputsChanged = errors.New(reasonInputsChanged)

// settleLegacyPanels settles the reported members of a review panel a main
// build left (panel_members), as far as it is safe: a member with no result
// fails (SettleMember), and a review whose members are all final is decided
// by ReleasePanel, which fails it when both failed. A member's result is
// never checked or merged from here: one with a result stays reported, for
// the recovery at the next start (RecoverLegacyPanels). This keeps a legacy
// API review whose members failed at dispatch from waiting for ever. M4.2
// removes it with the panel tables.
func (e *Engine) settleLegacyPanels(ctx context.Context) error {
	ms, err := e.Store.MembersToSettle(ctx, settleBatch)
	if err != nil {
		return fmt.Errorf("list the reported legacy panel members: %w", err)
	}
	for _, m := range ms {
		if m.Result != nil {
			continue
		}
		if _, err := e.Store.SettleMember(ctx, m.ID); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			e.log().Warn("couldn't settle a legacy panel member; trying again on the next pass",
				"run", m.RunID, "member", m.ID, "role", m.Role, "error", e.redact(err.Error()))
		}
	}
	roots, err := e.Store.PanelsToRelease(ctx)
	if err != nil {
		return fmt.Errorf("list the legacy panels to release: %w", err)
	}
	for _, root := range roots {
		log := e.log().With("run", root)
		released, failed, err := e.Store.ReleasePanel(ctx, root, e.now())
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return nil
			}
			log.Warn("couldn't release the legacy review panel; trying again on the next pass", "error", e.redact(err.Error()))
		case released, failed:
			log.Info("a legacy review panel's members are final", "released", released, "failed", failed)
			e.Wake()
		}
	}
	return nil
}

// deref is *s, "" for nil.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
