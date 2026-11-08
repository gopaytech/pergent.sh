// Small pieces the pages share. Every bit of text from the API is passed to
// React as a child, so it is shown as text: no HTML is ever built from it.
import type { ReactNode } from "react";

import { ApiError, safeHref, type Outcome, type SecretView, type SkillSource } from "./api";

// CenteredCard is the page the signed-out and gated screens (sign-in,
// password change, waiting) show: one card on the dark background.
export function CenteredCard({ children }: { children: ReactNode }) {
  return (
    <div className="signin">
      <div className="card signin-card">
        <div className="brand dark">
          <div className="logo" aria-hidden="true">
            🦉
          </div>
          pergent.sh
        </div>
        {children}
      </div>
    </div>
  );
}

export type Tone = "ok" | "bad" | "warn" | "run" | "draft" | "grey";

export function Pill({ tone, children }: { tone: Tone; children: ReactNode }) {
  return <span className={`pill ${tone}`}>{children}</span>;
}

const outcomes: Record<Outcome, [Tone, string]> = {
  approved: ["ok", "Approved"],
  running: ["run", "Running"],
  blocking: ["bad", "Blocking"],
  draft: ["draft", "Draft"],
  no_blocking: ["ok", "Not approved"],
  failed: ["warn", "Failed"],
  superseded: ["grey", "Superseded"],
  skipped: ["grey", "Not reviewed"],
  unknown: ["grey", "Unknown"],
  waiting: ["grey", "Waiting for a start"],
};

export function OutcomePill({ outcome }: { outcome: string }) {
  const [tone, label] = outcomes[outcome as Outcome] ?? ["grey", outcome];
  return <Pill tone={tone}>{label}</Pill>;
}

export const outcomeOptions: [Outcome, string][] = [
  ["approved", "Approved"],
  ["blocking", "Blocking"],
  ["no_blocking", "Not approved"],
  ["draft", "Draft"],
  ["running", "Running"],
  ["failed", "Failed"],
  ["superseded", "Superseded"],
  ["skipped", "Not reviewed"],
  ["unknown", "Unknown"],
  ["waiting", "Waiting for a start"],
];

export const platformName = (p: string) => (p === "github" ? "GitHub" : p === "gitlab" ? "GitLab" : p);

// changeRef is how the platform writes a change: acme/widgets !12 on
// GitLab, acme/widgets #12 on GitHub.
export const changeRef = (platform: string, project: string, number: number) =>
  `${project} ${platform === "github" ? "#" : "!"}${number}`;

// ago says how long before now t was, roughly.
export function ago(t: string, now = Date.now()): string {
  const ms = now - new Date(t).getTime();
  if (Number.isNaN(ms)) return "";
  const min = Math.round(ms / 60_000);
  if (min < 1) return "just now";
  if (min < 60) return `${min} min ago`;
  const h = Math.round(min / 60);
  if (h < 24) return `${h} h ago`;
  const d = Math.round(h / 24);
  return d === 1 ? "1 day ago" : `${d} days ago`;
}

// secretText says whether a secret is set and, for one stored in the
// dashboard, when it changed. The value is never known to the page.
export function secretText(s: SecretView): string {
  if (!s.set) return "not set";
  return s.updatedAt ? `set · changed ${ago(s.updatedAt)}` : "set";
}

// noFill keeps password managers out of a field. Next to a password field,
// browsers ignore autocomplete="off" and fill the admin's own sign-in;
// these are the opt-outs they honour. secretFill is the same for the
// password field itself.
export const noFill = {
  autoComplete: "off",
  "data-1p-ignore": "",
  "data-lpignore": "true",
  "data-bwignore": "",
  "data-form-type": "other",
} as const;
export const secretFill = { ...noFill, autoComplete: "new-password" } as const;

// bytesText is a size in bytes as "52 B" or "12.5 KiB".
export function bytesText(n: number): string {
  if (n < 1024) return `${n} B`;
  return `${(n / 1024).toFixed(1).replace(/\.0$/, "")} KiB`;
}

// SkillSourceText says where a library skill came from: written in the
// dashboard, a repository on a connected instance (a private one the viewer
// can't see is only named by its instance), or a public URL. Every part is
// text.
export function SkillSourceText({ source }: { source: SkillSource | null }) {
  if (!source) return <>written in the dashboard</>;
  const where =
    source.kind === "url"
      ? source.url
      : source.hidden
        ? `a private repository on ${source.instance}`
        : `${source.repo} on ${source.instance}`;
  return (
    <>
      <span>{where}</span> <span className="src">{source.ref}</span>
    </>
  );
}

// skillOrigin says where a skill a review used came from: "library v3" (the
// version the review used, none when it wasn't recorded), "repo (.claude/skills)"
// for a repository folder, "repo" for a run from before the folders, and
// "source not recorded" for null.
export function skillOrigin(source: string | null, version: number): string {
  if (source === null || source === "") return "source not recorded";
  if (source === "library") return version > 0 ? `library v${version}` : "library";
  if (source === "repo") return "repo";
  return `repo (${source})`;
}

// duration is seconds as "3m 41s".
export function duration(secs: number): string {
  const s = Math.round(secs);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${s % 60}s`;
  return `${Math.floor(m / 60)}h ${m % 60}m`;
}

// count is a token count as "48.2k".
export function count(n: number): string {
  if (n < 1000) return String(n);
  if (n < 1_000_000) return `${(n / 1000).toFixed(1).replace(/\.0$/, "")}k`;
  return `${(n / 1_000_000).toFixed(1).replace(/\.0$/, "")}M`;
}

// ExternalLink links to a page on GitLab or GitHub. Anything but an http(s)
// address is shown as no link at all.
export function ExternalLink({ href, children }: { href: string | undefined; children: ReactNode }) {
  const safe = safeHref(href);
  if (!safe) return null;
  return (
    <a href={safe} target="_blank" rel="noopener noreferrer">
      {children}
    </a>
  );
}

export function NotFound() {
  return (
    <>
      <div className="top">
        <div>
          <h1>Not found</h1>
          <div className="sub">This page doesn't exist or you don't have access to it.</div>
        </div>
      </div>
      <div className="card empty">Check the address, or ask an admin for access.</div>
    </>
  );
}

export function Loading() {
  return <div className="loading">Loading…</div>;
}

export function ErrorBox({ error }: { error: unknown }) {
  const message = error instanceof ApiError ? error.message : "Something went wrong. Please try again.";
  return (
    <div className="error" role="alert">
      {message}
    </div>
  );
}

// Tabs is a row of tabs; the selected one is marked for screen readers.
export function Tabs<T extends string>({
  tabs,
  value,
  onChange,
  inCard,
}: {
  tabs: [T, string][];
  value: T;
  onChange: (t: T) => void;
  inCard?: boolean;
}) {
  return (
    <div className={inCard ? "tabs in-card" : "tabs"} role="tablist">
      {tabs.map(([key, label]) => (
        <button
          key={key}
          type="button"
          role="tab"
          aria-selected={key === value}
          className={key === value ? "on" : undefined}
          onClick={() => onChange(key)}
        >
          {label}
        </button>
      ))}
    </div>
  );
}

// Switch is an on/off toggle drawn like the mockups'.
export function Switch({
  label,
  checked,
  disabled,
  onChange,
}: {
  label: string;
  checked: boolean;
  disabled?: boolean;
  onChange?: (on: boolean) => void;
}) {
  return (
    <input
      type="checkbox"
      role="switch"
      className="toggle"
      aria-label={label}
      checked={checked}
      disabled={disabled}
      onChange={(e) => onChange?.(e.target.checked)}
    />
  );
}
