import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router";

import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

import { api, isStatus, paths, type TeamPageView, type TeamTab } from "../api";
import { useMe } from "../layout/me";
import { useWrite } from "../layout/write";
import { ErrorBox, Loading, NotFound, PageHeader, Tabs } from "../ui";
import TeamLimits from "./TeamLimits";
import TeamMembers from "./TeamMembers";
import TeamModelsTab from "./TeamModels";
import TeamRepositories from "./TeamRepositories";
import TeamUsage from "./TeamUsage";
import { useTeamSave } from "./teamSave";

// Team is one team's page in the user area: its repositories, members,
// models, limits and usage. Members and admins see it; anyone else gets Not
// found. Limits and Usage are for maintainers and admins. The tab is in the
// address (?tab=), and Repositories is the default.
export default function Team() {
  const params = useParams();
  const id = /^[1-9]\d{0,15}$/.test(params.id ?? "") ? Number(params.id) : null;
  const page = useQuery({
    queryKey: ["team", id],
    queryFn: () => api.get<TeamPageView>(paths.apiTeam(id!)),
    enabled: id !== null,
  });
  const [search, setSearch] = useSearchParams();
  const me = useMe();
  if (id === null || isStatus(page.error, 404)) return <NotFound />;
  if (page.isPending) return <Loading />;
  if (page.isError) return <ErrorBox error={page.error} />;

  const v = page.data;
  const tabs: [TeamTab, string][] = [
    ["repositories", "Repositories"],
    ["members", "Members"],
    ["models", "Models"],
  ];
  // A viewer of the team sees no limits and no usage: the server answers 403.
  const maintains = v.role !== "viewer";
  if (maintains) tabs.push(["limits", "Limits"], ["usage", "Usage"]);
  const asked = search.get("tab") as TeamTab | null;
  const tab: TeamTab = tabs.some(([t]) => t === asked) ? (asked as TeamTab) : "repositories";

  return (
    <div className="max-w-[1100px]">
      <div className="text-muted-foreground mb-1.5 text-xs">
        <Link className="text-muted-foreground hover:underline" to="/teams">
          Teams
        </Link>{" "}
        / {v.team.name}
      </div>
      <PageHeader title={v.team.name} description={`Your role: ${v.role}`} className="mb-4" />
      {v.canEdit && <AdminActions v={v} />}
      {/* A maintainer adds repositories through the picker, for this team. */}
      {v.role === "maintainer" && me.onboardInstances.length > 0 && (
        <div className="mb-4 flex flex-wrap items-center gap-2">
          <Button asChild size="sm">
            <Link to={`/repos?add=1&team=${v.team.id}`}>+ Add repository</Link>
          </Button>
        </div>
      )}
      <Tabs<TeamTab> tabs={tabs} value={tab} onChange={(t) => setSearch(t === "repositories" ? {} : { tab: t }, { replace: true })} />
      <div role="tabpanel" className="flex flex-col gap-4">
        {tab === "repositories" && <TeamRepositories view={v} />}
        {tab === "members" && <TeamMembers view={v} />}
        {tab === "models" && <TeamModelsTab team={v.team.id} />}
        {tab === "limits" && <TeamLimits team={v.team.id} role={v.role} canEdit={maintains} />}
        {tab === "usage" && <TeamUsage team={v.team.id} />}
      </div>
    </div>
  );
}

// AdminActions are the team's Rename and Remove team, for an admin.
function AdminActions({ v }: { v: TeamPageView }) {
  const client = useQueryClient();
  const write = useWrite();
  const navigate = useNavigate();
  const [renaming, setRenaming] = useState(false);
  const [name, setName] = useState(v.team.name);
  const save = useTeamSave(v);
  const remove = useMutation({
    mutationFn: () => write<void>("DELETE", paths.apiTeam(v.team.id)),
    onSuccess: () => {
      client.invalidateQueries({ queryKey: ["teams"] });
      client.invalidateQueries({ queryKey: ["my-teams"] });
      // The repository list shows owner team names.
      client.invalidateQueries({ queryKey: ["repositories"] });
      client.invalidateQueries({ queryKey: ["users"] });
      // Members may have been waiting-free only through this team.
      client.invalidateQueries({ queryKey: ["me"] });
      navigate("/admin/teams");
    },
  });
  const confirmRemove = () => {
    if (window.confirm(`Remove the team ${v.team.name}? Its members stay, but lose the access it gave them.`)) remove.mutate();
  };
  const submit = (e: FormEvent) => {
    e.preventDefault();
    save.mutate({ name: name.trim() }, { onSuccess: () => setRenaming(false) });
  };
  return (
    <div className="team-actions mb-4 flex flex-col gap-3">
      <div className="flex flex-wrap items-center gap-2">
        <Button type="button" variant="outline" size="sm" onClick={() => setRenaming(!renaming)}>
          Rename
        </Button>
        <Button type="button" variant="destructive-outline" size="sm" disabled={remove.isPending} onClick={confirmRemove}>
          Remove team
        </Button>
      </div>
      {renaming && (
        <Card>
          <form className="flex items-end gap-3 px-4" aria-label="Rename the team" onSubmit={submit}>
            <div className="grid min-w-0 grow gap-2">
              <Label htmlFor="team-rename">Name</Label>
              <Input
                id="team-rename"
                type="text"
                autoComplete="off"
                maxLength={100}
                value={name}
                onChange={(e) => setName(e.target.value)}
                required
              />
            </div>
            <Button type="submit" disabled={save.isPending}>
              Save
            </Button>
          </form>
        </Card>
      )}
      {save.isError && <ErrorBox error={save.error} />}
      {remove.isError && <ErrorBox error={remove.error} />}
    </div>
  );
}
