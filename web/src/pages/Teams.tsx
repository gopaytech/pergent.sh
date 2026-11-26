import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { useNavigate } from "react-router";

import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

import { api, paths, type TeamView } from "../api";
import { useWrite } from "../layout/write";
import { ErrorBox, Loading, PageHeader } from "../ui";
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
  return (
    <div className="max-w-[1100px]">
      <PageHeader
        title="Teams"
        description="A team gives each member a role on the repositories it covers. Their highest role counts."
        actions={
          // The dialog gives the focus back to its trigger when it closes. The
          // trigger is Radix's own button: it needs a ref, which Button (a
          // plain function component on React 18) does not pass on.
          <Dialog open={creating} onOpenChange={setCreating}>
            <DialogTrigger asChild>
              <Button size="sm">+ New team</Button>
            </DialogTrigger>
            <DialogContent showCloseButton={false}>
              <NewTeam />
            </DialogContent>
          </Dialog>
        }
      />
      {teams.isPending && <Loading />}
      {teams.isError && <ErrorBox error={teams.error} />}
      {teams.isSuccess &&
        (teams.data.length === 0 ? (
          <Card className="text-muted-foreground px-4 py-8 text-center text-sm">No teams yet. Use + New team to give people access.</Card>
        ) : (
          <TeamsTable rows={teams.data.map(toRow)} showRole={false} />
        ))}
    </div>
  );
}

// NewTeam is the form of the dialog that names a new team. Members are the
// next thing to add, on the team's own page, so it asks for the name alone.
// Escape, Cancel or a click outside closes the dialog, and the form goes with
// it: the next one starts empty.
function NewTeam() {
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
  const submit = (e: FormEvent) => {
    e.preventDefault();
    create.mutate();
  };
  return (
    <form className="grid gap-4" onSubmit={submit}>
      <DialogHeader>
        <DialogTitle>New team</DialogTitle>
        <DialogDescription>Next you add its members, then its repositories.</DialogDescription>
      </DialogHeader>
      <div className="grid gap-2">
        <Label htmlFor="team-name">Team name</Label>
        <Input
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
      </div>
      {create.isError && <ErrorBox error={create.error} />}
      <DialogFooter>
        <DialogClose asChild>
          <Button type="button" variant="outline">
            Cancel
          </Button>
        </DialogClose>
        <Button type="submit" disabled={create.isPending}>
          Create team
        </Button>
      </DialogFooter>
    </form>
  );
}
