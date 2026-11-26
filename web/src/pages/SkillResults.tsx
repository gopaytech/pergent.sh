import { useQuery, type UseQueryResult } from "@tanstack/react-query";

import { Card, CardHeader, CardTitle } from "@/components/ui/card";
import { NativeSelect } from "@/components/ui/native-select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

import { api, paths, type SkillResultView, type SkillTokensView } from "../api";
import { useMe } from "../layout/me";
import { ErrorBox, Loading, Pill, count } from "../ui";
import { useUrlParam } from "../urlState";
import { TokensTotal, num, output, prompt, total, usageTable } from "./usageParts";

const periods: (30 | 90)[] = [30, 90];
const dayMs = 24 * 3600 * 1000;

// empty is the line a card shows when it has no rows.
const empty = "text-muted-foreground px-4 py-6 text-sm";

// editedText says a skill changed during the window, in whole days.
function editedText(editedAt: string, now = Date.now()): string {
  const days = Math.max(0, Math.floor((now - new Date(editedAt).getTime()) / dayMs));
  const when = days === 0 ? "today" : days === 1 ? "1 day ago" : `${days} days ago`;
  return `Edited ${when}: numbers include the old version`;
}

// SkillResults is what became of the findings of each skill once their change
// was merged or closed, so a maintainer can see which skills to fix or drop.
// Addressed is Fixed over Raised and shows from 10 raised. Everything is text;
// the skill names come from the repository. An admin also sees what each
// skill cost on the same changes.
export default function SkillResults({ instance, project }: { instance: string; project: string }) {
  const admin = useMe().admin;
  const [days, setDays] = useUrlParam<30 | 90>("days", 30, [30, 90]);
  const list = useQuery({
    queryKey: ["skill-results", instance, project, days],
    queryFn: () => api.get<SkillResultView[]>(paths.apiRepoSkillResults(instance, project, days)),
  });
  // The server answers 404 to anyone but an admin, so no one else asks.
  const tokens = useQuery({
    queryKey: ["skill-tokens", instance, project, days],
    queryFn: () => api.get<SkillTokensView[]>(paths.apiRepoSkillTokens(instance, project, days)),
    enabled: admin,
  });

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <p className="text-muted-foreground max-w-3xl text-sm">
          What became of each skill's findings when their change was merged or closed, in the last {days} days. Addressed is Fixed out of
          Raised, and shows from 10 raised. Raised, Fixed, Open and Went away count merged changes opened by people; Dismissed also counts
          closed ones.
        </p>
        <NativeSelect aria-label="Period" className="w-40" value={days} onChange={(e) => setDays(Number(e.target.value) as 30 | 90)}>
          {periods.map((d) => (
            <option key={d} value={d}>
              Last {d} days
            </option>
          ))}
        </NativeSelect>
      </div>
      {list.isPending && <Loading />}
      {list.isError && <ErrorBox error={list.error} />}
      {list.isSuccess &&
        (list.data.length === 0 ? (
          <Card role="region" aria-label="Skill results" className="gap-0 py-0">
            <CardHeader className="border-b py-3">
              <CardTitle>Skill results</CardTitle>
            </CardHeader>
            <div className={empty}>No merged or closed changes in this period yet.</div>
          </Card>
        ) : (
          <Card role="region" aria-label="Skill results" className="gap-0 overflow-hidden py-0">
            <Table className={usageTable}>
              <TableHeader className="bg-muted/40">
                <TableRow className="hover:bg-transparent">
                  <TableHead>Skill</TableHead>
                  <TableHead className={num}>Raised</TableHead>
                  <TableHead className={num}>Fixed</TableHead>
                  <TableHead className={num}>Dismissed</TableHead>
                  <TableHead className={num}>Open at merge</TableHead>
                  <TableHead className={num}>Went away</TableHead>
                  <TableHead className={num}>Addressed</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {list.data.map((r) => (
                  <TableRow key={`${r.skill}/${r.severity}`}>
                    <TableCell>
                      <div className="font-medium">{r.skill}</div>
                      <div>
                        <Pill tone={r.severity === "blocking" ? "bad" : "grey"}>{r.severity}</Pill>
                      </div>
                      {r.editedAt && <div className="text-muted-foreground text-xs">{editedText(r.editedAt)}</div>}
                    </TableCell>
                    <TableCell className={num}>{r.raised}</TableCell>
                    <TableCell className={num}>{r.fixed}</TableCell>
                    <TableCell className={num}>{r.dismissed}</TableCell>
                    <TableCell className={num}>{r.open}</TableCell>
                    <TableCell className={num}>{r.wentAway}</TableCell>
                    <TableCell className={num}>
                      {r.addressed === null ? <span className="text-muted-foreground text-xs">Under 10 raised</span> : <b>{`${r.addressed}%`}</b>}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </Card>
        ))}
      {admin && list.isSuccess && <SkillCost tokens={tokens} results={list.data} />}
    </div>
  );
}

// SkillCost is the tokens each skill used on the changes the table counts,
// with what a fixed finding cost: all tokens over the findings fixed,
// summed over the skill's rows (blocking and suggestion). A skill with no
// fixed finding has no such number.
function SkillCost({ tokens, results }: { tokens: UseQueryResult<SkillTokensView[]>; results: SkillResultView[] }) {
  const fixed = new Map<string, number>();
  for (const r of results) fixed.set(r.skill, (fixed.get(r.skill) ?? 0) + r.fixed);
  return (
    <Card role="region" aria-label="Skill cost on these changes" className="gap-0 overflow-hidden py-0">
      <CardHeader className="border-b py-3">
        <CardTitle>Skill cost on these changes</CardTitle>
      </CardHeader>
      {tokens.isPending && <Loading />}
      {tokens.isError && (
        <div className="px-4 py-3">
          <ErrorBox error={tokens.error} />
        </div>
      )}
      {tokens.isSuccess &&
        (tokens.data.length === 0 ? (
          <div className={empty}>No usage recorded for these changes.</div>
        ) : (
          <Table className={usageTable}>
            <TableHeader className="bg-muted/40">
              <TableRow className="hover:bg-transparent">
                <TableHead>Skill</TableHead>
                <TableHead className={num}>Prompt</TableHead>
                <TableHead className={num}>Output</TableHead>
                <TableHead className={num}>Tokens</TableHead>
                <TableHead className={num}>Fixed</TableHead>
                <TableHead className={num}>Tokens per fixed finding</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {tokens.data.map((t) => {
                const n = fixed.get(t.skill) ?? 0;
                return (
                  <TableRow key={t.skill}>
                    <TableCell>{t.skill}</TableCell>
                    <TableCell className={num}>{count(prompt(t.tokens))}</TableCell>
                    <TableCell className={num}>{count(output(t.tokens))}</TableCell>
                    <TableCell className={num}>
                      <TokensTotal tokens={t.tokens} />
                    </TableCell>
                    <TableCell className={num}>{n}</TableCell>
                    <TableCell className={num}>{n > 0 ? count(Math.round(total(t.tokens) / n)) : "—"}</TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        ))}
    </Card>
  );
}
