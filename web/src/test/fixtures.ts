import type {
  AdminSettingsView,
  IntegrationView,
  Me,
  OverviewView,
  RepositoryDetailView,
  RepositoryView,
  ReviewDetailView,
  ReviewSummary,
  Role,
  RunDetailView,
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

export const reviewDetail: ReviewDetailView = {
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
          severity: "blocking",
          title: "Refund can be applied twice on retry",
          file: "internal/refund/handler.go",
          line: 88,
          explanation: "The idempotency key is checked after the ledger write.\nA retry writes a second row.",
          suggestedFix: "if seen(key) {\n\treturn nil\n}",
        },
      ],
      checks: [{ batch: 1, section: "Money", name: "Idempotent writes", status: "fail", detail: "Checked after the write." }],
    },
    {
      name: "code-review",
      verdict: "no_blocking_issues",
      source: null,
      findings: [
        {
          id: "f00d12",
          severity: "suggestion",
          title: "Name the magic retry window",
          file: "internal/refund/handler.go",
          explanation: "",
          suggestedFix: "",
        },
      ],
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
  },
  attention: repo.health === "attention" ? [overview.attention[0]] : [],
});

export const librarySkills = [
  { name: "code-review", content: "---\nname: code-review\n---\nReview the code.\n" },
  { name: "security-review", content: "---\nname: security-review\n---\nLook for security bugs.\n" },
];

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

export const adminSettings: AdminSettingsView = {
  instance: "main",
  defaults: { approve: true, reviewDrafts: true, failureStatus: "neutral", jobTimeoutSeconds: 600, jobMemory: "" },
  llm: { provider: "anthropic", model: "claude-sonnet" },
};
