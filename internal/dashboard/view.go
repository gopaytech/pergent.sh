package dashboard

import (
	"time"

	"github.com/gopaytech/pergent.sh/internal/store"
)

// The JSON API's contract: every body the read API returns, field names in
// camelCase. The frontend mirrors these types. Times are RFC 3339 strings.
//
// Every endpoint needs a signed-in session (401 otherwise) and answers only
// about the repositories its user can see: for an operator (a password
// admin) every one, on every instance; for a platform account those its
// teams cover, on the instance it signed in with, that the platform also
// lets it see. A platform account on no team gets 403 {"error": "an admin
// needs to add you to a team", "code": "waiting"} for everything but GET
// /api/me. A repository, review or run the user can't see is answered
// exactly like one that doesn't exist: left out of lists, 404 for its own
// URL. Errors are {"error": "message"}. Nothing here ever holds a token,
// key or secret.
//
// Outcomes ("outcome") are one of:
//   - "running": queued, being reviewed, or reviewed and not posted yet;
//   - "approved": the approve bot approved this run (also when the approval
//     was withdrawn later, for example when the change merged);
//   - "blocking": a skill found blocking issues;
//   - "draft": clean, not approved because the change was a draft;
//   - "no_blocking": clean but not approved, for the reason given (approval
//     turned off, files skipped, the approve side can't reach the
//     repository, the change closed or moved before the approval);
//   - "failed": no usable review (the Job failed, a skill failed, the
//     result was unusable, or it couldn't be posted);
//   - "superseded": replaced by a newer push, or the change was closed;
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
	OutcomeDraft      = store.OutcomeDraft
	OutcomeNoBlocking = store.OutcomeNoBlocking
	OutcomeFailed     = store.OutcomeFailed
	OutcomeSuperseded = store.OutcomeSuperseded
	OutcomeUnknown    = store.OutcomeUnknown
)

// InstanceView is one instance users can sign in with. GET /api/instances
// is a list of them; it is the only endpoint that needs no session. The
// sign-in button goes to /auth/{platform}/{instance}/login.
type InstanceView struct {
	// Platform is "gitlab" or "github".
	Platform string `json:"platform"`
	Instance string `json:"instance"`
	// Label is the button's text after "Sign in with".
	Label string `json:"label"`
}

// OverviewView is GET /api/overview?days=7 (days 1 to 90, default 7):
// numbers over the runs started in the last days, the 10 most recently
// updated reviews of the last 90 days and the setup problems of the
// repositories the user can see (instance-wide ones only for the admins of
// that instance, and for operators).
type OverviewView struct {
	Stats     StatsView       `json:"stats"`
	Recent    []ReviewSummary `json:"recent"`
	Attention []AttentionView `json:"attention"`
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
}

// ReviewListView is GET /api/reviews?project=&instance=&outcome=&days=&cursor=:
// 50 reviews at most, the most recently updated first. project (a path, on
// instance when that is given, else on any) and outcome filter; days (1 to
// 365, default 90) leaves out reviews last updated longer ago. nextCursor, when not "", is passed as cursor for the
// next page.
type ReviewListView struct {
	Items      []ReviewSummary `json:"items"`
	NextCursor string          `json:"nextCursor"`
}

// ReviewDetailView is GET /api/reviews/{instance}/{project...}/{number}:
// the change and every run of it, newest first.
type ReviewDetailView struct {
	Header ReviewHeader `json:"header"`
	Runs   []RunSummary `json:"runs"`
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
	Model  *string     `json:"model"`
	Tokens TokensView  `json:"tokens"`
	Files  FilesView   `json:"files"`
	Skills []SkillView `json:"skills"`
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
	// Source is "library" for a skill of pergent.sh's library, "repo"
	// for the repository's own (also one replacing a library skill), as
	// recorded when the review started; null when it wasn't recorded.
	Source   *string       `json:"source"`
	Findings []FindingView `json:"findings"`
	Checks   []CheckView   `json:"checks"`
}

// FindingView is one finding.
type FindingView struct {
	ID string `json:"id"`
	// Severity is "blocking" or "suggestion".
	Severity string `json:"severity"`
	Title    string `json:"title"`
	File     string `json:"file"`
	// Line is left out when the finding has none.
	Line         int64  `json:"line,omitempty"`
	Explanation  string `json:"explanation"`
	SuggestedFix string `json:"suggestedFix"`
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

// RepositoryView is one repository switched on. GET /api/repositories is
// a list of them.
type RepositoryView struct {
	Instance string `json:"instance"`
	Project  string `json:"project"`
	Platform string `json:"platform"`
	// Source is "config" (read-only here) or "dashboard".
	Source string    `json:"source"`
	On     bool      `json:"on"`
	Last7  Last7View `json:"last7"`
	// Health is "attention" when it has setup problems, else "ok".
	Health string `json:"health"`
}

// Last7View counts the repository's runs of the last 7 days, as StatsView.
type Last7View struct {
	Reviews  int `json:"reviews"`
	Approved int `json:"approved"`
	Failed   int `json:"failed"`
}

// RepositoryDetailView is GET /api/repositories/{instance}/{project...}.
// Settings is keyed approve, reviewDrafts, failureStatus, skills (null:
// every library skill), jobTimeoutSeconds and jobMemory ("" when none).
type RepositoryDetailView struct {
	Repo      RepositoryView         `json:"repo"`
	Settings  map[string]SettingView `json:"settings"`
	Attention []AttentionView        `json:"attention"`
	// Role is the session's role on the repository: "admin", "maintainer"
	// or "viewer".
	Role string `json:"role"`
}

// SettingView is a setting's effective value and whether the repository
// sets it (otherwise it is the default).
type SettingView struct {
	Value      any  `json:"value"`
	Overridden bool `json:"overridden"`
}

// InputsView is GET /api/repositories/{instance}/{project...}/inputs: the
// skills and instructions a review would get, read from the default branch
// with the review bot or App and kept 5 minutes. 503 while pergent.sh
// hasn't set the repository up yet, 502 when they can't be read.
type InputsView struct {
	Branch       string       `json:"branch"`
	Skills       []InputSkill `json:"skills"`
	Instructions string       `json:"instructions"`
}

// InputSkill is one resolved skill.
type InputSkill struct {
	Name string `json:"name"`
	// Source is "library" or "repo" (the repository's own, including one
	// replacing a library skill).
	Source  string `json:"source"`
	Content string `json:"content"`
}

// LibrarySkill is one skill of the library. GET /api/skills is a list of
// them, by name.
type LibrarySkill struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// AttentionView is one setup problem. Project is "" for a problem of the
// whole instance (an expiring token).
type AttentionView struct {
	Instance  string    `json:"instance"`
	Project   string    `json:"project"`
	Kind      string    `json:"kind"`
	Message   string    `json:"message"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// IntegrationView is GET /api/integrations (admins only, 403 otherwise):
// the session's instance.
type IntegrationView struct {
	Instance string `json:"instance"`
	Platform string `json:"platform"`
	// Connected is true: pergent.sh doesn't start unless its tokens or
	// Apps for every instance work.
	Connected bool `json:"connected"`
	// Approver is "app" (a GitHub approve App) or "token" (a bot user).
	Approver string `json:"approver"`
	// TokenExpiry is set while the approve token expires within 14 days.
	TokenExpiry *time.Time        `json:"tokenExpiry,omitempty"`
	Repos       []IntegrationRepo `json:"repos"`
}

// IntegrationRepo is one repository the admin can see.
type IntegrationRepo struct {
	Project string `json:"project"`
	// Installed: pergent.sh's webhook and deploy token (GitLab) or the
	// review App (GitHub) are in place.
	Installed bool `json:"installed"`
	// ApproverReachable: the approve bot or App can approve there.
	ApproverReachable bool `json:"approverReachable"`
}

// AdminSettingsView is GET /api/admin/settings (admins only): the defaults
// a repository of the session's instance gets, and the model. Never a key.
type AdminSettingsView struct {
	Instance string       `json:"instance"`
	Defaults DefaultsView `json:"defaults"`
	LLM      LLMView      `json:"llm"`
}

// DefaultsView is the settings a repository gets when it sets none.
type DefaultsView struct {
	Approve           bool   `json:"approve"`
	ReviewDrafts      bool   `json:"reviewDrafts"`
	FailureStatus     string `json:"failureStatus"`
	JobTimeoutSeconds int64  `json:"jobTimeoutSeconds"`
	// JobMemory is the Job's memory limit, "" when none.
	JobMemory string `json:"jobMemory"`
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
// repositories on and off and for the picker, a maintainer or admin of the
// repository for its settings. A repository the user can't see is answered
// exactly like one that doesn't exist (404), whatever the write. A
// repository listed in the config is read-only here (409). Errors are
// {"error": "message"}. The picker and switching on need a platform token:
// an operator gets 400 {"error": "sign in with GitLab or GitHub to add
// repositories"}.
//
//   - GET /api/repositories/available?search=: AvailableView, the
//     repositories that can be switched on; search (at most 100
//     characters) keeps those whose path holds it (GitLab also matches
//     names), so ones beyond the first 500 can be found.
//   - POST /api/repositories {"project": "acme/widgets"}: switches on a
//     repository with every setting at its default. It is checked on its
//     own, not against the picker's lists: the admin must be a member with
//     read access (GitLab: Reporter or above; GitHub: pull) and the review
//     bot (GitLab: Maintainer or above) or App (GitHub: installed) must
//     reach it, else 404, whether it is on or not; then 409 when it is on
//     already. 201, RepositoryDetailView, the project spelled as the
//     platform does. pergent.sh sets it up (webhook and clone credentials)
//     within seconds.
//   - DELETE /api/repositories/{instance}/{project...}: switches it off.
//     200, RepositoryDetailView with repo.on false and the default
//     settings. pergent.sh removes its webhook and clone credentials.
//   - PUT /api/repositories/{instance}/{project...}/settings: changes the
//     settings in the body and leaves the others; a null resets one to its
//     default. The keys are those of RepositoryDetailView.Settings:
//     approve and reviewDrafts (true or false), failureStatus ("failed" or
//     "neutral"), skills (a non-empty list of library skill names),
//     jobTimeoutSeconds (a whole number, 60 to 7200) and jobMemory (a
//     Kubernetes quantity such as "6Gi", from 256Mi, and no less than
//     jobs.resources.requests.memory, to 32Gi). Any other key, or a value
//     out of bounds, is 400 and nothing changes. 200, RepositoryDetailView.
//   - POST /api/repositories/{instance}/{project...}/settings/reset: every
//     setting back to its default. 200, RepositoryDetailView.
//
// Users whose teams cover a repository switched on see it at their next
// request, once the platform says they may.

// AvailableView is GET /api/repositories/available: the repositories of the
// session's instance that the admin is a member of with read access
// (GitLab: Reporter or above; GitHub: pull) and the review bot (GitLab:
// Maintainer or above) or review App (GitHub: its installations) reaches,
// and that aren't switched on, by project ignoring case. 500 at most;
// truncated is true when there were more, or when a list it is made from
// was too long (5000) or too slow (60 s, each request 10 s) to read whole.
// 502 when GitLab or GitHub can't be asked, or nothing could be read in
// time.
type AvailableView struct {
	Items     []AvailableRepo `json:"items"`
	Truncated bool            `json:"truncated"`
}

// AvailableRepo is one repository that can be switched on.
type AvailableRepo struct {
	Instance string `json:"instance"`
	Project  string `json:"project"`
	// Platform is "gitlab" or "github".
	Platform string `json:"platform"`
}
