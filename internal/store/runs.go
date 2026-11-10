package store

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// MRKey identifies a merge request.
type MRKey struct {
	Instance  string
	ProjectID int64
	IID       int64
}

// RunState is where a run is in its life.
type RunState string

const (
	Queued        RunState = "queued"
	Running       RunState = "running"
	Reported      RunState = "reported"
	Published     RunState = "published"
	PublishFailed RunState = "publish_failed"
	Superseded    RunState = "superseded"
	// Skipped is a run that was not reviewed because it had no skills; its
	// SkipReason says so.
	Skipped RunState = "skipped"
	// Waiting is a push's run held because the repository doesn't trust its
	// author (or couldn't tell); its SkipReason says which. It is never
	// claimed: someone with write access starts it (StartRun), or a newer
	// head or a close supersedes it.
	Waiting RunState = "waiting"
)

// Why a run waits (Run.SkipReason of a Waiting run).
const (
	// WaitUntrusted: the author isn't trusted by the repository.
	WaitUntrusted = "untrusted_author"
	// WaitCheckFailed: the author's access couldn't be read.
	WaitCheckFailed = "trust_check_failed"
	// WaitQuota: the repository's monthly token quota is used up.
	WaitQuota = "quota_used_up"
)

var (
	// ErrInvalidResult is returned by StoreResult when the body is not valid
	// JSON. The run is left untouched.
	ErrInvalidResult = errors.New("store: result is not valid JSON")
	// ErrNotRunning is returned by MarkDispatched when the run is not in
	// state running (it was superseded, or does not exist).
	ErrNotRunning = errors.New("store: run is not running")
	// ErrNotReported is returned by ReplaceWithFailure when the run is not in
	// state reported with a result (or does not exist).
	ErrNotReported = errors.New("store: run is not reported with a result")
)

// maxPublishAttempts is the number of failed publishes after which a run
// becomes publish_failed.
const maxPublishAttempts = 10

// maxPublishError bounds the stored publish error text.
const maxPublishError = 2000

// Run is one review of one head commit of a merge request.
type Run struct {
	ID                                                    string
	MR                                                    MRKey
	ProjectPath, HeadSHA, BaseSHA, StartSHA, TargetBranch string
	State                                                 RunState
	JobName                                               string
	TokenExpiresAt                                        time.Time
	Result                                                []byte
	FailureReason                                         string
	// PublishError is the text of the last failed publish, if any.
	PublishError string
	// Attempts counts dispatch retries while the run is queued or running,
	// and failed publishes once it is reported (it restarts at 0 then).
	Attempts                                       int
	CreatedAt, DispatchedAt, ReportedAt, NotBefore time.Time
	CleanedUp                                      bool
	// Title, WebURL, SourceBranch and Author are the merge request's as
	// the dispatcher last read them ("" before), for the dashboard.
	Title, WebURL, SourceBranch, Author string
	// Model and SkillSources (skill name → "library" or "repo") were
	// recorded at dispatch; nil before.
	Model        *string
	SkillSources map[string]string
	// Provider is the provider the run was started with (SetRunModel); nil
	// before the column, or for a run that never started.
	Provider *string
	// MaxTurns is the step limit the run was dispatched with; nil before
	// the limit was recorded.
	MaxTurns *int
	// SkillVersions (library skill name → version) was recorded at
	// dispatch; nil before. SkipReason is why a Skipped run was skipped.
	SkillVersions map[string]int
	SkipReason    string
	// Outcome, OutcomeReason, DecisionStatus and Approved are the
	// publisher's decision (see RunDecision); Outcome is "" until then.
	Outcome, OutcomeReason, DecisionStatus string
	Approved                               bool
	// Trigger is how the run came to be: TriggerPush, TriggerRerun or
	// TriggerRerunFailed. SkillSubset names the skills it reviews (nil: all
	// of them) and BaseRunID is the run whose results a partial re-run keeps
	// (nil: none).
	Trigger     string
	SkillSubset []string
	BaseRunID   *string
	// Incremental is true for a run that reviews only the files changed since
	// its base run. It can't be read from BaseRunID, which is cleared when the
	// base run is pruned. SkillHashes (skill name → hash of its content) was
	// recorded at dispatch; nil before.
	Incremental bool
	SkillHashes map[string]string
	// Quiet is whether the run was started quiet, recorded when it started
	// (SetRunQuiet). False for a run from before the column, or one that
	// never started: the column is NULL there.
	Quiet bool
	// QuotaChecked is true once the monthly token quota has been looked at
	// for the run (it was asked for, or started), so the dispatcher never
	// holds it for the quota.
	QuotaChecked bool
	// CommentLayout is the layout of the comment, recorded when the run started
	// (SetRunLayout); nil is the default layout, for a run from before the
	// column or one that never started.
	CommentLayout []string
	// LinkedRepos are the links the run was started with, recorded when it
	// started (SetRunLinks): those the change matched, with the feature on.
	// Nil is none, for a run from before the column or one that never
	// started.
	LinkedRepos []Link
}

// How a run came to be (Run.Trigger).
const (
	// TriggerPush is a push, an opened merge request or a retarget.
	TriggerPush = "push"
	// TriggerRerun is a re-run of the whole review on request.
	TriggerRerun = "rerun"
	// TriggerRerunFailed is a re-run of the failed skills on request.
	TriggerRerunFailed = "rerun_failed"
	// TriggerStart is the review of an untrusted author's change, started by
	// someone with write access. It counts against the merge request's re-run
	// cap, not the repository's.
	TriggerStart = "start"
)

// A run's outcome, as the publisher records it (RunDecision.Outcome) and as
// the dashboard shows it. Running, superseded, skipped and unknown (published before
// outcomes were recorded) are never recorded: they follow from the state.
const (
	OutcomeRunning    = "running"
	OutcomeApproved   = "approved"
	OutcomeBlocking   = "blocking"
	OutcomeDraft      = "draft"
	OutcomeNoBlocking = "no_blocking"
	OutcomeFailed     = "failed"
	OutcomeSuperseded = "superseded"
	OutcomeSkipped    = "skipped"
	// OutcomeWaiting is a run held until someone with write access starts it.
	OutcomeWaiting = "waiting"
	OutcomeUnknown = "unknown"
)

// RunDecision is what the publisher decided for a run.
type RunDecision struct {
	// Outcome is OutcomeApproved, OutcomeBlocking, OutcomeDraft,
	// OutcomeNoBlocking or OutcomeFailed.
	Outcome string
	// Reason says why, in a sentence ("" when approved).
	Reason string
	// Status is the commit status set: "success", "failed" or "neutral".
	Status string
	// Approved is true when the approve bot approved this run.
	Approved bool
}

// ResultOutcome is what StoreResult did with a posted result.
type ResultOutcome int

const (
	ResultStored ResultOutcome = iota
	ResultDiscarded
	ResultConflict
	ResultUnauthorized
)

// Approval is the approve bot's recorded approval of a merge request: the
// commit it approved and the target branch that commit was reviewed against
// ("" for an approval recorded before targets were).
type Approval struct {
	MR             MRKey
	ApprovedSHA    string
	ApprovedTarget string
}

// ProjectApproval is a recorded approval with the stored path of its
// project; ProjectPath is "" when the project isn't stored.
type ProjectApproval struct {
	Approval
	ProjectPath string
}

const runColumns = `id::text, instance, project_id, mr_iid, project_path, head_sha, base_sha, start_sha, target_branch,
	state, job_name, token_expires_at, result, failure_reason, publish_error, attempts,
	created_at, dispatched_at, reported_at, not_before, cleaned_up,
	title, web_url, source_branch, author, model, skill_sources,
	outcome, outcome_reason, decision_status, approved, trigger, skill_subset, base_run_id::text,
	skill_versions, skip_reason, max_turns, incremental, skill_hashes, quiet_reviews, comment_layout,
	linked_repos, provider, quota_checked`

func scanRun(row pgx.Row) (Run, error) {
	var (
		r                                    Run
		state                                string
		tokenExp, dispatched, rep, notBefore *time.Time
		sources, versions, hashes            []byte
		links                                []byte
		quiet                                *bool // NULL is off
	)
	err := row.Scan(&r.ID, &r.MR.Instance, &r.MR.ProjectID, &r.MR.IID, &r.ProjectPath, &r.HeadSHA, &r.BaseSHA, &r.StartSHA,
		&r.TargetBranch, &state, &r.JobName, &tokenExp, &r.Result, &r.FailureReason, &r.PublishError, &r.Attempts,
		&r.CreatedAt, &dispatched, &rep, &notBefore, &r.CleanedUp,
		&r.Title, &r.WebURL, &r.SourceBranch, &r.Author, &r.Model, &sources,
		&r.Outcome, &r.OutcomeReason, &r.DecisionStatus, &r.Approved, &r.Trigger, &r.SkillSubset, &r.BaseRunID,
		&versions, &r.SkipReason, &r.MaxTurns, &r.Incremental, &hashes, &quiet, &r.CommentLayout, &links, &r.Provider, &r.QuotaChecked)
	if err != nil {
		return Run{}, err
	}
	if sources != nil {
		if err := json.Unmarshal(sources, &r.SkillSources); err != nil {
			return Run{}, fmt.Errorf("run %s: skill sources: %w", r.ID, err)
		}
	}
	if versions != nil {
		if err := json.Unmarshal(versions, &r.SkillVersions); err != nil {
			return Run{}, fmt.Errorf("run %s: skill versions: %w", r.ID, err)
		}
	}
	if hashes != nil {
		if err := json.Unmarshal(hashes, &r.SkillHashes); err != nil {
			return Run{}, fmt.Errorf("run %s: skill hashes: %w", r.ID, err)
		}
	}
	r.Quiet = quiet != nil && *quiet
	if len(r.CommentLayout) == 0 {
		r.CommentLayout = nil
	}
	if r.LinkedRepos, err = readLinks(links); err != nil {
		return Run{}, fmt.Errorf("run %s: %w", r.ID, err)
	}
	r.State = RunState(state)
	r.TokenExpiresAt = deref(tokenExp)
	r.DispatchedAt = deref(dispatched)
	r.ReportedAt = deref(rep)
	r.NotBefore = deref(notBefore)
	return r, nil
}

func deref(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

func scanRuns(rows pgx.Rows) ([]Run, error) {
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) getRun(ctx context.Context, id string) (Run, error) {
	return scanRun(s.pool.QueryRow(ctx, `SELECT `+runColumns+` FROM runs WHERE id = $1::uuid`, id))
}

// lockMR takes a transaction-scoped lock on one merge request, so concurrent
// enqueues for it cannot both leave a live run behind.
func lockMR(ctx context.Context, tx pgx.Tx, mr MRKey) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		fmt.Sprintf("mr/%s/%d/%d", mr.Instance, mr.ProjectID, mr.IID))
	return err
}

// EnqueueRun queues a review of headSHA against targetBranch. A run is one
// head reviewed against one target, so a retargeted merge request gets a new
// run of the same head. It supersedes the queued, running and reported (not
// yet published) runs of the same merge request for any other head or
// target, so an older review's result is never posted over a newer one.
//
// When the merge request's newest non-superseded run is already for this
// head and target (a repeated webhook, a reopen), no run is created: created
// is false and id is that run's. Otherwise a new run is created even if an
// older run of this head and target was published: since then another head
// or target was reviewed, so the comment and the approval show that review,
// and the commit status, which is per commit and not per target, may too
// (retargeted main → develop → main, or pushed A → B → A). That old run is
// superseded and, being finished and posted, marked cleaned up: there is no
// Job to delete and no status to cancel.
func (s *Store) EnqueueRun(ctx context.Context, mr MRKey, projectPath, headSHA, targetBranch string) (id string, created bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if err := lockMR(ctx, tx, mr); err != nil {
		return "", false, err
	}
	// The newest run is read before anything is superseded, so a live run
	// of another target (not yet published) still counts as newer than an
	// old published run of this head and target.
	var newestID, newestHead, newestTarget string
	err = tx.QueryRow(ctx, `SELECT id::text, head_sha, target_branch FROM runs
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND state <> 'superseded'
		ORDER BY created_at DESC, id DESC LIMIT 1`,
		mr.Instance, mr.ProjectID, mr.IID).Scan(&newestID, &newestHead, &newestTarget)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", false, err
	}
	if err == nil && newestHead == headSHA && newestTarget == targetBranch {
		// Only one non-superseded run per head and target exists (the
		// partial unique index), so every other live run is older and has
		// already been superseded by the enqueue that created this one.
		if err := tx.Commit(ctx); err != nil {
			return "", false, err
		}
		return newestID, false, nil
	}

	if _, err := tx.Exec(ctx, `UPDATE runs SET state = 'superseded'
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND (head_sha <> $4 OR target_branch <> $5)
		  AND state IN ('queued', 'running', 'reported', 'waiting')`,
		mr.Instance, mr.ProjectID, mr.IID, headSHA, targetBranch); err != nil {
		return "", false, err
	}
	// An older run of this head and target is out of the way of the new
	// one. It is published or publish_failed: a live one would be the
	// newest run.
	if _, err := tx.Exec(ctx, `UPDATE runs SET state = 'superseded',
			cleaned_up = cleaned_up OR state IN ('published', 'publish_failed')
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND head_sha = $4 AND target_branch = $5
		  AND state <> 'superseded'`,
		mr.Instance, mr.ProjectID, mr.IID, headSHA, targetBranch); err != nil {
		return "", false, err
	}
	err = tx.QueryRow(ctx, `INSERT INTO runs (instance, project_id, mr_iid, project_path, head_sha, target_branch)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id::text`,
		mr.Instance, mr.ProjectID, mr.IID, projectPath, headSHA, targetBranch).Scan(&id)
	if err != nil {
		return "", false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", false, err
	}
	return id, true, nil
}

// EnqueueRerun queues a run of headSHA against targetBranch that was asked for
// (trigger is TriggerRerun or TriggerRerunFailed), even when the merge
// request's newest run is already for that head and target: that run, be it
// published or failed to publish, is superseded and, being finished and
// posted, marked cleaned up, as EnqueueRun does for an older run of the head.
// The live runs of any other head or target are superseded too. skills
// limits the run to those skills (nil or empty: all of them) and baseRunID is
// the run whose results the new one keeps.
//
// A run of this head and target that is queued, running or reported (its
// review not yet published) is not replaced or duplicated: created is false
// and id is that run's, nothing is changed. Superseding a reported run would
// leave its commit status for the cleanup to cancel while the new run
// is running. The exception is replace (a full review asked for): it
// supersedes a push's run that hasn't been dispatched (queued, or claimed and
// not yet dispatched) or that reviews only some files (Incremental), since the
// review asked for covers more.
//
// The caller checks the re-run caps (CountReruns) before calling; with
// more than one caller at a time, for example two button presses, a cap may
// be overshot by a few.
func (s *Store) EnqueueRerun(ctx context.Context, mr MRKey, projectPath, headSHA, targetBranch, trigger string, skills []string, baseRunID *string, replace, quotaChecked bool) (id string, created bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if err := lockMR(ctx, tx, mr); err != nil {
		return "", false, err
	}
	// With replace, a push run with no dispatched_at is replaceable: one that
	// was never dispatched, which also matches a run that failed before
	// dispatch, not only one still waiting in the queue. So is an incremental
	// one.
	err = tx.QueryRow(ctx, `SELECT id::text FROM runs
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND head_sha = $4 AND target_branch = $5
		  AND state IN ('queued', 'running', 'reported')
		  AND NOT ($6 AND trigger = 'push' AND (dispatched_at IS NULL OR incremental))`,
		mr.Instance, mr.ProjectID, mr.IID, headSHA, targetBranch, replace).Scan(&id)
	if err == nil {
		// Someone asked for this review, so the quota is not looked at again
		// for the run it lands on (the publisher's own re-run, say).
		if quotaChecked {
			if err := markQuotaChecked(ctx, tx, id); err != nil {
				return "", false, err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return "", false, err
		}
		return id, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, err
	}

	if _, err := tx.Exec(ctx, `UPDATE runs SET state = 'superseded'
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND (head_sha <> $4 OR target_branch <> $5)
		  AND state IN ('queued', 'running', 'reported', 'waiting')`,
		mr.Instance, mr.ProjectID, mr.IID, headSHA, targetBranch); err != nil {
		return "", false, err
	}
	// What is left of this head and target is published or publish_failed,
	// and the live runs replace allows to go (an undispatched or incremental
	// push run); the other queued, running and reported ones returned above.
	if _, err := tx.Exec(ctx, `UPDATE runs SET state = 'superseded',
			cleaned_up = cleaned_up OR state IN ('published', 'publish_failed')
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND head_sha = $4 AND target_branch = $5
		  AND state <> 'superseded'`,
		mr.Instance, mr.ProjectID, mr.IID, headSHA, targetBranch); err != nil {
		return "", false, err
	}
	if len(skills) == 0 {
		skills = nil
	}
	err = tx.QueryRow(ctx, `INSERT INTO runs (instance, project_id, mr_iid, project_path, head_sha, target_branch,
			trigger, skill_subset, base_run_id, quota_checked)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::uuid, $10) RETURNING id::text`,
		mr.Instance, mr.ProjectID, mr.IID, projectPath, headSHA, targetBranch,
		trigger, skills, baseRunID, quotaChecked).Scan(&id)
	if err != nil {
		return "", false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", false, err
	}
	return id, true, nil
}

// CountReruns returns how many runs asked for (TriggerRerun,
// TriggerRerunFailed and, for one merge request, TriggerStart) were created at
// or after since: those of the merge request mr, or, with mr nil, of every
// merge request of the project projectID on instance, starts left out. A run since superseded still counts. The count and
// the EnqueueRerun after it are not one step, so a cap checked with them is
// exact for one caller at a time (one engine replica's command step) and
// may be overshot by a few by concurrent button presses.
func (s *Store) CountReruns(ctx context.Context, mr *MRKey, instance string, projectID int64, since time.Time) (int, error) {
	var iid *int64
	if mr != nil {
		instance, projectID, iid = mr.Instance, mr.ProjectID, &mr.IID
	}
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM runs
		WHERE instance = $1 AND project_id = $2 AND ($3::bigint IS NULL OR mr_iid = $3)
		  AND trigger <> 'push' AND ($3::bigint IS NOT NULL OR trigger <> 'start') AND created_at >= $4`,
		instance, projectID, iid, since).Scan(&n)
	return n, err
}

// SupersededDraft is the outcome reason of a run superseded because its merge
// request is a draft and the repository doesn't review drafts
// (SupersedeDraftMR, SupersedeDraft). A run superseded otherwise keeps the
// reason it had.
const SupersededDraft = "draft_not_reviewed"

// SupersedeMR supersedes the queued, running and reported (not yet published)
// runs of a merge request (for example when it is closed or merged) and
// returns how many it changed. It takes the same per-MR lock as EnqueueRun, so
// a close racing an enqueue cannot leave a queued run alive.
func (s *Store) SupersedeMR(ctx context.Context, mr MRKey) (int, error) {
	return s.supersedeMR(ctx, mr, "", "", "")
}

// SupersedeDraftMR is SupersedeMR for a merge request that is a draft the
// repository doesn't review, at headSHA onto targetBranch: its live runs of
// another head or target are superseded with the reason SupersededDraft. A
// run of that head and target is left alone: the draft event may be late or
// redelivered after the change was marked ready and the run queued, and the
// dispatcher reads the change's live draft state before reviewing it.
func (s *Store) SupersedeDraftMR(ctx context.Context, mr MRKey, headSHA, targetBranch string) (int, error) {
	return s.supersedeMR(ctx, mr, SupersededDraft, headSHA, targetBranch)
}

// supersedeMR supersedes the live runs of mr, setting their outcome reason to
// reason unless it is "". With headSHA set, a run of headSHA onto
// targetBranch is kept.
func (s *Store) supersedeMR(ctx context.Context, mr MRKey, reason, headSHA, targetBranch string) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if err := lockMR(ctx, tx, mr); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `UPDATE runs SET state = 'superseded', outcome_reason = COALESCE(NULLIF($4, ''), outcome_reason)
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND state IN ('queued', 'running', 'reported', 'waiting')
		  AND ($5 = '' OR head_sha <> $5 OR target_branch <> $6)`,
		mr.Instance, mr.ProjectID, mr.IID, reason, headSHA, targetBranch)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// SupersedeInstance supersedes the queued, running and reported runs of every
// merge request of the instance, for when the instance is removed, and
// returns how many it changed. It does it one merge request at a time
// (SupersedeMR), taking the same per-MR lock as EnqueueRun.
func (s *Store) SupersedeInstance(ctx context.Context, instance string) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT project_id, mr_iid FROM runs
		WHERE instance = $1 AND state IN ('queued', 'running', 'reported', 'waiting')`, instance)
	if err != nil {
		return 0, err
	}
	mrs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (MRKey, error) {
		mr := MRKey{Instance: instance}
		err := row.Scan(&mr.ProjectID, &mr.IID)
		return mr, err
	})
	if err != nil {
		return 0, err
	}
	total := 0
	for _, mr := range mrs {
		n, err := s.SupersedeMR(ctx, mr)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// Supersede moves one queued or running run to superseded, for example when
// the dispatcher finds its merge request closed or its head moved. A run in
// any other state, or an unknown id, is left alone and is not an error.
func (s *Store) Supersede(ctx context.Context, id string) error {
	return s.supersede(ctx, id, "")
}

// SupersedeDraft is Supersede for a run whose merge request is a draft the
// repository doesn't review: the run gets the reason SupersededDraft.
func (s *Store) SupersedeDraft(ctx context.Context, id string) error {
	return s.supersede(ctx, id, SupersededDraft)
}

// supersede supersedes run id, setting its outcome reason to reason unless
// it is "".
func (s *Store) supersede(ctx context.Context, id, reason string) error {
	if !validUUID(id) {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE runs SET state = 'superseded', outcome_reason = COALESCE(NULLIF($2, ''), outcome_reason)
		WHERE id = $1::uuid AND state IN ('queued', 'running')`, id, reason)
	return err
}

// CountRunning returns the number of runs in state running.
func (s *Store) CountRunning(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM runs WHERE state = 'running'`).Scan(&n)
	return n, err
}

// ClaimQueued moves up to limit queued runs that are due at now to running and
// returns them, oldest first. Concurrent callers never claim the same run.
// The runs of the instances in skip are left alone, queued and unclaimed, so
// runs that have to wait for an instance take no slot from the others.
func (s *Store) ClaimQueued(ctx context.Context, limit int, now time.Time, skip ...string) ([]Run, error) {
	if limit <= 0 {
		return nil, nil
	}
	if skip == nil {
		skip = []string{} // an empty array, not NULL: `<> ALL(NULL)` is never true
	}
	rows, err := s.pool.Query(ctx, `UPDATE runs SET state = 'running', claimed_at = $2 WHERE id IN (
			SELECT id FROM runs WHERE state = 'queued' AND (not_before IS NULL OR not_before <= $2)
			  AND instance <> ALL($3::text[])
			ORDER BY created_at, id LIMIT $1 FOR UPDATE SKIP LOCKED)
		RETURNING `+runColumns, limit, now, skip)
	if err != nil {
		return nil, err
	}
	runs, err := scanRuns(rows)
	if err != nil {
		return nil, err
	}
	sortRuns(runs)
	return runs, nil
}

// CountQueued returns how many runs are queued (due or not) on each of the
// instances; one with none is left out.
func (s *Store) CountQueued(ctx context.Context, instances []string) (map[string]int, error) {
	rows, err := s.pool.Query(ctx, `SELECT instance, count(*) FROM runs
		WHERE state = 'queued' AND instance = ANY($1::text[]) GROUP BY instance`, instances)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			return nil, err
		}
		out[name] = n
	}
	return out, rows.Err()
}

func sortRuns(runs []Run) {
	sort.SliceStable(runs, func(i, j int) bool {
		if !runs[i].CreatedAt.Equal(runs[j].CreatedAt) {
			return runs[i].CreatedAt.Before(runs[j].CreatedAt)
		}
		return runs[i].ID < runs[j].ID
	})
}

// RecoverUnlaunched puts running runs that never got a Job back in the queue,
// due now and without counting an attempt: those claimed before olderThan
// (or before claims were recorded). Such a run was stranded when pergent.sh
// stopped between ClaimQueued and MarkDispatched; left alone it would stay
// running for ever and hold a jobs.max_running slot. It returns how many runs
// it moved.
func (s *Store) RecoverUnlaunched(ctx context.Context, olderThan time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE runs SET state = 'queued', not_before = now()
		WHERE state = 'running' AND job_name = '' AND (claimed_at IS NULL OR claimed_at < $1)`, olderThan)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// Requeue puts a claimed run back in the queue, not to be claimed before
// notBefore, and counts the attempt.
func (s *Store) Requeue(ctx context.Context, id string, notBefore time.Time) error {
	return s.mustUpdate(ctx, id, `UPDATE runs SET state = 'queued', not_before = $2, attempts = attempts + 1
		WHERE id = $1::uuid AND state IN ('queued', 'running')`, id, notBefore)
}

// Postpone puts a claimed run back in the queue, not to be claimed before
// notBefore, without counting an attempt: nothing failed, the run only has to
// wait (GitLab is still computing the diff, or the dispatcher is shutting
// down). A run that is no longer queued or running, or an unknown id, is left
// alone and is not an error.
func (s *Store) Postpone(ctx context.Context, id string, notBefore time.Time) error {
	if !validUUID(id) {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE runs SET state = 'queued', not_before = $2
		WHERE id = $1::uuid AND state IN ('queued', 'running')`, id, notBefore)
	return err
}

// ChangeDetails is what the dashboard shows of a run's merge request.
type ChangeDetails struct {
	Title, WebURL, SourceBranch, Author string
}

// maxChangeDetail bounds each stored detail, in bytes.
const maxChangeDetail = 1000

// SetRunChange records the merge request's details on a run, each cut to
// 1000 bytes. An unknown run is not an error; a malformed id is.
func (s *Store) SetRunChange(ctx context.Context, id string, d ChangeDetails) error {
	if !validUUID(id) {
		return fmt.Errorf("store: run %q not found", id)
	}
	_, err := s.pool.Exec(ctx, `UPDATE runs SET title = $2, web_url = $3, source_branch = $4, author = $5
		WHERE id = $1::uuid`, id, cutText(d.Title), cutText(d.WebURL), cutText(d.SourceBranch), cutText(d.Author))
	return err
}

// SetRunInputs records the model, the step limit (maxTurns; NULL when it is
// not above 0), where each skill came from (name → "library" or "repo") and
// the library version of each library skill used (name → version) and the
// hash of each skill's content (name → hash) when a run is dispatched. An
// unknown run is not an error; a malformed id is.
func (s *Store) SetRunInputs(ctx context.Context, id, model string, maxTurns int, sources map[string]string, versions map[string]int, hashes map[string]string) error {
	if !validUUID(id) {
		return fmt.Errorf("store: run %q not found", id)
	}
	if sources == nil {
		sources = map[string]string{}
	}
	if versions == nil {
		versions = map[string]int{}
	}
	b, err := json.Marshal(sources)
	if err != nil {
		return err
	}
	v, err := json.Marshal(versions)
	if err != nil {
		return err
	}
	if hashes == nil {
		hashes = map[string]string{}
	}
	h, err := json.Marshal(hashes)
	if err != nil {
		return err
	}
	var turns *int
	if maxTurns > 0 {
		turns = &maxTurns
	}
	_, err = s.pool.Exec(ctx, `UPDATE runs SET model = $2, max_turns = $3, skill_sources = $4::jsonb, skill_versions = $5::jsonb,
			skill_hashes = $6::jsonb
		WHERE id = $1::uuid`, id, cutText(model), turns, string(b), string(v), string(h))
	return err
}

// SkipRun ends a queued or running run as skipped, with the reason, at now:
// it has no skills to review with, so no Job runs and nothing is published.
// ok is false when the run was in another state or unknown; a malformed id
// is an error.
func (s *Store) SkipRun(ctx context.Context, id, reason string, now time.Time) (ok bool, err error) {
	if !validUUID(id) {
		return false, fmt.Errorf("store: run %q not found", id)
	}
	tag, err := s.pool.Exec(ctx, `UPDATE runs SET state = 'skipped', skip_reason = $2, reported_at = $3
		WHERE id = $1::uuid AND state IN ('queued', 'running')`, id, cutText(reason), now)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// HoldRun moves a push's queued, or claimed and not yet dispatched, run to
// Waiting with the reason (WaitUntrusted or WaitCheckFailed). A run started
// meanwhile (StartRun made it a TriggerStart) is not held. ok is false when the
// run was in another state or unknown; a malformed id is an error.
func (s *Store) HoldRun(ctx context.Context, id, reason string) (ok bool, err error) {
	if !validUUID(id) {
		return false, fmt.Errorf("store: run %q not found", id)
	}
	tag, err := s.pool.Exec(ctx, `UPDATE runs SET state = 'waiting', skip_reason = $2
		WHERE id = $1::uuid AND state IN ('queued', 'running') AND dispatched_at IS NULL AND trigger = 'push'`, id, cutText(reason))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// StartRun starts the review of headSHA onto targetBranch of an untrusted
// author's merge request. A waiting run of that head and target becomes a
// queued run with TriggerStart (started is true, id is that run's), and so
// does a push's run of it not dispatched yet, queued or claimed (being
// checked): the dispatcher's hold then leaves it alone. With
// none, a live (queued, running or reported) run of that head and target is
// left as it is and returned, started false. Otherwise a new queued TriggerStart run is
// created, superseding the live runs of other heads and targets and an older
// finished run of this one, as EnqueueRerun does. The caller checks the
// merge request's re-run cap first.
func (s *Store) StartRun(ctx context.Context, mr MRKey, projectPath, headSHA, targetBranch string) (id string, started bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if err := lockMR(ctx, tx, mr); err != nil {
		return "", false, err
	}
	err = tx.QueryRow(ctx, `UPDATE runs SET state = 'queued', trigger = 'start', skip_reason = '', not_before = NULL, quota_checked = true
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND head_sha = $4 AND target_branch = $5
		  AND state = 'waiting'
		RETURNING id::text`,
		mr.Instance, mr.ProjectID, mr.IID, headSHA, targetBranch).Scan(&id)
	if err == nil {
		return id, true, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, err
	}
	err = tx.QueryRow(ctx, `UPDATE runs SET trigger = 'start', quota_checked = true
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND head_sha = $4 AND target_branch = $5
		  AND state IN ('queued', 'running') AND dispatched_at IS NULL AND trigger = 'push'
		RETURNING id::text`,
		mr.Instance, mr.ProjectID, mr.IID, headSHA, targetBranch).Scan(&id)
	if err == nil {
		return id, true, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, err
	}
	err = tx.QueryRow(ctx, `SELECT id::text FROM runs
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND head_sha = $4 AND target_branch = $5
		  AND state IN ('queued', 'running', 'reported')`,
		mr.Instance, mr.ProjectID, mr.IID, headSHA, targetBranch).Scan(&id)
	if err == nil {
		if err := markQuotaChecked(ctx, tx, id); err != nil {
			return "", false, err
		}
		return id, false, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE runs SET state = 'superseded'
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND (head_sha <> $4 OR target_branch <> $5)
		  AND state IN ('queued', 'running', 'reported', 'waiting')`,
		mr.Instance, mr.ProjectID, mr.IID, headSHA, targetBranch); err != nil {
		return "", false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE runs SET state = 'superseded',
			cleaned_up = cleaned_up OR state IN ('published', 'publish_failed')
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND head_sha = $4 AND target_branch = $5
		  AND state <> 'superseded'`,
		mr.Instance, mr.ProjectID, mr.IID, headSHA, targetBranch); err != nil {
		return "", false, err
	}
	err = tx.QueryRow(ctx, `INSERT INTO runs (instance, project_id, mr_iid, project_path, head_sha, target_branch, trigger, quota_checked)
		VALUES ($1, $2, $3, $4, $5, $6, 'start', true) RETURNING id::text`,
		mr.Instance, mr.ProjectID, mr.IID, projectPath, headSHA, targetBranch).Scan(&id)
	if err != nil {
		return "", false, err
	}
	return id, true, tx.Commit(ctx)
}

// HeadSeenBy reports whether pergent.sh had queued a run of headSHA onto
// targetBranch for mr at or before at: whether that head existed when
// someone asked, at at, for the review of the change to be started.
func (s *Store) HeadSeenBy(ctx context.Context, mr MRKey, headSHA, targetBranch string, at time.Time) (bool, error) {
	var seen bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runs
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND head_sha = $4 AND target_branch = $5
		  AND created_at <= $6)`,
		mr.Instance, mr.ProjectID, mr.IID, headSHA, targetBranch, at).Scan(&seen)
	return seen, err
}

// TrustCheckFailing records that run id's trust check failed at now and
// returns when it first failed, now on the first failure.
func (s *Store) TrustCheckFailing(ctx context.Context, id string, now time.Time) (time.Time, error) {
	if !validUUID(id) {
		return time.Time{}, fmt.Errorf("store: run %q not found", id)
	}
	var since time.Time
	err := s.pool.QueryRow(ctx, `UPDATE runs SET trust_check_since = COALESCE(trust_check_since, $2)
		WHERE id = $1::uuid RETURNING trust_check_since`, id, now).Scan(&since)
	return since, err
}

// ClaimUntrustedNote records that the note about an untrusted author is
// posted on mr, at now. It is true only for the first claim, so the note is
// posted once per merge request; ReleaseUntrustedNote undoes a claim whose
// note couldn't be posted.
func (s *Store) ClaimUntrustedNote(ctx context.Context, mr MRKey, now time.Time) (bool, error) {
	var first bool
	err := s.pool.QueryRow(ctx, `INSERT INTO merge_requests (instance, project_id, mr_iid, untrusted_noted_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (instance, project_id, mr_iid) DO UPDATE SET untrusted_noted_at = EXCLUDED.untrusted_noted_at
			WHERE merge_requests.untrusted_noted_at IS NULL
		RETURNING true`, mr.Instance, mr.ProjectID, mr.IID, now).Scan(&first)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return first, err
}

// ReleaseUntrustedNote forgets that the note about an untrusted author is
// posted on mr.
func (s *Store) ReleaseUntrustedNote(ctx context.Context, mr MRKey) error {
	_, err := s.pool.Exec(ctx, `UPDATE merge_requests SET untrusted_noted_at = NULL
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3`, mr.Instance, mr.ProjectID, mr.IID)
	return err
}

// cutText shortens s to at most maxChangeDetail bytes, on a rune boundary.
func cutText(s string) string {
	if len(s) <= maxChangeDetail {
		return s
	}
	s = s[:maxChangeDetail]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// MarkDispatched records the Job started for a running run and the SHA-256 of
// its result token, valid until expires. It returns ErrNotRunning when the run
// is not in state running (for example it was superseded after it was claimed).
func (s *Store) MarkDispatched(ctx context.Context, id, jobName, baseSHA, startSHA string, tokenHash []byte, expires, now time.Time) error {
	if !validUUID(id) {
		return ErrNotRunning
	}
	tag, err := s.pool.Exec(ctx, `UPDATE runs SET job_name = $2, base_sha = $3, start_sha = $4,
		token_hash = $5, token_expires_at = $6, dispatched_at = $7 WHERE id = $1::uuid AND state = 'running'`,
		id, jobName, baseSHA, startSHA, tokenHash, expires, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrNotRunning
	}
	return nil
}

// mustUpdate runs an UPDATE that has to touch exactly one run.
func (s *Store) mustUpdate(ctx context.Context, id, sql string, args ...any) error {
	if !validUUID(id) {
		return fmt.Errorf("store: run %q not found", id)
	}
	tag, err := s.pool.Exec(ctx, sql, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("store: run %s not found or in the wrong state", id)
	}
	return nil
}

// validUUID reports whether id is in the canonical 8-4-4-4-12 hex form.
func validUUID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return false
			}
		}
	}
	return true
}

// StoreResult accepts the result a Job posted. The token is checked against
// the stored SHA-256 in constant time and must not be expired. The token hash
// is kept after the result, so single use is enforced by the run's state. The
// outcome, in order:
//   - ResultUnauthorized: unknown run, no token, wrong token or expired;
//   - ResultDiscarded: the run was superseded; the result is dropped;
//   - ResultConflict: the run already has a result or failure, or is not running;
//   - ErrInvalidResult: body is not valid JSON; nothing is stored;
//   - ResultStored: the run is now reported.
func (s *Store) StoreResult(ctx context.Context, id string, token string, body []byte, now time.Time) (ResultOutcome, error) {
	if !validUUID(id) {
		return ResultUnauthorized, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var (
		stored  []byte
		expires *time.Time
		state   string
		hasRes  bool
		failure string
	)
	err = tx.QueryRow(ctx, `SELECT token_hash, token_expires_at, state, result IS NOT NULL, failure_reason
		FROM runs WHERE id = $1::uuid FOR UPDATE`, id).Scan(&stored, &expires, &state, &hasRes, &failure)
	if errors.Is(err, pgx.ErrNoRows) {
		return ResultUnauthorized, nil
	}
	if err != nil {
		return 0, err
	}
	presented := sha256.Sum256([]byte(token))
	if stored == nil || subtle.ConstantTimeCompare(presented[:], stored) != 1 {
		return ResultUnauthorized, nil
	}
	if expires == nil || now.After(*expires) {
		return ResultUnauthorized, nil
	}

	switch {
	case RunState(state) == Superseded:
		return ResultDiscarded, nil
	case hasRes || failure != "" || RunState(state) != Running:
		return ResultConflict, nil
	}
	if !json.Valid(body) {
		return 0, ErrInvalidResult
	}

	if _, err := tx.Exec(ctx, `UPDATE runs SET result = $2::jsonb, state = 'reported',
		reported_at = $3, not_before = $3, attempts = 0 WHERE id = $1::uuid`, id, string(body), now); err != nil {
		return 0, fmt.Errorf("store: save result: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return ResultStored, nil
}

// StoreMergedResult replaces the result of a reported run with merged, the
// result combined with the kept skills of an earlier run (result.Merge), so the
// dashboard and every later reading show the whole review. It also stores a
// result with its step-limit advice replaced (result.ReplaceStepAdvice). It is
// an error when the run is not reported with a result, or merged is not valid
// JSON.
func (s *Store) StoreMergedResult(ctx context.Context, runID string, merged []byte) error {
	if !json.Valid(merged) {
		return ErrInvalidResult
	}
	return s.mustUpdate(ctx, runID, `UPDATE runs SET result = $2::jsonb
		WHERE id = $1::uuid AND state = 'reported' AND result IS NOT NULL`, runID, string(merged))
}

// MakeFullRerun turns a run that was to review some skills into a full
// re-run (TriggerRerun, every skill, no base run), for when its skills can't
// be kept apart from the rest of the review. It applies to a run being
// dispatched (queued or running) only; any other run is an error.
func (s *Store) MakeFullRerun(ctx context.Context, runID string) error {
	return s.mustUpdate(ctx, runID, `UPDATE runs SET trigger = $2, skill_subset = NULL, base_run_id = NULL
		WHERE id = $1::uuid AND state IN ('queued', 'running')`, runID, TriggerRerun)
}

// SetSkillSubset records the skills a run being dispatched (queued or running)
// really reviews, when some of those asked for are gone. The publisher merges
// by it, treating a skill of it without a result as failed. Any other run is
// an error.
func (s *Store) SetSkillSubset(ctx context.Context, runID string, skills []string) error {
	return s.mustUpdate(ctx, runID, `UPDATE runs SET skill_subset = $2
		WHERE id = $1::uuid AND state IN ('queued', 'running') AND skill_subset IS NOT NULL`, runID, skills)
}

// SetIncremental makes a run being dispatched (queued or running) review only
// the files changed since the run baseRunID, or, with baseRunID nil, review
// everything. Any other run is an error.
func (s *Store) SetIncremental(ctx context.Context, runID string, baseRunID *string) error {
	return s.mustUpdate(ctx, runID, `UPDATE runs SET base_run_id = $2::uuid, incremental = $3
		WHERE id = $1::uuid AND state IN ('queued', 'running')`, runID, baseRunID, baseRunID != nil)
}

// SetRunQuiet records whether a run being dispatched (queued or running) is
// quiet. Any other run is an error.
func (s *Store) SetRunQuiet(ctx context.Context, runID string, quiet bool) error {
	return s.mustUpdate(ctx, runID, `UPDATE runs SET quiet_reviews = $2
		WHERE id = $1::uuid AND state IN ('queued', 'running')`, runID, quiet)
}

// SetRunModel records the provider and model of a run being dispatched
// (queued or running), before its Job starts. Any other run is an error.
func (s *Store) SetRunModel(ctx context.Context, runID, provider, model string) error {
	return s.mustUpdate(ctx, runID, `UPDATE runs SET provider = $2, model = $3
		WHERE id = $1::uuid AND state IN ('queued', 'running')`, runID, cutText(provider), cutText(model))
}

// SetRunLayout records the comment layout of a run being dispatched (queued
// or running); nil or an empty list is the default layout. Any other run is an
// error.
func (s *Store) SetRunLayout(ctx context.Context, runID string, layout []string) error {
	return s.mustUpdate(ctx, runID, `UPDATE runs SET comment_layout = $2
		WHERE id = $1::uuid AND state IN ('queued', 'running')`, runID, nilIfEmpty(layout))
}

// SetRunLinks records the links a run being dispatched (queued or running)
// uses; nil or an empty list is none. Any other run is an error.
func (s *Store) SetRunLinks(ctx context.Context, runID string, links []Link) error {
	v, err := linksParam(links)
	if err != nil {
		return err
	}
	return s.mustUpdate(ctx, runID, `UPDATE runs SET linked_repos = $2::jsonb
		WHERE id = $1::uuid AND state IN ('queued', 'running')`, runID, v)
}

// ReportFailure records that a queued or running run will never produce a
// result (the Job died, the inputs did not fit, ...) so it is reported with
// reason. It reports whether it changed the run; a run that already has a
// result or a failure is left alone.
func (s *Store) ReportFailure(ctx context.Context, id, reason string, now time.Time) (bool, error) {
	if !validUUID(id) {
		return false, nil
	}
	if reason == "" {
		reason = "review failed"
	}
	tag, err := s.pool.Exec(ctx, `UPDATE runs SET state = 'reported', failure_reason = $2,
		reported_at = $3, not_before = $3, attempts = 0
		WHERE id = $1::uuid AND state IN ('queued', 'running') AND result IS NULL AND failure_reason = ''`,
		id, reason, now)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// Run returns one run. A malformed or unknown id is reported as not found.
func (s *Store) Run(ctx context.Context, id string) (Run, bool, error) {
	if !validUUID(id) {
		return Run{}, false, nil
	}
	r, err := s.getRun(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, false, nil
	}
	if err != nil {
		return Run{}, false, err
	}
	return r, true, nil
}

// ReplaceWithFailure turns the stored result of a reported run into a failure
// with reason: it clears the result and keeps the state reported, so the
// publisher posts an error comment. It returns ErrNotReported when the run is
// not reported with a result.
func (s *Store) ReplaceWithFailure(ctx context.Context, id, reason string) error {
	if !validUUID(id) {
		return ErrNotReported
	}
	if reason == "" {
		reason = "review failed"
	}
	tag, err := s.pool.Exec(ctx, `UPDATE runs SET result = NULL, failure_reason = $2
		WHERE id = $1::uuid AND state = 'reported' AND result IS NOT NULL`, id, reason)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrNotReported
	}
	return nil
}

// RunsToPublish returns reported runs that are due at now, oldest first.
func (s *Store) RunsToPublish(ctx context.Context, now time.Time, limit int) ([]Run, error) {
	return s.queryRuns(ctx, `SELECT `+runColumns+` FROM runs
		WHERE state = 'reported' AND (not_before IS NULL OR not_before <= $1)
		ORDER BY reported_at, created_at, id LIMIT $2`, now, limit)
}

func (s *Store) queryRuns(ctx context.Context, sql string, args ...any) ([]Run, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return scanRuns(rows)
}

// MarkPublished records that a reported run's comment and status are out,
// and what was decided (each publish replaces the last decision). A run
// Republish flagged meanwhile stays reported instead, once, with no
// attempts counted and due now, so the publisher decides it again.
func (s *Store) MarkPublished(ctx context.Context, id string, dec RunDecision) error {
	return s.mustUpdate(ctx, id, `UPDATE runs SET
			state = CASE WHEN redecide THEN 'reported' ELSE 'published' END,
			attempts = CASE WHEN redecide THEN 0 ELSE attempts END,
			not_before = CASE WHEN redecide THEN NULL ELSE not_before END,
			publish_error = '', redecide = false,
			outcome = $2, outcome_reason = $3, decision_status = $4, approved = $5
		WHERE id = $1::uuid AND state = 'reported'`, id, dec.Outcome, dec.Reason, dec.Status, dec.Approved)
}

// PublishRetry records a failed publish: the run is tried again at notBefore,
// or becomes publish_failed when this was the 10th failed attempt, which it
// reports (gaveUp), only on that one transition. The retry decides afresh, so
// a pending Republish flag is dropped.
//
// On that transition, in the same statement, the merge request's recorded
// approval of the run's head and target (an approval recorded without a
// target is compared by head only) is marked to be withdrawn: a review that
// failed for good must never leave the approve bot's approval standing on its
// commit, and a crash right after the give-up can't lose the debt.
func (s *Store) PublishRetry(ctx context.Context, id, errText string, notBefore time.Time) (gaveUp bool, err error) {
	if len(errText) > maxPublishError {
		errText = errText[:maxPublishError]
	}
	if !validUUID(id) {
		return false, fmt.Errorf("store: run %q not found", id)
	}
	var state string
	// The data-modifying CTE w runs whether or not the final SELECT reads it.
	err = s.pool.QueryRow(ctx, `WITH r AS (
			UPDATE runs SET attempts = attempts + 1, publish_error = $2, not_before = $3, redecide = false,
				state = CASE WHEN attempts + 1 >= $4 THEN 'publish_failed' ELSE state END
			WHERE id = $1::uuid AND state = 'reported'
			RETURNING state, instance, project_id, mr_iid, head_sha, target_branch),
		w AS (
			UPDATE merge_requests m SET withdraw_pending = true
			FROM r
			WHERE r.state = 'publish_failed'
			  AND m.instance = r.instance AND m.project_id = r.project_id AND m.mr_iid = r.mr_iid
			  AND m.approved_sha <> '' AND m.approved_sha = r.head_sha
			  AND (m.approved_target = '' OR m.approved_target = r.target_branch)
			RETURNING 1)
		SELECT state FROM r`, id, errText, notBefore, maxPublishAttempts).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("store: run %s not found or in the wrong state", id)
	}
	if err != nil {
		return false, err
	}
	return RunState(state) == PublishFailed, nil
}

// Republish asks the publisher to decide the merge request's newest
// non-superseded run again, when that run is for headSHA and targetBranch (a
// draft marked ready is approved then), and reports whether it did. A run of
// another target is left alone: the merge request was retargeted since, and
// that review is not one of the branch it now has.
//   - published: it goes back to reported, with no attempts counted and due
//     now;
//   - reported: a publish may be under way that read the draft flag before
//     the change, so the run is flagged to stay reported when it is marked
//     published (see MarkPublished); its backoff is kept.
//
// Any other newest run (queued or running, publish_failed, of another head,
// of another target, or none) is left alone: the normal flow reads the flag when it publishes.
// It takes the same per-MR lock as EnqueueRun, so a push racing it can't
// leave an older head's review to be posted over the newer one; the update
// is one statement, so it serialises with MarkPublished on the row.
func (s *Store) Republish(ctx context.Context, mr MRKey, headSHA, targetBranch string) (bool, error) {
	return s.republish(ctx, mr, headSHA, targetBranch, false)
}

// RepublishNewest applies Republish's rules to the merge request's newest
// non-superseded run whatever its head and target. A dismissal made or
// restored on the dashboard reaches the review that is shown or about to be
// shown, which may be of a head the platform doesn't have yet (a push the
// webhook handled), or a same-head re-run whose first publish is under way.
// A queued or running newest run needs nothing: it loads the dismissals when
// it is published.
func (s *Store) RepublishNewest(ctx context.Context, mr MRKey) (bool, error) {
	return s.republish(ctx, mr, "", "", true)
}

func (s *Store) republish(ctx context.Context, mr MRKey, headSHA, targetBranch string, anyHead bool) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if err := lockMR(ctx, tx, mr); err != nil {
		return false, err
	}
	// In SET, state is the row's state before the update.
	tag, err := tx.Exec(ctx, `UPDATE runs SET
			redecide = state = 'reported',
			attempts = CASE WHEN state = 'published' THEN 0 ELSE attempts END,
			publish_error = CASE WHEN state = 'published' THEN '' ELSE publish_error END,
			not_before = CASE WHEN state = 'published' THEN NULL ELSE not_before END,
			state = 'reported'
		WHERE id = (
			SELECT id FROM runs
			WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND state <> 'superseded'
			ORDER BY created_at DESC, id DESC LIMIT 1)
		  AND state IN ('published', 'reported') AND ($6 OR (head_sha = $4 AND target_branch = $5))`,
		mr.Instance, mr.ProjectID, mr.IID, headSHA, targetBranch, anyHead)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// RunsToCleanUp returns superseded runs whose Job has not been cleaned up.
func (s *Store) RunsToCleanUp(ctx context.Context) ([]Run, error) {
	return s.queryRuns(ctx, `SELECT `+runColumns+` FROM runs
		WHERE state = 'superseded' AND NOT cleaned_up ORDER BY created_at, id`)
}

// MarkCleanedUp records that a superseded run's Job is gone.
func (s *Store) MarkCleanedUp(ctx context.Context, id string) error {
	return s.mustUpdate(ctx, id, `UPDATE runs SET cleaned_up = true WHERE id = $1::uuid`, id)
}

// RunningRuns returns every run in state running.
func (s *Store) RunningRuns(ctx context.Context) ([]Run, error) {
	return s.queryRuns(ctx, `SELECT `+runColumns+` FROM runs WHERE state = 'running' ORDER BY created_at, id`)
}

// LastReview returns the latest published run of the merge request whose
// result status is "completed", other than beforeRunID. found is false when
// there is none.
func (s *Store) LastReview(ctx context.Context, mr MRKey, beforeRunID string) (run Run, found bool, err error) {
	runs, err := s.queryRuns(ctx, `SELECT `+runColumns+` FROM runs
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND state = 'published'
		  AND result->>'status' = 'completed' AND id::text <> $4
		ORDER BY created_at DESC, id DESC LIMIT 1`,
		mr.Instance, mr.ProjectID, mr.IID, beforeRunID)
	if err != nil || len(runs) == 0 {
		return Run{}, false, err
	}
	return runs[0], true, nil
}

// NewerRunOfHead reports whether the merge request has a run of run's head
// and target that was created after run and is not superseded.
func (s *Store) NewerRunOfHead(ctx context.Context, run Run) (bool, error) {
	var newer bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runs
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND head_sha = $4 AND target_branch = $5
		  AND state <> 'superseded' AND (created_at, id) > ($6, $7::uuid))`,
		run.MR.Instance, run.MR.ProjectID, run.MR.IID, run.HeadSHA, run.TargetBranch, run.CreatedAt, run.ID).Scan(&newer)
	return newer, err
}

// LatestDecidedRun returns the merge request's newest run whose review is
// out: published, or reported again after Republish asked for the decision
// to be made anew (it has been decided before). It is the review the
// merge request's comment shows, and what a comment command refers to. A
// run still waiting for its first publish is not one, nor is a superseded
// run. found is false when there is none.
func (s *Store) LatestDecidedRun(ctx context.Context, mr MRKey) (run Run, found bool, err error) {
	runs, err := s.queryRuns(ctx, `SELECT `+runColumns+` FROM runs
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3
		  AND (state = 'published' OR (state = 'reported' AND outcome <> ''))
		ORDER BY created_at DESC, id DESC LIMIT 1`, mr.Instance, mr.ProjectID, mr.IID)
	if err != nil || len(runs) == 0 {
		return Run{}, false, err
	}
	return runs[0], true, nil
}

// NoteID returns the id of the merge request's pergent comment, or 0.
func (s *Store) NoteID(ctx context.Context, mr MRKey) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `SELECT note_id FROM merge_requests
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3`, mr.Instance, mr.ProjectID, mr.IID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// SetNoteID records the id of the merge request's pergent comment.
func (s *Store) SetNoteID(ctx context.Context, mr MRKey, noteID int64) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO merge_requests (instance, project_id, mr_iid, note_id)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (instance, project_id, mr_iid) DO UPDATE SET note_id = EXCLUDED.note_id`,
		mr.Instance, mr.ProjectID, mr.IID, noteID)
	return err
}

// SetApprovedSHA records the commit the bot approved and the target branch
// it was reviewed against; an empty sha clears the approval.
func (s *Store) SetApprovedSHA(ctx context.Context, mr MRKey, sha, target string) error {
	if sha == "" {
		target = ""
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO merge_requests (instance, project_id, mr_iid, approved_sha, approved_target)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (instance, project_id, mr_iid) DO UPDATE SET
			approved_sha = EXCLUDED.approved_sha, approved_target = EXCLUDED.approved_target,
			unapprove_failures = 0, unapprove_not_before = NULL, withdraw_pending = false`,
		mr.Instance, mr.ProjectID, mr.IID, sha, target)
	return err
}

// ClearApprovedSHA clears the recorded approval only while it is still the
// old commit and target, so an approval recorded meanwhile (of a newer head
// or target) is kept. A merge request without a row, or with another
// approval, is left alone. The unapprove backoff starts over.
func (s *Store) ClearApprovedSHA(ctx context.Context, mr MRKey, oldSHA, oldTarget string) error {
	_, err := s.pool.Exec(ctx, `UPDATE merge_requests SET approved_sha = '', approved_target = '',
			unapprove_failures = 0, unapprove_not_before = NULL, withdraw_pending = false
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND approved_sha = $4 AND approved_target = $5`,
		mr.Instance, mr.ProjectID, mr.IID, oldSHA, oldTarget)
	return err
}

// MarkWithdrawPending records that the approval of sha against target is to
// be withdrawn, until it is cleared or replaced. A merge request that records
// another approval (or none) is left alone.
func (s *Store) MarkWithdrawPending(ctx context.Context, mr MRKey, sha, target string) error {
	_, err := s.pool.Exec(ctx, `UPDATE merge_requests SET withdraw_pending = true
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND approved_sha = $4 AND approved_target = $5
			AND approved_sha <> ''`,
		mr.Instance, mr.ProjectID, mr.IID, sha, target)
	return err
}

// PendingWithdrawals returns the recorded approvals owed a withdrawal whose
// unapprove backoff (after a failed removal) is over at now: those marked for
// it, and, failing closed, those of the head and target of the merge
// request's newest non-superseded run when that run gave up publishing
// (publish_failed), marked or not: a row from before PublishRetry marked it, or
// a mark that was lost.
func (s *Store) PendingWithdrawals(ctx context.Context, now time.Time) ([]Approval, error) {
	return s.queryApprovals(ctx, `SELECT m.instance, m.project_id, m.mr_iid, m.approved_sha, m.approved_target
		FROM merge_requests m
		WHERE m.approved_sha <> ''
			AND (m.withdraw_pending OR EXISTS (
				SELECT 1 FROM (
					SELECT r.state, r.head_sha, r.target_branch FROM runs r
					WHERE r.instance = m.instance AND r.project_id = m.project_id AND r.mr_iid = m.mr_iid
					  AND r.state <> 'superseded'
					ORDER BY r.created_at DESC, r.id DESC LIMIT 1) n
				WHERE n.state = 'publish_failed' AND n.head_sha = m.approved_sha
				  AND (m.approved_target = '' OR m.approved_target = n.target_branch)))
			AND (m.unapprove_not_before IS NULL OR m.unapprove_not_before <= $1)
		ORDER BY m.instance, m.project_id, m.mr_iid`, now)
}

// UnapproveFailed records a failed removal of the stale approval of oldSHA
// against oldTarget: it is not listed by StaleApprovals again before now +
// 30 s × 2^(earlier failures), at most 5 minutes later. It returns that
// time, or the zero time when the merge request records another approval
// (or none) by now, which is left alone.
func (s *Store) UnapproveFailed(ctx context.Context, mr MRKey, oldSHA, oldTarget string, now time.Time) (time.Time, error) {
	var next time.Time
	// The SET expressions read the row before the update: the n-th failure
	// waits 30 s << (n-1), and from the 5th on the cap (30 s << 4 is past it).
	err := s.pool.QueryRow(ctx, `UPDATE merge_requests SET unapprove_failures = unapprove_failures + 1,
			unapprove_not_before = $6::timestamptz + LEAST(
				interval '30 seconds' * (1 << LEAST(unapprove_failures, 4)), interval '5 minutes')
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND approved_sha = $4 AND approved_target = $5
			AND approved_sha <> ''
		RETURNING unapprove_not_before`,
		mr.Instance, mr.ProjectID, mr.IID, oldSHA, oldTarget, now).Scan(&next)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	return next, err
}

// StaleApprovals returns merge requests whose recorded approval is for
// another head or another target branch than their newest non-superseded
// run, and whose unapprove backoff (after a failed removal) is over at now.
// An approval recorded without a target is compared by head only.
func (s *Store) StaleApprovals(ctx context.Context, now time.Time) ([]Approval, error) {
	return s.queryApprovals(ctx, `SELECT m.instance, m.project_id, m.mr_iid, m.approved_sha, m.approved_target
		FROM merge_requests m
		JOIN LATERAL (
			SELECT r.head_sha, r.target_branch FROM runs r
			WHERE r.instance = m.instance AND r.project_id = m.project_id AND r.mr_iid = m.mr_iid
			  AND r.state <> 'superseded'
			ORDER BY r.created_at DESC, r.id DESC LIMIT 1) newest ON true
		WHERE m.approved_sha <> '' AND (newest.head_sha <> m.approved_sha
			OR (m.approved_target <> '' AND newest.target_branch <> m.approved_target))
			AND (m.unapprove_not_before IS NULL OR m.unapprove_not_before <= $1)
		ORDER BY m.instance, m.project_id, m.mr_iid`, now)
}

// ApprovalsDue returns every recorded approval, stale or not, whose
// unapprove backoff (after a failed removal) is over at now, each with its
// project's stored path.
func (s *Store) ApprovalsDue(ctx context.Context, now time.Time) ([]ProjectApproval, error) {
	rows, err := s.pool.Query(ctx, `SELECT m.instance, m.project_id, m.mr_iid, m.approved_sha, m.approved_target,
			COALESCE(p.path, '')
		FROM merge_requests m
		LEFT JOIN projects p ON p.instance = m.instance AND p.project_id = m.project_id
		WHERE m.approved_sha <> ''
			AND (m.unapprove_not_before IS NULL OR m.unapprove_not_before <= $1)
		ORDER BY m.instance, m.project_id, m.mr_iid`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProjectApproval
	for rows.Next() {
		var a ProjectApproval
		if err := rows.Scan(&a.MR.Instance, &a.MR.ProjectID, &a.MR.IID, &a.ApprovedSHA, &a.ApprovedTarget, &a.ProjectPath); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Approval returns the recorded approval of a merge request, if it has one.
func (s *Store) Approval(ctx context.Context, mr MRKey) (Approval, bool, error) {
	as, err := s.queryApprovals(ctx, `SELECT instance, project_id, mr_iid, approved_sha, approved_target
		FROM merge_requests
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND approved_sha <> ''`,
		mr.Instance, mr.ProjectID, mr.IID)
	if err != nil || len(as) == 0 {
		return Approval{}, false, err
	}
	return as[0], true, nil
}

// Approvals returns every recorded approval.
func (s *Store) Approvals(ctx context.Context) ([]Approval, error) {
	return s.queryApprovals(ctx, `SELECT instance, project_id, mr_iid, approved_sha, approved_target
		FROM merge_requests WHERE approved_sha <> '' ORDER BY instance, project_id, mr_iid`)
}

func (s *Store) queryApprovals(ctx context.Context, sql string, args ...any) ([]Approval, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		var a Approval
		if err := rows.Scan(&a.MR.Instance, &a.MR.ProjectID, &a.MR.IID, &a.ApprovedSHA, &a.ApprovedTarget); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// WaitingChange is a merge request whose run waits for someone with write
// access to start it (a Waiting run): its number, what the dispatcher read
// of it, when the run was queued and why it waits.
type WaitingChange struct {
	IID                   int64
	Head                  string
	Title, Author, WebURL string
	Since                 time.Time
	Reason                string
}

// WaitingChanges returns the merge requests of project (its path, matched
// ignoring case) on instance whose newest live run waits, newest first, at
// most 100. A waiting run with a newer one (left by a release that didn't
// know waiting runs) isn't listed.
func (s *Store) WaitingChanges(ctx context.Context, instance, project string) ([]WaitingChange, error) {
	rows, err := s.pool.Query(ctx, `SELECT r.mr_iid, r.head_sha, r.title, r.author, r.web_url, r.created_at, r.skip_reason FROM runs r
		WHERE r.instance = $1 AND lower(r.project_path) = lower($2) AND r.state = 'waiting'
		  AND NOT EXISTS (SELECT 1 FROM runs n
			WHERE n.instance = r.instance AND n.project_id = r.project_id AND n.mr_iid = r.mr_iid
			  AND n.state <> 'superseded' AND (n.created_at, n.id) > (r.created_at, r.id))
		ORDER BY r.created_at DESC, r.id DESC LIMIT 100`, instance, project)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (WaitingChange, error) {
		var c WaitingChange
		err := row.Scan(&c.IID, &c.Head, &c.Title, &c.Author, &c.WebURL, &c.Since, &c.Reason)
		return c, err
	})
}
