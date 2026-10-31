import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";

import {
  api,
  type InstanceAdminView,
  type RepositoryView,
  type Role,
  type TeamRepoView,
  type TeamUpdate,
  type TeamView,
  type UserView,
} from "../api";
import { useMe } from "../layout/me";
import { useWrite } from "../layout/write";
import { ErrorBox, Loading, Pill, platformName } from "../ui";

const roles: Role[] = ["viewer", "maintainer", "admin"];

const adminText = "Admins see every repository they can see on their platform.";

// hiddenText is how a team's repositories the admin can't see are counted.
const hiddenText = (n: number) => `+${n} repositories you can't see`;

// hiddenMembersText is how a team's members of other instances are counted.
const hiddenMembersText = (n: number) => `+${n} ${n === 1 ? "member" : "members"} from other instances`;

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
          <div className="sub">A team gives its members a role on the repositories it covers. Their highest role counts.</div>
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
  const [role, setRole] = useState<Role>("viewer");
  const create = useMutation({
    mutationFn: () => write<TeamView>("POST", "/api/teams", { name: name.trim(), role }),
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
      <div>
        <label htmlFor="team-role">Role</label>
        <select id="team-role" value={role} onChange={(e) => setRole(e.target.value as Role)}>
          {roles.map((r) => (
            <option key={r} value={r}>
              {r}
            </option>
          ))}
        </select>
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
        <span>
          {team.name} <Pill tone={team.role === "admin" ? "run" : "grey"}>{team.role}</Pill>
        </span>
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
          Members:{" "}
          {team.members.length === 0 && team.hiddenMembers === 0
            ? "none"
            : [
                ...team.members.map((m) => m.username),
                ...(team.hiddenMembers > 0 ? [hiddenMembersText(team.hiddenMembers)] : []),
              ].join(", ")}
        </div>
        <div>
          Repositories:{" "}
          {team.role === "admin"
            ? adminText
            : team.repos.length === 0 && team.hiddenRepos === 0
              ? "none"
              : [...team.repos.map(repoLabel), ...(team.hiddenRepos > 0 ? [hiddenText(team.hiddenRepos)] : [])].join(", ")}
        </div>
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
  const me = useMe();
  const users = useQuery({ queryKey: ["users"], queryFn: () => api.get<UserView[]>("/api/users") });
  const repos = useQuery({ queryKey: ["repositories"], queryFn: () => api.get<RepositoryView[]>("/api/repositories") });
  // An operator may cover any instance; a platform admin its own.
  const instances = useQuery({
    queryKey: ["admin-instances"],
    queryFn: () => api.get<InstanceAdminView[]>("/api/admin/instances"),
    enabled: me.operator,
  });
  if (users.isPending || repos.isPending || (me.operator && instances.isPending)) return <Loading />;
  if (users.isError) return <ErrorBox error={users.error} />;
  if (repos.isError) return <ErrorBox error={repos.error} />;
  if (instances.isError) return <ErrorBox error={instances.error} />;
  const instanceNames = me.operator
    ? (instances.data ?? []).filter((i) => !i.removing).map((i) => i.name)
    : [me.instance];
  return (
    <EditorForm
      team={team}
      accounts={users.data.filter((u) => u.kind === "platform")}
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
  const [role, setRole] = useState<Role>(team.role);
  const [members, setMembers] = useState(() => new Set(team.members.map((m) => m.id)));
  const [covered, setCovered] = useState(() => new Set(team.repos.filter((r) => !isGone(r)).map(repoKey)));
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
  const submit = (e: FormEvent) => {
    e.preventDefault();
    save.mutate({
      name: name.trim(),
      role,
      members: [...members].sort((a, b) => a - b),
      // An admin team has no repository list.
      repos: role === "admin" ? [] : choices.filter((c) => covered.has(repoKey(c))),
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
      <label htmlFor={`team-${team.id}-role`}>Role</label>
      <select id={`team-${team.id}-role`} value={role} onChange={(e) => setRole(e.target.value as Role)}>
        {roles.map((r) => (
          <option key={r} value={r}>
            {r}
          </option>
        ))}
      </select>

      <fieldset>
        <legend>Members</legend>
        {accounts.length === 0 && <div className="meta">Nobody has signed in with GitLab or GitHub yet.</div>}
        {team.hiddenMembers > 0 && (
          <div className="meta">{hiddenMembersText(team.hiddenMembers)} stay as they are when you save.</div>
        )}
        {accounts.map((u) => (
          <label className="check" key={u.id}>
            <input
              type="checkbox"
              checked={members.has(u.id)}
              onChange={(e) => setMembers(toggle(members, u.id, e.target.checked))}
            />
            <span>
              {u.username} <span className="meta">{platformName(u.platform)} · {u.instance}</span>
            </span>
          </label>
        ))}
      </fieldset>

      {role === "admin" ? (
        <div className="meta">{adminText}</div>
      ) : (
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
      )}
      {team.hiddenRepos > 0 && (
        <div className="meta">
          {hiddenText(team.hiddenRepos)}.{" "}
          {role === "admin"
            ? "A team that covers repositories you can't see can't be made an admin team: the server refuses it."
            : "They stay as they are when you save."}
        </div>
      )}
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
