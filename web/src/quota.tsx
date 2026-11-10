import type { QuotaUseView } from "./api";

// quotaMax is the most a quota can be (the API's own bound).
const quotaMax = 1_000_000_000_000_000;

export const quotaProblem = `The token quota is a whole number from 1 to ${quotaMax}, or empty.`;

// parseQuota reads a quota typed in: digits, with commas, underscores and
// spaces left out. Empty is null (no quota); undefined is not a quota.
export function parseQuota(text: string): number | null | undefined {
  const t = text.replace(/[,_\s]/g, "");
  if (t === "") return null;
  if (!/^\d+$/.test(t)) return undefined;
  const n = Number(t);
  return n >= 1 && n <= quotaMax ? n : undefined;
}

// quotaText is a quota as the input shows it.
export const quotaText = (quota: number | null) => (quota === null ? "" : String(quota));

const tokens = (n: number) => n.toLocaleString("en-US");

// QuotaLine is what was used this month, with a bar against the limit when
// there is one: amber from 80%, red when it is used up.
export function QuotaLine({ use, label, ariaLabel }: { use: QuotaUseView; label?: string; ariaLabel: string }) {
  const { used, limit } = use;
  const state = limit === null ? "" : used >= limit ? "full" : used * 5 >= limit * 4 ? "warn" : "";
  return (
    <div className="quota-line">
      {label && <span className="name">{label}</span>}
      <span>{limit === null ? `Tokens this month: ${tokens(used)}` : `Tokens this month: ${tokens(used)} of ${tokens(limit)}`}</span>
      {limit !== null && (
        <meter className={`quota-meter ${state}`.trim()} aria-label={ariaLabel} min={0} max={limit} value={Math.min(used, limit)}>
          {tokens(used)} of {tokens(limit)}
        </meter>
      )}
    </div>
  );
}
