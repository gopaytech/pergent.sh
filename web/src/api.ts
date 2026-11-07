// The dashboard's JSON API: types mirroring internal/dashboard/view.go and
// a small client. Times are RFC 3339 strings.
//
// Everything the API returns that a person or a model wrote (titles,
// findings, logs, messages) is shown as text only, never as HTML.

export type Platform = "gitlab" | "github";

export type Outcome =
  | "running"
  | "approved"
  | "blocking"
  | "draft"
  | "no_blocking"
  | "failed"
  | "superseded"
  | "skipped"
  | "unknown";

// Me is the signed-in account, from GET /api/me: a local admin ("password")
// or an SSO account. role is the account's role and admin says the same as a
// flag. signIn is how the session began; the web signs out of a "jwt" session
// to /signed-out. waiting is a user on no team; waitingUsers, for an admin, is
// how many users wait for one.
export type Me = {
  kind: "password" | "sso";
  username: string;
  email: string;
  role: "admin" | "user";
  signIn: "password" | "oidc" | "jwt";
  admin: boolean;
  waiting: boolean;
  mustChangePassword: boolean;
  waitingUsers: number;
  csrfToken: string;
};

export type Role = "admin" | "maintainer" | "viewer";

// SignInOptions is GET /api/sign-in-options: whether the password form is
// shown (on /admin/sign-in), the SSO button when it is set up, and whether a
// proxy signs people in (jwt: no button, a page load starts the session).
export type SignInOptions = { password: boolean; sso?: { label: string } | null; jwt: boolean };

export type StatsView = { reviews: number; approved: number; blocking: number; failed: number };

export type ReviewSummary = {
  instance: string;
  project: string;
  platform: Platform;
  number: number;
  title: string;
  outcome: Outcome;
  reason: string;
  updatedAt: string;
  webURL: string;
};

export type AttentionView = {
  instance: string;
  project: string;
  kind: string;
  message: string;
  updatedAt: string;
};

// SetupView is the setup checklist of an admin: each item is true once done.
// The overview's setup is null for everyone else and once all four are done.
export type SetupView = { llm: boolean; instance: boolean; skill: boolean; repository: boolean };

export type OverviewView = {
  stats: StatsView;
  recent: ReviewSummary[];
  attention: AttentionView[];
  setup: SetupView | null;
};

export type ReviewListView = { items: ReviewSummary[]; nextCursor: string };

export type ReviewHeader = {
  instance: string;
  project: string;
  platform: Platform;
  number: number;
  title: string;
  sourceBranch: string;
  targetBranch: string;
  author?: string;
  webURL: string;
};

export type RunSummary = {
  id: string;
  head: string;
  trigger: "first" | "push" | "retarget" | "rerun";
  outcome: Outcome;
  reason: string;
  createdAt: string;
  durationSeconds: number | null;
};

// Can is what the session may do to a review with the buttons. The server
// computes it as a hint; the routes check again and may still refuse.
export type Can = {
  rerun: boolean;
  dismissBlocking: boolean;
  dismissSuggestion: boolean;
  restore: boolean;
};

// role is the session's role on the repository, "" for one switched off.
export type ReviewDetailView = { header: ReviewHeader; runs: RunSummary[]; role: Role | ""; can: Can };

// RerunStatus is the answer of the re-run routes (202).
export type RerunStatus = "queued" | "already queued" | "nothing failed" | "whole review";

// DismissStatus and RestoreStatus are the answers of the dismiss and restore
// routes (200).
export type DismissStatus = "dismissed" | "already dismissed";
export type RestoreStatus = "restored" | "not dismissed";

// DismissedView is who set a finding aside, and why. by and reason are user
// text.
export type DismissedView = { by: string; reason: string; at: string };

export type FindingView = {
  id: string;
  // fingerprint identifies the finding across runs: what dismiss and restore
  // name.
  fingerprint: string;
  severity: "blocking" | "suggestion";
  title: string;
  file: string;
  line?: number;
  explanation: string;
  suggestedFix: string;
  // dismissed is null for a finding that stands.
  dismissed: DismissedView | null;
};

export type CheckView = { batch: number; section: string; name: string; status: string; detail: string };

export type SkillView = {
  name: string;
  verdict: "no_blocking_issues" | "has_blocking_issues" | "failed";
  // source is "library", the repository folder the skill was read from
  // (".claude/skills"), "repo" for a run from before the folders, or null
  // when it wasn't recorded.
  source: string | null;
  // version is the library skill's version the review used; 0 for a
  // repository skill and for a run that didn't record it.
  version: number;
  // findings are those still standing; dismissed are those set aside.
  findings: FindingView[];
  dismissed: FindingView[];
  checks: CheckView[];
};

export type RunDetailView = {
  run: RunSummary;
  instance: string;
  project: string;
  number: number;
  status: string;
  approved: boolean;
  approvalReason: string;
  pergentVersion: string;
  model: string | null;
  // The step limit the run had; null for a run from before it was recorded.
  maxTurns: number | null;
  // What an incremental review covered ("Re-reviewed 2 files · 28 carried over
  // from `abc1234`"); empty for a full review.
  incrementalNote: string;
  // The review was started quiet (the repository's Quiet reviews switch, as it
  // was when the review began); false for a run from before it was recorded.
  quiet: boolean;
  tokens: { in: number; out: number };
  files: {
    reviewed: string[];
    skipped: { path: string; rule: string }[];
    notReviewed: { path: string; cause: string }[];
  };
  skills: SkillView[];
  // latestDecided: this is the run dismiss and restore act on (the change's
  // newest published or decided review), the only one that shows dismissals.
  latestDecided: boolean;
};

export type LogView = { text: string };

export type RepositoryView = {
  instance: string;
  project: string;
  platform: Platform;
  source: "config" | "dashboard";
  on: boolean;
  last7: { reviews: number; approved: number; failed: number };
  health: "ok" | "attention";
};

export type SettingView<T> = { value: T; overridden: boolean };

export type RepoSettings = {
  approve: SettingView<boolean>;
  reviewDrafts: SettingView<boolean>;
  inlineFindings: SettingView<boolean>;
  commandsAuthorAndMaintainersOnly: SettingView<boolean>;
  incrementalReviews: SettingView<boolean>;
  learnFromDismissals: SettingView<boolean>;
  quietReviews: SettingView<boolean>;
  // commentLayout is the comment's blocks as "block:state" entries in order:
  // the default in effect (the admin default, else the built-in layout) while
  // overridden is false.
  commentLayout: SettingView<string[]>;
  failureStatus: SettingView<string>;
  // skills is null for the default list, otherwise the library skills.
  skills: SettingView<string[] | null>;
  // repoSkills are the detected repository skills turned on.
  repoSkills: SettingView<string[]>;
  jobTimeoutSeconds: SettingView<number>;
  maxTurns: SettingView<number>;
  jobMemory: SettingView<string>;
  rerunLimitPerMR: SettingView<number>;
  rerunLimitPerRepo: SettingView<number>;
};

export type RepositoryDetailView = {
  repo: RepositoryView;
  settings: RepoSettings;
  attention: AttentionView[];
  // role is the session's role on this repository.
  role: Role;
};

// --- readiness (GET .../readiness) ---
//
// Every path, reason, skill name and error below comes from a scan Job: it is
// shown as text only, never as HTML and never in a link.

// ScanView is one readiness scan. state is "queued", "running", "done" or
// "failed". startedBy is "switch-on", "upgrade" or who asked ("alice (gitlab
// main)"). branch and commit are "" until the scan read them; error is why
// a failed scan couldn't scan the files; finishedAt is null until it ends.
export type ScanView = {
  id: string;
  state: "queued" | "running" | "done" | "failed";
  startedBy: string;
  branch: string;
  commit: string;
  error: string;
  queuedAt: string;
  finishedAt: string | null;
};

// "unknown" is "Couldn't check": it is never shown as passed.
export type ChecklistState = "passed" | "warning" | "missing" | "unknown";

// NoiseFile is a file a review reads that looks generated, vendored or very
// large; skills are the skills whose exclude would drop it.
export type NoiseFile = { path: string; why: string; size: number; skills?: string[] };

// ChecklistItem is one line of the checklist. why and fix are empty for a
// passed item; list, noise and more are left out when empty.
export type ChecklistItem = {
  key: string;
  state: ChecklistState;
  reason: string;
  why: string;
  fix: string;
  list?: string[];
  noise?: NoiseFile[];
  more?: number;
};

export type ChecklistSection = { key: string; items: ChecklistItem[] };

// Checklist is built from the latest finished scan; suggested names library
// skills that fit the files and are not turned on, budget the bytes of skills
// plus instructions.
export type Checklist = { sections: ChecklistSection[]; suggested: string[]; budget: number };

// ReadinessView is the Readiness tab. latest is the latest finished scan,
// active the queued or running one; checklist is null until a scan has
// finished. applyBase is today's library skill list: "Use these skills" saves
// it plus the ticked suggestions.
export type ReadinessView = {
  latest: ScanView | null;
  active: ScanView | null;
  checklist: Checklist | null;
  canScan: boolean;
  configRepo: boolean;
  followsDefaults: boolean;
  applyBase: string[];
};

// PastDismissalView is a finding whose dismissals teach the repository's reviews
// (GET .../past-dismissals, maintainers and admins). by, at and number are of the
// dismissal shown; number is its change. The text is what people wrote.
export type PastDismissalView = {
  skill: string;
  severity: "blocking" | "suggestion";
  file: string;
  title: string;
  reason: string;
  by: string;
  at: string;
  number: number;
  fingerprint: string;
};

// SkillResultView is what became of the findings of one skill at one severity
// decided in the window (GET .../skill-results?days=30|90, anyone who can see
// the repository). Raised, fixed, open and wentAway count merged changes;
// dismissed also counts closed ones, so raised can be less than the columns'
// sum. addressed is fixed over raised as a whole percent, null under 10 raised.
// editedAt is set when the window holds more than one version of the skill.
export type SkillResultView = {
  skill: string;
  severity: "blocking" | "suggestion";
  raised: number;
  fixed: number;
  dismissed: number;
  open: number;
  wentAway: number;
  addressed: number | null;
  editedAt: string | null;
};

// SettingsUpdate is the body of PUT .../settings: only the keys to change;
// null resets one to its default.
export type SettingsUpdate = Partial<{
  approve: boolean | null;
  reviewDrafts: boolean | null;
  inlineFindings: boolean | null;
  commandsAuthorAndMaintainersOnly: boolean | null;
  incrementalReviews: boolean | null;
  learnFromDismissals: boolean | null;
  quietReviews: boolean | null;
  commentLayout: string[] | null;
  failureStatus: string | null;
  skills: string[] | null;
  repoSkills: string[] | null;
  jobTimeoutSeconds: number | null;
  maxTurns: number | null;
  jobMemory: string | null;
  rerunLimitPerMR: number | null;
  rerunLimitPerRepo: number | null;
}>;

// InputSkill is one skill of the set a review would run. source is
// "library" or the repository folder; with a problem the review would fail on
// the skill and source and content are empty. url is a repository skill's file
// on the platform ("" for a library skill).
export type InputSkill = {
  name: string;
  source: string;
  folder: string;
  version: number;
  content: string;
  url: string;
  problem?: string;
};

// DetectedSkill is a skill found in a repository folder that runs only when
// turned on. name is the file or folder name when there is a problem.
export type DetectedSkill = { name: string; folder: string; problem?: string; on: boolean };

export type InputsView = {
  branch: string;
  skills: InputSkill[];
  detected: DetectedSkill[];
  instructions: string;
};

// skillBudget is what a review takes of library skill text, in bytes
// (inputs.ConfigMapLimit): saving a list over it is refused.
export const skillBudget = 900 * 1024;

// SkillSource is where an installed skill came from (SkillSourceView).
// hidden: the repository is private to the viewer, so repo is not sent and
// the page says "a private repository on <instance>". A url is public.
export type SkillSource = {
  kind: "instance" | "url";
  instance?: string;
  repo?: string;
  url?: string;
  // path is the skill's file in the source; never sent with hidden.
  path?: string;
  ref: string;
  commit: string;
  hidden?: boolean;
};

// LibrarySkill is one skill of the library without its text (LibrarySkillView).
// source is null for a skill written in the dashboard; updatedBy is sent to
// admins only; size is bytes.
export type LibrarySkill = {
  name: string;
  version: number;
  size: number;
  updatedAt: string;
  inDefaults: boolean;
  source: SkillSource | null;
  updatedBy?: string;
};

// SkillVersion is one saved version; actor is sent to admins only.
export type SkillVersion = { version: number; at: string; actor?: string; commit: string };

// SkillDetail is GET /api/skills/{name}: the skill with its text and its
// versions, newest first. The text is shown as plain text only.
export type SkillDetail = LibrarySkill & { content: string; versions: SkillVersion[] };

export type SkillVersionDetail = SkillVersion & { content: string };

// SkillInUse is the body of the 409 a delete gets while repositories or the
// default list name the skill.
export type SkillInUse = {
  error: string;
  repositories: { instance: string; project: string }[];
  inDefaults: boolean;
};

// FetchedSkill is one skill a read of a git source found (FetchedSkillView):
// size is bytes (0 when the text was not kept), problem says why it can't be
// installed, taken that a skill of that name is in the library already.
export type FetchedSkill = { name: string; path: string; size: number; problem?: string; taken: boolean };

// SkillUpdate is one installed skill in a check for updates. state is
// "changed", "same", "removed at the source" or "renamed at the source";
// problem says why it can't be updated. current and fetched are the two
// texts, shown as text only.
export type SkillUpdate = {
  name: string;
  path: string;
  state: string;
  problem?: string;
  version: number;
  current: string;
  fetched: string;
};

// SkillFetch is GET /api/admin/skill-fetches/{id} (SkillFetchView): a read of
// a git source. status is "running", "done" or "failed" (error says why);
// skills are what an install read found, updates what a check for updates
// matched with the installed skills.
export type SkillFetch = {
  id: string;
  status: "running" | "done" | "failed";
  commit: string;
  error: string;
  purpose: "install" | "update";
  instance?: string;
  repo?: string;
  url?: string;
  ref: string;
  fromRef?: string;
  path: string;
  skills: FetchedSkill[];
  updates: SkillUpdate[];
};

// SkillFetchResult is the answer for one name of an install or an update.
export type SkillFetchResult = { name: string; ok: boolean; error?: string };

// Approver is who approves for an instance: a second App, the review App
// itself, or a bot user's token.
export type Approver = "app" | "review_app" | "review_bot" | "token";

// SecretView says whether a secret is set and when it last changed; the
// value is never sent. updatedAt is null for a secret from the config's
// environment.
export type SecretView = { set: boolean; updatedAt: string | null };

// DefaultsView is the settings a repository switched on in the browser gets
// when it sets none (jobMemory "" is none).
export type DefaultValues = {
  approve: boolean;
  reviewDrafts: boolean;
  inlineFindings: boolean;
  commandsAuthorAndMaintainersOnly: boolean;
  incrementalReviews: boolean;
  learnFromDismissals: boolean;
  quietReviews: boolean;
  // The comment's blocks as "block:state" entries in order; the built-in
  // layout until one is saved.
  commentLayout: string[];
  failureStatus: string;
  jobTimeoutSeconds: number;
  maxTurns: number;
  jobMemory: string;
  // The most re-runs (buttons and comments) per merge request, and per
  // repository, in a UTC day: 1 to 1000.
  rerunLimitPerMR: number;
  rerunLimitPerRepo: number;
};

export type DefaultsView = DefaultValues & {
  // skills is the default list: the library skills a repository reviews when
  // its own skills setting is null. Empty means none.
  skills: string[];
  // config is what the config file gives (jobs.* and the built-in defaults),
  // before anything saved here.
  config: DefaultValues;
  // pinned says, by key, which defaults were saved here and so no longer
  // follow the config.
  pinned: Record<string, boolean>;
};

// LLMAdminView is GET /api/admin/llm: where the model comes from, the model
// and whether a key is set. editable is true for an admin when the config
// has no llm: block.
export type LLMAdminView = {
  source: "config" | "dashboard";
  provider: string;
  model: string;
  baseURL: string;
  key: SecretView;
  editable: boolean;
};

// InstanceAdminView is one instance in GET /api/admin/instances (admins).
// secrets has the fields the instance takes: review_token, approve_token
// (not when the review bot approves), webhook_secret (GitLab); review_app_key, approve_app_key or approve_token,
// webhook_secret (GitHub).
export type InstanceAdminView = {
  name: string;
  platform: Platform;
  source: "config" | "dashboard";
  removing: boolean;
  url: string;
  apiURL: string;
  // webhookURL is where the platform sends this instance's webhooks.
  webhookURL: string;
  reviewAppID: number;
  // approveAppID is the review App's own ID when the review App approves.
  approveAppID: number;
  approver: Approver;
  secrets: Record<string, SecretView>;
};

export type TeamRef = { id: number; name: string };

// UserView is one account in GET /api/users: a local admin ("password") or
// an SSO account. invited is an SSO account an admin added that has not
// signed in yet.
export type UserView = {
  id: number;
  kind: "password" | "sso";
  username: string;
  email: string;
  role: "admin" | "user";
  invited: boolean;
  lastSignInAt: string | null;
  waiting: boolean;
  teams: TeamRef[];
};

// NewAccountView is a local admin just made, with its generated password:
// the only time it is shown.
export type NewAccountView = { username: string; password: string };

// TeamMemberView is a team's member: an SSO account and its role on the team.
export type MemberRole = "viewer" | "maintainer";
export type TeamMemberView = { accountId: number; username: string; email: string; role: MemberRole };

// TeamRepoView is a repository a team covers; project "" covers every
// repository of the instance.
export type TeamRepoView = { instance: string; project: string };

// TeamView is one team. A team has no role of its own: each member has one.
export type TeamView = {
  id: number;
  name: string;
  members: TeamMemberView[];
  repos: TeamRepoView[];
};

// AddUserBody is the body of POST /api/admin/users: an email, and the teams to
// put the account on, each with its role (viewer when left out).
export type AddUserBody = { email: string; teams?: { teamId: number; role?: MemberRole }[] };

// RepoTeamView is a team on a repository's settings page: direct is set when
// it lists the repository itself, wholeInstance when it covers every
// repository of the instance.
export type RepoTeamView = { id: number; name: string; members: number; direct: boolean; wholeInstance: boolean };

// RepoTeamsView is GET and PUT .../teams of a repository (admins).
export type RepoTeamsView = { teams: RepoTeamView[] };

// SignInMode is how people other than local admins sign in: "" (not set up),
// OIDC, or a JWT header a proxy adds.
export type SignInMode = "" | "oidc" | "jwt";

// SignInAdminView is GET /api/admin/sign-in (and the answer of its PUT). The
// client secret is never sent, only whether one is set. source is "config"
// when the config file sets it: then editable is false.
export type SignInAdminView = {
  source: "config" | "dashboard";
  editable: boolean;
  mode: SignInMode;
  label: string;
  issuer: string;
  clientID: string;
  clientSecret: SecretView;
  header: string;
  audience: string;
  jwksURL: string;
  emailClaim: string;
  allowedDomains: string[];
  allowedEmails: string[];
  callbackURL: string;
};

// SignInBody is the body of PUT /api/admin/sign-in. A clientSecret left out
// keeps the stored one; "" removes it.
export type SignInBody = Omit<SignInAdminView, "source" | "editable" | "clientSecret" | "callbackURL"> & {
  clientSecret?: string;
};

// TeamUpdate is the body of PUT /api/teams/{id}: it replaces everything.
export type TeamUpdate = {
  name: string;
  members: { accountId: number; role: MemberRole }[];
  repos: TeamRepoView[];
};

export type AuditView = {
  id: number;
  at: string;
  actor: string;
  action: string;
  instance: string;
  project: string;
  detail: string;
};

// AuditPageView is a page of the audit log; next is the before of the page
// after, 0 at the end. A short page with a next is not the last.
export type AuditPageView = { entries: AuditView[]; next: number };

export type AvailableView = {
  items: { instance: string; project: string; platform: Platform }[];
  truncated: boolean;
};

// ApiError is a non-2xx answer; message is the API's {"error"} text, for
// people, and code its {"code"}, for the client to test.
export class ApiError extends Error {
  readonly status: number;
  readonly code?: string;
  // body is the whole JSON error body, for the answers that carry more than
  // a message (the 409 of a skill delete).
  readonly body?: unknown;
  constructor(status: number, message: string, code?: string, body?: unknown) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.body = body;
  }
}

export const isStatus = (err: unknown, status: number) => err instanceof ApiError && err.status === status;

// isCSRFError is the 403 a write gets when its CSRF token is missing or
// stale (not the 403 of a user who isn't allowed).
export const isCSRFError = (err: unknown) => isStatus(err, 403) && (err as ApiError).code === "csrf";

// --- 401 ---

type Listener = () => void;
const unauthorized = new Set<Listener>();

// onUnauthorized calls fn whenever a call answers 401; it returns the
// function that stops it.
export function onUnauthorized(fn: Listener): () => void {
  unauthorized.add(fn);
  return () => {
    unauthorized.delete(fn);
  };
}

// onGateChange calls fn whenever a call answers the 403 of an account that
// is waiting for a team or must change its password: the account's state
// changed under the page, so /api/me must be read again. It returns the
// function that stops it.
export function onGateChange(fn: Listener): () => void {
  gateChanged.add(fn);
  return () => {
    gateChanged.delete(fn);
  };
}

const gateChanged = new Set<Listener>();
const gateCodes = new Set(["waiting", "password_change_required"]);

// A 401 from these is a wrong password, not an ended session: the caller
// shows it, and the browser doesn't go to sign-in. The exemption covers
// /auth/password/change when the session really ended too (its 401 is the
// same), which the page then can't tell from a wrong password; Sign out is
// on that page and recovers from it.
const wrongPasswordIs401 = new Set(["/auth/password", "/auth/password/change"]);

async function request<T>(method: string, path: string, body?: unknown, csrf?: string, quiet401 = false): Promise<T> {
  const headers: Record<string, string> = { Accept: "application/json" };
  if (method !== "GET") {
    // Every write carries the session's CSRF token.
    headers["X-CSRF-Token"] = csrf ?? "";
  }
  if (body !== undefined) headers["Content-Type"] = "application/json";
  const res = await fetch(path, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
    credentials: "same-origin",
  });
  if (res.status === 401 && !quiet401 && !wrongPasswordIs401.has(path)) {
    for (const fn of [...unauthorized]) fn();
  }
  if (!res.ok) {
    let message = `request failed (${res.status})`;
    let code: string | undefined;
    let body: unknown;
    try {
      const data: unknown = await res.json();
      body = data;
      if (data && typeof data === "object") {
        const { error, code: c } = data as { error?: unknown; code?: unknown };
        if (typeof error === "string") message = error;
        if (typeof c === "string") code = c;
      }
    } catch {
      // Not JSON: keep the generic message.
    }
    if (res.status === 403 && code !== undefined && gateCodes.has(code)) {
      for (const fn of [...gateChanged]) fn();
    }
    throw new ApiError(res.status, message, code, body);
  }
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

export const api = {
  get: <T>(path: string) => request<T>("GET", path),
  // probe is get for a page that expects a 401 and shows it: the 401 does not
  // send the browser to the sign-in page.
  probe: <T>(path: string) => request<T>("GET", path, undefined, undefined, true),
  post: <T>(path: string, body: unknown, csrf: string) => request<T>("POST", path, body, csrf),
  put: <T>(path: string, body: unknown, csrf: string) => request<T>("PUT", path, body, csrf),
  del: <T>(path: string, csrf: string) => request<T>("DELETE", path, undefined, csrf),
  write: <T>(method: WriteMethod, path: string, body: unknown, csrf: string) => request<T>(method, path, body, csrf),
  // signIn signs a password account in: the sign-in form's CSRF token
  // first, then the password. The session is a cookie; nothing is returned.
  signIn: async (username: string, password: string): Promise<void> => {
    const { csrfToken } = await request<{ csrfToken: string }>("GET", "/auth/csrf");
    await request<void>("POST", "/auth/password", { username, password }, csrfToken);
  },
  // changePassword changes the signed-in password account's password; its
  // other sessions end.
  changePassword: (current: string, next: string, csrf: string) =>
    request<void>("POST", "/auth/password/change", { current, new: next }, csrf),
};

export type WriteMethod = "POST" | "PUT" | "DELETE";

// --- paths ---

const seg = encodeURIComponent;
// A project path keeps its slashes; each part is encoded on its own.
const proj = (project: string) => project.split("/").map(seg).join("/");

// query builds "?a=1&b=2" from the set values, in order.
export function query(params: [string, string | number | undefined][]): string {
  const q = new URLSearchParams();
  for (const [k, v] of params) {
    if (v !== undefined && v !== "") q.append(k, String(v));
  }
  const s = q.toString();
  return s ? `?${s}` : "";
}

export const paths = {
  review: (instance: string, project: string, number: number) => `/reviews/${seg(instance)}/${proj(project)}/${seg(String(number))}`,
  repo: (instance: string, project: string) => `/repos/${seg(instance)}/${proj(project)}`,

  apiReview: (instance: string, project: string, number: number) =>
    `/api/reviews/${seg(instance)}/${proj(project)}/${seg(String(number))}`,
  // action is "rerun", "dismiss" or "restore".
  apiReviewAction: (instance: string, project: string, number: number, action: "rerun" | "dismiss" | "restore") =>
    `/api/reviews/${seg(instance)}/${proj(project)}/${seg(String(number))}/${action}`,
  apiRun: (id: string) => `/api/runs/${seg(id)}`,
  apiRunLog: (id: string) => `/api/runs/${seg(id)}/log`,
  apiRepo: (instance: string, project: string) => `/api/repositories/${seg(instance)}/${proj(project)}`,
  apiRepoInputs: (instance: string, project: string) => `/api/repositories/${seg(instance)}/${proj(project)}/inputs`,
  apiRepoPastDismissals: (instance: string, project: string) =>
    `/api/repositories/${seg(instance)}/${proj(project)}/past-dismissals`,
  // days is 30 or 90.
  apiRepoSkillResults: (instance: string, project: string, days: 30 | 90) =>
    `/api/repositories/${seg(instance)}/${proj(project)}/skill-results${days === 30 ? "" : `?days=${days}`}`,
  // Stop using: POST {fingerprint}.
  apiRepoPastDismissalStop: (instance: string, project: string) =>
    `/api/repositories/${seg(instance)}/${proj(project)}/past-dismissals/stop`,
  apiRepoTeams: (instance: string, project: string) => `/api/repositories/${seg(instance)}/${proj(project)}/teams`,
  apiRepoSettings: (instance: string, project: string) => `/api/repositories/${seg(instance)}/${proj(project)}/settings`,
  apiRepoReadiness: (instance: string, project: string) => `/api/repositories/${seg(instance)}/${proj(project)}/readiness`,
  apiRepoReadinessScan: (instance: string, project: string) =>
    `/api/repositories/${seg(instance)}/${proj(project)}/readiness/scan`,
  apiRepoReset: (instance: string, project: string) =>
    `/api/repositories/${seg(instance)}/${proj(project)}/settings/reset`,
  // The client path is under /skills/view/ so a skill named like a page
  // (new, install, updates) still opens.
  skill: (name: string) => `/skills/view/${seg(name)}`,
  apiSkill: (name: string) => `/api/skills/${seg(name)}`,
  apiSkillVersion: (name: string, version: number) => `/api/skills/${seg(name)}/versions/${seg(String(version))}`,
  apiAdminSkill: (name: string) => `/api/admin/skills/${seg(name)}`,
  apiAdminSkillCopy: (name: string) => `/api/admin/skills/${seg(name)}/copy`,
  // The page that checks one installed skill's source for updates.
  skillUpdates: (name: string) => `/skills/updates${query([["skill", name]])}`,
  apiSkillFetches: "/api/admin/skill-fetches",
  apiSkillFetch: (id: string) => `/api/admin/skill-fetches/${seg(id)}`,
  // action is "install" or "update".
  apiSkillFetchAction: (id: string, action: "install" | "update") => `/api/admin/skill-fetches/${seg(id)}/${action}`,
};

// safeHref is u when it is an absolute http or https address, else
// undefined: the only links the pages make to other sites.
export function safeHref(u: string | undefined): string | undefined {
  if (!u) return undefined;
  try {
    const parsed = new URL(u);
    if ((parsed.protocol === "https:" || parsed.protocol === "http:") && parsed.host !== "" && /^https?:\/\//i.test(u)) {
      return u;
    }
  } catch {
    // Not an absolute URL.
  }
  return undefined;
}
