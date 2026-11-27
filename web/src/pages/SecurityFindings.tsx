import { useQuery } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { Link, useSearchParams } from "react-router";

import { Badge } from "@/components/ui/badge";
import { Card, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { NativeSelect } from "@/components/ui/native-select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

import { api, paths, type Severity, type SecurityFindingRowView, type SecurityFindingsView } from "../api";
import { Ago, ErrorBox, Loading, Pill, type Tone } from "../ui";
import { useUrlParam } from "../urlState";
import { useSearchText } from "../useSearchText";

export const severityTone: Record<Severity, [Tone, string]> = {
  critical: ["bad", "Critical"],
  high: ["bad", "High"],
  medium: ["warn", "Medium"],
  low: ["grey", "Low"],
};

export function SeverityPill({ severity }: { severity: Severity }) {
  const [tone, label] = severityTone[severity] ?? ["grey", severity];
  return <Pill tone={tone}>{label}</Pill>;
}

const states = ["open", "dismissed", "fixed", "all"] as const;
const stateText: Record<(typeof states)[number], string> = { open: "Open", dismissed: "Dismissed", fixed: "Fixed", all: "All" };
const severities = ["", "critical", "high", "medium", "low"] as const;
const selectWidth = "w-40";

// SecurityFindings is the findings list of the Security tab. The filters (state,
// severity, Rule, search) stay in the address. Rule is the attack class the
// scan skill gave (ruleId), never a CWE: a CWE shows beside it only when the
// API sends one. Titles and files are the model's: they are shown as text.
export default function SecurityFindings({ instance, project }: { instance: string; project: string }) {
  const [state, setState] = useUrlParam<string>("state", "open", states);
  const [severity, setSeverity] = useUrlParam<string>("severity", "", severities);
  const [params, setParams] = useSearchParams();
  const rule = params.get("rule") ?? "";
  const setRule = (v: string) =>
    setParams((old) => {
      const next = new URLSearchParams(old);
      if (v === "") next.delete("rule");
      else next.set("rule", v);
      return next;
    });
  const [typed, setTyped, q] = useSearchText();
  const filter = { state: state === "all" ? "" : state, severity, rule, q };
  const findings = useQuery({
    queryKey: ["security-findings", instance, project, filter],
    queryFn: () => api.get<SecurityFindingsView>(paths.apiRepoSecurityFindings(instance, project, filter)),
    // The list stays while the next one loads, so the filters do not jump.
    placeholderData: (old) => old,
  });
  // The Rule options come from the answers seen so far: with a rule chosen the
  // answer holds only that rule, and the others must stay to choose from.
  const [rules, setRules] = useState<string[]>([]);
  const answer = findings.data?.findings;
  useEffect(() => {
    if (!answer) return;
    setRules((old) => {
      const next = [...new Set([...old, ...answer.map((f) => f.ruleId)])].sort();
      return next.length === old.length ? old : next;
    });
  }, [answer]);
  const options = rule !== "" && !rules.includes(rule) ? [...rules, rule].sort() : rules;
  return (
    <Card role="region" aria-label="Findings" className="mb-4 gap-0 overflow-hidden py-0">
      <CardHeader className="border-b py-3 [.border-b]:pb-3">
        <CardTitle>Findings</CardTitle>
      </CardHeader>
      <div className="flex flex-wrap items-center gap-2 border-b px-4 py-3">
        <NativeSelect className={selectWidth} aria-label="State" value={state} onChange={(e) => setState(e.target.value)}>
          {states.map((s) => (
            <option key={s} value={s}>
              {stateText[s]}
            </option>
          ))}
        </NativeSelect>
        <NativeSelect className={selectWidth} aria-label="Severity" value={severity} onChange={(e) => setSeverity(e.target.value)}>
          <option value="">Any severity</option>
          {severities.slice(1).map((s) => (
            <option key={s} value={s}>
              {severityTone[s as Severity][1]}
            </option>
          ))}
        </NativeSelect>
        <NativeSelect className={selectWidth} aria-label="Rule" value={rule} onChange={(e) => setRule(e.target.value)}>
          <option value="">All rules</option>
          {options.map((r) => (
            <option key={r} value={r}>
              {r}
            </option>
          ))}
        </NativeSelect>
        <Input
          type="search"
          className="w-64"
          aria-label="Search findings"
          placeholder="Search file or title"
          value={typed}
          onChange={(e) => setTyped(e.target.value)}
        />
      </div>
      {findings.isPending ? (
        <Loading />
      ) : findings.isError ? (
        <div className="p-4">
          <ErrorBox error={findings.error} onRetry={() => void findings.refetch()} />
        </div>
      ) : (
        <Rows rows={findings.data.findings} truncated={findings.data.truncated} instance={instance} project={project} />
      )}
    </Card>
  );
}

function Rows({ rows, truncated, instance, project }: { rows: SecurityFindingRowView[]; truncated: boolean; instance: string; project: string }) {
  if (rows.length === 0) return <div className="text-muted-foreground px-4 py-6 text-sm">No findings match.</div>;
  return (
    <>
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Severity</TableHead>
            <TableHead>Finding</TableHead>
            <TableHead>Rule</TableHead>
            <TableHead>File</TableHead>
            <TableHead>First seen</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.map((f) => (
            <TableRow key={f.id}>
              <TableCell>
                <SeverityPill severity={f.severity} />
              </TableCell>
              <TableCell className="whitespace-normal">
                <Link className="text-foreground hover:underline" to={paths.repoSecurity(instance, project, { finding: f.id })}>
                  {f.title}
                </Link>
                {f.needsValidation && (
                  <Badge variant="outline" className="text-muted-foreground ml-2">
                    Needs validation
                  </Badge>
                )}
              </TableCell>
              <TableCell>
                {f.ruleId}
                {f.cwe !== "" && <span className="text-muted-foreground"> · {f.cwe}</span>}
              </TableCell>
              <TableCell className="break-all whitespace-normal">
                {f.file}
                {f.startLine > 0 ? `:${f.startLine}` : ""}
              </TableCell>
              <TableCell>
                <Ago t={f.firstSeenAt} />
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
      {truncated && <div className="text-muted-foreground border-t px-4 py-3 text-sm">Showing the first 1000. Narrow the filters to see more.</div>}
    </>
  );
}
