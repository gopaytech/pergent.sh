import type {
  APIClientRow,
  ChainEntryView,
  ChainView,
  APIReviewDetail,
  APIReviewRow,
  APIClientView,
  APIKeyView,
  AuditPageView,
  Can,
  CommentFooterView,
  DefaultsView,
  InstanceAdminView,
  LibrarySkill,
  LinkableView,
  Me,
  ModelAdminView,
  ModelsAdminView,
  MyTeamView,
  OverviewView,
  PastDismissalView,
  RepositoryDetailView,
  RepositoryView,
  RepoTeamsView,
  ReviewDetailView,
  ReviewSummary,
  Role,
  RunDetailView,
  SkillDetail,
  SkillFetch,
  SignInAdminView,
  SkillResultView,
  SkillTokensView,
  TeamModelsView,
  TeamModelView,
  TeamView,
  UsageOverviewView,
  UsageRepositoryView,
  UsageTokensView,
  UserView,
  RuleSet,
  RuleSetDetail,
} from "../api";

// An SSO account an admin made an admin.
export const ssoAdmin: Me = {
  kind: "sso",
  username: "sso-admin",
  email: "sso-admin@example.com",
  role: "admin",
  signIn: "oidc",
  admin: true,
  waiting: false,
  mustChangePassword: false,
  waitingUsers: 0,
  csrfToken: "csrf-123",
};

// The local admin: a password account.
export const operator: Me = {
  ...ssoAdmin,
  kind: "password",
  username: "admin",
  email: "",
  signIn: "password",
};

// A user on a team with the maintainer role, and one with the viewer role. The
// API tells the roles apart per repository; /api/me does not.
const userMe: Me = { ...ssoAdmin, username: "dev-user", email: "dev-user@example.com", role: "user", admin: false };
export const maintainer: Me = { ...userMe, username: "maint-user", email: "maint-user@example.com", csrfToken: "csrf-maint" };
export const viewer: Me = { ...userMe, csrfToken: "csrf-dev" };

// On no team yet: sees nothing.
export const waiting: Me = { ...viewer, username: "new-user", email: "new-user@example.com", waiting: true };

// A password account that must change its password first.
export const mustChange: Me = { ...operator, mustChangePassword: true };

const minutesAgo = (n: number) => new Date(Date.now() - n * 60_000).toISOString();

export const reviewApproved: ReviewSummary = {
  instance: "main",
  project: "acme/widgets",
  platform: "gitlab",
  number: 128,
  title: "Add retry to webhook sender",
  outcome: "approved",
  reason: "",
  updatedAt: minutesAgo(4),
  webURL: "https://gitlab.example.com/acme/widgets/-/merge_requests/128",
  attempts: 1,
};

export const reviewBlocking: ReviewSummary = {
  instance: "main",
  project: "acme/payments",
  platform: "gitlab",
  number: 77,
  title: "Refund idempotency key",
  outcome: "blocking",
  reason: "security-review found blocking issues.",
  updatedAt: minutesAgo(12),
  webURL: "https://gitlab.example.com/acme/payments/-/merge_requests/77",
  attempts: 1,
};

export const overview: OverviewView = {
  stats: { reviews: 184, approved: 121, blocking: 37, failed: 4 },
  recent: [reviewApproved, reviewBlocking],
  attention: [
    {
      instance: "main",
      project: "acme/payments",
      kind: "single_approval",
      message: "One approval is enough to merge here, so pergent's approval alone can unlock it.",
      updatedAt: minutesAgo(60),
    },
    {
      instance: "main",
      project: "",
      kind: "token_expiry",
      message: "The approve bot's token expires on 2026-11-09.",
      updatedAt: minutesAgo(60),
    },
  ],
  setup: null,
};

export const noCan: Can = { rerun: false, dismissBlocking: false, dismissSuggestion: false, restore: false };
export const allCan: Can = { rerun: true, dismissBlocking: true, dismissSuggestion: true, restore: true };

export const reviewDetail: ReviewDetailView = {
  role: "viewer",
  can: noCan,
  header: {
    instance: "main",
    project: "acme/payments",
    platform: "gitlab",
    number: 77,
    title: "Refund idempotency key",
    sourceBranch: "feature/refund-key",
    targetBranch: "main",
    author: "dev-user",
    webURL: "https://gitlab.example.com/acme/payments/-/merge_requests/77",
  },
  runs: [
    {
      id: "11111111-1111-1111-1111-111111111111",
      head: "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678",
      trigger: "push",
      outcome: "blocking",
      reason: "security-review found blocking issues.",
      createdAt: minutesAgo(12),
      durationSeconds: 221,
    },
    {
      id: "22222222-2222-2222-2222-222222222222",
      head: "5c4b3a2d1e0f5c4b3a2d1e0f5c4b3a2d1e0f5c4b",
      trigger: "first",
      outcome: "failed",
      reason: "The review Job ran out of memory.",
      createdAt: minutesAgo(60),
      durationSeconds: null,
    },
  ],
};

export const runDetail: RunDetailView = {
  run: reviewDetail.runs[0],
  instance: "main",
  project: "acme/payments",
  number: 77,
  status: "failed",
  approved: false,
  approvalReason: "Blocking issues were found.",
  latestDecided: true,
  pergentVersion: "v0.1.0",
  model: "claude-sonnet",
  provider: "anthropic",
  maxTurns: 40,
  incrementalNote: "",
  quiet: false,
  ciJobs: null,
  ciDeadline: null,
  tokens: { in: 48200, out: 6100 },
  files: {
    reviewed: ["internal/refund/handler.go", "internal/refund/key.go"],
    skipped: [{ path: "go.sum", rule: "lockfile" }],
    notReviewed: [{ path: "internal/ledger/big.go", cause: "context overflow" }],
    rules: [{ path: "internal/refund/handler.go", rule: "repo:internal/**/*.go" }],
  },
  rulesUsed: true,
  skills: [
    {
      name: "security-review",
      verdict: "has_blocking_issues",
      source: "library",
      version: 3,
      findings: [
        {
          id: "a1b2c3",
          fingerprint: "fp-refund-twice",
          severity: "blocking",
          title: "Refund can be applied twice on retry",
          file: "internal/refund/handler.go",
          line: 88,
          explanation: "The idempotency key is checked after the ledger write.\nA retry writes a second row.",
          suggestedFix: "if seen(key) {\n\treturn nil\n}",
          dismissed: null,
        },
      ],
      dismissed: [],
      checks: [{ batch: 1, section: "Money", name: "Idempotent writes", status: "fail", detail: "Checked after the write." }],
    },
    {
      name: "code-review",
      verdict: "no_blocking_issues",
      source: null,
      version: 0,
      findings: [
        {
          id: "f00d12",
          fingerprint: "fp-magic-window",
          severity: "suggestion",
          title: "Name the magic retry window",
          file: "internal/refund/handler.go",
          explanation: "",
          suggestedFix: "",
          dismissed: null,
        },
      ],
      dismissed: [],
      checks: [],
    },
  ],
  entry: null,
  attempts: [],
};

// What an admin's run detail carries when the run has a Job. The key is left
// out for everyone else and for a run with no Job.
export const runKubernetes: NonNullable<RunDetailView["kubernetes"]> = {
  namespace: "pergent-jobs",
  job: "pergent-3f9a1c2b7d4e",
  pod: "pergent-3f9a1c2b7d4e-x7k2p",
  detail: "",
};

export const repoPayments: RepositoryView = {
  instance: "main",
  project: "acme/payments",
  platform: "gitlab",
  source: "dashboard",
  on: true,
  paused: false,
  last7: { reviews: 40, approved: 31, failed: 2 },
  health: "attention",
};

export const repoWidgets: RepositoryView = {
  instance: "main",
  project: "acme/widgets",
  platform: "gitlab",
  source: "config",
  on: true,
  paused: false,
  last7: { reviews: 12, approved: 10, failed: 0 },
  health: "ok",
};

// A dashboard repository that is paused.
export const repoToolsPaused: RepositoryView = {
  instance: "main",
  project: "acme/tools",
  platform: "gitlab",
  source: "dashboard",
  on: false,
  paused: true,
  last7: { reviews: 0, approved: 0, failed: 0 },
  health: "paused",
};

// A GitHub repository switched on in the dashboard: the one that links.
export const repoGadgets: RepositoryView = {
  instance: "hub",
  project: "acme/gadgets",
  platform: "github",
  source: "dashboard",
  on: true,
  paused: false,
  last7: { reviews: 5, approved: 4, failed: 0 },
  health: "ok",
};

// What GET /api/admin/linkable says for acme/gadgets: one that is linkable,
// one that is not read yet, and two the rules refuse, each with its reason.
export const linkable: LinkableView = {
  items: [
    { repo: "acme/api", visibility: "public", checked: true, linkable: true, reason: "" },
    { repo: "acme/lib", visibility: "internal", checked: true, linkable: true, reason: "" },
    { repo: "acme/slow", visibility: "", checked: false, linkable: true, reason: "not checked yet; it is checked when you save" },
    {
      repo: "acme/other-app",
      visibility: "public",
      checked: true,
      linkable: false,
      reason: "it is in another installation of the review App, and a review's token covers one installation",
    },
    { repo: "acme/ghost", visibility: "", checked: true, linkable: false, reason: "the pergent review App can't read it" },
  ],
  truncated: false,
};

// The layout the server sends while a repository sets none.
export const builtInLayout = ["summary:open", "findings:collapsed", "dismissed:collapsed", "checks:open", "files:collapsed"];

export const repoDetail = (repo: RepositoryView, role: Role = "admin"): RepositoryDetailView => ({
  repo,
  role,
  settings: {
    approve: { value: true, overridden: true },
    reviewDrafts: { value: false, overridden: false },
    inlineFindings: { value: true, overridden: false },
    commandsAuthorAndMaintainersOnly: { value: true, overridden: false },
    incrementalReviews: { value: true, overridden: false },
    learnFromDismissals: { value: true, overridden: false },
    quietReviews: { value: false, overridden: false },
    checkDeleted: { value: false, overridden: false },
    ciJobs: { value: { wait: "30m", jobs: [] }, overridden: false },
    linkedReposEnabled: { value: false, overridden: false },
    linkedRepos: { value: [], overridden: false },
    commentLayout: { value: builtInLayout, overridden: false },
    failureStatus: { value: "failed", overridden: false },
    skills: { value: null, overridden: false },
    repoSkills: { value: [], overridden: false },
    jobTimeoutSeconds: { value: 600, overridden: false },
    maxTurns: { value: 40, overridden: false },
    jobMemory: { value: "6Gi", overridden: true },
    rerunLimitPerMR: { value: 10, overridden: false },
    rerunLimitPerRepo: { value: 50, overridden: false },
    trustedAuthorsOnly: { value: "follow_visibility", overridden: false },
    trustedAuthors: { value: [], overridden: false },
    // Only an admin is sent the quota.
    ...(role === "admin" ? { monthlyTokenQuota: { value: null, overridden: false } } : {}),
    ruleSets: { value: null, overridden: false },
    builtInRules: { value: true, overridden: false },
  },
  attention: repo.health === "attention" ? [overview.attention[0]] : [],
  waiting: [],
  instanceTrustedAuthorsOnly: "follow_visibility",
  model: "anthropic / claude-sonnet",
  panel: { on: false, mode: "inherit" },
  // Only an admin is given the usage against the quotas.
  quota: null,
  graph: {
    enabled: false,
    available: true,
    branch: "",
    commit: "",
    builtAt: null,
    sizeBytes: 0,
    nodes: 0,
    edges: 0,
    building: "",
    error: "",
    failedAt: null,
  },
});

// A skill written in the dashboard, on the default list.
export const skillWritten: LibrarySkill = {
  name: "code-review",
  version: 2,
  size: 52,
  updatedAt: "2026-11-01T08:00:00Z",
  inDefaults: true,
  source: null,
  updatedBy: "admin",
};

// A skill installed from a repository the viewer can see.
export const skillInstalled: LibrarySkill = {
  name: "security-review",
  version: 1,
  size: 61,
  updatedAt: "2026-10-30T08:00:00Z",
  inDefaults: false,
  source: { kind: "instance", instance: "main", repo: "acme/skills", path: "skills/security-review.md", ref: "v1.0", commit: "abc1234def5678" },
  updatedBy: "admin",
};

export const librarySkills: LibrarySkill[] = [skillWritten, skillInstalled];

export const skillWrittenDetail: SkillDetail = {
  ...skillWritten,
  content: "---\nname: code-review\n---\nReview the code.\n",
  versions: [
    { version: 2, at: "2026-11-01T08:00:00Z", actor: "admin", commit: "" },
    { version: 1, at: "2026-10-31T08:00:00Z", actor: "admin", commit: "" },
  ],
};

export const skillInstalledDetail: SkillDetail = {
  ...skillInstalled,
  content: "---\nname: security-review\n---\nLook for security bugs.\n",
  versions: [{ version: 1, at: "2026-10-30T08:00:00Z", actor: "admin", commit: "abc1234def5678" }],
};

// A read of acme/skills at v1.0, done: one skill to install, one whose name
// is taken, one that can't be installed.
export const fetchInstallDone: SkillFetch = {
  id: "0b2f6c1e-9f55-4c1e-8a57-3d1d5b8c7a10",
  status: "done",
  commit: "1234567890abcdef",
  error: "",
  purpose: "install",
  instance: "main",
  repo: "acme/skills",
  ref: "v1.0",
  path: "skills",
  skills: [
    { name: "style", path: "skills/style.md", size: 2048, taken: false },
    { name: "code-review", path: "skills/code-review.md", size: 52, taken: true },
    { name: "huge", path: "skills/huge.md", size: 0, problem: "larger than 128 KiB", taken: false },
  ],
  updates: [],
};

// A check for updates of security-review (installed from acme/skills at v1.0,
// file skills/security-review.md) against v1.1: one changed, one same, one
// removed at the source, one renamed at the source.
export const fetchUpdateDone: SkillFetch = {
  ...fetchInstallDone,
  purpose: "update",
  ref: "v1.1",
  fromRef: "v1.0",
  skills: [],
  updates: [
    {
      name: "security-review",
      path: "skills/security-review.md",
      state: "changed",
      version: 1,
      current: "Look for bugs.\nCheck inputs.\n",
      fetched: "Look for bugs.\nCheck all inputs.\nCheck secrets.\n",
    },
    { name: "style", path: "skills/style.md", state: "same", version: 3, current: "Be kind.\n", fetched: "Be kind.\n" },
    { name: "old", path: "skills/old.md", state: "removed at the source", version: 1, current: "Old text.\n", fetched: "" },
    {
      name: "moved",
      path: "skills/moved.md",
      state: "renamed at the source",
      version: 2,
      current: "Moved text.\n",
      fetched: "Moved text, new name.\n",
    },
  ],
};

// The sign-in settings: nothing set up, OIDC against Google, a JWT header, and
// OIDC set in the config file.
export const signInNone: SignInAdminView = {
  source: "dashboard",
  editable: true,
  mode: "",
  label: "",
  issuer: "",
  clientID: "",
  clientSecret: { set: false, updatedAt: null },
  header: "",
  audience: "",
  jwksURL: "",
  emailClaim: "",
  allowedDomains: [],
  allowedEmails: [],
  callbackURL: "https://dash.example.com/auth/oidc/callback",
};

export const signInOIDC: SignInAdminView = {
  ...signInNone,
  mode: "oidc",
  label: "Google",
  issuer: "https://accounts.google.com",
  clientID: "123-abc.apps.googleusercontent.com",
  clientSecret: { set: true, updatedAt: minutesAgo(60) },
  allowedDomains: ["example.com"],
  allowedEmails: ["friend@example.org"],
};

export const signInJWT: SignInAdminView = {
  ...signInNone,
  mode: "jwt",
  issuer: "https://proxy.example.com",
  header: "X-Auth-Token",
  audience: "pergent",
  jwksURL: "https://proxy.example.com/keys",
  emailClaim: "email",
  allowedDomains: ["example.com"],
};

export const signInFromConfig: SignInAdminView = {
  ...signInOIDC,
  source: "config",
  editable: false,
  clientSecret: { set: true, updatedAt: null },
};

// The teams of acme/payments: one lists it, one covers the whole instance, one
// has nothing to do with it.
export const repoTeams: RepoTeamsView = {
  teams: [
    { id: 2, name: "Payments", members: 1, direct: true, wholeInstance: false },
    { id: 3, name: "Platform", members: 0, direct: false, wholeInstance: false },
    { id: 4, name: "Everyone on main", members: 6, direct: false, wholeInstance: true },
  ],
};

export const userInvited: UserView = {
  id: 9,
  kind: "sso",
  username: "invited@example.com",
  email: "invited@example.com",
  role: "user",
  invited: true,
  lastSignInAt: null,
  waiting: false,
  teams: [{ id: 2, name: "Payments" }],
};

export const commentFooter: CommentFooterView = {
  note: "",
  default: "Reviewed by [pergent](https://github.com/gopaytech/pergent.sh)",
};

export const defaults: DefaultsView = {
  approve: true,
  reviewDrafts: true,
  inlineFindings: true,
  commandsAuthorAndMaintainersOnly: true,
  incrementalReviews: true,
  learnFromDismissals: true,
  quietReviews: false,
  linkedReposEnabled: false,
  commentLayout: builtInLayout,
  failureStatus: "failed",
  jobTimeoutSeconds: 600,
  maxTurns: 40,
  jobMemory: "",
  rerunLimitPerMR: 10,
  rerunLimitPerRepo: 50,
  skills: ["code-review"],
  ruleSets: [],
  builtInRules: true,
  // The config gives what is in effect, and nothing is saved here.
  config: {
    approve: true,
    reviewDrafts: true,
    inlineFindings: true,
    commandsAuthorAndMaintainersOnly: true,
    incrementalReviews: true,
    learnFromDismissals: true,
    quietReviews: false,
    linkedReposEnabled: false,
    commentLayout: builtInLayout,
    failureStatus: "failed",
    jobTimeoutSeconds: 600,
    maxTurns: 40,
    jobMemory: "",
    rerunLimitPerMR: 10,
    rerunLimitPerRepo: 50,
    builtInRules: true,
  },
  pinned: {},
};

const modelKey = { set: true, updatedAt: minutesAgo(3 * 24 * 60) };

// The default of the list, and one a repository names.
export const modelSonnet: ModelAdminView = {
  id: "m-sonnet",
  name: "Claude Sonnet",
  provider: "anthropic",
  model: "claude-sonnet",
  baseURL: "",
  source: "dashboard",
  default: true,
  teamsMayUse: false,
  key: modelKey,
  repositories: [],
  usedIn: ["global chain"],
  mark: null,
};
export const modelOpus: ModelAdminView = {
  id: "m-opus",
  name: "Claude Opus",
  provider: "anthropic",
  model: "claude-opus",
  baseURL: "https://llm.example.com/v1",
  source: "dashboard",
  default: false,
  teamsMayUse: false,
  key: modelKey,
  repositories: [{ instance: "main", project: "acme/payments" }],
  usedIn: ["main/acme/payments"],
  mark: null,
};

// The list of models: three repositories follow the default.
export const models: ModelsAdminView = { canSetDefault: true, followingDefault: 3, models: [modelSonnet, modelOpus] };

export const instanceGitLab: InstanceAdminView = {
  name: "main",
  trustedAuthorsOnly: "follow_visibility",
  monthlyTokenQuota: null,
  tokensThisMonth: 0,
  platform: "gitlab",
  source: "config",
  removing: false,
  url: "https://gitlab.example.com",
  apiURL: "",
  webhookURL: "https://pergent.example.com/webhooks/gitlab/main",
  reviewAppID: 0,
  approveAppID: 0,
  approver: "token",
  secrets: {
    review_token: { set: true, updatedAt: null },
    approve_token: { set: true, updatedAt: null },
    webhook_secret: { set: true, updatedAt: null },
  },
};

export const instanceGitHub: InstanceAdminView = {
  name: "hub",
  trustedAuthorsOnly: "follow_visibility",
  monthlyTokenQuota: null,
  tokensThisMonth: 0,
  platform: "github",
  source: "dashboard",
  removing: false,
  url: "",
  apiURL: "https://api.github.example.com",
  webhookURL: "https://pergent.example.com/webhooks/github/hub",
  reviewAppID: 11,
  approveAppID: 12,
  approver: "app",
  secrets: {
    review_app_key: { set: true, updatedAt: minutesAgo(120) },
    approve_app_key: { set: false, updatedAt: null },
    webhook_secret: { set: true, updatedAt: minutesAgo(120) },
  },
};

// A GitHub instance whose review App also approves: no approve secret.
export const instanceGitHubOneApp: InstanceAdminView = {
  ...instanceGitHub,
  name: "solo",
  webhookURL: "https://pergent.example.com/webhooks/github/solo",
  approveAppID: 11,
  approver: "review_app",
  secrets: {
    review_app_key: { set: true, updatedAt: minutesAgo(120) },
    webhook_secret: { set: true, updatedAt: minutesAgo(120) },
  },
};

export const instanceDashboardGitLab: InstanceAdminView = {
  ...instanceGitLab,
  name: "extra",
  webhookURL: "https://pergent.example.com/webhooks/gitlab/extra",
  source: "dashboard",
  url: "https://gitlab2.example.com",
  secrets: {
    review_token: { set: true, updatedAt: minutesAgo(120) },
    approve_token: { set: true, updatedAt: minutesAgo(120) },
    webhook_secret: { set: true, updatedAt: minutesAgo(120) },
  },
};

export const userWaiting: UserView = {
  id: 7,
  kind: "sso",
  username: "newcomer@example.com",
  email: "newcomer@example.com",
  role: "user",
  invited: false,
  lastSignInAt: minutesAgo(5),
  waiting: true,
  teams: [],
};

export const userDev: UserView = {
  id: 5,
  kind: "sso",
  username: "dev-user@example.com",
  email: "dev-user@example.com",
  role: "user",
  invited: false,
  lastSignInAt: minutesAgo(180),
  waiting: false,
  teams: [{ id: 2, name: "Payments" }],
};

export const userOperator: UserView = {
  id: 1,
  kind: "password",
  username: "admin",
  email: "",
  role: "admin",
  invited: false,
  lastSignInAt: null,
  waiting: false,
  teams: [],
};

export const teamPayments: TeamView = {
  id: 2,
  name: "Payments",
  members: [{ accountId: 5, username: "dev-user@example.com", email: "dev-user@example.com", role: "maintainer" }],
  repos: [{ instance: "main", project: "acme/payments", owner: true }],
};

export const teamEmpty: TeamView = {
  id: 3,
  name: "Platform",
  members: [],
  repos: [],
};

export const auditPage1: AuditPageView = {
  entries: [
    { id: 30, at: "2026-10-30T14:02:00Z", actor: "admin (password)", action: "team_created", instance: "", project: "", detail: "Payments (maintainer)" },
    { id: 29, at: "2026-10-30T13:00:00Z", actor: "admin (password)", action: "repository_on", instance: "main", project: "acme/payments", detail: "" },
  ],
  next: 29,
};

export const auditPage2: AuditPageView = {
  entries: [{ id: 28, at: "2026-10-29T09:30:00Z", actor: "dev-admin (gitlab main)", action: "account_deleted", instance: "", project: "", detail: "old-user" }],
  next: 0,
};

export const skillResults: SkillResultView[] = [
  { skill: "security", severity: "blocking", raised: 12, fixed: 3, dismissed: 7, open: 2, wentAway: 0, addressed: 25, editedAt: null },
  { skill: "code-review", severity: "blocking", raised: 20, fixed: 15, dismissed: 3, open: 1, wentAway: 1, addressed: 75, editedAt: null },
  { skill: "style", severity: "suggestion", raised: 4, fixed: 1, dismissed: 2, open: 1, wentAway: 0, addressed: null, editedAt: null },
];

export const pastDismissals: PastDismissalView[] = [
  {
    skill: "code-review",
    severity: "blocking",
    file: "api/handler.go",
    title: "Request body is used before validation",
    reason: "The body is a constant here",
    by: "alice (gitlab main)",
    at: "2026-10-30T09:00:00Z",
    number: 7,
    fingerprint: "a1b2c3d4e5f6",
  },
  {
    skill: "style",
    severity: "suggestion",
    file: "api/names.go",
    title: "Name it better",
    reason: "We keep this name for the old clients",
    by: "bob (gitlab main)",
    at: "2026-10-29T09:00:00Z",
    number: 9,
    fingerprint: "b2c3d4e5f6a1",
  },
];

// Tokens with the apart counts reported, and with none of them reported.
const tok = (inn: number, out: number, apart = { cacheRead: 0, cacheWrite: 0, reasoning: 0, unknown: 0 }): UsageTokensView => ({ in: inn, out, ...apart });

// The usage overview: the repository with the most tokens first, one switched
// off since (its usage is kept, it has no detail page).
export const usageOverview: UsageOverviewView = {
  days: 30,
  // The repositories (1,200k in, 300k out) plus the clients (55k in, 11k out); the reviews are 40 + 8.
  tokens: tok(1_255_000, 311_000, { cacheRead: 500_000, cacheWrite: 20_000, reasoning: 0, unknown: 0 }),
  reviews: 48,
  daily: [
    { day: "2026-11-05", tokens: tok(200_000, 50_000) },
    { day: "2026-11-06", tokens: tok(600_000, 150_000) },
    { day: "2026-11-07", tokens: tok(455_000, 111_000) },
  ],
  repositories: [
    { instance: "main", project: "acme/payments", switchedOn: true, paused: false, tokens: tok(900_000, 200_000), reviews: 30, globalTokens: 800_000, teamTokens: 300_000 },
    { instance: "main", project: "acme/widgets", switchedOn: true, paused: false, tokens: tok(200_000, 80_000), reviews: 8, globalTokens: 280_000, teamTokens: 0 },
    { instance: "main", project: "acme/old", switchedOn: false, paused: false, tokens: tok(100_000, 20_000), reviews: 2, globalTokens: 120_000, teamTokens: 0 },
  ],
  clients: [
    {
      name: "fastgateway",
      tokens: tok(50_000, 10_000),
      reviews: 6,
      tenants: [
        { tenant: "acme", tokens: tok(40_000, 8_000), reviews: 4 },
        { tenant: "<img src=x onerror=alert(1)>", tokens: tok(10_000, 2_000), reviews: 1 },
        { tenant: "", tokens: tok(0, 0), reviews: 1 },
      ],
      moreTenants: 0,
    },
    { name: "docs-bot", tokens: tok(5_000, 1_000), reviews: 2, tenants: [{ tenant: "beta", tokens: tok(5_000, 1_000), reviews: 2 }], moreTenants: 7 },
  ],
};

// One repository's usage: the apart counts of the first skill are unknown for
// three reviews, a median is missing for incremental reviews.
export const usageRepository: UsageRepositoryView = {
  days: 30,
  daily: [
    { day: "2026-11-06", tokens: tok(600_000, 150_000) },
    { day: "2026-11-07", tokens: tok(300_000, 50_000) },
  ],
  skills: [
    { provider: "", name: "security", tokens: tok(500_000, 100_000, { cacheRead: 0, cacheWrite: 0, reasoning: 0, unknown: 3 }), runs: 12, failed: 1 },
    { provider: "", name: "style", tokens: tok(400_000, 100_000, { cacheRead: 7_000, cacheWrite: 1_000, reasoning: 2_000, unknown: 0 }), runs: 9, failed: 0 },
  ],
  models: [{ provider: "anthropic", name: "claude-sonnet", tokens: tok(900_000, 200_000), runs: 21, failed: 0 }],
  mergeRequests: [
    { iid: 77, title: "Fix the ledger", webURL: "https://gitlab.example.com/acme/payments/-/merge_requests/77", tokens: tok(600_000, 100_000), reviews: 4, models: [{ provider: "anthropic", model: "claude-sonnet" }] },
    { iid: 78, title: "<img src=x onerror=alert(1)>", webURL: "https://gitlab.example.com/acme/payments/-/merge_requests/78", tokens: tok(300_000, 100_000), reviews: 2, models: [] },
  ],
  wasted: { failed: tok(40_000, 5_000), reruns: tok(90_000, 10_000), rerunCount: 2, noResult: 1, superseded: 3 },
  fullMedian: 120_000,
  incrementalMedian: null,
  outliers: [{ runID: "run-9", iid: 77, incremental: false, total: 450_000, at: "2026-11-06T10:15:00Z" }],
  globalTokens: 800_000,
  teamTokens: 300_000,
};

// What the Skill results tab reads for admins: tokens per skill on the same changes.
export const skillTokens: SkillTokensView[] = [
  { skill: "security", tokens: { in: 40_000, out: 8_000 } },
  { skill: "code-review", tokens: { in: 150_000, out: 30_000 } },
  { skill: "style", tokens: { in: 5_000, out: 1_000 } },
  { skill: "docs", tokens: { in: 2_000, out: 400 } },
];

// --- API clients ---

export const apiKeyUsed: APIKeyView = {
  id: 7,
  name: "ci",
  prefix: "pgk_ab12cd34",
  createdAt: "2026-11-01T08:00:00Z",
  expiresAt: "2027-01-01T00:00:00Z",
  lastUsedAt: "2026-11-10T09:30:00Z",
};

// A key that never expires and was never used.
export const apiKeyFresh: APIKeyView = {
  id: 8,
  name: "staging",
  prefix: "pgk_ef56gh78",
  createdAt: "2026-11-09T08:00:00Z",
  expiresAt: null,
  lastUsedAt: null,
};

export const apiClientFastgateway: APIClientView = {
  name: "fastgateway",
  skills: ["code-review"],
  modelId: "",
  callbackUrl: "https://fastgateway.example.com/hooks/pergent",
  limits: { maxRequestBytes: 524288, maxFiles: 200, maxInFlight: 5, requestsPerHour: 600, monthlyTokenBudget: 10_000_000 },
  keys: [apiKeyFresh, apiKeyUsed],
  createdAt: "2026-10-30T08:00:00Z",
};

export const apiClientRows: APIClientRow[] = [
  { name: "fastgateway", keys: 2, reviewsThisMonth: 42, failedThisMonth: 3, tokensThisMonth: 1_250_000 },
  { name: "docs-bot", keys: 0, reviewsThisMonth: 0, failedThisMonth: 0, tokensThisMonth: 0 },
];

// --- API reviews ---

export const apiReviewRowDone: APIReviewRow = {
  id: "0f1e2d3c4b5a",
  tenant: "payments",
  subject: "change-1042",
  title: "Add the retry loop",
  status: "completed",
  verdict: "has_blocking_issues",
  createdAt: "2026-11-10T09:00:00Z",
  finishedAt: "2026-11-10T09:04:00Z",
};

// A review that has not started: no verdict, not finished.
export const apiReviewRowQueued: APIReviewRow = {
  id: "9a8b7c6d5e4f",
  tenant: "",
  subject: "change-1043",
  title: "Rename the ledger",
  status: "queued",
  verdict: "",
  createdAt: "2026-11-10T10:00:00Z",
  finishedAt: null,
};

export const apiReviewRows: APIReviewRow[] = [apiReviewRowQueued, apiReviewRowDone];

// A completed review: one skill with a blocking finding (every text field
// set, one of them HTML), a suggestion and a dismissed finding, and files
// reviewed, skipped and not reviewed.
export const apiReviewDone: APIReviewDetail = {
  id: "0f1e2d3c4b5a",
  client: "fastgateway",
  tenant: "payments",
  subject: "change-1042",
  title: "Add the retry loop",
  keyName: "ci",
  status: "completed",
  model: "claude-sonnet",
  createdAt: "2026-11-10T09:00:00Z",
  finishedAt: "2026-11-10T09:04:00Z",
  failure: null,
  callback: { state: "none", attempts: [] },
  history: [],
  dismissals: [],
  review: {
    id: "0f1e2d3c4b5a",
    subject: "change-1042",
    tenant: "payments",
    title: "Add the retry loop",
    status: "completed",
    created_at: "2026-11-10T09:00:00Z",
    finished_at: "2026-11-10T09:04:00Z",
    verdict: "has_blocking_issues",
    usage: { tokens_in: 12_000, tokens_out: 3_400 },
    skills: [
      {
        name: "code-review",
        verdict: "has_blocking_issues",
        findings: [
          {
            id: "f1",
            fingerprint: "aa11",
            file: "internal/ledger/retry.go",
            line: 42,
            end_line: 45,
            severity: "blocking",
            title: "The loop never stops",
            explanation: "It retries on every error, <b>forever</b>.",
            fix: "Stop after 3 tries.",
            suggestion: "for i := 0; i < 3; i++ {",
          },
          {
            id: "f2",
            fingerprint: "bb22",
            file: "internal/ledger/names.go",
            severity: "suggestion",
            title: "Name it better",
            explanation: "The name hides what it does.",
          },
          {
            id: "f3",
            fingerprint: "cc33",
            file: "internal/ledger/old.go",
            line: 7,
            severity: "suggestion",
            title: "Old style",
            explanation: "Use the newer call.",
            dismissed: { reason: "we keep it on purpose", at: "2026-11-09T12:00:00Z" },
          },
        ],
        files: {
          reviewed: ["internal/ledger/retry.go", "internal/ledger/names.go"],
          skipped: [{ path: "go.sum", rule: "lockfile" }],
          not_reviewed: [{ path: "internal/ledger/big.go", reason: "context overflow" }],
        },
      },
    ],
  },
};

// A review that failed while its Job ran: the caller's reason and an admin's
// detail.
export const apiReviewFailed: APIReviewDetail = {
  id: "5e4d3c2b1a09",
  client: "fastgateway",
  tenant: "",
  subject: "change-1044",
  title: "",
  keyName: "ci",
  status: "failed",
  model: null,
  createdAt: "2026-11-10T11:00:00Z",
  finishedAt: "2026-11-10T11:01:00Z",
  failure: { step: "starting", reason: "the checkout failed", detail: "fatal: bad object <b>abc</b>" },
  callback: { state: "none", attempts: [] },
  history: [],
  dismissals: [],
  review: {
    id: "5e4d3c2b1a09",
    subject: "change-1044",
    tenant: "",
    title: "",
    status: "failed",
    created_at: "2026-11-10T11:00:00Z",
    finished_at: "2026-11-10T11:01:00Z",
    verdict: "failed",
    failure: { step: "starting", reason: "the checkout failed" },
  },
};

// Chain entries, as GET .../chain shows them: the config file's entry (fixed
// at the front of the global chain), two global entries, and a team's own.
export const entryConfig: ChainEntryView = {
  position: 1,
  source: "global",
  id: "config",
  name: "openai/gpt-5",
  provider: "openai",
  model: "gpt-5",
  team: "",
  state: "ready",
  class: "",
  until: null,
  canClear: false,
  fixed: true,
};
export const entrySonnet: ChainEntryView = {
  ...entryConfig,
  position: 2,
  id: "m-sonnet",
  name: "Claude Sonnet",
  provider: "anthropic",
  model: "claude-sonnet",
  fixed: false,
};
export const entryOpus: ChainEntryView = { ...entrySonnet, position: 3, id: "m-opus", name: "Claude Opus", model: "claude-opus" };
export const entryTeamQwen: ChainEntryView = {
  ...entrySonnet,
  position: 0,
  source: "team",
  id: "t-qwen",
  name: "qwen3.5",
  provider: "openrouter",
  model: "qwen/qwen3.5",
  team: "Payments",
};

// choice is an entry as the picker lists it: no place in the chain.
export const choice = (e: ChainEntryView): ChainEntryView => ({ ...e, position: 0 });

// The global chain an admin sees: the config entry in front, then Sonnet.
export const chainGlobal: ChainView = {
  scope: "global",
  source: "global",
  team: "",
  readOnly: false,
  savedByAdmin: true,
  entries: [entryConfig, entrySonnet],
  choices: [choice(entrySonnet), choice(entryOpus)],
};

// The teams of GET /api/me/teams for a maintainer of Payments.
export const myTeams: MyTeamView[] = [
  { id: 2, name: "Payments", role: "maintainer", repos: [{ instance: "main", project: "acme/payments", owner: true }] },
  { id: 3, name: "Platform", role: "viewer", repos: [{ instance: "main", project: "", owner: false }] },
];

// Payments' own entry, as a maintainer sees it (a viewer gets no base URL).
export const teamModelQwen: TeamModelView = {
  id: "t-qwen",
  name: "qwen3.5",
  provider: "openrouter",
  model: "qwen/qwen3.5",
  baseURL: "https://router.example.com/v1",
  key: { set: true, updatedAt: minutesAgo(60) },
  usedIn: ["team Payments", "main/acme/payments"],
  mark: null,
};

export const teamModelsPayments: TeamModelsView = {
  team: { id: 2, name: "Payments" },
  role: "maintainer",
  canEdit: true,
  models: [teamModelQwen],
};

// Payments' own chain: its entry, then a global entry teams may use.
export const chainTeam: ChainView = {
  scope: "team",
  source: "team",
  team: "Payments",
  readOnly: false,
  savedByAdmin: false,
  entries: [
    { ...entryTeamQwen, position: 1 },
    { ...entrySonnet, position: 2 },
  ],
  choices: [choice(entryTeamQwen), choice(entrySonnet)],
};

// acme/payments' chain in effect while it inherits its owner team's, as an
// admin of it sees it (every global entry and the team's are choices).
export const chainRepoInherit: ChainView = {
  ...chainTeam,
  scope: "repo",
  source: "team",
  choices: [choice(entryTeamQwen), choice(entrySonnet), choice(entryOpus)],
};

// acme/payments' own chain.
export const chainRepoCustom: ChainView = {
  ...chainRepoInherit,
  source: "repository",
  team: "",
  savedByAdmin: true,
  entries: [{ ...entryOpus, position: 1 }],
};

// The rule library: a set written in the dashboard and one of the config file.
export const ruleSets: RuleSet[] = [
  { name: "go-api", version: 2, updatedAt: minutesAgo(60), entries: 2, source: "dashboard", inDefaults: true },
  { name: "team", version: 0, updatedAt: "0001-01-01T00:00:00Z", entries: 1, source: "config", inDefaults: false },
];

export const ruleSetGoAPI: RuleSetDetail = {
  ...ruleSets[0],
  content: 'rules:\n  - path: "internal/api/**/*.go"\n    rule: Every handler must defer tx.Rollback().\n  - path: "**/*.pb.go"\n    off: true\n',
  notes: [],
};

// Chain B of acme/payments while nothing is set anywhere: no entries.
export const panelRepoInherit: ChainView = {
  ...chainRepoInherit,
  source: "global",
  team: "",
  mode: "inherit",
  entries: [],
};

// Its own chain B: Opus.
export const panelRepoCustom: ChainView = {
  ...chainRepoCustom,
  mode: "custom",
};

// The panel switched off for the repository.
export const panelRepoOff: ChainView = {
  ...chainRepoInherit,
  source: "repository",
  team: "",
  mode: "off",
  entries: [],
};
