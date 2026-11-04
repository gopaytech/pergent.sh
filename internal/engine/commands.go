package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gopaytech/pergent.sh/internal/actions"
	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/result"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

const (
	// commandsPerPass is how many commands one pass answers at most.
	commandsPerPass = 10
	// maxCommandRetries is how many times a command that met a platform or
	// store error is given back to be tried again, on later passes, before
	// it is given up on.
	maxCommandRetries = 3
	// commandRetryBackoff is the first retry delay; it doubles each time.
	commandRetryBackoff = 15 * time.Second
	// commandRepeatWindow is how long after a command was answered the same
	// command (same kind, same finding ID) on the same change is dropped.
	commandRepeatWindow = 30 * time.Second
	// commandThrottleWindow is how long after a command of someone on a
	// change was answered without doing anything their next command on it
	// is left unanswered (throttled): every answer costs platform calls and
	// a comment of the bot, which a refusal or help would otherwise let
	// anyone with a comment box make at will. That is a refusal, a help or
	// unknown command, a command with nothing to do, a drop and a command
	// given up on. A command that was carried out (outcome "done") does not
	// start the window: a maintainer dismissing finding after finding is
	// not ignored. A real command written within the window after one that
	// did nothing is throttled too; its author writes it again.
	commandThrottleWindow = 2 * time.Minute
	// maxGiveUpText bounds the error text kept as a given-up command's outcome.
	maxGiveUpText = 200
)

// Bounds of the command step. They are variables so tests can shorten them.
var (
	// commandsBudget bounds the platform calls of one pass (the claims,
	// reads, reactions and replies of all its commands). The pass can run
	// over it by the store writes of the command in hand, each at most
	// commandSettleTimeout, so a slow platform delays the next pass of
	// reviews by about this much.
	commandsBudget = 30 * time.Second
	// commandTimeout is the longest one command's work (its reads and
	// writes on the platform and in the store) takes; a command is far
	// inside the store's 5-minute claim.
	commandTimeout = 20 * time.Second
	// reactTimeout is the longest one reaction or reply takes. They have
	// their own time, so a hung one neither uses up the command's work
	// time nor keeps it from being finished.
	reactTimeout = 5 * time.Second
	// commandSettleTimeout bounds each store write that records what a
	// command did; every one gets a fresh context of this length.
	commandSettleTimeout = settleTimeout
)

// What the bot replies when it refuses a command or has nothing to do. The
// replies never echo what a commenter wrote, but a finding ID, which is
// reduced to hex digits first (replyID).
const (
	replyNoPermission  = "you don't have permission to do that here"
	replyAuthor        = "the author of a change can't dismiss its blocking findings"
	replyDraft         = "drafts aren't reviewed in this repository"
	replyNothingFailed = "nothing failed"
	replyWholeFailed   = "the whole review failed, so all of it is re-run"
)

func replyHelp(mention string) string {
	return fmt.Sprintf("Commands: `@%[1]s review` · `@%[1]s review failed` · `@%[1]s dismiss <id> <reason>`", mention)
}

// replyNeedsReason asks for the reason of a blocking dismissal, in the form
// that fits where the command was written: a finding's thread names no ID.
func replyNeedsReason(mention string, inThread bool) string {
	if inThread {
		return fmt.Sprintf("dismissing a blocking finding needs a reason: `@%s dismiss <reason>`", mention)
	}
	return fmt.Sprintf("dismissing a blocking finding needs a reason: `@%s dismiss <id> <reason>`", mention)
}

// replyNotFindingThread answers a dismissal written in a thread that is none
// of pergent's finding threads.
func replyNotFindingThread(mention string) string {
	return fmt.Sprintf("This thread isn't one of pergent's findings. To dismiss a finding, reply in its thread, or comment `@%s dismiss <id> <reason>`.", mention)
}

// replyThreadFindingGone answers a dismissal in one of pergent's finding
// threads whose finding the latest review no longer has.
const replyThreadFindingGone = "This thread's finding isn't in the latest review, so there is nothing to dismiss."

func replyUnknownID(id string) string {
	return fmt.Sprintf("no finding %s in the latest review", replyID(id))
}

func replyCap(term string) string { return actions.CapMessage(term) }

func replyAlreadyDismissed(id string) string {
	return fmt.Sprintf("finding %s is already dismissed", replyID(id))
}

// replyID keeps the hex digits of a finding ID, so no other text a commenter
// wrote can reach a reply.
func replyID(id string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(id) {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// changeTerm is what the platform calls a change.
func changeTerm(platform string) string { return actions.Term(platform) }

// verdict is how the engine answered a command: what it records as the
// command's outcome, the reaction on the comment and the reply, if any.
type verdict struct {
	// outcome is store.CommandDone for a command that was carried out,
	// store.CommandNoop for one that was answered with nothing to do, else
	// why it was refused or dropped.
	outcome string
	react   forge.Reaction
	reply   string
	// replyRequired is set for a refusal, whose reply is its point: failing
	// to post it is an error and the command is tried again. The reply of a
	// command that was carried out is only courtesy.
	replyRequired bool
	// thread is the finding thread the reply goes in, when the thread is
	// known; nil leaves it to the answer step, which looks a thread command's
	// thread up, and else posts a plain comment.
	thread *forge.ThreadRef
}

// done is a command carried out, with an optional reply. A request that
// holds already (a re-run of a head already queued) is carried out too.
func done(reply string) verdict {
	return verdict{outcome: store.CommandDone, react: forge.ReactDone, reply: reply}
}

// noop is a command that was answered and had nothing to do (help, nothing
// failed, a finding dismissed already), and why. It answers with a reply, so
// it counts for the throttle, which a carried-out command does not.
func noop(why, reply string) verdict {
	return verdict{outcome: store.CommandNoop(why), react: forge.ReactDone, reply: reply}
}

// refused is a command that was refused, and why, with the reply that says so.
func refused(why, reply string) verdict {
	return verdict{outcome: "refused: " + why, react: forge.ReactRefused, reply: reply, replyRequired: true}
}

// dropped is a command that is not answered at all: only a 👎.
func dropped(why string) verdict {
	return verdict{outcome: "dropped: " + why, react: forge.ReactRefused}
}

// CommandsOnce answers the commands people wrote in comments (webhooks
// stored them): up to commandsPerPass of them, and for at most
// commandsBudget, so the loop's reviews are never held up long. Each is
// reacted to, carried out or refused, replied to if need be, and finished.
// One that meets a platform or store error is given back and tried again on a
// later pass, up to maxCommandRetries times, then given up on. Commands of an
// instance pergent.sh isn't connected to wait. The returned error joins
// store failures only.
func (e *Engine) CommandsOnce(ctx context.Context) error {
	bctx, cancel := context.WithTimeout(ctx, commandsBudget)
	defer cancel()
	skip := e.unconnected()
	var errs []error
	for i := 0; i < commandsPerPass && bctx.Err() == nil; i++ {
		// One at a time, so a command the budget leaves no time for is
		// never claimed and left to wait out its lease.
		cmds, err := e.Store.ClaimCommands(bctx, 1, e.now(), skip...)
		if err != nil {
			if bctx.Err() == nil {
				errs = append(errs, fmt.Errorf("commands: claim: %w", err))
			}
			break
		}
		if len(cmds) == 0 {
			break
		}
		e.answerCommand(ctx, bctx, cmds[0])
	}
	return errors.Join(errs...)
}

// settle is a fresh context for one store write that records what a command
// did. It outlives a cancelled or expired context, so the outcome of a
// claimed command is recorded during shutdown and after a slow platform call.
func settle(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), commandSettleTimeout)
}

// answerCommand handles one claimed command and records the outcome. parent
// is the engine's context (cancelled on shutdown), bctx the pass's budget.
// The command's work, each reaction and reply, and each store write have
// contexts of their own, so none can starve the others.
func (e *Engine) answerCommand(parent, bctx context.Context, cmd store.Command) {
	log := e.log().With("command", cmd.ID, "kind", cmd.Kind, "instance", cmd.MR.Instance,
		"project", cmd.ProjectPath, "mr", cmd.MR.IID, "comment", cmd.CommentID, "attempt", cmd.Attempts+1)
	cctx, cancel := context.WithTimeout(bctx, commandTimeout)
	v, err := e.handleCommand(cctx, bctx, cmd, log)
	cancel()
	if err != nil {
		e.retryCommand(parent, bctx, cmd, err, log)
		return
	}
	if v.reply != "" {
		if err := e.reply(bctx, cmd, v.thread, v.reply); err != nil {
			if v.replyRequired {
				e.retryCommand(parent, bctx, cmd, fmt.Errorf("reply: %w", err), log)
				return
			}
			log.Warn("couldn't post the reply to a command", "error", e.redact(err.Error()))
		}
	}
	e.react(bctx, cmd, v.react, log)
	sctx, scancel := settle(parent)
	defer scancel()
	if err := e.Store.FinishCommand(sctx, cmd.ID, v.outcome, e.now()); err != nil {
		log.Error("couldn't record that a command was answered", "error", e.redact(err.Error()))
		return
	}
	log.Info("command answered", "outcome", v.outcome)
}

// retryCommand gives a command back to be tried again later, or, after
// maxCommandRetries, finishes it with a 👎 and no reply. While the engine
// shuts down it does neither: the claim lapses and the command is offered
// again. When the pass's budget has run out, the failure is put down to it,
// not to the command: it is put off without an attempt counted.
func (e *Engine) retryCommand(parent, bctx context.Context, cmd store.Command, cause error, log *slog.Logger) {
	if parent.Err() != nil {
		return
	}
	text := e.redact(cause.Error())
	if bctx.Err() != nil {
		// The pass ran out of its time, which a slow command ahead of this
		// one may have used up: this one was not given a fair try, so it
		// is not counted against it. It waits a backoff so the commands
		// behind it get their turn.
		log.Info("a command is put off: the pass ran out of time", "error", text)
		sctx, cancel := settle(parent)
		defer cancel()
		if err := e.Store.DeferCommand(sctx, cmd.ID, e.now().Add(commandRetryBackoff)); err != nil {
			log.Error("couldn't put a command off", "error", e.redact(err.Error()))
		}
		return
	}
	if cmd.Attempts >= maxCommandRetries {
		log.Error("giving up on a command", "error", text)
		e.react(bctx, cmd, forge.ReactRefused, log)
		why := text
		if r := []rune(why); len(r) > maxGiveUpText {
			why = string(r[:maxGiveUpText])
		}
		sctx, cancel := settle(parent)
		defer cancel()
		if err := e.Store.FinishCommand(sctx, cmd.ID, "gave up: "+why, e.now()); err != nil {
			log.Error("couldn't record that a command was given up on", "error", e.redact(err.Error()))
		}
		return
	}
	delay := commandRetryBackoff << cmd.Attempts
	log.Warn("a command will be tried again", "error", text, "in", delay)
	sctx, cancel := settle(parent)
	defer cancel()
	if err := e.Store.ReleaseCommand(sctx, cmd.ID, e.now().Add(delay)); err != nil {
		log.Error("couldn't give a command back", "error", e.redact(err.Error()))
	}
}

// react puts the reaction on the command's comment, within reactTimeout (and
// ctx). A reaction that can't be made never stops the command.
func (e *Engine) react(ctx context.Context, cmd store.Command, r forge.Reaction, log *slog.Logger) {
	if r == "" {
		return
	}
	plat, ok := e.Platforms.Get(cmd.MR.Instance)
	if !ok || plat.Forge == nil {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, reactTimeout)
	defer cancel()
	if err := reactOn(rctx, plat.Forge, cmd, r); err != nil {
		log.Debug("couldn't react to a command", "reaction", r, "error", e.redact(err.Error()))
	}
}

// reactOn puts the reaction on the command's comment. A comment written in a
// thread is a review comment (GitHub reacts to those through another
// endpoint than to a pull request comment), so it is reacted to as a thread's.
func reactOn(ctx context.Context, f forge.Forge, cmd store.Command, r forge.Reaction) error {
	if cmd.InThread() {
		if inl, ok := f.(forge.Inline); ok {
			return inl.ReactThread(ctx, cmd.MR.ProjectID, cmd.MR.IID, cmd.CommentID, r)
		}
	}
	return f.React(ctx, cmd.MR.ProjectID, cmd.MR.IID, cmd.CommentID, r)
}

// reply posts body on the command's change, within reactTimeout (and ctx): in
// thread when the command was written in a thread of pergent's and the
// thread is known, else as a plain comment.
func (e *Engine) reply(ctx context.Context, cmd store.Command, thread *forge.ThreadRef, body string) error {
	plat, ok := e.Platforms.Get(cmd.MR.Instance)
	if !ok || plat.Forge == nil {
		return errors.New("not connected to the instance")
	}
	if thread == nil && cmd.InThread() {
		thread = e.knownThread(ctx, cmd)
	}
	rctx, cancel := context.WithTimeout(ctx, reactTimeout)
	defer cancel()
	return replyOn(rctx, plat.Forge, cmd, thread, body)
}

// replyOn posts body in thread when there is one and the platform has
// threads, else as a plain comment on the change.
func replyOn(ctx context.Context, f forge.Forge, cmd store.Command, thread *forge.ThreadRef, body string) error {
	if thread != nil {
		if inl, ok := f.(forge.Inline); ok {
			return inl.ReplyThread(ctx, cmd.MR.ProjectID, cmd.MR.IID, *thread, body)
		}
	}
	return f.Reply(ctx, cmd.MR.ProjectID, cmd.MR.IID, body)
}

// knownThread is the finding thread a command was written in, nil when it
// was written in some other thread or the store can't say (the reply is then
// a plain comment).
func (e *Engine) knownThread(ctx context.Context, cmd store.Command) *forge.ThreadRef {
	t, found, err := e.Store.InlineThreadFor(ctx, cmd.MR, cmd.Discussion, cmd.ReplyTo)
	if err != nil {
		e.log().Warn("couldn't look up the thread of a command", "command", cmd.ID, "error", e.redact(err.Error()))
		return nil
	}
	if !found {
		return nil
	}
	return &forge.ThreadRef{ID: t.ThreadID, CommentID: t.CommentID}
}

// commandRun is one command being handled, with what its steps need.
type commandRun struct {
	e      *Engine
	cmd    store.Command
	plat   Platform
	repo   settings.Effective
	change forge.Change
	log    *slog.Logger
}

// handleCommand carries out or refuses one command within ctx; the first
// reaction is made within bctx, the pass's budget, so it takes none of ctx's.
// An error is a platform or store failure: the command is tried again.
func (e *Engine) handleCommand(ctx, bctx context.Context, cmd store.Command, log *slog.Logger) (verdict, error) {
	plat, ok := e.Platforms.Get(cmd.MR.Instance)
	if !ok || plat.Forge == nil {
		return verdict{}, errors.New("not connected to the instance")
	}
	// Before any platform call (the first reaction is one): a command that
	// is throttled costs the platform nothing and gets no answer at all.
	answered, err := e.Store.RecentAnswered(ctx, cmd.MR, cmd.AuthorID, e.now().Add(-commandThrottleWindow))
	if err != nil {
		return verdict{}, fmt.Errorf("check the commenter's recent commands: %w", err)
	}
	if answered {
		return verdict{outcome: store.CommandThrottled}, nil
	}
	if cmd.Attempts == 0 {
		e.react(bctx, cmd, forge.ReactSeen, log)
	}
	repo, on, err := e.settings().Get(ctx, cmd.MR.Instance, cmd.ProjectPath)
	if err != nil {
		return verdict{}, fmt.Errorf("load repository settings: %w", err)
	}
	if inst, registered := e.registry().Get(cmd.MR.Instance); !on || !registered || inst.Removing {
		return dropped("the repository is not switched on"), nil
	}
	// GitLab project, group and service-account bots are no person whatever
	// their comments say: asked of the platform, and when it can't say the
	// command is tried again, never acted on.
	if bot, err := plat.Forge.IsBot(ctx, cmd.AuthorID); err != nil {
		return verdict{}, fmt.Errorf("check that the commenter is a person: %w", err)
	} else if bot {
		return dropped("the author is a bot"), nil
	}
	// A dismissal in a thread names no ID (its thread says which finding), and
	// what it has in Arg may be the first word of its reason, so the repeat
	// check, which goes by kind and argument, would take a dismissal in one
	// thread for a repeat of one in another. Dismissing a finding twice is
	// answered as already dismissed anyway.
	recent := false
	if !(cmd.InThread() && cmd.Kind == "dismiss") {
		recent, err = e.Store.RecentCommand(ctx, cmd.MR, cmd.Kind, strings.ToLower(cmd.Arg), cmd.CreatedAt.Add(-commandRepeatWindow))
		if err != nil {
			return verdict{}, fmt.Errorf("check for a repeated command: %w", err)
		}
	}
	if recent {
		return dropped("the same command was answered within 30 seconds"), nil
	}
	change, err := plat.Forge.GetChange(ctx, cmd.MR.ProjectID, cmd.MR.IID)
	if errors.Is(err, forge.ErrNotFound) {
		return dropped("the change is gone"), nil
	}
	if err != nil {
		return verdict{}, fmt.Errorf("get the change: %w", err)
	}
	if change.State != forge.StateOpen {
		return dropped("the change is not open"), nil
	}
	c := &commandRun{e: e, cmd: cmd, plat: plat, repo: repo, change: change, log: log}

	switch cmd.Kind {
	case "review":
		return c.rerun(ctx, false)
	case "review_failed":
		return c.rerun(ctx, true)
	case "dismiss":
		return c.dismiss(ctx)
	}
	// help, and a command the bot doesn't know.
	return noop("help", replyHelp(plat.Forge.Mention())), nil
}

// access is the commander's access on the platform now.
func (c *commandRun) access(ctx context.Context) (forge.Access, error) {
	a, err := c.plat.Forge.AccessLevel(ctx, c.cmd.MR.ProjectID, c.cmd.Author, c.cmd.AuthorID)
	if err != nil {
		return forge.AccessNone, fmt.Errorf("get the commenter's access: %w", err)
	}
	return a, nil
}

// actor is who the audit log names for a comment command.
func (c *commandRun) actor() string {
	return c.cmd.Author + " (" + c.cmd.Platform + " " + c.cmd.MR.Instance + ")"
}

// audit records what the command did to the change at head. A failure is logged and never undoes
// the command.
func (c *commandRun) audit(ctx context.Context, action, head, detail string) {
	head = fmt.Sprintf("mr=%d head=%s", c.cmd.MR.IID, head)
	if detail != "" {
		head += " " + detail
	}
	actx, cancel := settle(ctx)
	defer cancel()
	if err := c.e.Store.Audit(actx, store.AuditEntry{
		At: c.e.now(), Actor: c.actor(), Action: action,
		Instance: c.cmd.MR.Instance, Project: c.cmd.ProjectPath, Detail: head,
	}); err != nil {
		c.log.Error("recording an audit entry failed", "action", action, "error", c.e.redact(err.Error()))
	}
}

// rerun is "review" and, with failedOnly, "review failed".
func (c *commandRun) rerun(ctx context.Context, failedOnly bool) (verdict, error) {
	access, err := c.access(ctx)
	if err != nil {
		return verdict{}, err
	}
	if access < forge.AccessWrite {
		return refused("no permission", replyNoPermission), nil
	}
	if c.change.Draft && !c.repo.ReviewDrafts {
		return refused("draft", replyDraft), nil
	}
	if c.change.HeadSHA == "" {
		return verdict{}, errors.New("the platform reports no head commit")
	}

	res, err := actions.Rerun(ctx, c.e.Store, actions.RerunRequest{
		MR: c.cmd.MR, ProjectPath: c.cmd.ProjectPath, Head: c.change.HeadSHA, TargetBranch: c.change.TargetBranch,
		FailedOnly: failedOnly, Repo: c.repo, Now: c.e.now(),
	})
	if err != nil {
		return verdict{}, err
	}
	switch res.Status {
	case actions.RerunNothingFailed:
		return noop("nothing failed", replyNothingFailed), nil
	case actions.RerunCapped:
		return refused("re-run limit reached", replyCap(changeTerm(c.cmd.Platform))), nil
	case actions.RerunAlreadyQueued:
		// Stays done, unlike "already dismissed" (a no-op that replies): no
		// reply, and the review the commenter wants is on its way.
		// A head already queued or running is reviewed once: the request
		// is acknowledged as done.
		return done(""), nil
	}
	c.e.Wake()
	c.audit(ctx, res.Action, c.change.HeadSHA, res.Detail)
	if res.Whole {
		return done(replyWholeFailed), nil
	}
	return done(""), nil
}

// dismiss is "dismiss <id> [reason]", or, written in a finding's thread,
// "dismiss [reason]".
func (c *commandRun) dismiss(ctx context.Context) (verdict, error) {
	if c.cmd.InThread() {
		return c.dismissInThread(ctx)
	}
	return c.dismissByID(ctx)
}

// dismissByID is "dismiss <id> [reason]" written anywhere but in one of
// pergent's finding threads.
func (c *commandRun) dismissByID(ctx context.Context) (verdict, error) {
	access, err := c.access(ctx)
	if err != nil {
		return verdict{}, err
	}
	if access < forge.AccessWrite {
		return refused("no permission", replyNoPermission), nil
	}
	f, reviewed, found, err := c.findFinding(ctx)
	if err != nil {
		return verdict{}, err
	}
	if !found {
		return refused("unknown finding", replyUnknownID(c.cmd.Arg)), nil
	}
	return c.dismissFinding(ctx, access, f, reviewed, c.cmd.Reason, c.cmd.Arg, false)
}

// dismissInThread is a dismissal written as a reply in a thread. The thread
// says which finding is meant: the latest review's finding with the thread's
// fingerprint. A first word that looks like an ID but isn't the finding's is
// the start of the reason.
func (c *commandRun) dismissInThread(ctx context.Context) (verdict, error) {
	t, found, err := c.e.Store.InlineThreadFor(ctx, c.cmd.MR, c.cmd.Discussion, c.cmd.ReplyTo)
	if err != nil {
		return verdict{}, fmt.Errorf("find the thread: %w", err)
	}
	if !found {
		if looksLikeFindingID(c.cmd.Arg) {
			// "dismiss <id> <reason>" in a thread that isn't pergent's is the
			// plain command: the ID says which finding.
			return c.dismissByID(ctx)
		}
		return refused("not a finding thread", replyNotFindingThread(c.plat.Forge.Mention())), nil
	}
	ref := &forge.ThreadRef{ID: t.ThreadID, CommentID: t.CommentID}
	with := func(v verdict) verdict {
		v.thread = ref
		return v
	}
	all, reviewed, ok, err := actions.LatestFindings(ctx, c.e.Store, c.cmd.MR)
	if err != nil {
		return verdict{}, err
	}
	var f result.Finding
	if ok {
		f, ok = actions.MostSevere(all, t.Fingerprint)
	}
	if !ok {
		return with(refused("unknown finding", replyThreadFindingGone)), nil
	}
	reason := threadReason(c.cmd.Arg, f.ID, c.cmd.Reason)
	access, err := c.access(ctx)
	if err != nil {
		return verdict{}, err
	}
	if access < forge.AccessWrite {
		return with(refused("no permission", replyNoPermission)), nil
	}
	v, err := c.dismissFinding(ctx, access, f, reviewed, reason, f.ID, true)
	if err != nil {
		return verdict{}, err
	}
	return with(v), nil
}

// looksLikeFindingID reports whether s is shaped like a finding ID: 6 to 12
// hex digits.
func looksLikeFindingID(s string) bool {
	if len(s) < 6 || len(s) > 12 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

// threadReason is the reason of a dismissal written in a thread. arg is the
// word the commander wrote where an ID goes ("" when there was none): when it
// isn't the finding's ID (id) it was part of the reason. The result is kept
// within the longest reason.
func threadReason(arg, id, reason string) string {
	if arg != "" && !strings.EqualFold(arg, id) {
		reason = strings.TrimSpace(arg + " " + reason)
	}
	if r := []rune(reason); len(r) > actions.MaxReasonRunes {
		reason = string(r[:actions.MaxReasonRunes])
	}
	return reason
}

// dismissFinding sets f aside, for a commander with access: the rules by the
// finding's severity, then the dismissal and the review decided again. id
// names the finding in a reply; inThread says the command was written in the
// finding's thread, which a reply asking for a reason has to match.
func (c *commandRun) dismissFinding(ctx context.Context, access forge.Access, f result.Finding, reviewed store.Run, rawReason, id string, inThread bool) (verdict, error) {
	reason := actions.CleanReason(rawReason)
	if f.Severity != "suggestion" {
		// Anything but a suggestion is dismissed as a blocking finding.
		f.Severity = "blocking"
		switch {
		case access < forge.AccessMaintain:
			return refused("no permission", replyNoPermission), nil
		case c.isAuthor():
			return refused("author of the change", replyAuthor), nil
		case reason == "":
			return refused("reason needed", replyNeedsReason(c.plat.Forge.Mention(), inThread)), nil
		}
	}

	sctx, cancel := settle(ctx)
	defer cancel()
	created, err := c.e.Store.Dismiss(sctx, store.Dismissal{
		MR: c.cmd.MR, Fingerprint: f.Fingerprint, Severity: f.Severity,
		Reason: reason, Actor: c.actor(), CreatedAt: c.e.now(),
	})
	if err != nil {
		return verdict{}, fmt.Errorf("store the dismissal: %w", err)
	}
	if created {
		// Audited as soon as it is stored: re-deciding the review below may
		// fail and be retried, which then finds the dismissal made.
		detail := "fingerprint=" + f.Fingerprint + " severity=" + f.Severity
		if reason != "" {
			detail += fmt.Sprintf(" reason=%q", c.e.redact(reason))
		}
		c.audit(ctx, "finding_dismissed", reviewed.HeadSHA, detail)
	} else {
		// Dismissed already, at this severity or a higher one: unless it is
		// this very command's own, stored by an attempt that then failed to
		// re-decide the review.
		mine, err := c.ownDismissal(ctx, f.Fingerprint, reason)
		if err != nil {
			return verdict{}, err
		}
		if !mine {
			return noop("already dismissed", replyAlreadyDismissed(id)), nil
		}
	}
	// The review is decided again with the dismissal applied: no run starts.
	rctx, rcancel := settle(ctx)
	defer rcancel()
	redecided, err := c.e.Store.Republish(rctx, c.cmd.MR, c.change.HeadSHA, c.change.TargetBranch)
	if err != nil {
		return verdict{}, fmt.Errorf("re-decide the review: %w", err)
	}
	if !redecided {
		// The change moved on, or its newest run isn't a published review
		// of this head: the dismissal applies to the next review of the
		// finding, and there is nothing to publish now.
		c.log.Info("a dismissal has no review of the current head to re-decide", "reviewed_head", reviewed.HeadSHA, "head", c.change.HeadSHA)
	}
	c.e.Wake()
	return done(""), nil
}

// isAuthor tells whether the commenter is the author of the change, by user
// ID (see actions.IsAuthor).
func (c *commandRun) isAuthor() bool {
	return actions.IsAuthor(c.cmd.AuthorID, c.change.AuthorID)
}

// ownDismissal tells whether the stored dismissal of fingerprint is the one
// this command's actor made with this reason.
func (c *commandRun) ownDismissal(ctx context.Context, fingerprint, reason string) (bool, error) {
	ds, err := c.e.Store.Dismissals(ctx, c.cmd.MR)
	if err != nil {
		return false, fmt.Errorf("load dismissals: %w", err)
	}
	for _, d := range ds {
		if d.Fingerprint == fingerprint {
			// Made after the comment was written, so not by an earlier one.
			return d.Actor == c.actor() && d.Reason == reason && !d.CreatedAt.Before(c.cmd.CreatedAt), nil
		}
	}
	return false, nil
}

// findFinding resolves the command's finding ID against the newest review of
// the change: its finding with that ID or, when none has it, the one finding
// whose fingerprint starts with it. The finding is as severe as the most
// severe report of its fingerprint (two skills may report it differently).
// reviewed is the run whose result it was found in.
func (c *commandRun) findFinding(ctx context.Context) (f result.Finding, reviewed store.Run, found bool, err error) {
	all, run, ok, err := actions.LatestFindings(ctx, c.e.Store, c.cmd.MR)
	if err != nil || !ok {
		return f, run, false, err
	}

	id := strings.ToLower(c.cmd.Arg)
	var fingerprint string
	for _, x := range all {
		if strings.EqualFold(x.ID, id) {
			fingerprint = x.Fingerprint
			break
		}
	}
	if fingerprint == "" {
		// By fingerprint prefix, when that names one finding.
		for _, x := range all {
			if !strings.HasPrefix(strings.ToLower(x.Fingerprint), id) {
				continue
			}
			if fingerprint != "" && x.Fingerprint != fingerprint {
				return f, run, false, nil
			}
			fingerprint = x.Fingerprint
		}
	}
	if fingerprint == "" {
		return f, run, false, nil
	}
	f, found = actions.MostSevere(all, fingerprint)
	return f, run, found, nil
}
