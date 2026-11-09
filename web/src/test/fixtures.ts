import type {
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
  TeamView,
  UsageOverviewView,
  UsageRepositoryView,
  UsageTokensView,
  UserView,
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
  tokens: { in: 48200, out: 6100 },
  files: {
    reviewed: ["internal/refund/handler.go", "internal/refund/key.go"],
    skipped: [{ path: "go.sum", rule: "lockfile" }],
    notReviewed: [{ path: "internal/ledger/big.go", cause: "context overflow" }],
  },
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
};

export const repoPayments: RepositoryView = {
  instance: "main",
  project: "acme/payments",
  platform: "gitlab",
  source: "dashboard",
  on: true,
  last7: { reviews: 40, approved: 31, failed: 2 },
  health: "attention",
};

export const repoWidgets: RepositoryView = {
  instance: "main",
  project: "acme/widgets",
  platform: "gitlab",
  source: "config",
  on: true,
  last7: { reviews: 12, approved: 10, failed: 0 },
  health: "ok",
};

// A GitHub repository switched on in the dashboard: the one that links.
export const repoGadgets: RepositoryView = {
  instance: "hub",
  project: "acme/gadgets",
  platform: "github",
  source: "dashboard",
  on: true,
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
    model: { value: null, overridden: false },
  },
  attention: repo.health === "attention" ? [overview.attention[0]] : [],
  waiting: [],
  instanceTrustedAuthorsOnly: "follow_visibility",
  model: "anthropic / claude-sonnet",
  // Only an admin of the repository is given the choices.
  modelChoices:
    role === "admin"
      ? [
          { id: "m-sonnet", label: "anthropic / claude-sonnet", default: true },
          { id: "m-opus", label: "anthropic / claude-opus", default: false },
        ]
      : [],
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
  },
  pinned: {},
};

const modelKey = { set: true, updatedAt: minutesAgo(3 * 24 * 60) };

// The default of the list, and one a repository names.
export const modelSonnet: ModelAdminView = {
  id: "m-sonnet",
  provider: "anthropic",
  model: "claude-sonnet",
  baseURL: "",
  source: "dashboard",
  default: true,
  key: modelKey,
  repositories: [],
};
export const modelOpus: ModelAdminView = {
  id: "m-opus",
  provider: "anthropic",
  model: "claude-opus",
  baseURL: "https://llm.example.com/v1",
  source: "dashboard",
  default: false,
  key: modelKey,
  repositories: [{ instance: "main", project: "acme/payments" }],
};

// The list of models: three repositories follow the default.
export const models: ModelsAdminView = { canSetDefault: true, followingDefault: 3, models: [modelSonnet, modelOpus] };

export const instanceGitLab: InstanceAdminView = {
  name: "main",
  trustedAuthorsOnly: "follow_visibility",
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
  repos: [{ instance: "main", project: "acme/payments" }],
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
  tokens: tok(1_200_000, 300_000, { cacheRead: 500_000, cacheWrite: 20_000, reasoning: 0, unknown: 0 }),
  reviews: 40,
  daily: [
    { day: "2026-11-05", tokens: tok(200_000, 50_000) },
    { day: "2026-11-06", tokens: tok(600_000, 150_000) },
    { day: "2026-11-07", tokens: tok(400_000, 100_000) },
  ],
  repositories: [
    { instance: "main", project: "acme/payments", switchedOn: true, tokens: tok(900_000, 200_000), reviews: 30 },
    { instance: "main", project: "acme/widgets", switchedOn: true, tokens: tok(200_000, 80_000), reviews: 8 },
    { instance: "main", project: "acme/old", switchedOn: false, tokens: tok(100_000, 20_000), reviews: 2 },
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
};

// What the Skill results tab reads for admins: tokens per skill on the same changes.
export const skillTokens: SkillTokensView[] = [
  { skill: "security", tokens: { in: 40_000, out: 8_000 } },
  { skill: "code-review", tokens: { in: 150_000, out: 30_000 } },
  { skill: "style", tokens: { in: 5_000, out: 1_000 } },
  { skill: "docs", tokens: { in: 2_000, out: 400 } },
];
