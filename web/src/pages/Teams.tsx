import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";

import {
  api,
  type AddUserBody,
  type InstanceAdminView,
  type MemberRole,
  type RepositoryView,
  type TeamRepoView,
  type TeamUpdate,
  type TeamView,
  type UserView,
} from "../api";
import { useWrite } from "../layout/write";
import { ErrorBox, Loading } from "../ui";

// memberRoles are the roles a member has on a team.
const memberRoles: MemberRole[] = ["viewer", "maintainer"];

// repoLabel names what a team covers: a whole instance or one repository.
const repoLabel = (r: TeamRepoView) => (r.project === "" ? `All repositories of ${r.instance}` : r.project);

// Teams lists the teams, makes new ones, and opens one to change its
// members and the repositories it covers.
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
  const [name, setName] = useState("");
  const create = useMutation({
    mutationFn: () => write<TeamView>("POST", "/api/teams", { name: name.trim() }),
    onSuccess: () => {
      setName("");
      return client.invalidateQueries({ queryKey: ["teams"] });
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
  const [editing, setEditing] = useState(false);
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
  return (
    <section className="card" aria-label={team.name}>
      <h2>
        <span>{team.name}</span>
        <span className="actions-row">
          <button type="button" className="btn small" onClick={() => setEditing(!editing)}>
            {editing ? "Close" : "Edit"}
          </button>
          <button type="button" className="btn small" disabled={remove.isPending} onClick={confirmRemove}>
            Remove
          </button>
        </span>
      </h2>
      {remove.isError && <ErrorBox error={remove.error} />}
      <div className="facts">
        <div>
          Members: {team.members.length === 0 ? "none" : team.members.map((m) => `${m.username} (${m.role})`).join(", ")}
        </div>
        <div>Repositories: {team.repos.length === 0 ? "none" : team.repos.map(repoLabel).join(", ")}</div>
      </div>
      {/* A new answer from the server starts the editor again from it. */}
      {editing && <Editor key={JSON.stringify(team)} team={team} onClose={() => setEditing(false)} />}
    </section>
  );
}

// repoKey identifies a team's repository in the editor's selection.
const repoKey = (r: TeamRepoView) => JSON.stringify([r.instance, r.project]);

// Editor loads what the choices are made from, then shows the form.
function Editor({ team, onClose }: { team: TeamView; onClose: () => void }) {
  const users = useQuery({ queryKey: ["users"], queryFn: () => api.get<UserView[]>("/api/users") });
  const repos = useQuery({ queryKey: ["repositories"], queryFn: () => api.get<RepositoryView[]>("/api/repositories") });
  // A team may cover any instance.
  const instances = useQuery({
    queryKey: ["admin-instances"],
    queryFn: () => api.get<InstanceAdminView[]>("/api/admin/instances"),
  });
  if (users.isPending || repos.isPending || instances.isPending) return <Loading />;
  if (users.isError) return <ErrorBox error={users.error} />;
  if (repos.isError) return <ErrorBox error={repos.error} />;
  if (instances.isError) return <ErrorBox error={instances.error} />;
  const instanceNames = instances.data.filter((i) => !i.removing).map((i) => i.name);
  return (
    <EditorForm
      team={team}
      accounts={users.data.filter((u) => u.kind === "sso")}
      instanceNames={instanceNames}
      switchedOn={repos.data.filter((r) => r.on).map((r) => ({ instance: r.instance, project: r.project }))}
      onClose={onClose}
    />
  );
}

function EditorForm({
  team,
  accounts,
  instanceNames,
  switchedOn,
  onClose,
}: {
  team: TeamView;
  accounts: UserView[];
  instanceNames: string[];
  switchedOn: TeamRepoView[];
  onClose: () => void;
}) {
  const client = useQueryClient();
  const write = useWrite();
  // The choices: whole instances and the repositories switched on, and what
  // the team covers that is neither: shown as switched off and unticked,
  // since saving it would be refused.
  const available: TeamRepoView[] = [...instanceNames.map((instance) => ({ instance, project: "" })), ...switchedOn];
  const gone = team.repos.filter((r) => !available.some((c) => repoKey(c) === repoKey(r)));
  const choices = [...available, ...gone];
  const isGone = (c: TeamRepoView) => gone.some((g) => repoKey(g) === repoKey(c));

  const [name, setName] = useState(team.name);
  // The members: account ID to the role on this team.
  const [members, setMembers] = useState(() => new Map<number, MemberRole>(team.members.map((m) => [m.accountId, m.role])));
  const [covered, setCovered] = useState(() => new Set(team.repos.filter((r) => !isGone(r)).map(repoKey)));
  // Adding by email: the account is made now, as invited, and joins the
  // team when the team is saved.
  const [email, setEmail] = useState("");
  const addEmail = useMutation({
    mutationFn: (body: AddUserBody) => write<UserView>("POST", "/api/admin/users", body),
    onSuccess: (user) => {
      client.invalidateQueries({ queryKey: ["users"] });
      setMember(user.id, "viewer");
      setEmail("");
    },
  });
  const save = useMutation({
    mutationFn: (body: TeamUpdate) => write<TeamView>("PUT", `/api/teams/${team.id}`, body),
    onSuccess: () => {
      client.invalidateQueries({ queryKey: ["teams"] });
      client.invalidateQueries({ queryKey: ["users"] });
      client.invalidateQueries({ queryKey: ["me"] });
      onClose();
    },
  });

  const toggle = <T,>(set: Set<T>, v: T, on: boolean) => {
    const next = new Set(set);
    if (on) next.add(v);
    else next.delete(v);
    return next;
  };
  const setMember = (id: number, role: MemberRole | null) => {
    const next = new Map(members);
    if (role === null) next.delete(id);
    else next.set(id, role);
    setMembers(next);
  };
  // An email that is an account already ticks it; a new one is added.
  const tryAddEmail = () => {
    const typed = email.trim();
    if (typed === "") return;
    const known = accounts.find((u) => u.email.toLowerCase() === typed.toLowerCase());
    if (known) {
      if (!members.has(known.id)) setMember(known.id, "viewer");
      setEmail("");
      return;
    }
    addEmail.mutate({ email: typed });
  };
  const submit = (e: FormEvent) => {
    e.preventDefault();
    save.mutate({
      name: name.trim(),
      members: [...members].map(([accountId, role]) => ({ accountId, role })).sort((a, b) => a.accountId - b.accountId),
      repos: choices.filter((c) => covered.has(repoKey(c))),
    });
  };
  return (
    <form className="pad stack editor" aria-label={`Edit ${team.name}`} onSubmit={submit}>
      <label htmlFor={`team-${team.id}-name`}>Name</label>
      <input
        id={`team-${team.id}-name`}
        type="text"
        autoComplete="off"
        maxLength={100}
        value={name}
        onChange={(e) => setName(e.target.value)}
        required
      />

      <fieldset>
        <legend>Members</legend>
        {accounts.length === 0 && <div className="meta">Nobody yet. Add a person by email below.</div>}
        {accounts.map((u) => (
          <div className="check" key={u.id}>
            <label className="check">
              <input
                type="checkbox"
                checked={members.has(u.id)}
                onChange={(e) => setMember(u.id, e.target.checked ? "viewer" : null)}
              />
              <span>
                {u.username}
                {u.invited && <span className="meta"> (invited)</span>}
              </span>
            </label>
            {members.has(u.id) && (
              <select
                aria-label={`Role of ${u.username}`}
                value={members.get(u.id)}
                onChange={(e) => setMember(u.id, e.target.value as MemberRole)}
              >
                {memberRoles.map((r) => (
                  <option key={r} value={r}>
                    {r}
                  </option>
                ))}
              </select>
            )}
          </div>
        ))}
        <div className="add-email">
          <label htmlFor={`team-${team.id}-email`}>Add by email</label>
          <input
            id={`team-${team.id}-email`}
            type="text"
            inputMode="email"
            autoComplete="off"
            autoCapitalize="none"
            spellCheck={false}
            maxLength={254}
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            // Enter adds the email; it must not save the whole team.
            onKeyDown={(e) => {
              if (e.key === "Enter") {
                e.preventDefault();
                tryAddEmail();
              }
            }}
          />
          <button type="button" className="btn small" disabled={addEmail.isPending} onClick={tryAddEmail}>
            Add email
          </button>
        </div>
        <div className="meta">
          A new email makes the account now, as invited, and it can sign in whatever the allowed domains say. It joins the team
          when you save.
        </div>
        {addEmail.isError && <ErrorBox error={addEmail.error} />}
      </fieldset>

      <fieldset>
        <legend>Repositories</legend>
        {choices.map((c) => (
          <label className="check" key={repoKey(c)}>
            <input
              type="checkbox"
              checked={covered.has(repoKey(c))}
              onChange={(e) => setCovered(toggle(covered, repoKey(c), e.target.checked))}
            />
            <span>
              {c.project === "" ? (
                repoLabel(c)
              ) : (
                <>
                  <span className="mr">{c.project}</span> <span className="meta">{c.instance}</span>
                </>
              )}
              {isGone(c) && <span className="meta"> (switched off)</span>}
            </span>
          </label>
        ))}
        {choices.length === 0 && <div className="meta">No repository is switched on yet.</div>}
      </fieldset>
      {save.isError && <ErrorBox error={save.error} />}
      <div className="actions-row">
        <button type="submit" className="btn primary" disabled={save.isPending}>
          Save
        </button>
        <button type="button" className="btn" onClick={onClose}>
          Cancel
        </button>
      </div>
    </form>
  );
}
