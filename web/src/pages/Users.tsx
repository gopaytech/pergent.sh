import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Link } from "react-router";

import {
  api,
  type AddUserBody,
  type MemberRole,
  type NewAccountView,
  type SignInAdminView,
  type TeamView,
  type UserView,
} from "../api";
import { useMe } from "../layout/me";
import { useWrite } from "../layout/write";
import { ErrorBox, Loading, Pill, ago } from "../ui";

// waitingFirst puts the accounts on no team first, keeping the order of the
// rest.
const waitingFirst = (users: UserView[]) => [...users.filter((u) => u.waiting), ...users.filter((u) => !u.waiting)];

// Users lists the accounts, the ones waiting for a team first. An admin adds
// a person by email, before they have ever signed in, or a local admin.
export default function Users() {
  const [adding, setAdding] = useState<"user" | "admin" | null>(null);
  const users = useQuery({ queryKey: ["users"], queryFn: () => api.get<UserView[]>("/api/users") });
  // A failure to read the settings says nothing: the notice only shows when
  // the answer is that no sign-in is set up.
  const signIn = useQuery({ queryKey: ["admin-sign-in"], queryFn: () => api.get<SignInAdminView>("/api/admin/sign-in") });
  return (
    <>
      <div className="top">
        <div>
          <h1>Users</h1>
          <div className="sub">Add a person by email before they sign in, then put them on a team to give them access.</div>
        </div>
        {adding === null && (
          <div className="actions-row">
            <button type="button" className="btn primary" onClick={() => setAdding("user")}>
              Add user
            </button>
            <button type="button" className="btn" onClick={() => setAdding("admin")}>
              Add local admin
            </button>
          </div>
        )}
      </div>
      {signIn.isSuccess && signIn.data.mode === "" && (
        <div className="alert" role="note">
          <span aria-hidden="true">⚠️</span>
          <span className="prose">
            Sign-in isn't set up, so only local admins can get in. <Link to="/admin/integrations">Set it up on Integrations</Link>
          </span>
        </div>
      )}
      {adding === "user" && <AddUser onClose={() => setAdding(null)} />}
      {adding === "admin" && <AddAdmin onClose={() => setAdding(null)} />}
      {users.isPending && <Loading />}
      {users.isError && <ErrorBox error={users.error} />}
      {users.isSuccess &&
        (users.data.length === 0 ? (
          <div className="card empty">Nobody yet. Use Add user to give someone access.</div>
        ) : (
          <div className="card scroll">
            <table className="table">
              <thead>
                <tr>
                  <th>User</th>
                  <th>Sign-in</th>
                  <th>Role</th>
                  <th>Teams</th>
                  <th>Last sign-in</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {waitingFirst(users.data).map((u) => (
                  <Row key={u.id} user={u} />
                ))}
              </tbody>
            </table>
          </div>
        ))}
    </>
  );
}

function Row({ user }: { user: UserView }) {
  const client = useQueryClient();
  const write = useWrite();
  const me = useMe();
  // The server refuses a change of your own role, so there is no choice here.
  const self = user.kind === me.kind && user.username === me.username;
  const refresh = () => {
    client.invalidateQueries({ queryKey: ["users"] });
    // The menu's count of waiting accounts.
    client.invalidateQueries({ queryKey: ["me"] });
  };
  const remove = useMutation({
    mutationFn: () => write<void>("DELETE", `/api/users/${user.id}`),
    onSuccess: () => {
      refresh();
      client.invalidateQueries({ queryKey: ["teams"] });
    },
  });
  const setRole = useMutation({
    mutationFn: (role: "admin" | "user") => write<void>("PUT", `/api/admin/users/${user.id}/role`, { role }),
    onSuccess: refresh,
  });
  const confirmRemove = () => {
    if (window.confirm(`Remove ${user.username}? Their sessions end and they leave every team.`)) remove.mutate();
  };
  return (
    <tr>
      <td>
        {user.username}
        {user.waiting && <Pill tone="warn">waiting</Pill>}
        {user.invited && <Pill tone="grey">invited</Pill>}
        {remove.isError && <ErrorBox error={remove.error} />}
        {setRole.isError && <ErrorBox error={setRole.error} />}
      </td>
      <td>{user.kind === "password" ? "Local" : "SSO"}</td>
      <td>
        {user.kind === "sso" && !self ? (
          <select
            aria-label={`Role of ${user.username}`}
            value={user.role}
            disabled={setRole.isPending}
            onChange={(e) => setRole.mutate(e.target.value as "admin" | "user")}
          >
            <option value="user">user</option>
            <option value="admin">admin</option>
          </select>
        ) : (
          user.role
        )}
      </td>
      <td>{user.teams.length === 0 ? <span className="meta">none</span> : user.teams.map((t) => t.name).join(", ")}</td>
      <td>{user.lastSignInAt ? ago(user.lastSignInAt) : "never"}</td>
      <td className="num">
        <button type="button" className="btn small" disabled={remove.isPending} onClick={confirmRemove}>
          Remove
        </button>
      </td>
    </tr>
  );
}

// teamRoles are the roles an added user can have on a team.
const teamRoles: MemberRole[] = ["viewer", "maintainer"];

// AddUser gives a person access by email: the account shows as invited until
// they sign in, on the teams ticked here, each with its role.
function AddUser({ onClose }: { onClose: () => void }) {
  const client = useQueryClient();
  const write = useWrite();
  const [email, setEmail] = useState("");
  // The teams ticked: team ID to the role on it.
  const [chosen, setChosen] = useState(() => new Map<number, MemberRole>());
  const teams = useQuery({ queryKey: ["teams"], queryFn: () => api.get<TeamView[]>("/api/teams") });
  const add = useMutation({
    mutationFn: (body: AddUserBody) => write<UserView>("POST", "/api/admin/users", body),
    onSuccess: () => {
      client.invalidateQueries({ queryKey: ["users"] });
      client.invalidateQueries({ queryKey: ["teams"] });
      client.invalidateQueries({ queryKey: ["me"] });
      onClose();
    },
  });
  const setTeam = (id: number, role: MemberRole | null) => {
    const next = new Map(chosen);
    if (role === null) next.delete(id);
    else next.set(id, role);
    setChosen(next);
  };
  const submit = (e: FormEvent) => {
    e.preventDefault();
    const picked = [...chosen].map(([teamId, role]) => ({ teamId, role })).sort((a, b) => a.teamId - b.teamId);
    add.mutate({ email: email.trim(), ...(picked.length > 0 ? { teams: picked } : {}) });
  };
  return (
    <form className="card pad stack" aria-label="Add user" onSubmit={submit}>
      <label htmlFor="new-user-email">Email</label>
      <input
        id="new-user-email"
        type="text"
        inputMode="email"
        autoComplete="off"
        autoCapitalize="none"
        spellCheck={false}
        maxLength={254}
        value={email}
        onChange={(e) => setEmail(e.target.value)}
        required
      />
      <div className="meta">
        They can sign in with this email whatever the allowed domains say. They show as invited until they do. They see nothing
        until they are on a team.
      </div>
      <fieldset>
        <legend>Teams</legend>
        {teams.isPending && <Loading />}
        {teams.isError && <ErrorBox error={teams.error} />}
        {teams.isSuccess && teams.data.length === 0 && <div className="meta">No team yet. Create one on Teams.</div>}
        {teams.isSuccess &&
          teams.data.map((t) => (
            <div className="check" key={t.id}>
              <label className="check">
                <input
                  type="checkbox"
                  checked={chosen.has(t.id)}
                  onChange={(e) => setTeam(t.id, e.target.checked ? "viewer" : null)}
                />
                <span>{t.name}</span>
              </label>
              {chosen.has(t.id) && (
                <select
                  aria-label={`Role on ${t.name}`}
                  value={chosen.get(t.id)}
                  onChange={(e) => setTeam(t.id, e.target.value as MemberRole)}
                >
                  {teamRoles.map((r) => (
                    <option key={r} value={r}>
                      {r}
                    </option>
                  ))}
                </select>
              )}
            </div>
          ))}
      </fieldset>
      {add.isError && <ErrorBox error={add.error} />}
      <div className="actions-row">
        <button type="submit" className="btn primary" disabled={add.isPending}>
          Add
        </button>
        <button type="button" className="btn" onClick={onClose}>
          Cancel
        </button>
      </div>
    </form>
  );
}

// AddAdmin makes a local admin and shows its generated password once.
function AddAdmin({ onClose }: { onClose: () => void }) {
  const client = useQueryClient();
  const write = useWrite();
  const [username, setUsername] = useState("");
  const [copied, setCopied] = useState(false);
  const create = useMutation({
    mutationFn: () => write<NewAccountView>("POST", "/api/admin/accounts", { username: username.trim() }),
    // The answer holds the password: it goes with the box, not after 5
    // minutes.
    gcTime: 0,
    onSuccess: () => client.invalidateQueries({ queryKey: ["users"] }),
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    create.mutate();
  };
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(create.data?.password ?? "");
      setCopied(true);
    } catch {
      // No clipboard here: the password is on screen to select.
    }
  };
  const done = () => {
    create.reset();
    onClose();
  };

  if (create.isSuccess) {
    return (
      <section className="card pad secret-box" aria-label="New local admin">
        <p>Local admin {create.data.username} was created. Its password is</p>
        <code className="secret-value">{create.data.password}</code>
        <p>
          Copy it now: it won't be shown again. They must change it the first time they sign in, at /admin/sign-in.
        </p>
        <div className="actions-row">
          <button type="button" className="btn" onClick={copy}>
            {copied ? "Copied" : "Copy"}
          </button>
          <button type="button" className="btn primary" onClick={done}>
            Done
          </button>
        </div>
      </section>
    );
  }
  return (
    <form className="card pad stack" aria-label="Add local admin" onSubmit={submit}>
      <label htmlFor="new-admin-username">Username</label>
      <input
        id="new-admin-username"
        type="text"
        autoComplete="off"
        autoCapitalize="none"
        spellCheck={false}
        maxLength={64}
        value={username}
        onChange={(e) => setUsername(e.target.value)}
        required
      />
      <div className="meta">1 to 64 letters, digits, "." "_" or "-". A password is generated for it.</div>
      {create.isError && <ErrorBox error={create.error} />}
      <div className="actions-row">
        <button type="submit" className="btn primary" disabled={create.isPending}>
          Create
        </button>
        <button type="button" className="btn" onClick={onClose}>
          Cancel
        </button>
      </div>
    </form>
  );
}
