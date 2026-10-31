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
// the session's instance; an operator, who has none, names it with
// ?instance=.
type IntegrationView struct {
	Instance string `json:"instance"`
	Platform string `json:"platform"`
	// Connected is whether pergent.sh is connected to the instance now: its
	// review side serves the dashboard. An instance of the config always
	// is (pergent.sh doesn't start otherwise); one added in the dashboard
	// that couldn't be reached at startup, or whose connection failed, is
	// not until it is saved again. Without that wiring it is true.
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
// a repository switched on in the dashboard gets, and the model. Never a key.
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
// repository listed in the config is read-only here (409). Each change is
// recorded in the audit log: repository_on, repository_off,
// settings_updated (detail: the settings in the body) and settings_reset.
// Errors are {"error": "message"}. The picker needs a platform token: an
// operator gets 400 {"error": "sign in with GitLab or GitHub to add
// repositories"} and uses GET /api/admin/instances/{name}/repositories.
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
//     within seconds. An operator names the instance too ({"instance":
//     "lab", "project": ...}) and needs no membership: only the review bot
//     or App must reach it.
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

// --- Users, teams and the audit log (admins only, 403 otherwise) ---
//
// Every write needs the X-CSRF-Token header, as above, and is recorded in
// the audit log: account_created and account_deleted (detail: the
// username), team_created (detail: "name (role)"), team_updated (detail:
// "name: " and the parts changed, of name, role, members and repos) and
// team_deleted (detail: the name). Errors are {"error": "message"}.
//
//   - GET /api/users: []UserView, the waiting accounts first, then
//     everyone by username.
//   - DELETE /api/users/{id}: removes the account with its sessions and team
//     memberships (its next request is 401; a platform account is made
//     again, on no team, at its next sign-in). A password account needs a
//     password admin (403); the last one can't be removed (409). 204.
//   - POST /api/admin/accounts {"username"} (password admins only): a new
//     password admin with a generated password, which must be changed at
//     its first sign-in. The username is 1 to 64 letters, digits, ".", "_"
//     or "-" (400), not taken ignoring case (409). 201, NewAccountView: the
//     only time the password is shown.
//   - GET /api/teams: []TeamView, by name ignoring case.
//   - POST /api/teams {"name", "role"}: a team with no members and no
//     repositories; a name taken ignoring case is 409. 201, TeamView.
//   - PUT /api/teams/{id} {"name", "role", "members": [account IDs],
//     "repos": [{"instance", "project"}]}: replaces them all. Members are
//     platform accounts (400 otherwise). An admin team has "repos": [] (400
//     otherwise). A repository is one switched on, or {"instance": X,
//     "project": ""} for every repository of instance X (400 otherwise).
//     For a platform admin the team's repositories it can't see are kept
//     as they are, and it can name repositories of its own instance only
//     (400); making a team that covers repositories it can't see an admin
//     team is 409 {"error": "this team covers repositories you can't see;
//     a password admin can change its role or remove it"}, since an admin
//     team has none. 200, TeamView; 404 for no such team.
//   - DELETE /api/teams/{id}: removes the team (not its members' accounts).
//     For a platform admin a team covering repositories it can't see is
//     409, as above. 204.
//   - GET /api/audit?before=<id>: AuditPageView, 100 entries at most,
//     newest first, older than the entry before when given. Its next is
//     the before of the page after, 0 at the end of the log (the page after
//     can still be empty when the log ends just there). A platform admin's
//     request reads at most 50 chunks of 100 entries: when those don't fill
//     the page, it answers what they held, so a short page with next not 0
//     isn't the last.
//
// A platform admin sees only what it can see: a team's repositories of
// other instances, and those its platform hides from it, are counted in
// hiddenRepos and never named; audit entries about a repository it can't
// see are left out.

// UserView is one account.
type UserView struct {
	ID int64 `json:"id"`
	// Kind is "password" or "platform".
	Kind     string `json:"kind"`
	Username string `json:"username"`
	// Platform and Instance are "" for a password account.
	Platform string `json:"platform"`
	Instance string `json:"instance"`
	// LastSignInAt is null for an account that never signed in.
	LastSignInAt *time.Time `json:"lastSignInAt"`
	// Waiting: a platform account on no team, which sees nothing.
	Waiting bool      `json:"waiting"`
	Teams   []TeamRef `json:"teams"`
}

// TeamRef names a team.
type TeamRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// NewAccountView is a password admin just made, with its generated
// password.
type NewAccountView struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// TeamView is one team.
type TeamView struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// Role is "admin", "maintainer" or "viewer".
	Role    string           `json:"role"`
	Members []TeamMemberView `json:"members"`
	// Repos are the repositories it covers that the session can see, by
	// instance and project; a project "" covers every repository of the
	// instance. An admin team has none.
	Repos []TeamRepoView `json:"repos"`
	// HiddenRepos counts the team's repositories the session can't see.
	HiddenRepos int `json:"hiddenRepos"`
}

// TeamMemberView is a team's member: a platform account.
type TeamMemberView struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Platform string `json:"platform"`
	Instance string `json:"instance"`
}

// TeamRepoView is a repository a team covers.
type TeamRepoView struct {
	Instance string `json:"instance"`
	Project  string `json:"project"`
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

// --- Instances, the LLM and the defaults (operators; the last two are
// readable by admins) ---
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
//     token), "oauthClientID", "secrets": {field: value}}: checks the
//     credentials the way startup does (400 with the reason when they fail,
//     and nothing is stored), stores them sealed and connects. A GitLab
//     instance's webhook_secret is generated; giving one is 400. 201,
//     InstanceAdminView; 409 when the name is taken in the config or the
//     dashboard.
//   - PUT /api/admin/instances/{name} {"oauthClientID", "secrets": {...}}:
//     replaces what it names, checks the whole, connects again. An address
//     or App ID that differs is 400; an instance of the config is 409.
//     200, InstanceAdminView.
//   - DELETE /api/admin/instances/{name}: 202 {"status": "removing"}. Its
//     runs are superseded and its repositories, accounts and sessions go
//     now; pergent.sh takes the webhooks off the platform and deletes the
//     instance and its secrets within a minute or so. 409 for the config's.
//   - GET /api/admin/instances/{name}/repositories?search=: AvailableView,
//     the repositories the instance's review bot or App can set up that
//     aren't switched on, for an operator, who has no platform account.
//   - GET /api/admin/llm: LLMAdminView (admins). PUT /api/admin/llm
//     {"provider", "model", "baseURL", "apiKey"} (operators; 409 when the
//     config has an llm: block; the key is needed only when none is
//     stored): 200, LLMAdminView.
//   - GET /api/admin/defaults, PUT /api/admin/defaults (admins): DefaultsView
//     for the repositories switched on in the dashboard; the PUT body has the
//     same keys with the same bounds as a repository's settings, and
//     replaces them all (a missing key or null is the built-in default).

// SecretView says whether a secret is set and when it last changed. The
// value is never sent. UpdatedAt is null for a secret that comes from the
// config's environment.
type SecretView struct {
	Set       bool       `json:"set"`
	UpdatedAt *time.Time `json:"updatedAt"`
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
	// Approver is "app" (a GitHub approve App) or "token" (a bot user).
	Approver      string `json:"approver"`
	OAuthClientID string `json:"oauthClientID"`
	// Secrets lists the fields the instance takes: review_token,
	// approve_token, webhook_secret and oauth_client_secret (GitLab);
	// review_app_key, approve_app_key or approve_token, webhook_secret and
	// oauth_client_secret (GitHub).
	Secrets map[string]SecretView `json:"secrets"`
}

// LLMAdminView is GET /api/admin/llm: where the model comes from ("config"
// or "dashboard"), the model, and whether a key is set. Editable is true for
// an operator when the config has no llm: block.
type LLMAdminView struct {
	Source   string     `json:"source"`
	Provider string     `json:"provider"`
	Model    string     `json:"model"`
	BaseURL  string     `json:"baseURL"`
	Key      SecretView `json:"key"`
	Editable bool       `json:"editable"`
}
