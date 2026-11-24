import { useMutation, useQueryClient } from "@tanstack/react-query";

import { paths, type MemberRole, type TeamPageView, type TeamRepoView, type TeamUpdate } from "../api";
import { useWrite } from "../layout/write";

export type TeamMemberInput = { accountId: number; role: MemberRole };

// teamUpdate is the body of PUT /api/teams/{id}: the team as the page last
// read it, plus one change. What the change leaves out is sent as read, so a
// save never drops the other tab's data. An entry that is removed (no longer
// switched on) is left out, because the server answers 400 for it: the next
// save drops it, as the page says.
export function teamUpdate(
  v: TeamPageView,
  change: { name?: string; members?: TeamMemberInput[]; repos?: TeamRepoView[] },
): TeamUpdate {
  const removed = new Set(v.repos.filter((r) => r.removed).map((r) => JSON.stringify([r.instance, r.project])));
  const repos = change.repos ?? v.repos.map((r) => ({ instance: r.instance, project: r.project, owner: r.owner }));
  return {
    name: change.name ?? v.team.name,
    members: change.members ?? v.members.map((m) => ({ accountId: m.accountId, role: m.role })),
    repos: repos.filter((r) => !removed.has(JSON.stringify([r.instance, r.project]))),
  };
}

// useTeamSave saves one change of the team with a PUT, then reads again what
// it moves: the page, the lists of teams and accounts, and the session (a
// member may have been waiting only because of this team).
export function useTeamSave(v: TeamPageView) {
  const client = useQueryClient();
  const write = useWrite();
  return useMutation({
    mutationFn: (change: Parameters<typeof teamUpdate>[1]) => write<unknown>("PUT", paths.apiTeam(v.team.id), teamUpdate(v, change)),
    onSuccess: () => {
      client.invalidateQueries({ queryKey: ["team", v.team.id] });
      client.invalidateQueries({ queryKey: ["my-teams"] });
      client.invalidateQueries({ queryKey: ["teams"] });
      client.invalidateQueries({ queryKey: ["users"] });
      client.invalidateQueries({ queryKey: ["me"] });
      // The owner ticks and the repositories move which models and chains a
      // repository uses.
      client.invalidateQueries({ queryKey: ["team-models", v.team.id] });
      client.invalidateQueries({ queryKey: ["team-limits", v.team.id] });
      client.invalidateQueries({ queryKey: ["team-chain", v.team.id] });
      client.invalidateQueries({ queryKey: ["repo-chain"] });
      client.invalidateQueries({ queryKey: ["repo-reviewers"] });
    },
  });
}
