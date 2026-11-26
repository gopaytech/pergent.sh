import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState, type FormEvent } from "react";
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

// Teams lists the teams as a table. + New team opens a dialog that names a
// new one. A team is changed on its own page.
export default function Teams() {
  const teams = useQuery({ queryKey: ["teams"], queryFn: () => api.get<TeamView[]>("/api/teams") });
  const [creating, setCreating] = useState(false);
  const opener = useRef<HTMLButtonElement>(null);
  const close = () => {
    setCreating(false);
    opener.current?.focus();
  };
  return (
    <div className="narrow">
      <div className="top">
        <div>
          <h1>Teams</h1>
          <div className="sub">A team gives each member a role on the repositories it covers. Their highest role counts.</div>
        </div>
        <button ref={opener} type="button" className="btn primary" onClick={() => setCreating(true)}>
          + New team
        </button>
      </div>
      {creating && <NewTeam onClose={close} />}
      {teams.isPending && <Loading />}
      {teams.isError && <ErrorBox error={teams.error} />}
      {teams.isSuccess &&
        (teams.data.length === 0 ? (
          <div className="card empty">No teams yet. Use + New team to give people access.</div>
        ) : (
          <TeamsTable rows={teams.data.map(toRow)} showRole={false} />
        ))}
    </div>
  );
}

// NewTeam is the dialog that names a new team. Members are the next thing to
// add, on the team's own page, so the dialog asks for the name alone. Escape,
// Cancel or a click outside it closes it.
function NewTeam({ onClose }: { onClose: () => void }) {
  const client = useQueryClient();
  const write = useWrite();
  const navigate = useNavigate();
  const [name, setName] = useState("");
  const create = useMutation({
    mutationFn: () => write<TeamView>("POST", "/api/teams", { name: name.trim() }),
    onSuccess: (team) => {
      client.invalidateQueries({ queryKey: ["teams"] });
      navigate(paths.team(team.id, "members"));
    },
  });
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [onClose]);
  const submit = (e: FormEvent) => {
    e.preventDefault();
    create.mutate();
  };
  return (
    <div className="modal-backdrop" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <form className="card modal" role="dialog" aria-modal="true" aria-labelledby="new-team-title" onSubmit={submit}>
        <div className="pad">
          <h2 id="new-team-title">New team</h2>
          <label htmlFor="team-name">Team name</label>
          <input
            id="team-name"
            type="text"
            autoComplete="off"
            // The dialog opens to type a name, so the field takes the focus.
            autoFocus
            maxLength={100}
            value={name}
            onChange={(e) => setName(e.target.value)}
            required
          />
          <div className="meta">Next you add its members, then its repositories.</div>
          {create.isError && <ErrorBox error={create.error} />}
        </div>
        <div className="modal-foot">
          <button type="button" className="btn" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="btn primary" disabled={create.isPending}>
            Create team
          </button>
        </div>
      </form>
    </div>
  );
}
