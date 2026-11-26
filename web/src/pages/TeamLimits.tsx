import { useQuery, useQueryClient } from "@tanstack/react-query";

import { Badge } from "@/components/ui/badge";
import { Card, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { cn } from "@/lib/utils";

import { api, isStatus, paths, type Role, type TeamGlobalLimitView, type TeamLimitsView } from "../api";
import LimitField from "../LimitField";
import { QuotaLine } from "../quota";
import { ErrorBox, Loading } from "../ui";

// modelLimitText is a global entry's model limit as a team sees it: its
// state alone, because its numbers count other teams.
const modelLimitText: Record<TeamGlobalLimitView["modelLimit"], string> = {
  none: "",
  room_left: "Model limit for all teams: room left",
  used_up: "Model limit for all teams: used up",
};

// entry is one model's block in the card.
const entry = "flex flex-col gap-2 border-b px-4 py-3 text-sm last:border-b-0";

// TeamLimits is the Model limits card of a team's page, for the team's
// maintainers and admins (canEdit); a viewer sees nothing and asks nothing.
// Each of the team's own entries has its limit, which they set with the
// team's route. Each global entry an admin limited for the team shows that
// limit, read-only for a maintainer; an admin changes it with the admin
// route, since the team's route takes the team's entries only.
export default function TeamLimits({ team, role, canEdit }: { team: number; role: Role; canEdit: boolean }) {
  const client = useQueryClient();
  const key = ["team-limits", team];
  const limits = useQuery({ queryKey: key, queryFn: () => api.get<TeamLimitsView>(paths.apiTeamLimits(team)), enabled: canEdit });
  if (!canEdit || isStatus(limits.error, 403)) return null;
  // A limit moves the bars here and the tags of the chains.
  const changed = () => {
    client.invalidateQueries({ queryKey: key });
    client.invalidateQueries({ queryKey: ["team-chain", team] });
    client.invalidateQueries({ queryKey: ["repo-chain"] });
    client.invalidateQueries({ queryKey: ["repo-reviewers"] });
    client.invalidateQueries({ queryKey: ["admin-models"] });
  };
  return (
    <Card role="region" aria-label="Model limits" className="gap-0 py-0">
      <CardHeader className="border-b py-4">
        <CardTitle>Model limits</CardTitle>
        <CardDescription>
          Tokens per calendar month (UTC): input, cache read, cache write, output and reasoning. The repositories this team owns may use this many on a model. Reviews
          skip a model at its limit. Empty: no limit.
        </CardDescription>
      </CardHeader>
      {limits.isPending && <Loading />}
      {limits.isError && (
        <div className="px-4 py-3">
          <ErrorBox error={limits.error} />
        </div>
      )}
      {limits.isSuccess && limits.data.models.length === 0 && limits.data.global.length === 0 && (
        <div className="text-muted-foreground px-4 py-3 text-sm">No model of the team's own, and no admin limit for the team.</div>
      )}
      {limits.isSuccess &&
        limits.data.models.map((m) => (
          <div className={entry} role="group" aria-label={`${m.name} limit`} key={m.id}>
            <b className="font-semibold">{m.name}</b>
            <QuotaLine ariaLabel={`Tokens of ${m.name} this month`} use={{ used: m.used, limit: m.limit }} />
            <LimitField
              key={String(m.limit)}
              label={`Limit of ${m.name}`}
              saveLabel={`Save the limit of ${m.name}`}
              limit={m.limit}
              path={paths.apiTeamModelLimit(team, m.id)}
              onSaved={changed}
            />
          </div>
        ))}
      {limits.isSuccess &&
        limits.data.global.map((g) => (
          <div className={entry} role="group" aria-label={`${g.name} limit`} key={g.id}>
            <div className="flex items-center gap-3">
              <b className="min-w-0 grow font-semibold">{g.name}</b>
              <Badge variant="outline">global</Badge>
            </div>
            <QuotaLine ariaLabel={`Tokens of ${g.name} this month`} use={{ used: g.used, limit: g.limit }} />
            {role === "admin" ? (
              <LimitField
                key={g.limit}
                label={`Limit of ${g.name}`}
                saveLabel={`Save the limit of ${g.name}`}
                limit={g.limit}
                path={paths.apiAdminModelTeamLimit(g.id, team)}
                onSaved={changed}
              />
            ) : (
              <div className="text-muted-foreground text-xs">Set by an admin.</div>
            )}
            {g.modelLimit !== "none" && (
              <div className={cn("text-xs", g.modelLimit === "used_up" ? "text-warn" : "text-muted-foreground")}>{modelLimitText[g.modelLimit]}</div>
            )}
          </div>
        ))}
    </Card>
  );
}
