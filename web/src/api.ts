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
  | "unknown";

export type Me = {
  platform: Platform;
  instance: string;
  username: string;
  isAdmin: boolean;
  csrfToken: string;
};

export type InstanceView = { platform: string; instance: string; label: string };

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

export type OverviewView = { stats: StatsView; recent: ReviewSummary[]; attention: AttentionView[] };

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

export type ReviewDetailView = { header: ReviewHeader; runs: RunSummary[] };

export type FindingView = {
  id: string;
  severity: "blocking" | "suggestion";
  title: string;
  file: string;
  line?: number;
  explanation: string;
  suggestedFix: string;
};

export type CheckView = { batch: number; section: string; name: string; status: string; detail: string };

export type SkillView = {
  name: string;
  verdict: "no_blocking_issues" | "has_blocking_issues" | "failed";
  source: "library" | "repo" | null;
  findings: FindingView[];
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
  tokens: { in: number; out: number };
  files: {
    reviewed: string[];
    skipped: { path: string; rule: string }[];
    notReviewed: { path: string; cause: string }[];
  };
  skills: SkillView[];
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
  failureStatus: SettingView<string>;
  skills: SettingView<string[] | null>;
  jobTimeoutSeconds: SettingView<number>;
  jobMemory: SettingView<string>;
};

export type RepositoryDetailView = {
  repo: RepositoryView;
  settings: RepoSettings;
  attention: AttentionView[];
};

// SettingsUpdate is the body of PUT .../settings: only the keys to change;
// null resets one to its default.
export type SettingsUpdate = Partial<{
  approve: boolean | null;
  reviewDrafts: boolean | null;
  failureStatus: string | null;
  skills: string[] | null;
  jobTimeoutSeconds: number | null;
  jobMemory: string | null;
}>;

export type InputsView = {
  branch: string;
  skills: { name: string; source: "library" | "repo"; content: string }[];
  instructions: string;
};

export type LibrarySkill = { name: string; content: string };

export type IntegrationView = {
  instance: string;
  platform: Platform;
  connected: boolean;
  approver: "app" | "token";
  tokenExpiry?: string;
  repos: { project: string; installed: boolean; approverReachable: boolean }[];
};

export type AdminSettingsView = {
  instance: string;
  defaults: {
    approve: boolean;
    reviewDrafts: boolean;
    failureStatus: string;
    jobTimeoutSeconds: number;
    jobMemory: string;
  };
  llm: { provider: string; model: string };
  admins: { instance: string; group?: string; team?: string }[];
};

export type AvailableView = {
  items: { instance: string; project: string; platform: Platform }[];
  truncated: boolean;
};

// ApiError is a non-2xx answer; message is the API's {"error"} text, for
// people, and code its {"code"}, for the client to test.
export class ApiError extends Error {
  readonly status: number;
  readonly code?: string;
  constructor(status: number, message: string, code?: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
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

async function request<T>(method: string, path: string, body?: unknown, csrf?: string): Promise<T> {
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
  if (res.status === 401) {
    for (const fn of [...unauthorized]) fn();
  }
  if (!res.ok) {
    let message = `request failed (${res.status})`;
    let code: string | undefined;
    try {
      const data: unknown = await res.json();
      if (data && typeof data === "object") {
        const { error, code: c } = data as { error?: unknown; code?: unknown };
        if (typeof error === "string") message = error;
        if (typeof c === "string") code = c;
      }
    } catch {
      // Not JSON: keep the generic message.
    }
    throw new ApiError(res.status, message, code);
  }
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

export const api = {
  get: <T>(path: string) => request<T>("GET", path),
  post: <T>(path: string, body: unknown, csrf: string) => request<T>("POST", path, body, csrf),
  put: <T>(path: string, body: unknown, csrf: string) => request<T>("PUT", path, body, csrf),
  del: <T>(path: string, csrf: string) => request<T>("DELETE", path, undefined, csrf),
  write: <T>(method: WriteMethod, path: string, body: unknown, csrf: string) => request<T>(method, path, body, csrf),
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
  login: (platform: string, instance: string) => `/auth/${seg(platform)}/${seg(instance)}/login`,

  apiReview: (instance: string, project: string, number: number) =>
    `/api/reviews/${seg(instance)}/${proj(project)}/${seg(String(number))}`,
  apiRun: (id: string) => `/api/runs/${seg(id)}`,
  apiRunLog: (id: string) => `/api/runs/${seg(id)}/log`,
  apiRepo: (instance: string, project: string) => `/api/repositories/${seg(instance)}/${proj(project)}`,
  apiRepoInputs: (instance: string, project: string) => `/api/repositories/${seg(instance)}/${proj(project)}/inputs`,
  apiRepoSettings: (instance: string, project: string) => `/api/repositories/${seg(instance)}/${proj(project)}/settings`,
  apiRepoReset: (instance: string, project: string) =>
    `/api/repositories/${seg(instance)}/${proj(project)}/settings/reset`,
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
