import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardFooter, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { NativeSelect } from "@/components/ui/native-select";
import { cn } from "@/lib/utils";

import { api, ApiError, type AddUserBody, type MemberRole, type TeamMemberView, type TeamPageView, type UserView } from "../api";
import { useWrite } from "../layout/write";
import { ErrorBox, Loading } from "../ui";
import { useTeamSave } from "./teamSave";

const memberRoles: MemberRole[] = ["viewer", "maintainer"];
const MAX_RESULTS = 10;
const asRead = (m: TeamMemberView) => ({ accountId: m.accountId, role: m.role });

// row is one line of a list in a card: a person and what can be done to them.
const row = "flex flex-wrap items-center gap-3 border-b px-4 py-3 text-sm last:border-b-0";

// TeamMembers is the team page's Members tab: each member and role, and for
// an admin a role select, Remove, Add member (a search over the accounts that
// exist) and Add by email. Each change is one save of the team with the
// repositories as read.
export default function TeamMembers({ view }: { view: TeamPageView }) {
  const save = useTeamSave(view);
  const [adding, setAdding] = useState(false);
  const live = view.members.map(asRead);
  const addViewer = (accountId: number) => save.mutate({ members: [...live, { accountId, role: "viewer" }] });
  return (
    <Card role="region" aria-label="Team members" className="gap-0 py-0">
      <CardHeader className="border-b py-4">
        <CardTitle>Members</CardTitle>
      </CardHeader>
      {view.members.length === 0 && <div className="text-muted-foreground px-4 py-3 text-sm">This team has no member yet.</div>}
      {view.members.map((m) => (
        <div className={row} key={m.accountId}>
          <div className="min-w-0 grow">
            <span className="font-medium">{m.username}</span>
            {view.canEdit && m.email !== "" && <span className="text-muted-foreground text-xs"> {m.email}</span>}
          </div>
          {view.canEdit ? (
            <>
              <NativeSelect
                className="w-32"
                aria-label={`Role of ${m.username}`}
                value={m.role}
                disabled={save.isPending}
                onChange={(e) =>
                  save.mutate({ members: live.map((x) => (x.accountId === m.accountId ? { ...x, role: e.target.value as MemberRole } : x)) })
                }
              >
                {memberRoles.map((r) => (
                  <option key={r} value={r}>
                    {r}
                  </option>
                ))}
              </NativeSelect>
              <Button
                type="button"
                variant="destructive-outline"
                size="sm"
                aria-label={`Remove ${m.username}`}
                disabled={save.isPending}
                onClick={() => {
                  // It saves at once: ask first.
                  if (window.confirm(`Remove ${m.username} from this team? They lose the access it gives them now.`)) {
                    save.mutate({ members: live.filter((x) => x.accountId !== m.accountId) });
                  }
                }}
              >
                Remove
              </Button>
            </>
          ) : (
            <Badge variant="outline">{m.role}</Badge>
          )}
        </div>
      ))}
      {save.isError && (
        <div className="px-4 py-3">
          <ErrorBox error={save.error} />
        </div>
      )}
      {view.canEdit && (
        <>
          <CardFooter className="bg-muted/40 justify-end gap-2 border-t py-3">
            <Button type="button" variant="outline" size="sm" onClick={() => setAdding(!adding)}>
              Add member
            </Button>
          </CardFooter>
          {adding && <Search view={view} onAdd={addViewer} busy={save.isPending} />}
          <AddByEmail view={view} onKnown={addViewer} />
        </>
      )}
    </Card>
  );
}

// Search filters the SSO accounts in the browser, by username or email. It
// lists nothing until something is typed, and at most 10 accounts. Only an
// admin gets here, so /api/users is read for an admin only.
function Search({ view, onAdd, busy }: { view: TeamPageView; onAdd: (accountId: number) => void; busy: boolean }) {
  const [text, setText] = useState("");
  const users = useQuery({ queryKey: ["users"], queryFn: () => api.get<UserView[]>("/api/users"), enabled: view.canEdit });
  const q = text.trim().toLowerCase();
  const onTeam = new Set(view.members.map((m) => m.accountId));
  const found =
    q === ""
      ? []
      : (users.data ?? [])
          .filter((u) => u.kind === "sso" && !onTeam.has(u.id) && (u.username.toLowerCase().includes(q) || u.email.toLowerCase().includes(q)))
          .slice(0, MAX_RESULTS);
  return (
    <section className="border-t" aria-label="Add member">
      <div className="px-4 py-3">
        <Input type="search" aria-label="Search" placeholder="Search by name or email" maxLength={100} value={text} onChange={(e) => setText(e.target.value)} />
      </div>
      {users.isPending && <Loading />}
      {users.isError && (
        <div className="px-4 pb-3">
          <ErrorBox error={users.error} />
        </div>
      )}
      {users.data && q !== "" && found.length === 0 && <div className={cn(row, "text-muted-foreground")}>Nobody to add.</div>}
      {found.map((u) => (
        <div className={row} key={u.id}>
          <div className="min-w-0 grow">
            <span className="font-medium">{u.username}</span> <span className="text-muted-foreground text-xs">{u.email}</span>
          </div>
          <Button type="button" variant="outline" size="sm" aria-label={`Add ${u.username}`} disabled={busy} onClick={() => onAdd(u.id)}>
            Add
          </Button>
        </div>
      ))}
    </section>
  );
}

// AddByEmail makes the account (as invited) and puts it on this team in the
// same request, so no save follows. A 409 says the email is added before: the
// account is found in the list and the team is saved with it as a viewer.
function AddByEmail({ view, onKnown }: { view: TeamPageView; onKnown: (accountId: number) => void }) {
  const client = useQueryClient();
  const write = useWrite();
  const [email, setEmail] = useState("");
  // note says why a 409 added nobody: the box is not cleared then.
  const [note, setNote] = useState<{ error: boolean; text: string } | null>(null);
  const add = useMutation({
    mutationFn: async (body: AddUserBody): Promise<UserView | null> => {
      try {
        return await write<UserView>("POST", "/api/admin/users", body);
      } catch (err) {
        if (err instanceof ApiError && err.status === 409) return null;
        throw err;
      }
    },
    onSuccess: async (user, body) => {
      if (user !== null) {
        client.invalidateQueries({ queryKey: ["team", view.team.id] });
        client.invalidateQueries({ queryKey: ["teams"] });
        client.invalidateQueries({ queryKey: ["my-teams"] });
        client.invalidateQueries({ queryKey: ["users"] });
        setEmail("");
        setNote(null);
        return;
      }
      const fresh = await client.fetchQuery({ queryKey: ["users"], queryFn: () => api.get<UserView[]>("/api/users"), staleTime: 0 });
      const found = fresh.find((u) => u.email.toLowerCase() === body.email.toLowerCase());
      if (found === undefined) {
        setNote({ error: true, text: "This email is added already, but its account was not found." });
        return;
      }
      // Someone on the team already keeps their role.
      if (view.members.some((m) => m.accountId === found.id)) {
        setNote({ error: false, text: "Already on this team." });
        return;
      }
      onKnown(found.id);
      setEmail("");
      setNote(null);
    },
  });
  const submit = () => {
    const typed = email.trim();
    if (typed !== "") add.mutate({ email: typed, teams: [{ teamId: view.team.id, role: "viewer" }] });
  };
  return (
    <div className="add-email grid gap-2 border-t px-4 py-4">
      <Label htmlFor={`team-${view.team.id}-email`}>Add by email</Label>
      <div className="flex items-center gap-2">
        <Input
          id={`team-${view.team.id}-email`}
          type="text"
          inputMode="email"
          autoComplete="off"
          autoCapitalize="none"
          spellCheck={false}
          maxLength={254}
          value={email}
          onChange={(e) => {
            setEmail(e.target.value);
            setNote(null);
          }}
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              e.preventDefault();
              submit();
            }
          }}
        />
        <Button type="button" variant="outline" size="sm" disabled={add.isPending} onClick={submit}>
          Add email
        </Button>
      </div>
      <div className="text-muted-foreground text-xs">
        A new email makes the account now, as invited, and puts it on this team as a viewer. It can sign in whatever the allowed domains say.
      </div>
      {note !== null && (
        <div className={note.error ? "text-bad text-sm" : "text-muted-foreground text-sm"} role={note.error ? "alert" : "status"}>
          {note.text}
        </div>
      )}
      {add.isError && <ErrorBox error={add.error} />}
    </div>
  );
}
