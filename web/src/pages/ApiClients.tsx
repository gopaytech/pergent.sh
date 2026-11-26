import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Link } from "react-router";

import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

import { api, paths, type APIClientCreated, type APIClientInput, type APIClientRow } from "../api";
import { useWrite } from "../layout/write";
import OnceSecret from "../OnceSecret";
import { ErrorBox, Loading, PageHeader } from "../ui";
import { ClientFields, type ClientFieldsValue } from "./ApiClientFields";

export const clientsKey = ["api-clients"];

// ApiClients lists the API clients of the review API and makes new ones. An
// admin's page: the route is behind AdminOnly.
export default function ApiClients() {
  const [adding, setAdding] = useState(false);
  const clients = useQuery({
    queryKey: clientsKey,
    queryFn: () => api.get<{ items: APIClientRow[] }>("/api/admin/api-clients"),
  });
  return (
    <div className="max-w-[1100px]">
      <PageHeader
        title="API clients"
        description="A client is a system that sends changes to pergent.sh to review. It has its own skills, model, keys and limits."
        actions={
          !adding && (
            <Button type="button" size="sm" onClick={() => setAdding(true)}>
              New client
            </Button>
          )
        }
      />
      {adding && <NewClient onClose={() => setAdding(false)} />}
      {clients.isPending && <Loading />}
      {clients.isError && <ErrorBox error={clients.error} />}
      {clients.isSuccess &&
        (clients.data.items.length === 0 ? (
          <Card className="text-muted-foreground px-4 py-8 text-center text-sm">
            No API clients yet. Make one to let a system send changes for review.
          </Card>
        ) : (
          <Card className="gap-0 overflow-hidden py-0">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Client</TableHead>
                  <TableHead className="text-right">Keys</TableHead>
                  <TableHead className="text-right">Reviews this month</TableHead>
                  <TableHead className="text-right">Failed this month</TableHead>
                  <TableHead className="text-right">Tokens this month</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {clients.data.items.map((c) => (
                  <TableRow key={c.name}>
                    <TableCell>
                      <Link className="text-foreground font-medium no-underline hover:underline" to={paths.clientPage(c.name)}>
                        {c.name}
                      </Link>
                    </TableCell>
                    <TableCell className="text-right tabular-nums">{c.keys}</TableCell>
                    <TableCell className="text-right tabular-nums">{c.reviewsThisMonth}</TableCell>
                    <TableCell className="text-right tabular-nums">{c.failedThisMonth}</TableCell>
                    <TableCell className="text-right tabular-nums">{c.tokensThisMonth.toLocaleString("en-US")}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </Card>
        ))}
    </div>
  );
}

// NewClient makes a client and shows its callback secret once. Limits are
// left out of the body, so the client gets the defaults; they are changed on
// its page.
function NewClient({ onClose }: { onClose: () => void }) {
  const client = useQueryClient();
  const write = useWrite();
  const [name, setName] = useState("");
  const [fields, setFields] = useState<ClientFieldsValue>({ skills: [], modelId: "", callbackUrl: "" });
  const create = useMutation({
    mutationFn: (body: APIClientInput) => write<APIClientCreated>("POST", "/api/admin/api-clients", body),
    // The answer holds the secret: it goes with the box, not after 5 minutes.
    gcTime: 0,
    onSuccess: () => client.invalidateQueries({ queryKey: clientsKey }),
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    create.mutate({ name: name.trim(), skills: fields.skills, modelId: fields.modelId, callbackUrl: fields.callbackUrl.trim() });
  };
  const done = () => {
    create.reset();
    onClose();
  };

  if (create.isSuccess) {
    const made = create.data.client.name;
    return (
      <OnceSecret title="New API client" what="callback secret" secret={create.data.callbackSecret} onDone={done}>
        <p>This is the callback secret of {made}: it signs the callbacks pergent.sh sends. Make its first key on its page.</p>
        <p>
          <Link className="font-medium underline underline-offset-4" to={paths.clientPage(made)}>
            Open {made}
          </Link>
        </p>
      </OnceSecret>
    );
  }
  return (
    <form aria-label="New API client form" onSubmit={submit}>
      <Card className="mb-4 gap-4 px-4 py-4">
        <div className="grid gap-2">
          <Label htmlFor="client-name">Name</Label>
          <Input
            id="client-name"
            type="text"
            autoComplete="off"
            autoCapitalize="none"
            spellCheck={false}
            maxLength={63}
            value={name}
            onChange={(e) => setName(e.target.value)}
            required
          />
          <div className="text-muted-foreground text-sm">1 to 63 characters: a-z, 0-9 and -. It never changes.</div>
        </div>
        <ClientFields value={fields} onChange={setFields} />
        {create.isError && <ErrorBox error={create.error} />}
        <div className="flex gap-2">
          <Button type="submit" disabled={create.isPending}>
            Create client
          </Button>
          <Button type="button" variant="outline" onClick={onClose}>
            Cancel
          </Button>
        </div>
      </Card>
    </form>
  );
}
