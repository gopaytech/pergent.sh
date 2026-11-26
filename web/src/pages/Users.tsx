import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { TriangleAlertIcon } from "lucide-react";
import { Link } from "react-router";

import { Alert } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { NativeSelect } from "@/components/ui/native-select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

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
import { secretCard, secretValue } from "../OnceSecret";
import { Ago, ErrorBox, Loading, PageHeader, Pill } from "../ui";

// formCard is a card that holds a form of stacked fields.
const formCard = "mb-4 gap-4 px-4 py-4";

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
      <PageHeader
        title="Users"
        description="Add a person by email before they sign in, then put them on a team to give them access."
        actions={
          adding === null && (
            <>
              <Button type="button" size="sm" onClick={() => setAdding("user")}>
                Add user
              </Button>
              <Button type="button" variant="outline" size="sm" onClick={() => setAdding("admin")}>
                Add local admin
              </Button>
            </>
          )
        }
      />
      {signIn.isSuccess && signIn.data.mode === "" && (
        <Alert variant="warn" role="note" className="mb-4">
          <TriangleAlertIcon aria-hidden="true" />
          <span className="prose">
            Sign-in isn't set up, so only local admins can get in.{" "}
            <Link className="font-medium underline underline-offset-4" to="/admin/integrations">
              Set it up on Integrations
            </Link>
          </span>
        </Alert>
      )}
      {adding === "user" && <AddUser onClose={() => setAdding(null)} />}
      {adding === "admin" && <AddAdmin onClose={() => setAdding(null)} />}
      {users.isPending && <Loading />}
      {users.isError && <ErrorBox error={users.error} />}
      {users.isSuccess &&
        (users.data.length === 0 ? (
          <Card className="text-muted-foreground px-4 py-8 text-center text-sm">Nobody yet. Use Add user to give someone access.</Card>
        ) : (
          <Card className="gap-0 overflow-hidden py-0">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>User</TableHead>
                  <TableHead>Sign-in</TableHead>
                  <TableHead>Role</TableHead>
                  <TableHead>Teams</TableHead>
                  <TableHead>Last sign-in</TableHead>
                  <TableHead />
                </TableRow>
              </TableHeader>
              <TableBody>
                {waitingFirst(users.data).map((u) => (
                  <Row
                    key={u.id}
                    user={u}
                    lastLocalAdmin={u.kind === "password" && users.data.filter((x) => x.kind === "password" && x.role === "admin").length === 1}
                  />
                ))}
              </TableBody>
            </Table>
          </Card>
        ))}
    </>
  );
}

// lastLocalAdmin is true for the one local admin left: the server refuses to
// remove it, so the page offers no Remove for it.
function Row({ user, lastLocalAdmin }: { user: UserView; lastLocalAdmin: boolean }) {
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
  // Why Remove is off for this row, or "" when it is offered.
  const keep = self ? "You can't remove your own account." : lastLocalAdmin ? "The last local admin can't be removed." : "";
  const changeRole = (role: "admin" | "user") => {
    const question =
      role === "admin"
        ? `Make ${user.username} an admin? They can change every setting, the models and their keys.`
        : `Make ${user.username} a user? They lose the admin pages at once.`;
    if (window.confirm(question)) setRole.mutate(role);
  };
  const confirmRemove = () => {
    if (window.confirm(`Remove ${user.username}? Their sessions end and they leave every team.`)) remove.mutate();
  };
  return (
    <TableRow>
      <TableCell>
        <div className="flex flex-wrap items-center gap-2">
          <span className="font-medium">{user.username}</span>
          {user.waiting && <Pill tone="warn">waiting</Pill>}
          {user.invited && <Pill tone="grey">invited</Pill>}
        </div>
        {remove.isError && <ErrorBox error={remove.error} />}
        {setRole.isError && <ErrorBox error={setRole.error} />}
      </TableCell>
      <TableCell>{user.kind === "password" ? "Local" : "SSO"}</TableCell>
      <TableCell>
        {user.kind === "sso" && !self ? (
          <NativeSelect
            className="w-28"
            aria-label={`Role of ${user.username}`}
            value={user.role}
            disabled={setRole.isPending}
            onChange={(e) => changeRole(e.target.value as "admin" | "user")}
          >
            <option value="user">user</option>
            <option value="admin">admin</option>
          </NativeSelect>
        ) : (
          user.role
        )}
      </TableCell>
      <TableCell>{user.teams.length === 0 ? <span className="text-muted-foreground">none</span> : user.teams.map((t) => t.name).join(", ")}</TableCell>
      <TableCell className="text-muted-foreground">{user.lastSignInAt ? <Ago t={user.lastSignInAt} /> : "never"}</TableCell>
      <TableCell className="text-right">
        {keep ? (
          <span className="text-muted-foreground" title={keep}>
            {self ? "you" : "last local admin"}
          </span>
        ) : (
          <Button
            type="button"
            variant="destructive-outline"
            size="sm"
            aria-label={`Remove ${user.username}`}
            disabled={remove.isPending}
            onClick={confirmRemove}
          >
            Remove
          </Button>
        )}
      </TableCell>
    </TableRow>
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
    <form aria-label="Add user" onSubmit={submit}>
      <Card className={formCard}>
        <div className="grid gap-2">
          <Label htmlFor="new-user-email">Email</Label>
          <Input
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
          <div className="text-muted-foreground text-sm">
            They can sign in with this email whatever the allowed domains say. They show as invited until they do. They see
            nothing until they are on a team.
          </div>
        </div>
        <fieldset className="m-0 grid gap-2 rounded-md border bg-card px-3 py-2.5">
          <legend className="px-1 text-sm font-medium">Teams</legend>
          {teams.isPending && <Loading />}
          {teams.isError && <ErrorBox error={teams.error} />}
          {teams.isSuccess && teams.data.length === 0 && <div className="text-muted-foreground text-sm">No team yet. Create one on Teams.</div>}
          {teams.isSuccess &&
            teams.data.map((t) => (
              <div className="flex min-h-9 items-center gap-3" key={t.id}>
                <Label className="font-normal">
                  <input
                    type="checkbox"
                    className="accent-primary size-4"
                    checked={chosen.has(t.id)}
                    onChange={(e) => setTeam(t.id, e.target.checked ? "viewer" : null)}
                  />
                  <span>{t.name}</span>
                </Label>
                {chosen.has(t.id) && (
                  <NativeSelect
                    className="w-36"
                    aria-label={`Role on ${t.name}`}
                    value={chosen.get(t.id)}
                    onChange={(e) => setTeam(t.id, e.target.value as MemberRole)}
                  >
                    {teamRoles.map((r) => (
                      <option key={r} value={r}>
                        {r}
                      </option>
                    ))}
                  </NativeSelect>
                )}
              </div>
            ))}
        </fieldset>
        {add.isError && <ErrorBox error={add.error} />}
        <div className="flex justify-end gap-2">
          <Button type="button" variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" disabled={add.isPending}>
            Add
          </Button>
        </div>
      </Card>
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
      <Card role="region" className={secretCard} aria-label="New local admin">
        <p>Local admin {create.data.username} was created. Its password is</p>
        <code className={secretValue}>{create.data.password}</code>
        <p>
          Copy it now: it won't be shown again. They must change it the first time they sign in, at /admin/sign-in.
        </p>
        <div className="flex gap-2">
          <Button type="button" variant="outline" size="sm" onClick={copy}>
            {copied ? "Copied" : "Copy"}
          </Button>
          <Button type="button" size="sm" onClick={done}>
            Done
          </Button>
        </div>
      </Card>
    );
  }
  return (
    <form aria-label="Add local admin" onSubmit={submit}>
      <Card className={formCard}>
        <div className="grid gap-2">
          <Label htmlFor="new-admin-username">Username</Label>
          <Input
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
          <div className="text-muted-foreground text-sm">1 to 64 letters, digits, "." "_" or "-". A password is generated for it.</div>
        </div>
        {create.isError && <ErrorBox error={create.error} />}
        <div className="flex justify-end gap-2">
          <Button type="button" variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" disabled={create.isPending}>
            Create
          </Button>
        </div>
      </Card>
    </form>
  );
}
