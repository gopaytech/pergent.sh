import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";

import { api, type NewAccountView, type UserView } from "../api";
import { useMe } from "../layout/me";
import { useWrite } from "../layout/write";
import { ErrorBox, Loading, Pill, ago, platformName } from "../ui";

// waitingFirst puts the accounts on no team first, keeping the order of the
// rest.
const waitingFirst = (users: UserView[]) => [...users.filter((u) => u.waiting), ...users.filter((u) => !u.waiting)];

// Users lists the accounts, the ones waiting for a team first. An operator
// adds password admins; a platform admin never touches those.
export default function Users() {
  const me = useMe();
  const [adding, setAdding] = useState(false);
  const users = useQuery({ queryKey: ["users"], queryFn: () => api.get<UserView[]>("/api/users") });
  return (
    <>
      <div className="top">
        <div>
          <h1>Users</h1>
          <div className="sub">
            {me.operator
              ? "Everyone who has signed in. Add them to a team to give them access."
              : `Everyone who has signed in to ${me.instance}. Add them to a team to give them access.`}
          </div>
        </div>
        {me.operator && !adding && (
          <button type="button" className="btn primary" onClick={() => setAdding(true)}>
            Add password admin
          </button>
        )}
      </div>
      {adding && <AddAdmin onClose={() => setAdding(false)} />}
      {users.isPending && <Loading />}
      {users.isError && <ErrorBox error={users.error} />}
      {users.isSuccess &&
        (users.data.length === 0 ? (
          <div className="card empty">Nobody has signed in yet.</div>
        ) : (
          <div className="card scroll">
            <table className="table">
              <thead>
                <tr>
                  <th>Username</th>
                  <th>Account</th>
                  <th>Teams</th>
                  <th>Last sign-in</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {waitingFirst(users.data).map((u) => (
                  <Row key={u.id} user={u} canRemove={u.kind === "platform" || me.operator} />
                ))}
              </tbody>
            </table>
          </div>
        ))}
    </>
  );
}

function Row({ user, canRemove }: { user: UserView; canRemove: boolean }) {
  const client = useQueryClient();
  const write = useWrite();
  const remove = useMutation({
    mutationFn: () => write<void>("DELETE", `/api/users/${user.id}`),
    onSuccess: () => {
      client.invalidateQueries({ queryKey: ["users"] });
      // The menu's count of waiting accounts.
      client.invalidateQueries({ queryKey: ["me"] });
      client.invalidateQueries({ queryKey: ["teams"] });
    },
  });
  const confirmRemove = () => {
    if (window.confirm(`Remove ${user.username}? Their sessions end and they leave every team.`)) remove.mutate();
  };
  return (
    <tr>
      <td>
        {user.username}
        {user.waiting && <Pill tone="warn">waiting</Pill>}
        {remove.isError && <ErrorBox error={remove.error} />}
      </td>
      <td>{user.kind === "password" ? "Password admin" : `${platformName(user.platform)} · ${user.instance}`}</td>
      <td>{user.teams.length === 0 ? <span className="meta">none</span> : user.teams.map((t) => t.name).join(", ")}</td>
      <td>{user.lastSignInAt ? ago(user.lastSignInAt) : "never"}</td>
      <td className="num">
        {canRemove && (
          <button type="button" className="btn small" disabled={remove.isPending} onClick={confirmRemove}>
            Remove
          </button>
        )}
      </td>
    </tr>
  );
}

// AddAdmin makes a password admin and shows its generated password once.
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
      <section className="card pad secret-box" aria-label="New password admin">
        <p>Password admin {create.data.username} was created. Its password is</p>
        <code className="secret-value">{create.data.password}</code>
        <p>
          Copy it now: it won't be shown again. They must change it the first time they sign in.
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
    <form className="card pad stack" aria-label="Add password admin" onSubmit={submit}>
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
