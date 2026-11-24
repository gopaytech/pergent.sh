import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Link, useNavigate } from "react-router";

import { api, paths, type TeamView } from "../api";
import { useWrite } from "../layout/write";
import { ErrorBox, Loading } from "../ui";
import { membersText, teamCountsText } from "./teamText";

// Teams lists the teams with counts and makes new ones. A team is changed on
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
          teams.data.map((t) => <TeamCard key={t.id} team={t} />)
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

function TeamCard({ team }: { team: TeamView }) {
  const client = useQueryClient();
  const write = useWrite();
  const remove = useMutation({
    mutationFn: () => write<void>("DELETE", `/api/teams/${team.id}`),
    onSuccess: () => {
      client.invalidateQueries({ queryKey: ["teams"] });
      client.invalidateQueries({ queryKey: ["users"] });
      // Members may have been waiting-free only through this team.
      client.invalidateQueries({ queryKey: ["me"] });
    },
  });
  const confirmRemove = () => {
    if (window.confirm(`Remove the team ${team.name}? Its members stay, but lose the access it gave them.`)) remove.mutate();
  };
  // A whole instance has no project.
  const counts = {
    repoCount: team.repos.filter((r) => r.project !== "").length,
    instanceCount: team.repos.filter((r) => r.project === "").length,
    ownedCount: team.repos.filter((r) => r.owner).length,
  };
  return (
    <section className="card" aria-label={team.name}>
      <h2>
        <Link to={paths.team(team.id)}>{team.name}</Link>
        <span className="actions-row">
          <button type="button" className="btn small danger" disabled={remove.isPending} onClick={confirmRemove}>
            Remove
          </button>
        </span>
      </h2>
      {remove.isError && <ErrorBox error={remove.error} />}
      <div className="facts">
        <div>{membersText(team.members.length)}</div>
        <div>{teamCountsText(counts)}</div>
      </div>
    </section>
  );
}
