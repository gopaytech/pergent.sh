package githubforge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/github"
)

const (
	// dismissMessage is the message of every review pergent.sh dismisses.
	dismissMessage = "pergent: the reviewed commit or target changed"
	// expiryWarnAhead is how close to its expiry the approve token is warned
	// about, and expiryWarnEvery how often at most.
	expiryWarnAhead = 14 * 24 * time.Hour
	expiryWarnEvery = time.Hour
)

// Approver is the approve side of one GitHub instance: the approve App's
// installation on each repository (NewAppApprover) or a bot user's token
// (NewTokenApprover). Either way its approvals are reviews by one bot user,
// which is how they are found again.
type Approver struct {
	repos Repos
	// bot is the approve side's bot user.
	bot int64

	// clientFor returns the client that approves on r.
	clientFor func(ctx context.Context, r Repo) (*github.Client, error)
	// failed is told about an error of a call through r's client.
	failed func(r Repo, err error)
	// reach reports whether the approve side can reach owner/name.
	reach func(ctx context.Context, owner, name string) (bool, error)
	// done runs after every call through a client.
	done func(c *github.Client)

	// The token approver's expiry warning.
	now      func() time.Time
	log      *slog.Logger
	mu       sync.Mutex
	lastWarn time.Time
	// onExpiry is told when what GitHub says about the expiry changes (see
	// OnTokenExpiry); told* is what it was last told.
	onExpiry    func(expires time.Time, soon bool)
	told        bool
	toldSoon    bool
	toldExpires time.Time
}

// NewAppApprover returns the approve App's Approver. botUserID is the App's
// bot user (app.BotUserID), resolved once by the caller. The App's
// installation on a repository is looked up once and remembered; it is
// forgotten when a call through it fails with 404 or its token can't be
// minted, and CanReach always asks GitHub afresh and remembers the answer.
func NewAppApprover(app *github.App, botUserID int64, repos Repos) *Approver {
	var (
		mu    sync.Mutex
		known = map[string]int64{}
	)
	key := func(owner, name string) string { return strings.ToLower(owner + "/" + name) }
	forget := func(owner, name string) {
		mu.Lock()
		delete(known, key(owner, name))
		mu.Unlock()
	}
	lookup := func(ctx context.Context, owner, name string) (int64, error) {
		id, err := app.Installation(ctx, owner, name)
		if errors.Is(err, github.ErrNotFound) {
			forget(owner, name)
		}
		if err != nil {
			return 0, err
		}
		mu.Lock()
		known[key(owner, name)] = id
		mu.Unlock()
		return id, nil
	}
	installation := func(ctx context.Context, owner, name string) (int64, error) {
		mu.Lock()
		id, ok := known[key(owner, name)]
		mu.Unlock()
		if ok {
			return id, nil
		}
		return lookup(ctx, owner, name)
	}
	a := newApprover(botUserID, repos)
	a.clientFor = func(ctx context.Context, r Repo) (*github.Client, error) {
		id, err := installation(ctx, r.Owner, r.Name)
		if err != nil {
			return nil, fmt.Errorf("approve App installation: %w", err)
		}
		return app.Client(id), nil
	}
	a.failed = func(r Repo, err error) {
		if errors.Is(err, github.ErrNotFound) || errors.Is(err, github.ErrInstallation) {
			forget(r.Owner, r.Name)
		}
	}
	a.reach = func(ctx context.Context, owner, name string) (bool, error) {
		_, err := lookup(ctx, owner, name)
		if errors.Is(err, github.ErrNotFound) {
			return false, nil
		}
		return err == nil, err
	}
	return a
}

// NewTokenApprover returns the Approver of a bot user whose token c holds.
// botUserID is that user (c.CurrentUser), resolved once by the caller. When
// GitHub reports the token expires within 14 days, it logs a warning at most
// once an hour; log may be nil.
func NewTokenApprover(c *github.Client, botUserID int64, repos Repos, log *slog.Logger) *Approver {
	a := newApprover(botUserID, repos)
	if log != nil {
		a.log = log
	}
	a.clientFor = func(context.Context, Repo) (*github.Client, error) { return c, nil }
	a.reach = func(ctx context.Context, owner, name string) (bool, error) {
		_, err := c.Repo(ctx, owner, name)
		a.done(c)
		if status(err) == http.StatusNotFound {
			return false, nil
		}
		return err == nil, err
	}
	a.done = a.warnExpiry
	return a
}

func newApprover(botUserID int64, repos Repos) *Approver {
	return &Approver{
		repos:  repos,
		bot:    botUserID,
		failed: func(Repo, error) {},
		done:   func(*github.Client) {},
		now:    time.Now,
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// OnTokenExpiry sets fn, called when a token approver learns something new
// about its token's expiry: the first time GitHub reports one, when the token
// turns soon-to-expire (within 14 days) or stops being so, and when the
// expiry moves while it is. fn must not block for long. It never sees the
// token.
func (a *Approver) OnTokenExpiry(fn func(expires time.Time, soon bool)) {
	a.mu.Lock()
	a.onExpiry = fn
	a.mu.Unlock()
}

// noteExpiry tells onExpiry about exp when it differs from what it was told.
func (a *Approver) noteExpiry(exp time.Time, soon bool) {
	a.mu.Lock()
	fn := a.onExpiry
	changed := !a.told || a.toldSoon != soon || (soon && !a.toldExpires.Equal(exp))
	if fn != nil && changed {
		a.told, a.toldSoon, a.toldExpires = true, soon, exp
	}
	a.mu.Unlock()
	if fn != nil && changed {
		fn(exp, soon)
	}
}

// warnExpiry logs a warning when c's token expires within 14 days, at most
// once an hour, and reports the expiry to OnTokenExpiry. The token itself is
// never logged.
func (a *Approver) warnExpiry(c *github.Client) {
	exp, ok := c.TokenExpiry()
	if !ok {
		return
	}
	now := a.now()
	soon := exp.Sub(now) <= expiryWarnAhead
	a.noteExpiry(exp, soon)
	if !soon {
		return
	}
	a.mu.Lock()
	if !a.lastWarn.IsZero() && now.Sub(a.lastWarn) < expiryWarnEvery {
		a.mu.Unlock()
		return
	}
	a.lastWarn = now
	a.mu.Unlock()
	a.log.Warn("the GitHub approve token expires soon; rotate it", "expires_at", exp.UTC().Format(time.RFC3339))
}

// CanReach reports whether the approve side can reach owner/name: the
// approve App is installed there, or the token can read the repository. It
// always asks GitHub. Only a 404 is "can't reach"; any other failure is an
// error.
func (a *Approver) CanReach(ctx context.Context, owner, name string) (bool, error) {
	return a.reach(ctx, owner, name)
}

// call runs fn with the client for repoID.
func (a *Approver) call(ctx context.Context, repoID int64, fn func(c *github.Client, r Repo) error) error {
	r, err := a.repos.Repo(ctx, repoID)
	if err != nil {
		return err
	}
	c, err := a.clientFor(ctx, r)
	if err != nil {
		a.failed(r, err)
		return err
	}
	err = fn(c, r)
	a.done(c)
	if err != nil {
		a.failed(r, err)
	}
	return err
}

// Approve approves pull request number at sha with an APPROVE review on that
// commit. GitHub doesn't check the head itself, so the pull request is read
// first: a head that is no longer sha is forge.ErrHeadMoved.
func (a *Approver) Approve(ctx context.Context, repoID, number int64, sha string) error {
	return a.call(ctx, repoID, func(c *github.Client, r Repo) error {
		pr, err := c.PullRequest(ctx, r.Owner, r.Name, number)
		if err != nil {
			return mapErr(err)
		}
		if pr.Head.SHA != sha {
			return fmt.Errorf("github: pull request head is %s, not %s: %w", pr.Head.SHA, sha, forge.ErrHeadMoved)
		}
		_, err = c.CreateReview(ctx, r.Owner, r.Name, number, sha, "APPROVE")
		return mapErr(err)
	})
}

// Unapprove dismisses every approving review of the bot that isn't dismissed
// yet. A review that is gone (404) is done. GitHub answers 422 both for a
// review dismissed meanwhile and for a dismissal branch protection doesn't
// allow, so after a 422 the reviews are listed again: any approval of the bot
// still standing is an error (the caller keeps its record and tries again).
// None to dismiss is not an error.
func (a *Approver) Unapprove(ctx context.Context, repoID, number int64) error {
	if a.bot == 0 {
		return errors.New("githubforge: the approve bot user is unknown")
	}
	return a.call(ctx, repoID, func(c *github.Client, r Repo) error {
		reviews, err := c.ListReviews(ctx, r.Owner, r.Name, number)
		if err != nil {
			return mapErr(err)
		}
		var (
			errs    []error
			refused bool
		)
		for _, rv := range reviews {
			if rv.User.ID != a.bot || rv.State != "APPROVED" {
				continue
			}
			err := c.DismissReview(ctx, r.Owner, r.Name, number, rv.ID, dismissMessage)
			switch status(err) {
			case http.StatusNotFound:
				err = nil
			case http.StatusUnprocessableEntity:
				refused, err = true, nil
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("dismiss review %d: %w", rv.ID, err))
			}
		}
		if refused {
			again, err := c.ListReviews(ctx, r.Owner, r.Name, number)
			if err != nil {
				return errors.Join(append(errs, mapErr(err))...)
			}
			for _, rv := range again {
				if rv.User.ID == a.bot && rv.State == "APPROVED" {
					errs = append(errs, fmt.Errorf("review %d still approves: GitHub refused to dismiss it (422)", rv.ID))
				}
			}
		}
		return errors.Join(errs...)
	})
}

// HasApproved reports whether the bot's latest review of the pull request is
// an approval of sha. Without a known bot user it is false, and GitHub isn't
// asked.
func (a *Approver) HasApproved(ctx context.Context, repoID, number int64, sha string) (bool, error) {
	if a.bot == 0 {
		return false, nil
	}
	var approved bool
	err := a.call(ctx, repoID, func(c *github.Client, r Repo) error {
		reviews, err := c.ListReviews(ctx, r.Owner, r.Name, number)
		if err != nil {
			return mapErr(err)
		}
		var latest *github.Review
		for i := range reviews {
			if reviews[i].User.ID == a.bot {
				latest = &reviews[i]
			}
		}
		approved = latest != nil && latest.State == "APPROVED" && latest.CommitID == sha
		return nil
	})
	return approved, err
}
