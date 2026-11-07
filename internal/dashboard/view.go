package dashboard

import (
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
//   - "draft": clean, not approved because the change was a draft;
//   - "no_blocking": clean but not approved, for the reason given (approval
//     turned off, files skipped, the approve side can't reach the
//     repository, the change closed or moved before the approval);
//   - "failed": no usable review (the Job failed, a skill failed, the
//     result was unusable, or it couldn't be posted);
//   - "superseded": replaced by a newer push, or the change was closed;
//   - "skipped": not reviewed because the repository has no skills (no Job,
//     no comment, no status); never counted as a review or a failure;
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
	OutcomeSkipped    = store.OutcomeSkipped
	OutcomeUnknown    = store.OutcomeUnknown
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
	// MaxTurns is the step limit each opencode run of the review had, as
	// recorded when it started; null for a run that didn't record it.
	MaxTurns *int `json:"maxTurns"`
	// IncrementalNote says what an incremental review covered, for example
	// "Re-reviewed 2 files · 28 carried over from `abc1234`"; "" for a full
	// review.
	IncrementalNote string `json:"incrementalNote"`
	// Quiet is whether the review was started quiet, as recorded when it
	// started; false for a run that didn't record it.
	Quiet  bool        `json:"quiet"`
	Tokens TokensView  `json:"tokens"`
	Files  FilesView   `json:"files"`
	Skills []SkillView `json:"skills"`
	// LatestDecided is true for the one run of the change that dismiss and
	// restore act on: its newest published review, or reported one with an
	// outcome. Only that run shows dismissals; false when the change has no
	// such run.
	LatestDecided bool `json:"latestDecided"`
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
// only, an empty list to anyone else), failureStatus, skills (null: the default list of skills; otherwise the library skills
// reviewed), repoSkills
// (the detected repository skills turned on, a list, empty when none),
// jobTimeoutSeconds, maxTurns, jobMemory ("" when none), rerunLimitPerMR and
// rerunLimitPerRepo.
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
	// CanScan: the session is a maintainer or admin of the repository.
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
// whole instance (an expiring token).
type AttentionView struct {
	Instance  string    `json:"instance"`
	Project   string    `json:"project"`
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
	CommentLayout     []string `json:"commentLayout"`
	FailureStatus     string   `json:"failureStatus"`
	JobTimeoutSeconds int64    `json:"jobTimeoutSeconds"`
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
	// Config holds the values the config gives (jobs.* and the built-in
	// defaults), before anything saved here. Skills has no config value.
	Config DefaultValues `json:"config"`
	// Pinned says which defaults were saved here and so no longer follow the
	// config, by JSON key (approve, reviewDrafts, inlineFindings,
	// commandsAuthorAndMaintainersOnly, incrementalReviews,
	// learnFromDismissals, quietReviews, commentLayout, linkedReposEnabled, failureStatus, jobTimeoutSeconds, maxTurns, jobMemory,
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
// repositories on and off and for the picker, a maintainer or admin of the
// repository for its settings. A repository the user can't see is answered
// exactly like one that doesn't exist (404), whatever the write. A
// repository listed in the config is read-only here (409). Each change is
// recorded in the audit log: repository_on, repository_off,
// settings_updated (detail: the settings in the body) and settings_reset.
// Errors are {"error": "message"}.
//
//   - GET /api/repositories/available?instance=&search=: AvailableView, the
//     repositories of the instance that can be switched on (400 without an
//     instance, 404 for one that is not there); search (at most 100
//     characters) keeps those whose path holds it (GitLab also matches
//     names), so ones beyond the first 500 can be found.
//   - POST /api/repositories {"instance": "lab", "project": "acme/widgets"}:
//     switches on a repository with every setting at its default. It is
//     checked on its own, not against the picker's lists: the review bot
//     (GitLab: Maintainer or above) or App (GitHub: installed) must reach
//     it, else 404, whether it is on or not; then 409 when it is on
//     already. 201, RepositoryDetailView, the project spelled as given.
//     pergent.sh sets it up (webhook and clone credentials) within
//     seconds.
//   - DELETE /api/repositories/{instance}/{project...}: switches it off.
//     200, RepositoryDetailView with repo.on false and the default
//     settings. pergent.sh removes its webhook and clone credentials.
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
//     installation, and 502 when GitHub can't tell, with nothing saved), failureStatus ("failed" or
//     "neutral"), skills (a non-empty list of library skill names, or
//     null for the default list; a name the library lacks is 400 `skill
//     "x" is not in the skill library`, and so is a list whose texts come to
//     more than 900 KiB), repoSkills (the names of detected repository
//     skills to turn on; a list, empty or null for none),
//     jobTimeoutSeconds (a whole number, 60 to 7200), maxTurns (a whole
//     number, 5 to 200) and jobMemory (a Kubernetes quantity such as
//     "6Gi", from 256Mi, and no less than jobs.resources.requests.memory,
//     to 32Gi), and rerunLimitPerMR and
//     rerunLimitPerRepo (a whole number, 1 to 1000; only an admin changes
//     these, else 403, and a maintainer's reset leaves them). Any other key, or a value
//     out of bounds, is 400 and nothing changes. 200, RepositoryDetailView.
//   - POST /api/repositories/{instance}/{project...}/settings/reset: every
//     setting back to its default, except that a maintainer's reset leaves
//     the two re-run caps, the linked repositories setting and the links as
//     they are (only an admin resets those). 200, RepositoryDetailView.
//   - GET /api/admin/linkable?instance=&repo=&search= (admins only): LinkableView,
//     the repositories the instance's review App reaches (at most 200, by name),
//     each with whether `repo` (a repository switched on, GitHub only; 400 for
//     GitLab) may link it, and why not. See LinkableRepo.
//
// Users whose teams cover a repository switched on see it at their next
// request.

// AvailableView is GET /api/repositories/available: the repositories of the
// instance that the review bot (GitLab: Maintainer or above) or review App
// (GitHub: its installations) reaches and that aren't switched on, by project
// ignoring case. 500 at most; truncated is true when there were more, or when
// the list it is made from was too long (5000) or too slow (20 s, each
// request 10 s) to read whole. 502 when GitLab or GitHub can't be asked, or
// nothing could be read in time.
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

// TeamRepoView is a repository a team covers.
type TeamRepoView struct {
	Instance string `json:"instance"`
	Project  string `json:"project"`
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
// llm_updated (detail: the fields changed, "api_key" among them) and
// defaults_updated.
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
//   - DELETE /api/admin/instances/{name}: 202 {"status": "removing"}. Its
//     runs are superseded and its repositories, accounts and sessions go
//     now; pergent.sh takes the webhooks off the platform and deletes the
//     instance and its secrets within a minute or so. 409 for the config's.
//   - GET /api/admin/instances/{name}/repositories?search=: AvailableView,
//     the repositories the instance's review bot or App can set up that
//     aren't switched on (the same list as the picker's).
//   - GET /api/admin/llm: LLMAdminView (admins). PUT /api/admin/llm
//     {"provider", "model", "baseURL", "apiKey"} (admins; 409 when the
//     config has an llm: block; the key is needed only when none is
//     stored): 200, LLMAdminView.
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

// LLMAdminView is GET /api/admin/llm: where the model comes from ("config"
// or "dashboard"), the model, and whether a key is set. Editable is true for
// an admin when the config has no llm: block.
type LLMAdminView struct {
	Source   string     `json:"source"`
	Provider string     `json:"provider"`
	Model    string     `json:"model"`
	BaseURL  string     `json:"baseURL"`
	Key      SecretView `json:"key"`
	Editable bool       `json:"editable"`
}
