package engine

import (
	"context"

	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/security"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// securityDroppedMsg is logged when the inputs have no room for
// security-findings.md.
const securityDroppedMsg = "reviewing without the open security findings: the inputs are too large for them"

// addSecurityFindings gives the review the repository's open security
// findings in the files the change touches, as security-findings.md, when the
// repository's "use security findings in reviews" switch is on. The judge is
// given none. It is a hint, so it never fails or holds the review: any error
// leaves the file out and logs a warning. A finding is only read here; its
// state never changes.
func (d *dispatcher) addSecurityFindings(ctx context.Context, plat Platform, repo settings.Effective, mr forge.Change, last store.Run, in *inputs.Inputs) {
	if d.stage == stageJudge {
		return
	}
	set, err := d.e.Store.SecurityScanSettingsOf(ctx, repo.Instance, repo.Project)
	if err != nil {
		d.log.Warn("reviewing without the open security findings: the settings can't be read", "error", d.redact(err.Error()))
		return
	}
	if !set.FindingsInReviews {
		return
	}
	changed, err := d.changedForLinks(ctx, plat, mr, last, in)
	if err != nil {
		d.log.Warn("reviewing without the open security findings: the changed files can't be read", "error", d.redact(err.Error()))
		return
	}
	found, err := d.e.Store.OpenSecurityFindingsIn(ctx, repo.Instance, repo.Project, changed)
	if err != nil {
		d.log.Warn("reviewing without the open security findings: they can't be read", "error", d.redact(err.Error()))
		return
	}
	if b := security.ReviewInput(found, changed); len(b) > 0 {
		in.SecurityFindings = []byte(d.redact(d.e.redact(string(b))))
	}
}
