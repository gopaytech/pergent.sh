import type {
  AuditPageView,
  Can,
  DefaultsView,
  InstanceAdminView,
  IntegrationView,
  LibrarySkill,
  LLMAdminView,
  Me,
  OverviewView,
  RepositoryDetailView,
  RepositoryView,
  ReviewDetailView,
  ReviewSummary,
  Role,
  RunDetailView,
  SkillDetail,
  SkillFetch,
  TeamView,
  UserView,
} from "../api";

// A platform admin: on an admin team of its instance.
export const platformAdmin: Me = {
  kind: "platform",
  username: "platform-user",
  platform: "gitlab",
  instance: "main",
  admin: true,
  operator: false,
  waiting: false,
  mustChangePassword: false,
  waitingUsers: 0,
  csrfToken: "csrf-123",
};

// The operator: a password account, which sees everything.
export const operator: Me = {
  ...platformAdmin,
  kind: "password",
  username: "admin",
  platform: "",
  instance: "",
  operator: true,
};

export const maintainer: Me = { ...platformAdmin, username: "maint-user", admin: false, csrfToken: "csrf-maint" };
export const viewer: Me = { ...platformAdmin, username: "dev-user", admin: false, csrfToken: "csrf-dev" };

// On no team yet: sees nothing.
export const waiting: Me = { ...viewer, username: "new-user", waiting: true };

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

export const repoDetail = (repo: RepositoryView, role: Role = "admin"): RepositoryDetailView => ({
  repo,
  role,
  settings: {
    approve: { value: true, overridden: true },
    reviewDrafts: { value: false, overridden: false },
    failureStatus: { value: "failed", overridden: false },
    skills: { value: null, overridden: false },
    jobTimeoutSeconds: { value: 600, overridden: false },
    jobMemory: { value: "6Gi", overridden: true },
    rerunLimitPerMR: { value: 10, overridden: false },
    rerunLimitPerRepo: { value: 50, overridden: false },
  },
  attention: repo.health === "attention" ? [overview.attention[0]] : [],
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

export const integrations: IntegrationView = {
  instance: "main",
  platform: "gitlab",
  connected: true,
  approver: "token",
  tokenExpiry: "2026-11-09T00:00:00Z",
  repos: [
    { project: "acme/payments", installed: true, approverReachable: false },
    { project: "acme/widgets", installed: true, approverReachable: true },
  ],
};

export const defaults: DefaultsView = {
  approve: true,
  reviewDrafts: true,
  failureStatus: "neutral",
  jobTimeoutSeconds: 600,
  jobMemory: "",
  rerunLimitPerMR: 10,
  rerunLimitPerRepo: 50,
};

export const llm: LLMAdminView = {
  source: "dashboard",
  provider: "anthropic",
  model: "claude-sonnet",
  baseURL: "",
  key: { set: true, updatedAt: minutesAgo(3 * 24 * 60) },
  editable: true,
};

export const instanceGitLab: InstanceAdminView = {
  name: "main",
  platform: "gitlab",
  source: "config",
  removing: false,
  url: "https://gitlab.example.com",
  apiURL: "",
  reviewAppID: 0,
  approveAppID: 0,
  approver: "token",
  oauthClientID: "app-id-1",
  secrets: {
    review_token: { set: true, updatedAt: null },
    approve_token: { set: true, updatedAt: null },
    webhook_secret: { set: true, updatedAt: null },
    oauth_client_secret: { set: false, updatedAt: null },
  },
};

export const instanceGitHub: InstanceAdminView = {
  name: "hub",
  platform: "github",
  source: "dashboard",
  removing: false,
  url: "",
  apiURL: "https://api.github.example.com",
  reviewAppID: 11,
  approveAppID: 12,
  approver: "app",
  oauthClientID: "",
  secrets: {
    review_app_key: { set: true, updatedAt: minutesAgo(120) },
    approve_app_key: { set: false, updatedAt: null },
    webhook_secret: { set: true, updatedAt: minutesAgo(120) },
    oauth_client_secret: { set: false, updatedAt: null },
  },
};

export const instanceDashboardGitLab: InstanceAdminView = {
  ...instanceGitLab,
  name: "extra",
  source: "dashboard",
  url: "https://gitlab2.example.com",
  oauthClientID: "",
  secrets: {
    review_token: { set: true, updatedAt: minutesAgo(120) },
    approve_token: { set: true, updatedAt: minutesAgo(120) },
    webhook_secret: { set: true, updatedAt: minutesAgo(120) },
    oauth_client_secret: { set: false, updatedAt: null },
  },
};

export const userWaiting: UserView = {
  id: 7,
  kind: "platform",
  username: "newcomer",
  platform: "gitlab",
  instance: "main",
  lastSignInAt: minutesAgo(5),
  waiting: true,
  teams: [],
};

export const userDev: UserView = {
  id: 5,
  kind: "platform",
  username: "dev-user",
  platform: "gitlab",
  instance: "main",
  lastSignInAt: minutesAgo(180),
  waiting: false,
  teams: [{ id: 2, name: "Payments" }],
};

export const userOperator: UserView = {
  id: 1,
  kind: "password",
  username: "admin",
  platform: "",
  instance: "",
  lastSignInAt: null,
  waiting: false,
  teams: [],
};

export const teamPayments: TeamView = {
  id: 2,
  name: "Payments",
  role: "maintainer",
  members: [{ id: 5, username: "dev-user", platform: "gitlab", instance: "main" }],
  repos: [{ instance: "main", project: "acme/payments" }],
  hiddenRepos: 0,
  hiddenMembers: 0,
};

export const teamAdmins: TeamView = {
  id: 3,
  name: "Admins",
  role: "admin",
  members: [],
  repos: [],
  hiddenRepos: 0,
  hiddenMembers: 0,
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
