import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { useNavigate } from "react-router";

import { api, paths, type TeamView } from "../api";
import { useWrite } from "../layout/write";
import { ErrorBox, Loading } from "../ui";
import TeamsTable, { type TeamRow } from "./TeamsTable";

// toRow counts what a team covers from its entries. A whole instance has no
// project.
function toRow(t: TeamView): TeamRow {
  return {
    id: t.id,
    name: t.name,
    repoCount: t.repos.filter((r) => r.project !== "").length,
    instanceCount: t.repos.filter((r) => r.project === "").length,
    ownedCount: t.repos.filter((r) => r.owner).length,
    memberCount: t.members.length,
  };
}

// Teams lists the teams as a table and makes new ones. A team is changed on
// its own page.
export default function Teams() {
  const teams = useQuery({ queryKey: ["teams"], queryFn: () => api.get<TeamView[]>("/api/teams") });
  return (
    <div className="narrow">
      <div className="top">
        <div>
          <h1>Teams</h1>
          <div className="sub">A team gives each member a role on the repositories it covers. Their highest role counts.</div>
        </div>
      </div>
      <CreateTeam />
      {teams.isPending && <Loading />}
      {teams.isError && <ErrorBox error={teams.error} />}
      {teams.isSuccess &&
        (teams.data.length === 0 ? (
          <div className="card empty">No teams yet. Create one to give people access.</div>
        ) : (
          <TeamsTable rows={teams.data.map(toRow)} showRole={false} />
        ))}
    </div>
  );
}

function CreateTeam() {
  const client = useQueryClient();
  const write = useWrite();
  const navigate = useNavigate();
  const [name, setName] = useState("");
  const create = useMutation({
    mutationFn: () => write<TeamView>("POST", "/api/teams", { name: name.trim() }),
    onSuccess: (team) => {
      setName("");
      client.invalidateQueries({ queryKey: ["teams"] });
      // Members are the first thing to add to a new team.
      navigate(paths.team(team.id, "members"));
    },
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    create.mutate();
  };
  return (
    <form className="card pad inline-form" aria-label="Create a team" onSubmit={submit}>
      <div className="grow">
        <label htmlFor="team-name">Team name</label>
        <input
          id="team-name"
          type="text"
          autoComplete="off"
          maxLength={100}
          value={name}
          onChange={(e) => setName(e.target.value)}
          required
        />
      </div>
      <button type="submit" className="btn primary" disabled={create.isPending}>
        Create team
      </button>
      {create.isError && <ErrorBox error={create.error} />}
    </form>
  );
}
