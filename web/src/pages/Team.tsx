import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Link, useNavigate, useParams, useSearchParams } from "react-router";

import { api, isStatus, paths, type TeamPageView, type TeamTab } from "../api";
import { useWrite } from "../layout/write";
import { ErrorBox, Loading, NotFound, Tabs } from "../ui";
import TeamLimits from "./TeamLimits";
import TeamMembers from "./TeamMembers";
import TeamModelsTab from "./TeamModels";
import TeamRepositories from "./TeamRepositories";
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
    <div className="narrow">
      <div className="crumbs">
        <Link to="/teams">Teams</Link> / {v.team.name}
      </div>
      <h1>{v.team.name}</h1>
      <div className="sub spaced">Your role: {v.role}</div>
      {v.canEdit && <AdminActions v={v} />}
      <Tabs<TeamTab> tabs={tabs} value={tab} onChange={(t) => setSearch(t === "repositories" ? {} : { tab: t }, { replace: true })} />
      <div role="tabpanel">
        {tab === "repositories" && <TeamRepositories view={v} />}
        {tab === "members" && <TeamMembers view={v} />}
        {tab === "models" && <TeamModelsTab team={v.team.id} />}
        {tab === "limits" && <TeamLimits team={v.team.id} role={v.role} canEdit={maintains} />}
        {tab === "usage" && <Placeholder name="Usage" />}
      </div>
    </div>
  );
}

// Placeholder holds the place of a tab that is not built yet.
function Placeholder({ name }: { name: string }) {
  return (
    <section className="card" aria-label={name}>
      <h2>{name}</h2>
      <div className="pad meta">Not available yet.</div>
    </section>
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
    <div className="stack">
      <div className="actions-row">
        <button type="button" className="btn small" onClick={() => setRenaming(!renaming)}>
          Rename
        </button>
        <button type="button" className="btn small" disabled={remove.isPending} onClick={confirmRemove}>
          Remove team
        </button>
      </div>
      {renaming && (
        <form className="card pad inline-form" aria-label="Rename the team" onSubmit={submit}>
          <div className="grow">
            <label htmlFor="team-rename">Name</label>
            <input
              id="team-rename"
              type="text"
              autoComplete="off"
              maxLength={100}
              value={name}
              onChange={(e) => setName(e.target.value)}
              required
            />
          </div>
          <button type="submit" className="btn primary" disabled={save.isPending}>
            Save
          </button>
        </form>
      )}
      {save.isError && <ErrorBox error={save.error} />}
      {remove.isError && <ErrorBox error={remove.error} />}
    </div>
  );
}
