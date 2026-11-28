import { useQuery } from "@tanstack/react-query";
import { Link, useSearchParams } from "react-router";

import { Badge } from "@/components/ui/badge";
import { buttonVariants } from "@/components/ui/button";
import { Card, CardHeader, CardTitle } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableRow } from "@/components/ui/table";

import { api, paths, type SecurityFindingRowView, type SecurityScanDetailView } from "../api";
import { ErrorBox, Loading, Pill, duration } from "../ui";
import { SeverityPill } from "./SecurityFindings";
import { scanStateTone } from "./SecurityScans";
import { TokenTable } from "./usageParts";

const cardClass = "mb-4 gap-0 overflow-hidden py-0";
const headClass = "border-b py-3 [.border-b]:pb-3";
const lineClass = "border-b px-4 py-3 text-sm last:border-b-0";

// The same polling as the tab: ask again while the scan is queued or running.
const pollMs = 10_000;

// SecurityScan is a scan's page (?tab=security&scan=<id>): its state and
// reason, coverage, usage, the log, the findings it added and fixed, and the
// SARIF download. The reason and the log are the server's and the model's
// text and are shown as plain text. The SARIF is a plain link to the API
// route, never inlined: a scan whose SARIF was dropped (hasSarif false) shows
// no link.
export default function SecurityScan({ instance, project, id }: { instance: string; project: string; id: string }) {
  const [params] = useSearchParams();
  // Back keeps the filters of the findings list, and no scan or finding.
  const list = new URLSearchParams(params);
  list.delete("scan");
  list.delete("finding");
  list.set("tab", "security");
  const scan = useQuery({
    queryKey: ["security-scan", instance, project, id],
    queryFn: () => api.get<SecurityScanDetailView>(paths.apiRepoSecurityScan(instance, project, id)),
    refetchInterval: (q) => (q.state.data?.scan.state === "queued" || q.state.data?.scan.state === "running" ? pollMs : false),
  });
  const back = (
    <Link className="text-muted-foreground mb-3 inline-block text-sm hover:underline" to={`${paths.repo(instance, project)}?${list.toString()}`}>
      Back to findings
    </Link>
  );
  if (scan.isPending) return <Loading />;
  if (scan.isError)
    return (
      <>
        {back}
        <ErrorBox error={scan.error} onRetry={() => void scan.refetch()} />
      </>
    );
  const { scan: s, log, new: added, fixed } = scan.data;
  const [tone, label] = scanStateTone[s.state];
  // A finding's page drops scan and keeps the list filters.
  const findingHref = (fid: number) => {
    const next = new URLSearchParams(list);
    next.set("finding", String(fid));
    return `${paths.repo(instance, project)}?${next.toString()}`;
  };
  return (
    <>
      {back}
      <h2 className="mb-3 text-lg font-semibold">Scan {s.number}</h2>
      <Card role="region" aria-label="Scan" className={cardClass}>
        <div className={lineClass + " flex flex-wrap items-center gap-2"}>
          <Pill tone={tone}>{label}</Pill>
          <span>{[s.durationSeconds !== null ? duration(s.durationSeconds) : null, s.branch, s.commit.slice(0, 7), s.model, s.skillName !== "" ? `skill ${s.skillName}` : null].filter((f) => f).join(" · ")}</span>
          {s.hasSarif && (
            <a className={buttonVariants({ variant: "outline", size: "sm" }) + " ml-auto"} href={paths.apiRepoSecurityScanSarif(instance, project, s.id)} download>
              Download SARIF
            </a>
          )}
        </div>
        {(s.state === "partial" || s.state === "failed") && s.reason !== "" && <div className={lineClass}>Reason: {s.reason}</div>}
        <div className={lineClass}>{s.coverage ? `Coverage: ${s.coverage.done} of ${s.coverage.planned} planned` : "Coverage not reported"}</div>
        {s.skillUpdated && <div className={lineClass + " text-muted-foreground"}>Skill updated: some findings may show as new</div>}
        <div className={lineClass + " text-muted-foreground"}>
          The scan read the platform&apos;s archive of this commit: Git LFS files and submodules are not in it.
        </div>
        {s.tokens && (
          <div className={lineClass}>
            <TokenTable tokens={s.tokens} />
            {s.usageSource === "stream" && (
              <div className="text-muted-foreground mt-1.5">Sub-agent tokens are missing: only the main agent&apos;s tokens were counted.</div>
            )}
          </div>
        )}
      </Card>
      <Card role="region" aria-label="Log" className={cardClass}>
        <CardHeader className={headClass}>
          <CardTitle>Log</CardTitle>
        </CardHeader>
        {log !== "" ? <pre className="log">{log}</pre> : <div className="text-muted-foreground px-4 py-3 text-sm">No log was kept for this scan.</div>}
      </Card>
      <Rows label="New findings" rows={added} href={findingHref} />
      <Rows label="Fixed findings" rows={fixed} href={findingHref} />
    </>
  );
}

function Rows({ label, rows, href }: { label: string; rows: SecurityFindingRowView[]; href: (id: number) => string }) {
  return (
    <Card role="region" aria-label={label} className={cardClass}>
      <CardHeader className={headClass}>
        <CardTitle>{label}</CardTitle>
      </CardHeader>
      {rows.length === 0 ? (
        <div className="text-muted-foreground px-4 py-3 text-sm">None.</div>
      ) : (
        <Table>
          <TableBody>
            {rows.map((f) => (
              <TableRow key={f.id}>
                <TableCell>
                  <SeverityPill severity={f.severity} />
                </TableCell>
                <TableCell className="whitespace-normal">
                  <Link className="text-foreground hover:underline" to={href(f.id)}>
                    {f.title}
                  </Link>
                  {f.needsValidation && (
                    <Badge variant="outline" className="text-muted-foreground ml-2">
                      Needs validation
                    </Badge>
                  )}
                </TableCell>
                <TableCell className="break-all whitespace-normal">
                  {f.file}
                  {f.startLine > 0 ? `:${f.startLine}` : ""}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </Card>
  );
}
