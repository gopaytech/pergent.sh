import { useQuery } from "@tanstack/react-query";
import { Link, useSearchParams } from "react-router";

import { Card, CardHeader, CardTitle } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

import { api, paths, type SecurityScansView, type SecurityScanView } from "../api";
import { Ago, ErrorBox, Loading, Pill, duration, type Tone } from "../ui";

export const scanStateTone: Record<SecurityScanView["state"], [Tone, string]> = {
  queued: ["grey", "Queued"],
  running: ["warn", "Running"],
  complete: ["ok", "Complete"],
  partial: ["warn", "Partial"],
  failed: ["bad", "Failed"],
};

// scanHref is a scan's page. The list filters stay in the address (the page's
// Back link returns to them) and no finding is carried: one page at a time.
export function scanHref(instance: string, project: string, params: URLSearchParams, id: string): string {
  const next = new URLSearchParams(params);
  next.set("tab", "security");
  next.delete("finding");
  next.set("scan", id);
  return `${paths.repo(instance, project)}?${next.toString()}`;
}

// SecurityScans is the scans list of the Security tab, newest first. The reason
// of a partial or failed scan is the server's text, shown as text.
export default function SecurityScans({ instance, project }: { instance: string; project: string }) {
  const [params] = useSearchParams();
  const scans = useQuery({
    queryKey: ["security-scans", instance, project],
    queryFn: () => api.get<SecurityScansView>(paths.apiRepoSecurityScans(instance, project)),
  });
  return (
    <Card role="region" aria-label="Scans" className="mb-4 gap-0 overflow-hidden py-0">
      <CardHeader className="border-b py-3 [.border-b]:pb-3">
        <CardTitle>Scans</CardTitle>
      </CardHeader>
      {scans.isPending ? (
        <Loading />
      ) : scans.isError ? (
        <div className="p-4">
          <ErrorBox error={scans.error} onRetry={() => void scans.refetch()} />
        </div>
      ) : scans.data.scans.length === 0 ? (
        <div className="text-muted-foreground px-4 py-6 text-sm">No scans yet.</div>
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Scan</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Date</TableHead>
              <TableHead>Duration</TableHead>
              <TableHead>New</TableHead>
              <TableHead>Fixed</TableHead>
              <TableHead>Reason</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {scans.data.scans.map((s) => {
              const [tone, label] = scanStateTone[s.state];
              return (
                <TableRow key={s.id}>
                  <TableCell>
                    <Link className="text-foreground hover:underline" to={scanHref(instance, project, params, s.id)}>
                      {s.number}
                    </Link>
                  </TableCell>
                  <TableCell>
                    <Pill tone={tone}>{label}</Pill>
                  </TableCell>
                  <TableCell>
                    <Ago t={s.queuedAt} />
                  </TableCell>
                  <TableCell>{s.durationSeconds !== null ? duration(s.durationSeconds) : ""}</TableCell>
                  <TableCell>{s.newCount}</TableCell>
                  <TableCell>{s.fixedCount}</TableCell>
                  <TableCell className="whitespace-normal break-words">{s.state === "partial" || s.state === "failed" ? s.reason : ""}</TableCell>
                </TableRow>
              );
            })}
          </TableBody>
        </Table>
      )}
    </Card>
  );
}
