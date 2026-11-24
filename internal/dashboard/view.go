package dashboard

import (
	"encoding/json"
	"time"

	"github.com/gopaytech/pergent.sh/internal/readiness"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// The JSON API's contract: every body the read API returns, field names in
// camelCase. The frontend mirrors these types. Times are RFC 3339 strings.
//
// Every endpoint needs a signed-in session (401 otherwise) and answers only
// about the repositories its user can see: for an admin every one, on every
// instance; for any other account those its teams cover, on every instance.
// The platform is never asked. An account that is not an admin and is on no
// team gets 403 {"error": "an admin needs to add you to a team", "code":
// "waiting"} for everything but GET /api/me. A repository, review or run the user can't see is answered
// exactly like one that doesn't exist: left out of lists, 404 for its own
// URL. Errors are {"error": "message"}. Nothing here ever holds a token,
// key or secret.
//
// Outcomes ("outcome") are one of:
//   - "running": queued, being reviewed, or reviewed and not posted yet;
//   - "approved": the approve bot approved this run (also when the approval
//     was withdrawn later, for example when the change merged);
//   - "blocking": a skill found blocking issues;
//   - "ci_blocked": a blocking CI job the repository chose isn't passed
//     (failed, cancelled, skipped, missing, running or unreadable); the
//     review itself had no blocking issues;
//   - "draft": clean, not approved because the change was a draft;
//   - "no_blocking": clean but not approved, for the reason given (approval
//     turned off, files skipped, the approve side can't reach the
//     repository, the change closed or moved before the approval);
//   - "failed": no usable review (the Job failed, a skill failed, the
//     result was unusable, or it couldn't be posted);
//   - "superseded": replaced by a newer push, or the change was closed;
//   - "fell_back": an attempt dropped because the provider refused its model
//     entry (out of quota or credit, or its key rejected); the review went on
//     with the next entry of the chain, so it is never a failure;
//   - "skipped": not reviewed because the repository has no skills (no Job,
//     no comment, no status); never counted as a review or a failure;
//   - "waiting": held because the repository doesn't trust the change's
//     author, until someone with write access starts it;
//   - "unknown": published before pergent.sh recorded outcomes.
//
// A published run's outcome, reason and status are what the publisher
// recorded when it posted the review; later changes to settings or to the
// approval don't change them.

// Outcome values.
const (
	OutcomeRunning    = store.OutcomeRunning
	OutcomeApproved   = store.OutcomeApproved
	OutcomeBlocking   = store.OutcomeBlocking
	OutcomeCI         = store.OutcomeCI
	OutcomeDraft      = store.OutcomeDraft
	OutcomeNoBlocking = store.OutcomeNoBlocking
	OutcomeFailed     = store.OutcomeFailed
	OutcomeSuperseded = store.OutcomeSuperseded
	OutcomeFellBack   = store.OutcomeFellBack
	OutcomeSkipped    = store.OutcomeSkipped
	OutcomeUnknown    = store.OutcomeUnknown
	OutcomeWaiting    = store.OutcomeWaiting
)

// OverviewView is GET /api/overview?days=7 (days 1 to 90, default 7):
// numbers over the runs started in the last days, the 10 most recently
// updated reviews of the last 90 days and the setup problems of the
// repositories the user can see (instance-wide ones and the library's own,
// with no instance, only for admins). Setup is the setup checklist, for
// admins only, and null once all four items are done (and for everyone
// else).
type OverviewView struct {
	Stats     StatsView       `json:"stats"`
	Recent    []ReviewSummary `json:"recent"`
	Attention []AttentionView `json:"attention"`
	Setup     *SetupView      `json:"setup"`
}

// SetupView is what an admin still has to set up, one item each: an LLM
// with its key, a connected instance, a skill in the library (a repository's
// own skills don't count) and a repository switched on.
type SetupView struct {
	LLM        bool `json:"llm"`
	Instance   bool `json:"instance"`
	Skill      bool `json:"skill"`
	Repository bool `json:"repository"`
}

// StatsView counts the runs started in the period that weren't superseded,
// by outcome; approved counts the runs the approve bot approved.
type StatsView struct {
	Reviews  int `json:"reviews"`
	Approved int `json:"approved"`
	Blocking int `json:"blocking"`
	Failed   int `json:"failed"`
}

// ReviewSummary is one merge request or pull request, as its newest run
// left it.
type ReviewSummary struct {
	Instance string `json:"instance"`
	Project  string `json:"project"`
	// Platform is "gitlab" or "github".
	Platform string `json:"platform"`
	// Number is the MR's IID or the PR's number.
	Number  int64  `json:"number"`
	Title   string `json:"title"`
	Outcome string `json:"outcome"`
	// Reason says why, in a sentence ("" for approved).
	Reason    string    `json:"reason"`
	UpdatedAt time.Time `json:"updatedAt"`
	// WebURL is the change's page on the platform; "" when unknown.
	WebURL string `json:"webURL"`
	// Attempts is how many attempts the newest review took: more than 1
	// when it fell back to a backup model.
	Attempts int `json:"attempts"`
}

// ReviewListView is GET
// /api/reviews?project=&instance=&outcome=&days=&cursor=: 50 reviews at
// most, the most recently updated first. project (a path, on instance when
// that is given, else on any) and outcome filter; days (1 to 365, default
// 90) leaves out reviews last updated longer ago. nextCursor, when not "",
// is passed as cursor for the next page.
type ReviewListView struct {
	Items      []ReviewSummary `json:"items"`
	NextCursor string          `json:"nextCursor"`
}

// ReviewDetailView is GET /api/reviews/{instance}/{project...}/{number}:
// the change and every run of it, newest first.
type ReviewDetailView struct {
	Header ReviewHeader `json:"header"`
	Runs   []RunSummary `json:"runs"`
	// Role is the session's role on the repository: "viewer", "maintainer"
	// or "admin" ("" for a repository switched off).
	Role string  `json:"role"`
	Can  CanView `json:"can"`
}

// CanView is what the session may do to the review with the buttons. The
// platform user who opened the change is told apart by username here (the
// answer is a hint for the page); the dismiss route checks by user ID.
type CanView struct {
	// Rerun starts the review again, whole or only its failed skills.
	Rerun bool `json:"rerun"`
	// DismissBlocking dismisses a blocking finding: not the change's author.
	DismissBlocking bool `json:"dismissBlocking"`
	// DismissSuggestion dismisses a suggestion.
	DismissSuggestion bool `json:"dismissSuggestion"`
	// Restore undoes a dismissal.
	Restore bool `json:"restore"`
}

// ReviewHeader is the change as pergent.sh last read it.
type ReviewHeader struct {
	Instance     string `json:"instance"`
	Project      string `json:"project"`
	Platform     string `json:"platform"`
	Number       int64  `json:"number"`
	Title        string `json:"title"`
	SourceBranch string `json:"sourceBranch"`
	TargetBranch string `json:"targetBranch"`
	// Author is the username who opened it; left out when unknown.
	Author string `json:"author,omitempty"`
	WebURL string `json:"webURL"`
}

// RunSummary is one run in a review's history.
type RunSummary struct {
	ID   string `json:"id"`
	Head string `json:"head"`
	// Trigger is what started it, read from the history: "first" (the
	// first run), "push" (a new head), "retarget" (a new target branch) or
	// "rerun" (the same head and target again: reopened, marked ready, or
	// the repository switched on again).
	Trigger   string    `json:"trigger"`
	Outcome   string    `json:"outcome"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"createdAt"`
	// DurationSeconds is from the Job's start to its result; null while it
	// runs or when it never started.
	DurationSeconds *float64 `json:"durationSeconds"`
}

// RunDetailView is GET /api/runs/{id}. A run without a usable result has
// no skills or files; its reason (run.reason, approvalReason) says why.
type RunDetailView struct {
	Run      RunSummary `json:"run"`
	Instance string     `json:"instance"`
	Project  string     `json:"project"`
	Number   int64      `json:"number"`
	// Status is the status pergent.sh set on the commit: "running",
	// "success", "failed", "neutral", "canceled", or "" when none.
	Status string `json:"status"`
	// Approved: the approve bot approved this run.
	Approved bool `json:"approved"`
	// ApprovalReason is why it isn't approved; "" when it is.
	ApprovalReason string `json:"approvalReason"`
	PergentVersion string `json:"pergentVersion"`
	// Model is the model the review ran with, as recorded when it started;
	// null for a run that didn't record it.
	Model *string `json:"model"`
	// Provider is the provider the review ran with, as recorded when it
	// started; null for a run that didn't record it (an earlier release
	// recorded the model alone).
	Provider *string `json:"provider"`
	// MaxTurns is the step limit each opencode run of the review had, as
	// recorded when it started; null for a run that didn't record it.
	MaxTurns *int `json:"maxTurns"`
	// IncrementalNote says what an incremental review covered, for example
	// "Re-reviewed 2 files · 28 carried over from `abc1234`"; "" for a full
	// review.
	IncrementalNote string `json:"incrementalNote"`
	// Quiet is whether the review was started quiet, as recorded when it
	// started; false for a run that didn't record it.
	Quiet bool `json:"quiet"`
	// CIJobs is the snapshot of the CI jobs the repository chose, as the run
	// was decided with them: no CI text. Null for a run that has none.
	CIJobs []CIJobResultView `json:"ciJobs"`
	// CIDeadline is when a run waiting for CI stops waiting; null when it
	// isn't waiting for CI.
	CIDeadline *time.Time  `json:"ciDeadline"`
	Tokens     TokensView  `json:"tokens"`
	Files      FilesView   `json:"files"`
	Skills     []SkillView `json:"skills"`
	// RulesUsed is whether the image that reviewed knows path rules (its
	// result has a rules object); false for an older image or no result.
	RulesUsed bool `json:"rulesUsed"`
	// BuiltInRulesNote is why the built-in rules the repository chose were
	// left out of this review (its image can't load only those); "" when
	// they were not.
	BuiltInRulesNote string `json:"builtInRulesNote"`
	// LatestDecided is true for the one run of the change that dismiss and
	// restore act on: its newest published review, or reported one with an
	// outcome. Only that run shows dismissals; false when the change has no
	// such run.
	LatestDecided bool `json:"latestDecided"`
	// Kubernetes names the review's Job and pod, and says what the cluster
	// told us when it failed to start. Admins only: for anyone else the key
	// is left out. Present from the moment the run has a Job.
	Kubernetes *KubernetesView `json:"kubernetes,omitempty"`
	// Entry is the chain entry the run ran on, by the name it had then;
	// null for a run that recorded none (before chains, or before it was
	// dispatched).
	Entry *RunEntryView `json:"entry"`
	// Attempts are every attempt of the review this run is one of, oldest
	// first, this run included; empty for a review of one attempt.
	Attempts []AttemptView `json:"attempts"`
	// MultiReview is the multi review of a run that was split, or of a later
	// attempt of its judge: its reviewers, the judge and the total, or why
	// it did not start. The key is left out for any other run.
	MultiReview *RunMultiReviewView `json:"multiReview,omitempty"`
}

// RunMultiReviewView is a multi review on its run's page: why it did not
// start (Off), or one row per reviewer, the judge row and the total. The
// merged findings are the run's own result; raw reviewer findings are only
// in the member logs.
type RunMultiReviewView struct {
	Off       string         `json:"off,omitempty"`
	Reviewers []ReviewerView `json:"reviewers"`
	// Judge is what this run judged with; nil when it recorded nothing (its
	// reviewers still run, or its judge Job was not dispatched yet).
	Judge *JudgeView `json:"judge"`
	// Total is every token of the review: each reviewer attempt and each
	// judge run, the same on each attempt's page. The run's own tokens stay
	// in its Tokens.
	Total TokensView `json:"total"`
}

// ReviewerView is one reviewer of a multi review, by its number (1 to 5).
// Entry is the name, in the frozen copy of its chain, of the entry its final
// attempt ran on (the chain's first entry when it never started); Provider,
// Model and State are that attempt's. In and Out are the tokens of all its
// attempts.
type ReviewerView struct {
	Number   int                 `json:"number"`
	Entry    string              `json:"entry"` // from the frozen copy
	Provider string              `json:"provider"`
	Model    string              `json:"model"`
	State    string              `json:"state"`   // the final attempt's
	Skipped  string              `json:"skipped"` // the reason, "" when it has a result
	Findings int                 `json:"findings"`
	In       int64               `json:"in"`
	Out      int64               `json:"out"`
	Attempts []MemberAttemptView `json:"attempts"` // each with its log link by ID
}

// MemberAttemptView is one attempt of a reviewer: the entry it ran on (its
// name in the frozen chain and its place there), the model, its state, and
// why it failed with the step it failed at ("" when it did not fail).
// Provider and Model are "" before the attempt was dispatched.
type MemberAttemptView struct {
	ID       string `json:"id"`
	Entry    string `json:"entry"`
	Position int    `json:"position"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// State is "queued", "running", "reported", "done", "failed",
	// "fell_back", "superseded" or "skipped".
	State   string `json:"state"`
	Failure string `json:"failure"`
	Step    string `json:"step"`
}

// JudgeView is the judge of a multi review: the entry, provider and model
// this run judged with, or NoJudge and why (Reason) when it ran with no
// judge. Attempts is how many judge runs the review has (the run that was
// split and each later attempt); In and Out are the tokens of all of them.
type JudgeView struct {
	Entry    string `json:"entry"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	NoJudge  bool   `json:"noJudge"`
	Reason   string `json:"reason"`
	Attempts int    `json:"attempts"`
	In       int64  `json:"in"`
	Out      int64  `json:"out"`
}

// RunEntryView is the entry a run ran on: its name, its place in the chain
// in effect (Position of Length), its source ("global" or "team") and its
// team's name ("" for a global one).
type RunEntryView struct {
	Name     string `json:"name"`
	Position int    `json:"position"`
	Length   int    `json:"length"`
	Source   string `json:"source"`
	Team     string `json:"team"`
}

// AttemptView is one attempt of a review. Outcome is the run's ("fell_back"
// for one that was dropped for the next entry, "" while it runs); Reason is
// the class a dropped attempt fell back on ("quota", "billing" or "auth"),
// "" otherwise.
type AttemptView struct {
	RunID    string `json:"runId"`
	Name     string `json:"name"`
	Position int    `json:"position"`
	Outcome  string `json:"outcome"`
	Reason   string `json:"reason"`
}

// KubernetesView is the cluster side of a run, for an admin to look it up
// with kubectl. Pod is "" until a pod was seen; Detail is more text about
// the failure ("" when there is none).
type KubernetesView struct {
	Namespace string `json:"namespace"`
	Job       string `json:"job"`
	Pod       string `json:"pod"`
	Detail    string `json:"detail"`
}

// TokensView sums the skills' token counts.
type TokensView struct {
	In  int64 `json:"in"`
	Out int64 `json:"out"`
}

// FilesView is the files of every skill together, each path once.
type FilesView struct {
	Reviewed    []string          `json:"reviewed"`
	Skipped     []SkippedFile     `json:"skipped"`
	NotReviewed []NotReviewedFile `json:"notReviewed"`
	// Rules is the path rule each reviewed file was checked with, for the
	// files that had one.
	Rules []FileRuleView `json:"rules"`
}

// FileRuleView is the rule a reviewed file was checked with:
// "repo:<glob>", "dashboard:<set>:<glob>" or "built-in:<name>".
type FileRuleView struct {
	Path string `json:"path"`
	Rule string `json:"rule"`
}

// SkippedFile is a file no skill reviewed, and the rule that left it out.
type SkippedFile struct {
	Path string `json:"path"`
	Rule string `json:"rule"`
}

// NotReviewedFile is a file whose batch failed, and why.
type NotReviewedFile struct {
	Path  string `json:"path"`
	Cause string `json:"cause"`
}

// SkillView is one skill's outcome.
type SkillView struct {
	Name string `json:"name"`
	// Verdict is "no_blocking_issues", "has_blocking_issues" or "failed".
	Verdict string `json:"verdict"`
	// Source is "library" for a skill of pergent.sh's library, or the
	// repository folder it was read from (".pergent/skills", ".agents/skills",
	// ".claude/skills" or ".github/skills"; also one replacing a library
	// skill), as recorded when the review started; "repo" for a run from
	// before the folders; null when it wasn't recorded.
	Source *string `json:"source"`
	// Version is the library skill's version the review used; 0 for a
	// repository skill and for a run that didn't record it.
	Version int `json:"version"`
	// Findings are those still standing; Dismissed are those set aside by
	// a dismissal, each with who dismissed it. Verdict counts the blocking
	// findings left in Findings.
	Findings  []FindingView `json:"findings"`
	Dismissed []FindingView `json:"dismissed"`
	Checks    []CheckView   `json:"checks"`
}

// FindingView is one finding.
type FindingView struct {
	ID string `json:"id"`
	// Fingerprint identifies the finding across runs: what a dismissal and a
	// restore name.
	Fingerprint string `json:"fingerprint"`
	// Severity is "blocking" or "suggestion".
	Severity string `json:"severity"`
	Title    string `json:"title"`
	File     string `json:"file"`
	// Line is left out when the finding has none.
	Line         int64  `json:"line,omitempty"`
	Explanation  string `json:"explanation"`
	SuggestedFix string `json:"suggestedFix"`
	// Dismissed is who set the finding aside, and why; null for a finding
	// that stands.
	Dismissed *DismissedView `json:"dismissed"`
}

// DismissedView is a dismissal of a finding.
type DismissedView struct {
	// By is who dismissed it, as the thread names them: the part of an SSO
	// account's email before the "@", "root (password)" for a local admin, and
	// for a dismissal by the "@bot dismiss" command the form it always had.
	By     string    `json:"by"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// CheckView is one check of a skill's checklist.
type CheckView struct {
	Batch   int    `json:"batch"`
	Section string `json:"section"`
	Name    string `json:"name"`
	// Status is "pass", "fail" or "n/a".
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// LogView is GET /api/runs/{id}/log: the redacted tail of the review Job's
// log (404 when none was kept).
type LogView struct {
	Text string `json:"text"`
}

// DiagnosticsView is GET /api/runs/{id}/diagnostics: what was kept of a failed
// Job. An admin gets the clone log as it was and the pod's state; anyone else
// gets the clone log with linked repositories masked (it may be empty) and
// no podState key.
type DiagnosticsView struct {
	CloneLog string  `json:"cloneLog"`
	PodState *string `json:"podState,omitempty"`
}

// RepositoryView is one repository switched on or paused. GET /api/repositories is
// a list of them.
type RepositoryView struct {
	Instance string `json:"instance"`
	Project  string `json:"project"`
	Platform string `json:"platform"`
	// Source is "config" (read-only here) or "dashboard".
	Source string `json:"source"`
	// On is false while it is paused (Paused) and once it is removed.
	On     bool      `json:"on"`
	Paused bool      `json:"paused"`
	Last7  Last7View `json:"last7"`
	// Health is "paused" while it is paused, else "attention" when it has
	// setup problems, else "ok".
	Health string `json:"health"`
}

// Last7View counts the repository's runs of the last 7 days, as StatsView.
type Last7View struct {
	Reviews  int `json:"reviews"`
	Approved int `json:"approved"`
	Failed   int `json:"failed"`
}

// RepositoryDetailView is GET /api/repositories/{instance}/{project...}.
// Settings is keyed approve, reviewDrafts, inlineFindings,
// commandsAuthorAndMaintainersOnly (whether only a change's author and the
// repository's maintainers and owners use pergent commands),
// incrementalReviews (whether a push reviews only the files changed since the
// last review), learnFromDismissals (whether later reviews are told what people
// with write access dismissed, and why), quietReviews (whether a review says
// less: fewer nitpicks, no suggestion threads, a shorter comment), commentLayout (the
// comment's blocks as a list of "block:state" texts in order, such as
// "findings:collapsed"; the default in effect, the admin default else the built-in layout, until the repository sets one),
// linkedReposEnabled (whether the impact check across linked repositories
// is on; off by default), linkedRepos (the repository's links, at most 3, each
// {target, watched, canBlock}: kept while the setting is off; shown to admins
// only, an empty list to anyone else), failureStatus, blockingStatus, skills (null: the default list of skills; otherwise the library skills
// reviewed), repoSkills
// (the detected repository skills turned on, a list, empty when none),
// jobTimeoutSeconds, maxTurns, jobMemory ("" when none), rerunLimitPerMR,
// rerunLimitPerRepo, trustedAuthorsOnly, trustedAuthors and monthlyTokenQuota
// (the repository's token quota per UTC month, null for none; shown to admins
// only, left out for anyone else). The model is the model chain's (GET
// .../chain).
type RepositoryDetailView struct {
	Repo      RepositoryView         `json:"repo"`
	Settings  map[string]SettingView `json:"settings"`
	Attention []AttentionView        `json:"attention"`
	// Role is the session's role on the repository: "admin", "maintainer"
	// or "viewer".
	Role string `json:"role"`
	// Waiting are the changes whose review waits for someone with write
	// access to start it (Start review: admins and maintainers), newest
	// first.
	Waiting []WaitingView `json:"waiting"`
	// InstanceTrustedAuthorsOnly is the instance's trusted-authors mode,
	// what the repository follows when it sets none.
	InstanceTrustedAuthorsOnly string `json:"instanceTrustedAuthorsOnly"`
	// Model is the provider and model of position 1 of the chain in effect
	// (GET .../chain has the chain), as "provider / model"; "" when the
	// chain is empty or that entry was removed. Anyone who sees the
	// repository sees it; the base URL and the key are never in it.
	Model string `json:"model"`
	// MultiReview is the repository's multi review: whether it has
	// reviewers 2 to 5 (GET .../reviewers), how many reviewers in all and
	// its judge.
	MultiReview MultiReviewView `json:"multiReview"`
	// Quota is this month's tokens against the repository's and its
	// instance's quotas, for an admin; null for anyone else, and when the
	// usage can't be read.
	Quota *QuotaView `json:"quota"`
	// Graph is the repository's Code graph: the switch and the stored
	// graph's status, never its contents.
	Graph GraphView `json:"graph"`
	// BuiltInRulesOff is why a review would leave out the built-in rules
	// the repository chooses (jobs.image can't load only those); "" when it
	// loads them.
	BuiltInRulesOff string `json:"builtInRulesOff"`
}

// MultiReviewView is a repository's multi review on its page. On is
// whether it has a reviewer beyond reviewer 1. Reviewers counts them all,
// reviewer 1 included: 1 when off. Judge is the name of the judge chain's
// first entry; "" with no judge, an empty judge chain or a removed entry.
// Never a model, a base URL or a key.
type MultiReviewView struct {
	On        bool   `json:"on"`
	Reviewers int    `json:"reviewers"`
	Judge     string `json:"judge"`
}

// GraphView is a repository's Code graph switch and status. Available is
// whether this install keeps graphs (the switch can only be turned on then).
// The rest is the stored graph (Commit "" is none yet), the build under way
// ("", "queued" or "running") and the last build's error.
type GraphView struct {
	Enabled   bool       `json:"enabled"`
	Available bool       `json:"available"`
	Branch    string     `json:"branch"`
	Commit    string     `json:"commit"`
	BuiltAt   *time.Time `json:"builtAt"`
	SizeBytes int64      `json:"sizeBytes"`
	Nodes     int        `json:"nodes"`
	Edges     int        `json:"edges"`
	Building  string     `json:"building"`
	Error     string     `json:"error"`
	FailedAt  *time.Time `json:"failedAt"`
}

// QuotaView is what a repository and its instance used this month (UTC), each
// against its quota.
type QuotaView struct {
	Repository QuotaUseView `json:"repository"`
	Instance   QuotaUseView `json:"instance"`
}

// QuotaUseView is the tokens (in plus out) used this month and the quota they
// count against; Limit is null when there is none.
type QuotaUseView struct {
	Used  int64  `json:"used"`
	Limit *int64 `json:"limit"`
}

// WaitingView is a change of an author the repository doesn't trust, not
// reviewed until someone starts it, or a change the monthly token quota holds.
// Reason is "untrusted_author", "trust_check_failed" (the author's access
// couldn't be read), "quota_used_up" (only an admin starts it) or
// "limit_used_up" (held like "quota_used_up"; a limit took part).
type WaitingView struct {
	Number int64 `json:"number"`
	// Head is the commit that waits; Start review sends it back, and a
	// change pushed since doesn't start.
	Head   string    `json:"head"`
	Title  string    `json:"title"`
	Author string    `json:"author"`
	WebURL string    `json:"webUrl"`
	Since  time.Time `json:"since"`
	Reason string    `json:"reason"`
}

// SettingView is a setting's effective value and whether the repository
// sets it (otherwise it is the default).
type SettingView struct {
	Value      any  `json:"value"`
	Overridden bool `json:"overridden"`
}

// InputsView is GET /api/repositories/{instance}/{project...}/inputs: the
// skills and instructions a review would get, read from the default branch
// with the review bot or App and kept 5 minutes (a change of a skill setting
// or of the default list clears it). 503 while pergent.sh hasn't set the
// repository up yet, 502 when they can't be read.
type InputsView struct {
	Branch string `json:"branch"`
	// Skills are the skills a review would run, by name, and each skill a
	// review would fail on, with its Problem: a library skill the repository
	// names that is gone, or a repository skill turned on that is in no
	// repository folder.
	Skills []InputSkill `json:"skills"`
	// Detected are the skills of the repository folders that run only when
	// turned on (".agents/skills", ".claude/skills", ".github/skills"), in
	// the order of precedence, each with whether it is on.
	Detected     []DetectedSkillView `json:"detected"`
	Instructions string              `json:"instructions"`
	// Rules is the path rule list a review of the default branch would get.
	Rules RulesView `json:"rules"`
}

// RulesView is a review's rule list: the repository's .pergent/rules.yaml,
// then its rule sets, then the built-in rules when BuiltIn. Problem is why a
// review would fail on its rules (Entries is then empty).
type RulesView struct {
	// BuiltIn names the built-in rules the repository chose, in
	// builtin.yaml order; empty for none.
	BuiltIn []string        `json:"builtIn"`
	Entries []RuleEntryView `json:"entries"`
	Problem string          `json:"problem"`
}

// RuleEntryView is one entry of a rule list. Source is "repo" or
// "dashboard:<set>"; Rule is empty for an off entry.
type RuleEntryView struct {
	Source string `json:"source"`
	Path   string `json:"path"`
	Off    bool   `json:"off"`
	Rule   string `json:"rule"`
}

// InputSkill is one skill of the final set.
type InputSkill struct {
	Name string `json:"name"`
	// Source is "library" or the repository folder the skill was read from
	// (also when it replaces a library skill); "" with a Problem.
	Source string `json:"source"`
	// Folder is the repository folder, "" for a library skill.
	Folder string `json:"folder"`
	// Version is the library skill's version, 0 for a repository skill.
	Version int    `json:"version"`
	Content string `json:"content"`
	// URL is where a repository skill is on the platform (its file at the
	// default branch); "" for a library skill, and when the platform's web
	// address isn't known.
	URL string `json:"url"`
	// Problem says why a review would fail on this skill; Content is empty.
	Problem string `json:"problem,omitempty"`
}

// DetectedSkillView is a skill found in a repository folder that runs only
// when turned on. Name is the file or folder name when Problem is set.
type DetectedSkillView struct {
	Name    string `json:"name"`
	Folder  string `json:"folder"`
	Problem string `json:"problem,omitempty"`
	// On is whether the repository turned it on (repoSkills).
	On bool `json:"on"`
}

// ReadinessView is GET /api/repositories/{instance}/{project...}/readiness:
// the Readiness tab, for anyone who can see the repository. Checklist is
// built now from the latest finished scan's stored results and the skills
// turned on now; it is null until a scan has finished. An item's state is
// "passed", "warning", "missing" or "unknown" (couldn't check: never shown
// or counted as passed). Paths, names and reasons in it come from the scan
// Job: text only, never markup.
//
// POST .../readiness/scan (Scan now, maintainers and admins, config
// repositories too) answers 202 {"scan"}; 409 {"error", "scan"} while one is
// queued or running; 429 {"error", "waitSeconds"} with Retry-After within 5
// minutes of the latest; 403 for a viewer.
type ReadinessView struct {
	// Latest is the latest finished scan (done or failed), Active the queued
	// or running one; each null when there is none.
	Latest    *ScanView            `json:"latest"`
	Active    *ScanView            `json:"active"`
	Checklist *readiness.Checklist `json:"checklist"`
	// CanScan: the session is a maintainer or admin of the repository, and
	// it is not paused.
	CanScan bool `json:"canScan"`
	// ConfigRepo: the config file sets the repository up, so its skills
	// are changed there ("Use these skills" shows the lines to paste).
	ConfigRepo bool `json:"configRepo"`
	// FollowsDefaults: the repository's skills are null, so it follows the
	// default list; saving a list stops that.
	FollowsDefaults bool `json:"followsDefaults"`
	// ApplyBase is today's list of library skills: the repository's, or the
	// default list. "Use these skills" saves it plus the ticked ones.
	ApplyBase []string `json:"applyBase"`
}

// ScanView is one readiness scan. State is "queued", "running", "done" or
// "failed". StartedBy is who asked (an SSO account's email), "switch-on" or
// "upgrade". Branch and Commit are "" until the scan read them; Error is why
// a failed scan couldn't scan the files. FinishedAt is null until it ends.
type ScanView struct {
	ID         string     `json:"id"`
	State      string     `json:"state"`
	StartedBy  string     `json:"startedBy"`
	Branch     string     `json:"branch"`
	Commit     string     `json:"commit"`
	Error      string     `json:"error"`
	QueuedAt   time.Time  `json:"queuedAt"`
	FinishedAt *time.Time `json:"finishedAt"`
}

// PastDismissalView is one row of GET /api/repositories/{instance}/{project...}
// /past-dismissals, for maintainers and admins (403 otherwise): a finding
// whose dismissals teach the repository's reviews, one row each, a blocking
// dismissal over a suggestion and then the newest. By, At and Number are of
// the dismissal shown; Number is its change. The list is [] when nothing
// teaches.
//
// POST .../past-dismissals/stop {"fingerprint"} (Stop using) answers 200
// {"status": "stopped" | "not teaching"}: every dismissal of that finding in
// the repository stops teaching, config repositories included. The
// dismissals stay on their changes, no review is decided again and the
// platform isn't asked. 400 for a body without a fingerprint.
type PastDismissalView struct {
	Skill       string    `json:"skill"`
	Severity    string    `json:"severity"`
	File        string    `json:"file"`
	Title       string    `json:"title"`
	Reason      string    `json:"reason"`
	By          string    `json:"by"`
	At          time.Time `json:"at"`
	Number      int64     `json:"number"`
	Fingerprint string    `json:"fingerprint"`
}

// SkillResultView is one row of GET /api/repositories/{instance}/{project...}
// /skill-results?days=30|90 (any other days is 400; none is 30), for anyone who
// can see the repository: what became of the findings of one skill at one
// severity that were decided in the window. Raised, Fixed, Open and WentAway
// count the findings of merged changes opened by people; Dismissed also counts
// those of changes they closed, so Raised can be less than the columns' sum.
// Addressed is Fixed*100/Raised, a whole percent, and null under 10 raised.
// EditedAt is set when the window holds more than one version of the skill:
// when the newest first appears. Rows come lowest Addressed first, nulls last,
// then by skill, a blocking row before a suggestion. The list is [] when
// nothing was decided.
type SkillResultView struct {
	Skill     string     `json:"skill"`
	Severity  string     `json:"severity"`
	Raised    int        `json:"raised"`
	Fixed     int        `json:"fixed"`
	Dismissed int        `json:"dismissed"`
	Open      int        `json:"open"`
	WentAway  int        `json:"wentAway"`
	Addressed *int       `json:"addressed"`
	EditedAt  *time.Time `json:"editedAt"`
}

// SkillTokensView is one row of GET /api/repositories/{instance}/{project...}
// /skill-results/tokens?days=30|90 (any other days is 400; none is 30), for
// admins only (404 for anyone else): the tokens one skill used on the changes
// the Skill results tab counts for the same window, all the reviews of each
// change, not only those inside the window. Rows come by skill, and the list
// is [] when no change was counted or none has recorded usage.
type SkillTokensView struct {
	Skill  string     `json:"skill"`
	Tokens TokensView `json:"tokens"`
}

// SkillSourceView is where an installed skill came from. Kind is "instance"
// (read through the API of a connected instance) or "url" (a public git
// URL). Repo is shown to admins only; others get Hidden, and the page says
// it is "a private repository on <instance>". URL is public. Path is the skill's
// file in the repository, sent with Repo and URL, never with Hidden (a path
// in a private repository is private too), for a check for updates to name
// the folder.
type SkillSourceView struct {
	Kind     string `json:"kind"`
	Instance string `json:"instance,omitempty"`
	Repo     string `json:"repo,omitempty"`
	URL      string `json:"url,omitempty"`
	Path     string `json:"path,omitempty"`
	Ref      string `json:"ref"`
	Commit   string `json:"commit"`
	Hidden   bool   `json:"hidden,omitempty"`
}

// LibrarySkillView is one skill of the library, without its text. GET
// /api/skills is a list of them, by name. UpdatedBy is shown to admins only.
type LibrarySkillView struct {
	Name       string           `json:"name"`
	UpdatedBy  string           `json:"updatedBy,omitempty"`
	Version    int              `json:"version"`
	Size       int              `json:"size"`
	UpdatedAt  time.Time        `json:"updatedAt"`
	InDefaults bool             `json:"inDefaults"`
	Source     *SkillSourceView `json:"source"`
}

// SkillVersionView is one saved version of a skill; Actor is shown to admins
// only.
type SkillVersionView struct {
	Version int       `json:"version"`
	At      time.Time `json:"at"`
	Actor   string    `json:"actor,omitempty"`
	Commit  string    `json:"commit"`
}

// SkillDetailView is GET /api/skills/{name}: the skill with its text (shown
// as plain text only) and its versions, newest first. 404 for a name not in
// the library.
type SkillDetailView struct {
	LibrarySkillView
	Content  string             `json:"content"`
	Versions []SkillVersionView `json:"versions"`
}

// SkillVersionDetailView is GET /api/skills/{name}/versions/{v}: one version
// with its text. The history outlives a deleted skill.
type SkillVersionDetailView struct {
	SkillVersionView
	Content string `json:"content"`
}

// SkillWrittenView is the answer to a create, an edit or a copy of a library
// skill: the version it was saved as.
type SkillWrittenView struct {
	Version int `json:"version"`
}

// SkillInUseView is the 409 of a delete of a skill that is in use (D8):
// Repositories are the ones that name it, InDefaults says the default list
// names it.
type SkillInUseView struct {
	Error        string              `json:"error"`
	Repositories []SkillUserRepoView `json:"repositories"`
	InDefaults   bool                `json:"inDefaults"`
}

// SkillUserRepoView names a repository that uses a skill.
type SkillUserRepoView struct {
	Instance string `json:"instance"`
	Project  string `json:"project"`
}

// SkillFetchStartedView is the 202 of POST /api/admin/skill-fetches: the
// read to ask GET /api/admin/skill-fetches/{id} about.
type SkillFetchStartedView struct {
	ID string `json:"id"`
}

// SkillFetchView is GET /api/admin/skill-fetches/{id}, for the admin who
// started the read only (404 for anyone else). Status is "running", "done"
// or "failed" (Error says why, for the admin); Commit is the commit read.
// Purpose is "install" or "update"; the source is Instance and Repo, or URL,
// at Ref, in the folder Path.
//
// Skills are the skills found, once done. Updates are set for a check for
// updates that is done (D16): each skill installed from the source at
// FromRef whose file is in Path, matched with what was read by its file
// path. Its State is "changed", "same", "removed at the source" (the file is
// gone) or "renamed at the source" (the file names another skill: install it
// again); Problem says why it can't be updated. Current and Fetched are the
// two texts, for a diff shown as plain text, never HTML.
type SkillFetchView struct {
	ID       string             `json:"id"`
	Status   string             `json:"status"`
	Commit   string             `json:"commit"`
	Error    string             `json:"error"`
	Purpose  string             `json:"purpose"`
	Instance string             `json:"instance,omitempty"`
	Repo     string             `json:"repo,omitempty"`
	URL      string             `json:"url,omitempty"`
	Ref      string             `json:"ref"`
	FromRef  string             `json:"fromRef,omitempty"`
	Path     string             `json:"path"`
	Skills   []FetchedSkillView `json:"skills"`
	Updates  []SkillUpdateView  `json:"updates"`
}

// FetchedSkillView is one skill a read found: Size is the bytes of its text
// (0 when the text was not kept: too large, or holding a secret), Problem
// why it can't be installed, Taken that a skill of that name is in the
// library already ("name already in the library").
type FetchedSkillView struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Size    int    `json:"size"`
	Problem string `json:"problem,omitempty"`
	Taken   bool   `json:"taken"`
}

// SkillUpdateView is one installed skill in a check for updates; Version is
// its library version now.
type SkillUpdateView struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	State   string `json:"state"`
	Problem string `json:"problem,omitempty"`
	Version int    `json:"version"`
	Current string `json:"current"`
	Fetched string `json:"fetched"`
}

// SkillFetchResultView is the answer for one name of POST
// /api/admin/skill-fetches/{id}/install or .../update: OK, or the Error why
// not. The answer is a list of them, in the order asked.
type SkillFetchResultView struct {
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// AttentionView is one setup problem. Project is "" for a problem of the
// whole instance (an expiring token). Client is the API client a problem is
// about (failing callbacks), with Instance and Project "".
type AttentionView struct {
	Instance  string    `json:"instance"`
	Project   string    `json:"project"`
	Client    string    `json:"client,omitempty"`
	Kind      string    `json:"kind"`
	Message   string    `json:"message"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// AdminSettingsView is GET /api/admin/settings (admins only): the defaults
// a repository switched on in the dashboard gets, and the model. Never a key.
type AdminSettingsView struct {
	Defaults DefaultsView `json:"defaults"`
	LLM      LLMView      `json:"llm"`
}

// CommentFooterView is the text at the bottom of every review comment.
type CommentFooterView struct {
	// Note is what an admin set; "" means Default is used.
	Note    string `json:"note"`
	Default string `json:"default"`
}

// DefaultsView is the settings a repository gets when it sets none.
type DefaultsView struct {
	Approve        bool `json:"approve"`
	ReviewDrafts   bool `json:"reviewDrafts"`
	InlineFindings bool `json:"inlineFindings"`
	// CommandsAuthorAndMaintainersOnly is whether only a change's author and
	// the repository's maintainers and owners use pergent commands.
	CommandsAuthorAndMaintainersOnly bool `json:"commandsAuthorAndMaintainersOnly"`
	// IncrementalReviews is whether a push reviews only the files changed
	// since the last review.
	IncrementalReviews bool `json:"incrementalReviews"`
	// LearnFromDismissals is whether later reviews are told what people with
	// write access dismissed, and why.
	LearnFromDismissals bool `json:"learnFromDismissals"`
	// QuietReviews is whether a review says less: fewer nitpicks, no
	// suggestion threads and a shorter comment.
	QuietReviews bool `json:"quietReviews"`
	// LinkedReposEnabled is whether a repository that sets none has the impact
	// check across its linked repositories on. Links are per repository, so
	// there is no default list.
	LinkedReposEnabled bool `json:"linkedReposEnabled"`
	// CommentLayout is the shape of the review comment, as "block:state"
	// entries in order. A repository that sets none gets this one: the layout
	// saved here, else the built-in layout. It is never empty.
	CommentLayout []string `json:"commentLayout"`
	FailureStatus string   `json:"failureStatus"`
	// BlockingStatus is the status of a review with blocking issues,
	// "failed" or "neutral" (never a failed status).
	BlockingStatus    string `json:"blockingStatus"`
	JobTimeoutSeconds int64  `json:"jobTimeoutSeconds"`
	// MaxTurns is the step limit of each opencode run.
	MaxTurns int `json:"maxTurns"`
	// JobMemory is the Job's memory limit, "" when none.
	JobMemory string `json:"jobMemory"`
	// RerunLimitPerMR and RerunLimitPerRepo cap the re-runs per merge
	// request and per repository and UTC day.
	RerunLimitPerMR   int `json:"rerunLimitPerMR"`
	RerunLimitPerRepo int `json:"rerunLimitPerRepo"`
	// Skills is the default list: the library skills a repository reviews
	// when its own skills setting is null. Empty means none.
	Skills []string `json:"skills"`
	// RuleSets is the default rule-set list, in match order, for a
	// repository whose own ruleSets is null. Empty means none.
	RuleSets []string `json:"ruleSets"`
	// Config holds the values the config gives (jobs.* and the built-in
	// defaults), before anything saved here. Skills has no config value.
	Config DefaultValues `json:"config"`
	// Pinned says which defaults were saved here and so no longer follow the
	// config, by JSON key (approve, reviewDrafts, inlineFindings,
	// commandsAuthorAndMaintainersOnly, incrementalReviews,
	// learnFromDismissals, quietReviews, commentLayout, linkedReposEnabled, failureStatus, blockingStatus, jobTimeoutSeconds, maxTurns, jobMemory,
	// rerunLimitPerMR, rerunLimitPerRepo). Every key is present.
	Pinned map[string]bool `json:"pinned"`
}

// DefaultValues is DefaultsView's settings without the skill list, with the
// same JSON keys.
type DefaultValues struct {
	Approve        bool `json:"approve"`
	ReviewDrafts   bool `json:"reviewDrafts"`
	InlineFindings bool `json:"inlineFindings"`
	// CommandsAuthorAndMaintainersOnly: see DefaultsView.
	CommandsAuthorAndMaintainersOnly bool `json:"commandsAuthorAndMaintainersOnly"`
	// IncrementalReviews: see DefaultsView.
	IncrementalReviews bool `json:"incrementalReviews"`
	// LearnFromDismissals: see DefaultsView.
	LearnFromDismissals bool `json:"learnFromDismissals"`
	// QuietReviews: see DefaultsView.
	QuietReviews bool `json:"quietReviews"`
	// LinkedReposEnabled: see DefaultsView.
	LinkedReposEnabled bool `json:"linkedReposEnabled"`
	// CommentLayout: see DefaultsView. The config has no layout, so here it
	// is always the built-in one.
	CommentLayout     []string `json:"commentLayout"`
	FailureStatus     string   `json:"failureStatus"`
	BlockingStatus    string   `json:"blockingStatus"`
	JobTimeoutSeconds int64    `json:"jobTimeoutSeconds"`
	MaxTurns          int      `json:"maxTurns"`
	// JobMemory is the Job's memory limit, "" when none.
	JobMemory         string `json:"jobMemory"`
	RerunLimitPerMR   int    `json:"rerunLimitPerMR"`
	RerunLimitPerRepo int    `json:"rerunLimitPerRepo"`
}

// LLMView is the model reviews use.
type LLMView struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// --- The write API ---
//
// Every write needs the X-CSRF-Token header (403 with "code": "csrf"
// without it, so the client knows to fetch the token again) and the right
// role (403 otherwise, and nothing changes): an admin to switch
// repositories on, pause, resume and remove them and for the picker, a maintainer or admin of the
// repository for its settings. A repository the user can't see is answered
// exactly like one that doesn't exist (404), whatever the write. A
// repository listed in the config is read-only here (409). Each change is
// recorded in the audit log: repository_on, repository_paused,
// repository_resumed, repository_removed, settings_updated (detail: the settings in the body), settings_reset,
// graph_on and graph_off.
// Errors are {"error": "message"}.
//
//   - GET /api/repositories/available?instance=&search=&page=: AvailableView,
//     a page of the repositories of the instance that can be switched on
//     (400 without an instance or with a page that is not a whole number
//     from 1 to 10000, 404 for an instance that is not there); search (at
//     most 100 characters) keeps those whose path holds it (GitLab also
//     matches names); page is 1 when left out.
//   - POST /api/repositories {"instance": "lab", "project": "acme/widgets"}:
//     switches on a repository with every setting at its default. It is
//     checked on its own, not against the picker's lists: the review bot
//     (GitLab: Maintainer or above) or App (GitHub: installed) must reach
//     it, else 404, whether it is on or not; then 409 when it is on
//     already. 201, RepositoryDetailView, the project spelled as GitLab spells it for
//     GitLab, and as sent (trimmed) for GitHub.
//     pergent.sh sets it up (webhook and clone credentials) within
//     seconds. A paused repository is resumed instead, with its settings:
//     200.
//   - PUT /api/admin/repositories/{instance}/{project...} {"enabled": bool}:
//     pauses (false) or resumes (true) it, its settings kept. 200,
//     RepositoryDetailView; the state it is in already changes nothing.
//     409 for a resume while its instance is being removed.
//   - DELETE /api/repositories/{instance}/{project...}: removes it for good,
//     with its settings and team links, paused or not. 200,
//     RepositoryDetailView with repo.on and repo.paused false and the
//     default settings. pergent.sh removes its webhook and clone credentials.
//   - PUT /api/repositories/{instance}/{project...}/settings: changes the
//     settings in the body and leaves the others; a null resets one to its
//     default. The keys are those of RepositoryDetailView.Settings:
//     approve, reviewDrafts, inlineFindings, commandsAuthorAndMaintainersOnly, incrementalReviews, learnFromDismissals and quietReviews (true or false),
//     commentLayout (a non-empty list of "block:state" texts, or null to follow
//     the default, the admin default else the built-in layout: the blocks are summary, findings, dismissed, checks and
//     files, the states open and collapsed, a block left out is hidden,
//     findings is required and each block is named once; anything else is
//     400), linkedReposEnabled (true or false; only an admin changes it, else
//     403), linkedRepos (null or a list of at most 3 links, each {"target":
//     "owner/name", "watched": [globs, at most 50], "canBlock": true}; only an
//     admin changes it, else 403; GitHub repositories only; 400 with the reason
//     when a rule fails: itself, twice, a bad glob, a target more private than
//     the repository, a target the review App can't read or in another
//     installation, and 502 when GitHub can't tell, with nothing saved), failureStatus and
//     blockingStatus ("failed" or "neutral"; only an admin changes them, else
//     403, and a maintainer's reset leaves them), skills (a non-empty list of library skill names, or
//     null for the default list; a name the library lacks is 400 `skill
//     "x" is not in the skill library`, and so is a list whose texts come to
//     more than 900 KiB), repoSkills (the names of detected repository
//     skills to turn on; a list, empty or null for none),
//     jobTimeoutSeconds (a whole number, 60 to 7200), maxTurns (a whole
//     number, 5 to 200) and jobMemory (a Kubernetes quantity such as
//     "6Gi", from 256Mi, and no less than jobs.resources.requests.memory,
//     to 32Gi), and rerunLimitPerMR and
//     rerunLimitPerRepo (a whole number, 1 to 1000; only an admin changes
//     these, else 403, and a maintainer's reset leaves them), and monthlyTokenQuota (a
//     whole number from 1 to 1000000000000000, or null for none; only an admin
//     changes it, else 403, and a maintainer's reset leaves it). Any other key, or a value
//     out of bounds, is 400 and nothing changes; so is model, which the
//     model chain replaced. 200, RepositoryDetailView.
//   - POST /api/repositories/{instance}/{project...}/settings/reset: every
//     setting back to its default, except that a maintainer's reset leaves
//     the two re-run caps, the linked repositories setting, the links, who is
//     trusted and the token quota as they are (only an admin resets those).
//     The model chain stays. 200, RepositoryDetailView.
//   - GET .../chain: ChainView, the model chain in effect, for anyone who
//     sees the repository. PUT .../chain {"entries"} and POST
//     .../chain/reset: admins and maintainers of the owner team (403 for
//     anyone else who sees it); see api_chains.go.
//   - GET .../reviewers: ReviewersView, reviewers 2 to 5 and the judge, for
//     anyone who sees the repository. PUT .../reviewers {"reviewers",
//     "judge"}: the rights of PUT .../chain; adding a reviewer needs a review
//     image with multi review (400 otherwise); see api_reviewers.go.
//   - GET /api/admin/linkable?instance=&repo=&search= (admins only): LinkableView,
//     the repositories the instance's review App reaches (at most 200, by name),
//     each with whether `repo` (a repository switched on, GitHub only; 400 for
//     GitLab) may link it, and why not. See LinkableRepo.
//
// Users whose teams cover a repository switched on see it at their next
// request.

// AvailableView is GET /api/repositories/available: one page of the
// repositories of the instance that the review bot (GitLab: Maintainer or
// above) or review App (GitHub: its installations) reaches and that aren't
// switched on, by project ignoring case. Items holds 50 at most and Page is
// the page asked for (1-based). On GitLab a page is one request to the
// platform in its order, so one with switched-on repositories left out holds
// fewer than 50; on GitHub, which can't search on its side, the list is read
// whole, sorted, cut into pages of 50, and the switched-on ones are left out
// of each page after (so a page can hold fewer there too, and switching one
// on moves no later page).
// HasMore is true when a later page may hold more. Truncated is true when
// the list a page is made from was too long (5000) or too slow (20 s, each
// request 10 s) to read whole; GitLab pages are never truncated. 502 when
// GitLab or GitHub can't be asked, or nothing could be read in time.
type AvailableView struct {
	Items     []AvailableRepo `json:"items"`
	Page      int             `json:"page"`
	HasMore   bool            `json:"hasMore"`
	Truncated bool            `json:"truncated"`
}

// AvailableRepo is one repository that can be switched on.
type AvailableRepo struct {
	Instance string `json:"instance"`
	Project  string `json:"project"`
	// Platform is "gitlab" or "github".
	Platform string `json:"platform"`
}

// --- Users, teams and the audit log (admins only, 403 otherwise) ---
//
// Every write needs the X-CSRF-Token header, as above, and is recorded in
// the audit log: account_created and account_deleted (detail: the
// username), account_role_changed (detail: "email: user->admin"),
// team_created (detail: the name), team_updated (detail: "name: " and the
// parts changed, of name, "members +added:role -removed name:old->new" by
// username, and repos) and team_deleted (detail: the name). Sign-ins are
// recorded too (sign_in; for a local admin also sign_in_failed, locked,
// password_changed). Errors are {"error": "message"}.
//
// An entry's actor tells who acted: the full email of an SSO account, "root
// (password)" for a local admin, "cli" for a password reset from the command
// line; entries made before this change read "alice (gitlab main)" for a
// platform account and stay as they are.
//
//   - GET /api/users: []UserView, everyone by username (the old platform
//     accounts are left out).
//   - DELETE /api/users/{id}: removes the account with its sessions and team
//     memberships (its next request is 401; an SSO account is made again, on
//     no team, at its next sign-in if the allowlist lets it in). Your own
//     account is 409; the last local admin can't be removed (409). 204.
//   - POST /api/admin/users {"email", "teams": [{"teamId", "role"}]}: gives a
//     person access before they sign in: the SSO account is made as invited
//     (Added, so the allowlist lets the email in) and put on each team with
//     its role ("viewer" when none). A bad email, a team that isn't there or
//     a role that isn't viewer or maintainer is 400 and nothing is made; an
//     email added already is 409. 201, UserView; 200 for an account that
//     signed in already without being added.
//   - GET /api/repositories/{instance}/{project...}/teams and PUT the same
//     with {"teams": [team ID, ...]} (admins; 404 for anyone else):
//     RepoTeamsView, every team with whether it lists the repository itself
//     or covers its whole instance. The PUT sets which teams list the
//     repository itself, as the Teams page does through PUT /api/teams/{id};
//     a team that isn't there is 400. 200, RepoTeamsView.
//   - PUT /api/admin/users/{id}/role {"role": "admin" or "user"}: an SSO
//     account's role. Your own role is 409, and so is a local admin's (always
//     an admin). 204.
//   - POST /api/admin/accounts {"username"}: a new local admin with a
//     generated password, which must be changed at its first sign-in. The
//     username is 1 to 64 letters, digits, ".", "_" or "-" (400), not taken
//     ignoring case (409). 201, NewAccountView: the only time the password is
//     shown.
//   - GET /api/teams: []TeamView, by name ignoring case.
//   - POST /api/teams {"name"}: a team with no members and no repositories;
//     a name taken ignoring case is 409. 201, TeamView.
//   - PUT /api/teams/{id} {"name", "members": [{"accountId", "role"}],
//     "repos": [{"instance", "project"}]}: replaces them all. Members are SSO
//     accounts (400 otherwise), each with the role "viewer" or "maintainer"
//     (400 otherwise); an account named twice has the higher role. A
//     repository is one switched on, or {"instance": X, "project": ""} for
//     every repository of instance X, which also covers repositories
//     switched on later (400 otherwise). 200, TeamView; 404 for no such
//     team.
//   - DELETE /api/teams/{id}: removes the team (not its members' accounts).
//     204.
//   - GET /api/audit?before=<id>: AuditPageView, 100 entries at most,
//     newest first, older than the entry before when given. Its next is
//     the before of the page after, 0 at the end of the log (the page after
//     can still be empty when the log ends just there).

// UserView is one account. Role is "admin" or "user". Invited is set for an SSO
// account an admin added that has not signed in yet.
type UserView struct {
	ID int64 `json:"id"`
	// Kind is "password" (a local admin) or "sso".
	Kind     string `json:"kind"`
	Username string `json:"username"`
	// Email is an SSO account's email; "" for a local admin.
	Email   string `json:"email"`
	Role    string `json:"role"`
	Invited bool   `json:"invited"`
	// LastSignInAt is null for an account that never signed in.
	LastSignInAt *time.Time `json:"lastSignInAt"`
	// Waiting: an SSO account that signed in, is not an admin and is on no
	// team, which sees nothing.
	Waiting bool      `json:"waiting"`
	Teams   []TeamRef `json:"teams"`
}

// TeamRef names a team.
type TeamRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// NewAccountView is a local admin just made, with its generated password.
type NewAccountView struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// TeamView is one team.
type TeamView struct {
	ID      int64            `json:"id"`
	Name    string           `json:"name"`
	Members []TeamMemberView `json:"members"`
	// Repos are the repositories it covers, by instance and project; a
	// project "" covers every repository of the instance.
	Repos []TeamRepoView `json:"repos"`
}

// TeamMemberView is a team's member: an SSO account and its role on the team,
// "viewer" or "maintainer".
type TeamMemberView struct {
	AccountID int64  `json:"accountId"`
	Username  string `json:"username"`
	Email     string `json:"email"`
	Role      string `json:"role"`
}

// TeamRepoView is a repository a team covers. Owner is the owner tick: the
// team whose chain the repository (or, for an instance entry, each
// repository of the instance with no owner of its own) uses. In a team
// change, a missing owner is false: the change replaces the ticks too.
type TeamRepoView struct {
	Instance string `json:"instance"`
	Project  string `json:"project"`
	Owner    bool   `json:"owner"`
}

// MyTeamView is a team of GET /api/me/teams: Role is the session's on it,
// "viewer" or "maintainer" ("admin" for an admin, who sees every team).
type MyTeamView struct {
	ID    int64          `json:"id"`
	Name  string         `json:"name"`
	Role  string         `json:"role"`
	Repos []TeamRepoView `json:"repos"`
}

// TeamModelsView is GET /api/teams/{id}/models: the team's own entries.
// Role is the session's on the team; CanEdit is whether it may add, change
// and remove them (a maintainer or an admin).
type TeamModelsView struct {
	Team    TeamRef         `json:"team"`
	Role    string          `json:"role"`
	CanEdit bool            `json:"canEdit"`
	Models  []TeamModelView `json:"models"`
}

// TeamModelView is one of a team's entries. BaseURL is "" for a viewer; the
// key is never sent, only whether one is set. UsedIn and Mark are
// ModelAdminView's.
type TeamModelView struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Provider string     `json:"provider"`
	Model    string     `json:"model"`
	BaseURL  string     `json:"baseURL"`
	Key      SecretView `json:"key"`
	UsedIn   []string   `json:"usedIn"`
	Mark     *MarkView  `json:"mark"`
}

// TeamLimitsView is GET /api/teams/{id}/limits, for the team's maintainers
// and admins: each of the team's own entries with its limit and the team's
// use of it this month, and each global entry with an admin's limit for the
// team. Never another team's numbers.
type TeamLimitsView struct {
	Team   TeamRef               `json:"team"`
	Models []TeamModelLimitView  `json:"models"`
	Global []TeamGlobalLimitView `json:"global"`
}

// TeamModelLimitView is one of the team's entries: its limit (null: none,
// which a maintainer of the team sets) and its tokens this month.
type TeamModelLimitView struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Limit *int64 `json:"limit"`
	Used  int64  `json:"used"`
}

// TeamGlobalLimitView is a global entry with an admin's limit for the team:
// that limit and the team's tokens on the entry this month, read-only for a
// maintainer. ModelLimit is the state of the entry's model limit alone,
// "none", "room_left" or "used_up": its numbers count other teams.
type TeamGlobalLimitView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Limit      int64  `json:"limit"`
	Used       int64  `json:"used"`
	ModelLimit string `json:"modelLimit"`
}

// RepoTeamsView is GET .../teams of a repository: every team, by name
// ignoring case.
type RepoTeamsView struct {
	Teams []RepoTeamView `json:"teams"`
}

// RepoTeamView is a team as one repository's settings show it. Direct is set
// when the team lists the repository itself; WholeInstance when it covers
// every repository of the repository's instance (so it covers this one too).
type RepoTeamView struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Members       int    `json:"members"`
	Direct        bool   `json:"direct"`
	WholeInstance bool   `json:"wholeInstance"`
}

// AuditPageView is a page of the audit log. Next is the before of the
// page after; 0 when the log has nothing older.
type AuditPageView struct {
	Entries []AuditView `json:"entries"`
	Next    int64       `json:"next"`
}

// AuditView is one audit entry. Instance and Project are "" when it isn't
// about a repository. Detail never holds a secret.
type AuditView struct {
	ID       int64     `json:"id"`
	At       time.Time `json:"at"`
	Actor    string    `json:"actor"`
	Action   string    `json:"action"`
	Instance string    `json:"instance"`
	Project  string    `json:"project"`
	Detail   string    `json:"detail"`
}

// --- Instances, the LLM and the defaults (admins) ---
//
// Secrets are write-only: a request carries a value, a response only whether
// it is set and when it last changed. Every write needs the CSRF token and is
// recorded in the audit log without a value: instance_added,
// instance_updated (detail: the fields changed), instance_removed,
// model_added, model_updated (detail: the name and model, then the fields
// changed, "api_key" among them), teams_may_use_changed, model_default_changed,
// model_removed (detail: the name and model), model_mark_cleared,
// chain_changed (detail: the chain and its entry names in order) and
// defaults_updated. The team routes add owner_set, owner_cleared,
// team_model_added, team_model_updated, team_model_removed and
// team_models_deleted.
//
//   - GET /api/admin/instances: [InstanceAdminView], the config's first,
//     then those added in the dashboard.
//   - POST /api/admin/instances {"name", "platform", "url" (GitLab) or
//     "apiURL", "reviewAppID" and "approveAppID" (GitHub; 0 approves with a
//     token, the reviewAppID with the review App itself),
//     "secrets": {field: value}}: checks the credentials the way startup
//     does (400 with the reason when they fail, and nothing is stored),
//     stores them sealed and connects. A GitLab instance's webhook_secret
//     is generated; giving one is 400. 201,
//     InstanceAdminView; 409 when the name is taken in the config or the
//     dashboard.
//   - PUT /api/admin/instances/{name} {"secrets": {...}}:
//     replaces what it names, checks the whole, connects again. An address
//     or App ID that differs is 400; an instance of the config is 409.
//     200, InstanceAdminView.
//   - PUT /api/admin/instances/{name}/token-quota {"monthlyTokenQuota": n|null}:
//     the instance's token quota per UTC month (a whole number from 1 to
//     1000000000000000, or null for none; else 400). 204; 409 for an instance
//     of the config (set it there) or one being removed.
//   - DELETE /api/admin/instances/{name}: 202 {"status": "removing"}. Its
//     runs are superseded and its repositories, accounts and sessions go
//     now; pergent.sh takes the webhooks off the platform and deletes the
//     instance and its secrets within a minute or so. 409 for the config's.
//   - GET /api/admin/instances/{name}/repositories?search=&page=: AvailableView,
//     the repositories the instance's review bot or App can set up that
//     aren't switched on (the same list as the picker's).
//   - GET /api/admin/models: ModelsAdminView (admins), the list of models:
//     the config file's entries first (its llm: block, ID "config", then
//     llm.fallbacks, "config-1" on), read-only, then those added in the
//     dashboard, oldest first. POST /api/admin/models {"name", "provider",
//     "model", "baseURL", "apiKey", "teamsMayUse"?} adds one (name and key
//     are required; 400 for a missing or wrong field, 409 for a name another
//     global entry has): 201, ModelAdminView. The first entry is the default.
//     PUT /api/admin/models/{id} {"baseURL", "apiKey"?, "name"?,
//     "teamsMayUse"?} changes the base URL and what else is sent; a provider
//     or model in the body is 400, as they never change (add another entry),
//     and the config's entries are 409: 200, ModelAdminView. POST
//     /api/admin/models/{id}/default makes it the default (409 while the
//     config has an llm: block, which is the default): 200, ModelsAdminView.
//     DELETE /api/admin/models/{id}: 204, chains that name it keeping it as
//     removed; 409 for the default (not while the config has an llm: block:
//     that entry is the default then) and the config's entries. DELETE
//     /api/admin/models/{id}/mark clears its mark: 204. A key is sealed and
//     never returned.
//   - GET /api/admin/chain: ChainView (admins), the global chain. PUT
//     /api/admin/chain {"entries": [{"source", "id"}]}: global entries only,
//     the config's in front, at most 5; 200, ChainView.
//   - GET /api/admin/sign-in: SignInAdminView (admins). PUT /api/admin/sign-in
//     {"mode", "label", "issuer", "clientID", "clientSecret", "header",
//     "audience", "jwksURL", "emailClaim", "allowedDomains", "allowedEmails"}
//     (admins; 409 when dashboard.sign_in is set in the config file; a
//     missing clientSecret keeps the stored one, "" removes it; a new issuer
//     or mode ends every SSO session, and a new issuer needs the secret
//     again): 200, SignInAdminView. POST /api/admin/sign-in/test reads the
//     provider's discovery document or key set: 200, SignInTestView.
//   - GET /api/admin/defaults, PUT /api/admin/defaults (admins): DefaultsView
//     for the repositories switched on in the dashboard; the PUT body has the
//     same keys with the same bounds as a repository's settings, and
//     replaces them all (a missing key or null is the built-in default),
//     except skills, the default list of library skill names: a missing key
//     keeps the stored list, null or [] clears it, and a name the library
//     lacks or a list over 900 KiB is 400 as for a repository.
//   - the review API's clients (admins; see api_clients.go): GET and POST
//     /api/admin/api-clients, GET, PUT and DELETE /api/admin/api-clients/{name},
//     POST .../{name}/secret and POST .../{name}/keys, DELETE .../{name}/keys/{id}.
//     The audit actions are api_client_created, api_client_updated,
//     api_client_removed, api_key_created, api_key_revoked and
//     api_callback_secret_rotated; the detail is the client's name, with the
//     key's name and prefix for a key, and never a key or a secret.

// SecretView says whether a secret is set and when it last changed. The
// value is never sent. UpdatedAt is null for a secret that comes from the
// config's environment.
type SecretView struct {
	Set       bool       `json:"set"`
	UpdatedAt *time.Time `json:"updatedAt"`
}

// SignInAdminView is GET /api/admin/sign-in (and the answer of its PUT): the
// sign-in settings in force. Source is "config" when dashboard.sign_in in the
// config file sets them (then Editable is false and the PUT is 409), else
// "dashboard". The client secret is never sent, only whether one is set.
// CallbackURL is the redirect URI to register at the identity provider.
type SignInAdminView struct {
	Source         string     `json:"source"`
	Editable       bool       `json:"editable"`
	Mode           string     `json:"mode"`
	Label          string     `json:"label"`
	Issuer         string     `json:"issuer"`
	ClientID       string     `json:"clientID"`
	ClientSecret   SecretView `json:"clientSecret"`
	Header         string     `json:"header"`
	Audience       string     `json:"audience"`
	JWKSURL        string     `json:"jwksURL"`
	EmailClaim     string     `json:"emailClaim"`
	AllowedDomains []string   `json:"allowedDomains"`
	AllowedEmails  []string   `json:"allowedEmails"`
	CallbackURL    string     `json:"callbackURL"`
}

// SignInTestView is the answer of POST /api/admin/sign-in/test: whether the
// provider's discovery document or key set could be read, and nothing else.
type SignInTestView struct {
	OK bool `json:"ok"`
}

// InstanceAdminView is one instance in GET /api/admin/instances.
type InstanceAdminView struct {
	Name string `json:"name"`
	// TrustedAuthorsOnly is the default of the instance's repositories:
	// "follow_visibility", "on" or "off".
	TrustedAuthorsOnly string `json:"trustedAuthorsOnly"`
	// MonthlyTokenQuota is the instance's token quota per UTC month; null
	// for none. TokensThisMonth is what its reviews used this month.
	MonthlyTokenQuota *int64 `json:"monthlyTokenQuota"`
	TokensThisMonth   int64  `json:"tokensThisMonth"`
	// Platform is "gitlab" or "github"; Source is "config" or "dashboard".
	Platform string `json:"platform"`
	Source   string `json:"source"`
	// Removing is set while the instance is being taken out.
	Removing bool `json:"removing"`
	// URL is a GitLab instance's address; APIURL and the App IDs are a
	// GitHub instance's.
	URL          string `json:"url"`
	APIURL       string `json:"apiURL"`
	ReviewAppID  int64  `json:"reviewAppID"`
	ApproveAppID int64  `json:"approveAppID"`
	// WebhookURL is where the platform sends this instance's webhooks.
	WebhookURL string `json:"webhookURL"`
	// Approver is "app" (a GitHub approve App), "review_app" (the review
	// App itself; ApproveAppID is then the review App's ID), "review_bot"
	// (a GitLab review bot itself) or "token" (a bot user).
	Approver string `json:"approver"`
	// Secrets lists the fields the instance takes: review_token,
	// approve_token (not when the review bot approves) and webhook_secret
	// (GitLab); review_app_key, approve_app_key or approve_token (neither
	// when the review App approves) and webhook_secret (GitHub).
	Secrets map[string]SecretView `json:"secrets"`
}

// ModelsAdminView is GET /api/admin/models: the list of models and how many
// repositories follow the default. CanSetDefault is false while the config
// file has an llm: block: that entry is the default then, and "set as
// default" is 409. Entries can still be added, changed and removed.
type ModelsAdminView struct {
	CanSetDefault bool `json:"canSetDefault"`
	// FollowingDefault counts the repositories that name no model: those of
	// the dashboard that chose none and those of the config file. A change of
	// the default moves them.
	FollowingDefault int              `json:"followingDefault"`
	Models           []ModelAdminView `json:"models"`
}

// ModelAdminView is one entry of the list. Source is "config" (read-only, ID
// "config", or "config-1" to "config-4" for llm.fallbacks) or "dashboard".
// Name is unique among global entries ignoring case, and can hold no
// markdown: it reaches the MR comment after a fallback. Default is true for
// the entry reviews use when a repository names none; while the config has
// an llm: block that is its entry alone. TeamsMayUse lets team chains name
// it. Key says whether a key is set and when it changed; the key is never
// sent. Repositories are those whose old model setting names the entry;
// UsedIn labels every chain that names it ("global chain", "team <name>",
// "<instance>/<project>"). Mark is null unless the provider marked it.
// Limit is its model limit (null: none) and Used its tokens this month on
// every instance; TeamLimits each team's limit on it, by team name.
type ModelAdminView struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Provider     string          `json:"provider"`
	Model        string          `json:"model"`
	BaseURL      string          `json:"baseURL"`
	Source       string          `json:"source"`
	Default      bool            `json:"default"`
	TeamsMayUse  bool            `json:"teamsMayUse"`
	Key          SecretView      `json:"key"`
	Repositories []RepoRefView   `json:"repositories"`
	UsedIn       []string        `json:"usedIn"`
	Mark         *MarkView       `json:"mark"`
	Limit        *int64          `json:"limit"`
	Used         int64           `json:"used"`
	TeamLimits   []TeamLimitView `json:"teamLimits"`
}

// TeamLimitView is a team's limit on a global entry and that team's tokens on
// it this month.
type TeamLimitView struct {
	TeamID int64  `json:"teamId"`
	Team   string `json:"team"`
	Limit  int64  `json:"limit"`
	Used   int64  `json:"used"`
}

// MarkView is what the provider said about an entry: Class "quota" or
// "billing" until Until, or "auth" (Until null: until its key is edited).
// Live is false once Until has passed; the mark is then ignored.
type MarkView struct {
	Class string     `json:"class"`
	Until *time.Time `json:"until"`
	SetAt time.Time  `json:"setAt"`
	Live  bool       `json:"live"`
}

// ChainView is a chain as GET .../chain shows it. Scope is the chain asked
// for: "global", "team" or "repo". Source is where its entries come from:
// "repository" (a repository's own chain), "team" (its owner team's, Team
// naming it) or "global". ReadOnly is true when the session may not change
// it. SavedByAdmin is whether an admin saved the chain shown (a repository
// chain an admin saved may name global entries teams may not use). Choices
// are the entries the session may put in it, empty when ReadOnly.
type ChainView struct {
	Scope        string           `json:"scope"`
	Source       string           `json:"source"`
	Team         string           `json:"team"`
	ReadOnly     bool             `json:"readOnly"`
	SavedByAdmin bool             `json:"savedByAdmin"`
	Entries      []ChainEntryView `json:"entries"`
	Choices      []ChainEntryView `json:"choices"`
}

// ReviewersView is GET .../reviewers: reviewers 2 to 5 and the judge
// (reviewer 1 is GET .../chain). Each ChainView is the repository chain's
// shape, its choices included. Never a base URL or a key. ReadOnly is true
// when the session may not change them. Reviewers are in order, reviewer 2
// first. Judge is null with no judge row; its entries are empty when its
// last entry was deleted. Warnings are what the card shows above them:
// "No judge: findings are merged without checking", and "Reviewer <k>
// starts with the same model as reviewer <j>, so it runs on its next entry"
// when reviewer 1's chain or an entry removed outside the card made them
// equal. AddRefusal is why a reviewer can't be added now ("" when it can):
// the image check, or at most 5 reviewers.
type ReviewersView struct {
	ReadOnly   bool            `json:"readOnly"`
	Reviewers  []ReviewerChain `json:"reviewers"`
	Judge      *ChainView      `json:"judge"`
	Warnings   []string        `json:"warnings"`
	AddRefusal string          `json:"addRefusal"`
}

// ReviewerChain is one of reviewers 2 to 5: its number and its chain.
type ReviewerChain struct {
	Number int       `json:"number"`
	Chain  ChainView `json:"chain"`
}

// ChainEntryView is an entry of a chain, or one that may be put in it. Source
// is "global" or "team" (Team is then its team's name). State is what the
// resolver does with it now: "ready", "exhausted" (a quota or billing mark),
// "key_rejected" (an auth mark), "removed", "not_allowed" (a global entry
// teams may not use), "not_owner" (a team entry of a team that doesn't own
// the repository), "key_unusable", "model_limit" (its model limit is used up
// this month) or "team_limit" (the owner team's limit on it is; the global
// chain reads the model limits only). Class and Until are its mark's, "" and
// null with none; CanClear is whether the session may clear it. Fixed is set
// on the config file's entries, which stay at the front of the global chain.
// Never a base URL or a key.
type ChainEntryView struct {
	Position int        `json:"position"`
	Source   string     `json:"source"`
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Provider string     `json:"provider"`
	Model    string     `json:"model"`
	Team     string     `json:"team"`
	State    string     `json:"state"`
	Class    string     `json:"class"`
	Until    *time.Time `json:"until"`
	CanClear bool       `json:"canClear"`
	Fixed    bool       `json:"fixed"`
}

// RepoRefView names a repository by instance and project.
type RepoRefView struct {
	Instance string `json:"instance"`
	Project  string `json:"project"`
}

// UsageTokensView is tokens used: input and output, which every total and
// ranking adds up, and the counts reported apart. Unknown counts the
// review-and-skill rows whose pergent didn't report the apart counts.
type UsageTokensView struct {
	In         int64 `json:"in"`
	Out        int64 `json:"out"`
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
	Reasoning  int64 `json:"reasoning"`
	Unknown    int64 `json:"unknown"`
}

// UsageDayView is one UTC day's usage.
type UsageDayView struct {
	Day    string          `json:"day"` // YYYY-MM-DD
	Tokens UsageTokensView `json:"tokens"`
}

// UsageRepositoryRowView is one repository in GET /api/admin/usage.
// SwitchedOn is false for a repository paused (Paused) or removed since, or
// on an instance that was removed: its usage is kept.
// GlobalTokens and TeamTokens split its tokens (in plus out) by whose key
// paid: the global entries' or teams' own.
type UsageRepositoryRowView struct {
	Instance     string          `json:"instance"`
	Project      string          `json:"project"`
	SwitchedOn   bool            `json:"switchedOn"`
	Paused       bool            `json:"paused"`
	Tokens       UsageTokensView `json:"tokens"`
	Reviews      int64           `json:"reviews"`
	GlobalTokens int64           `json:"globalTokens"`
	TeamTokens   int64           `json:"teamTokens"`
}

// UsageTenantView is one tenant of an API client in GET /api/admin/usage.
// Tenant is "" for reviews asked for with no tenant.
type UsageTenantView struct {
	Tenant  string          `json:"tenant"`
	Tokens  UsageTokensView `json:"tokens"`
	Reviews int64           `json:"reviews"`
}

// UsageClientView is one API client in GET /api/admin/usage, with its busiest
// tenants. MoreTenants counts the tenants left out of Tenants.
type UsageClientView struct {
	Name        string            `json:"name"`
	Tokens      UsageTokensView   `json:"tokens"`
	Reviews     int64             `json:"reviews"`
	Tenants     []UsageTenantView `json:"tenants"`
	MoreTenants int               `json:"moreTenants"`
}

// UsageOverviewView is GET /api/admin/usage (admins). Tokens and Reviews
// cover the repositories and the clients.
type UsageOverviewView struct {
	Days         int                      `json:"days"`
	Tokens       UsageTokensView          `json:"tokens"`
	Reviews      int64                    `json:"reviews"`
	Daily        []UsageDayView           `json:"daily"`
	Repositories []UsageRepositoryRowView `json:"repositories"`
	Clients      []UsageClientView        `json:"clients"`
}

// UsageNamedView is one skill or model of a repository's usage. Provider is
// the model's provider; it is empty for a skill.
type UsageNamedView struct {
	Provider string          `json:"provider"`
	Name     string          `json:"name"`
	Tokens   UsageTokensView `json:"tokens"`
	Runs     int64           `json:"runs"`
	Failed   int64           `json:"failed"`
}

// UsageModelView is a model a merge request's reviews ran on.
type UsageModelView struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// UsageMRView is one merge request of a repository's usage.
type UsageMRView struct {
	IID     int64            `json:"iid"`
	Title   string           `json:"title"`
	WebURL  string           `json:"webURL"`
	Tokens  UsageTokensView  `json:"tokens"`
	Reviews int64            `json:"reviews"`
	Models  []UsageModelView `json:"models"`
}

// UsageWastedView is a repository's wasted tokens: those of skills that
// failed. Reruns are those of the re-runs someone asked for (a rerun or a
// rerun of the failed skills), shown apart and not waste; a skill that
// failed in a re-run is in both Failed and Reruns. NoResult and Superseded
// count reviews whose tokens are unknown.
type UsageWastedView struct {
	Failed     UsageTokensView `json:"failed"`
	Reruns     UsageTokensView `json:"reruns"`
	RerunCount int64           `json:"rerunCount"`
	NoResult   int64           `json:"noResult"`
	Superseded int64           `json:"superseded"`
}

// UsageOutlierView is a review that used more than three times its kind's
// median.
type UsageOutlierView struct {
	RunID       string `json:"runID"`
	IID         int64  `json:"iid"`
	Incremental bool   `json:"incremental"`
	Total       int64  `json:"total"`
	At          string `json:"at"`
}

// UsageRepositoryView is GET /api/repositories/{instance}/{project}/usage
// (admins).
type UsageRepositoryView struct {
	Days              int                `json:"days"`
	Daily             []UsageDayView     `json:"daily"`
	Skills            []UsageNamedView   `json:"skills"`
	Models            []UsageNamedView   `json:"models"`
	MergeRequests     []UsageMRView      `json:"mergeRequests"`
	Wasted            UsageWastedView    `json:"wasted"`
	FullMedian        *float64           `json:"fullMedian"`
	IncrementalMedian *float64           `json:"incrementalMedian"`
	Outliers          []UsageOutlierView `json:"outliers"`
	// GlobalTokens and TeamTokens split its tokens (in plus out) by whose key
	// paid, as UsageRepositoryRowView does.
	GlobalTokens int64 `json:"globalTokens"`
	TeamTokens   int64 `json:"teamTokens"`
}

// --- API clients (admins) ---
//
// A key and a callback secret are in the answer that makes them and in no
// other: they are stored as a hash and sealed, and never read back.

// APILimitsView is a client's caps on its requests.
type APILimitsView struct {
	MaxRequestBytes    int   `json:"maxRequestBytes"`
	MaxFiles           int   `json:"maxFiles"`
	MaxInFlight        int   `json:"maxInFlight"`
	RequestsPerHour    int   `json:"requestsPerHour"`
	MonthlyTokenBudget int64 `json:"monthlyTokenBudget"`
}

// APIClientInput is the body of POST /api/admin/api-clients. Limits may be
// left out for the defaults; if it is given, every field of it is checked.
type APIClientInput struct {
	Name        string         `json:"name"`
	Skills      []string       `json:"skills"`
	ModelID     string         `json:"modelId"`
	CallbackURL string         `json:"callbackUrl"`
	Limits      *APILimitsView `json:"limits"`
}

// APIClientEdit is the body of PUT /api/admin/api-clients/{name}: all of
// the input but the name, and Limits is required.
type APIClientEdit struct {
	Skills      []string       `json:"skills"`
	ModelID     string         `json:"modelId"`
	CallbackURL string         `json:"callbackUrl"`
	Limits      *APILimitsView `json:"limits"`
}

// APIKeyInput is the body of POST /api/admin/api-clients/{name}/keys.
// ExpiresAt, when given, is in the future.
type APIKeyInput struct {
	Name      string     `json:"name"`
	ExpiresAt *time.Time `json:"expiresAt"`
}

// APIClientRowView is one line of the list of clients. The counts are of the
// reviews created from the 1st of the UTC month; tokens are in plus out.
type APIClientRowView struct {
	Name             string `json:"name"`
	Keys             int    `json:"keys"`
	ReviewsThisMonth int64  `json:"reviewsThisMonth"`
	FailedThisMonth  int64  `json:"failedThisMonth"`
	TokensThisMonth  int64  `json:"tokensThisMonth"`
}

// APIClientListView is GET /api/admin/api-clients, by name.
type APIClientListView struct {
	Items []APIClientRowView `json:"items"`
}

// APIKeyView is one of a client's keys: its prefix finds it in a leak scan,
// and the key itself is never shown again.
type APIKeyView struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"createdAt"`
	ExpiresAt  *time.Time `json:"expiresAt"`
	LastUsedAt *time.Time `json:"lastUsedAt"`
}

// APIClientView is one client. PreviousSecretUntil is there while the
// callback secret before the last rotation still signs; ModelID is "" for the
// default model. Keys are newest first.
type APIClientView struct {
	Name                string        `json:"name"`
	Skills              []string      `json:"skills"`
	ModelID             string        `json:"modelId"`
	CallbackURL         string        `json:"callbackUrl"`
	PreviousSecretUntil *time.Time    `json:"previousSecretUntil,omitempty"`
	Limits              APILimitsView `json:"limits"`
	Keys                []APIKeyView  `json:"keys"`
	CreatedAt           time.Time     `json:"createdAt"`
}

// APIClientCreatedView is the 201 of POST /api/admin/api-clients: the only
// time the callback secret is shown.
type APIClientCreatedView struct {
	Client         APIClientView `json:"client"`
	CallbackSecret string        `json:"callbackSecret"`
}

// APICallbackSecretView is the answer of a rotation: the new callback secret,
// shown once.
type APICallbackSecretView struct {
	CallbackSecret string `json:"callbackSecret"`
}

// APICallbackTestView is the answer of Send test callback: what the URL
// answered, or why it did not. StatusCode is 0 for no answer; Error is the
// redacted text of a failure, "" when the URL answered without a redirect.
type APICallbackTestView struct {
	StatusCode int    `json:"statusCode"`
	Error      string `json:"error"`
	DurationMs int64  `json:"durationMs"`
}

// APICallbackAttemptView is one try of a review's callback.
type APICallbackAttemptView struct {
	At         time.Time `json:"at"`
	StatusCode int       `json:"statusCode"`
	Error      string    `json:"error"`
	DurationMs int64     `json:"durationMs"`
}

// APICallbackView is a review's callback: none, pending, delivered or
// gave_up, and the tries so far, oldest first.
type APICallbackView struct {
	State    string                   `json:"state"`
	Attempts []APICallbackAttemptView `json:"attempts"`
}

// APIKeyCreatedView is the 201 of POST /api/admin/api-clients/{name}/keys:
// the only time the key is shown.
type APIKeyCreatedView struct {
	Key    APIKeyView `json:"key"`
	Secret string     `json:"secret"`
}

// APIReviewRowView is one line of a client's reviews. Verdict is "" while the
// review has none.
type APIReviewRowView struct {
	ID         string     `json:"id"`
	Tenant     string     `json:"tenant"`
	Subject    string     `json:"subject"`
	Title      string     `json:"title"`
	Status     string     `json:"status"`
	Verdict    string     `json:"verdict"`
	CreatedAt  time.Time  `json:"createdAt"`
	FinishedAt *time.Time `json:"finishedAt"`
}

// APIReviewListView is GET /api/admin/api-clients/{name}/reviews, newest
// first. NextCursor is not "" when there is a next page.
type APIReviewListView struct {
	Items      []APIReviewRowView `json:"items"`
	NextCursor string             `json:"nextCursor"`
}

// APIReviewFailureView is why an API review failed. Reason is what the caller
// is told; Detail is more, for admins, and may be "".
type APIReviewFailureView struct {
	Step   string `json:"step"`
	Reason string `json:"reason"`
	Detail string `json:"detail"`
}

// APIReviewDetailView is GET /api/admin/api-reviews/{id}. Review is the v1
// body of the review: the one stored when it was published, or, before that,
// the body the API would answer with.
type APIReviewDetailView struct {
	ID         string                `json:"id"`
	Client     string                `json:"client"`
	Tenant     string                `json:"tenant"`
	Subject    string                `json:"subject"`
	Title      string                `json:"title"`
	KeyName    string                `json:"keyName"`
	Status     string                `json:"status"`
	Model      *string               `json:"model"`
	CreatedAt  time.Time             `json:"createdAt"`
	FinishedAt *time.Time            `json:"finishedAt"`
	Failure    *APIReviewFailureView `json:"failure"`
	Callback   APICallbackView       `json:"callback"`
	Review     json.RawMessage       `json:"review"`
	// History is the dismissals and restores made on this review, oldest
	// first. Dismissals is what is dismissed now for its subject, from this
	// review or another, so the page knows which findings to offer a Stop for.
	History    []APIReviewEventView `json:"history"`
	Dismissals []APIDismissalView   `json:"dismissals"`
}

// APIReviewEventView is a dismissal or a restore made on an API review. Actor
// is "key <name>" or an admin's username.
type APIReviewEventView struct {
	At          time.Time `json:"at"`
	Kind        string    `json:"kind"`
	Fingerprint string    `json:"fingerprint"`
	Title       string    `json:"title"`
	Actor       string    `json:"actor"`
	Reason      string    `json:"reason"`
}

// APIDismissalView is a finding dismissed for a review's subject.
type APIDismissalView struct {
	Fingerprint string    `json:"fingerprint"`
	Severity    string    `json:"severity"`
	Reason      string    `json:"reason"`
	Actor       string    `json:"actor"`
	CreatedAt   time.Time `json:"createdAt"`
}

// RuleSetView is one rule set of the library, without its text. Source is
// "dashboard" (stored, admins edit it) or "config" (the config file's,
// read-only; it has no version). Entries is 0 for a stored set that no longer
// loads.
type RuleSetView struct {
	Name       string    `json:"name"`
	Version    int       `json:"version"`
	UpdatedAt  time.Time `json:"updatedAt"`
	Entries    int       `json:"entries"`
	Source     string    `json:"source"`
	InDefaults bool      `json:"inDefaults"`
}

// RuleSetDetailView is a rule set with its text and the editor's notes.
type RuleSetDetailView struct {
	RuleSetView
	Content string   `json:"content"`
	Notes   []string `json:"notes"`
}

// RuleSetWrittenView answers a rule-set save: the new version and the notes
// on the saved text.
type RuleSetWrittenView struct {
	Version int      `json:"version"`
	Notes   []string `json:"notes"`
}

// BuiltInRuleView is one of pergent's built-in path rules: its name, the
// globs it is for in match order (a file takes the first rule that matches,
// across all rows) and its text. No shipped built-in is off.
type BuiltInRuleView struct {
	Name  string   `json:"name"`
	Paths []string `json:"paths"`
	Rule  string   `json:"rule"`
}
