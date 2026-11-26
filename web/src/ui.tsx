// Small pieces the pages share. Every bit of text from the API is passed to
// React as a child, so it is shown as text: no HTML is ever built from it.
import { useEffect, useState, type KeyboardEvent, type ReactNode } from "react";

import { SearchIcon } from "lucide-react";

import { Alert } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Switch as SwitchBox } from "@/components/ui/switch";
import { cn } from "@/lib/utils";

import { ApiError, safeHref, type Outcome, type SecretView, type SkillSource } from "./api";

// Logo is the pergent mark: two lines of code, the second one dipping at a
// finding. It draws in the text colour, so it works on the dark sidebar and on
// the light sign-in card. This is the small cut, with thicker lines for 24 px.
export function Logo() {
  return (
    <svg className="logo size-6 shrink-0" viewBox="32 48 192 144" aria-hidden="true" focusable="false">
      <path
        fill="currentColor"
        d="M48 80L160 80A16 16 0 0 0 160 48L48 48A16 16 0 0 0 48 80ZM48 144L110.76 144L133.86 184A16 16 0 0 0 161.57 184L184.66 144L208 144A16 16 0 0 0 208 112L175.43 112A16 16 0 0 0 161.57 120L147.71 144L133.86 120A16 16 0 0 0 120 112L48 112A16 16 0 0 0 48 144Z"
      />
    </svg>
  );
}

// CenteredCard is the page the signed-out and gated screens (sign-in,
// password change, waiting) show: one card on the page grey.
export function CenteredCard({ children }: { children: ReactNode }) {
  return (
    <div className="signin bg-background grid min-h-svh place-items-center p-4">
      <Card className="signin-card w-full max-w-sm gap-4 px-6 py-6">
        <div className="flex items-center gap-2 text-[15px] font-semibold">
          <Logo />
          pergent.sh
        </div>
        {children}
      </Card>
    </div>
  );
}

// PageHeader is the top of a page: its title, a line under it, and the
// page's own buttons on the right.
export function PageHeader({
  title,
  description,
  actions,
  className,
}: {
  title: ReactNode;
  description?: ReactNode;
  actions?: ReactNode;
  className?: string;
}) {
  return (
    <div className={cn("top mb-6 flex flex-wrap items-start justify-between gap-4", className)}>
      <div className="min-w-0">
        <h1 className="text-2xl font-semibold tracking-tight">{title}</h1>
        {description && <div className="sub text-muted-foreground mt-1 text-sm">{description}</div>}
      </div>
      {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
    </div>
  );
}

// SearchField is a search box with a magnifier in front of the text. label
// names it for screen readers.
export function SearchField({
  label,
  value,
  onChange,
  placeholder = "Search by name",
}: {
  label: string;
  value: string;
  onChange: (v: string) => void;
  placeholder?: string;
}) {
  return (
    <span className="search-field relative min-w-[180px] flex-[0_1_320px] max-sm:flex-[1_1_100%]">
      <SearchIcon className="text-muted-foreground pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2" aria-hidden="true" />
      <Input
        className="pl-8"
        type="search"
        aria-label={label}
        placeholder={placeholder}
        maxLength={200}
        value={value}
        onChange={(e) => onChange(e.target.value)}
      />
    </span>
  );
}

export type Tone = "ok" | "bad" | "warn" | "run" | "draft" | "grey";

// Pill is a review state as a badge. "pill" and the tone stay as class
// names: the tab title drops .pill from the h1, and tests read the tone.
export function Pill({ tone, children }: { tone: Tone; children: ReactNode }) {
  return (
    <Badge variant={tone} className={`pill ${tone}`}>
      {children}
    </Badge>
  );
}

const outcomes: Record<Outcome, [Tone, string]> = {
  approved: ["ok", "Approved"],
  running: ["run", "Running"],
  blocking: ["bad", "Blocking"],
  ci_blocked: ["warn", "Blocked by CI"],
  draft: ["draft", "Draft"],
  // No blocking issues, but not approved (a draft, files not reviewed, a
  // change closed first): grey, not the green of an approval.
  no_blocking: ["grey", "Not approved"],
  failed: ["warn", "Failed"],
  superseded: ["grey", "Superseded"],
  fell_back: ["grey", "Retried on backup"],
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
  ["ci_blocked", "Blocked by CI"],
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
  `${project} ${changeNumber(platform, number)}`;

// changeNumber is a change's number alone: "#45" on GitHub, "!45" on GitLab.
export const changeNumber = (platform: string, number: number) => `${platform === "github" ? "#" : "!"}${number}`;

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

// Ago is a time as "4 days ago", with the exact time on hover (and for a
// screen reader, in the time element's datetime).
export function Ago({ t }: { t: string }) {
  return (
    <time dateTime={t} title={utcText(t)}>
      {ago(t)}
    </time>
  );
}

// utcText is a time as "2026-11-12 10:00 UTC", the same whatever the viewer's
// zone and language. Text it can't read as a time is shown as it is.
export function utcText(t: string): string {
  const d = new Date(t);
  return Number.isNaN(d.getTime()) ? t : `${d.toISOString().slice(0, 16).replace("T", " ")} UTC`;
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

// CopyButton puts text on the clipboard. what names it for screen readers
// ("Copy <what>"); it says Copied for a moment. No clipboard is no harm: the
// text is on screen to select.
export function CopyButton({ text, what }: { text: string; what: string }) {
  const [copied, setCopied] = useState(false);
  useEffect(() => {
    if (!copied) return;
    const timer = setTimeout(() => setCopied(false), 1500);
    return () => clearTimeout(timer);
  }, [copied]);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text);
      setCopied(true);
    } catch {
      // No clipboard here: the text is on screen to select.
    }
  };
  return (
    <Button type="button" variant="outline" size="sm" aria-label={copied ? "Copied" : `Copy ${what}`} onClick={copy}>
      {copied ? "Copied" : "Copy"}
    </Button>
  );
}

export function NotFound() {
  return (
    <>
      <PageHeader title="Not found" description="This page doesn't exist or you don't have access to it." />
      <Card className="text-muted-foreground px-4 py-8 text-center">Check the address, or ask an admin for access.</Card>
    </>
  );
}

export function Loading() {
  return <div className="loading text-muted-foreground p-6 text-sm">Loading…</div>;
}

// ErrorBox shows why something failed. With onRetry, a page's own read that
// failed, it offers Try again, so the page is not a dead end.
export function ErrorBox({ error, onRetry }: { error: unknown; onRetry?: () => void }) {
  const message = error instanceof ApiError ? error.message : "Something went wrong. Please try again.";
  return (
    <Alert variant="destructive" className="error flex items-center gap-3">
      <span className="grow">{message}</span>
      {onRetry && (
        <Button type="button" variant="outline" size="sm" className="text-foreground" onClick={onRetry}>
          Try again
        </Button>
      )}
    </Alert>
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
  // The keys of a tab list: Left and Right move to the tab before or after
  // (around the ends), Home and End to the first and last. Only the chosen
  // tab is in the Tab order, so Tab moves on to the panel.
  const move = (e: KeyboardEvent<HTMLDivElement>) => {
    const at = tabs.findIndex(([key]) => key === value);
    const to =
      e.key === "ArrowRight" ? (at + 1) % tabs.length
        : e.key === "ArrowLeft" ? (at - 1 + tabs.length) % tabs.length
          : e.key === "Home" ? 0
            : e.key === "End" ? tabs.length - 1
              : -1;
    if (to < 0) return;
    e.preventDefault();
    onChange(tabs[to][0]);
    const buttons = e.currentTarget.querySelectorAll<HTMLButtonElement>('[role="tab"]');
    buttons[to]?.focus();
  };
  return (
    <div
      className={cn(
        "tabs bg-muted text-muted-foreground inline-flex h-9 w-fit max-w-full items-center overflow-x-auto rounded-lg p-[3px]",
        inCard ? "in-card mx-4 mt-3" : "mb-4",
      )}
      role="tablist"
      onKeyDown={move}
    >
      {tabs.map(([key, label]) => (
        <button
          key={key}
          type="button"
          role="tab"
          aria-selected={key === value}
          tabIndex={key === value ? 0 : -1}
          className={cn(
            "inline-flex h-full items-center justify-center rounded-md border border-transparent px-3 text-sm font-medium whitespace-nowrap transition-[color,box-shadow] outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50",
            key === value ? "on bg-card text-foreground shadow-sm" : "hover:text-foreground",
          )}
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
    <SwitchBox
      aria-label={label}
      checked={checked}
      disabled={disabled}
      onChange={(e) => onChange?.(e.target.checked)}
    />
  );
}
