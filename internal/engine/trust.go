package engine

import (
	"context"
	"log/slog"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/settings"
)

// The trust check's waits: a check that fails is tried again every
// trustRetryDelay until trustPatience after the run was queued, then the run
// waits with the check failed. Vars, so tests can shorten them.
var (
	trustPatience   = 10 * time.Minute
	trustRetryDelay = 30 * time.Second
)

// waitingDescription is the pending status of an untrusted author's head.
const waitingDescription = "Waiting for someone with write access to start the review"

// checkFailedDescription is the pending status of a head whose author's
// access couldn't be read.
func checkFailedDescription(mention string) string {
	return "Couldn't check the author's access. Someone with write access can start the review with @" + mention + " review"
}

// untrustedNote is the one note on an untrusted author's change.
func untrustedNote(mention string) string {
	return "Not reviewed automatically: the author isn't a trusted author of this repository. " +
		"Someone with write access can start a review with `@" + mention + " review`."
}

// authorTrust reports whether the repository trusts the change's author to
// have it reviewed without anyone starting it. It does when the repository's
// mode is off (follow_visibility: when the repository is private or internal), when the
// author's ID is on the repository's allowlist, or when the author has write
// access or better. A visibility that can't be read counts as public; a mode
// it doesn't know counts as on. err is only an error reading the author's
// access: the caller must not take it for trust. log gets the visibility
// error, which isn't returned, through redact.
func authorTrust(ctx context.Context, f forge.Forge, repo settings.Effective, projectID int64, projectPath string, ch forge.Change,
	log *slog.Logger, redact func(string) string) (bool, error) {
	switch repo.TrustedAuthorsOnly {
	case config.TrustOff:
		return true, nil
	case config.TrustFollowVisibility:
		vis, err := f.Visibility(ctx, projectPath)
		if err == nil && (vis == forge.VisPrivate || vis == forge.VisInternal) {
			return true, nil
		}
		if err != nil {
			log.Warn("the repository's visibility can't be read; it counts as public", "error", redact(err.Error()))
		}
	}
	if ch.AuthorID == 0 {
		return false, nil
	}
	for _, a := range repo.TrustedAuthors {
		if a.ID == ch.AuthorID {
			return true, nil
		}
	}
	access, err := f.AccessLevel(ctx, projectID, ch.Author, ch.AuthorID)
	if err != nil {
		return false, err
	}
	return access >= forge.AccessWrite, nil
}
