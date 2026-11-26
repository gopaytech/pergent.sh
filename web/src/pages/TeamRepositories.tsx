import { useInfiniteQuery } from "@tanstack/react-query";
import { useMemo, useState, type FormEvent } from "react";
import { Link } from "react-router";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardFooter, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { cn } from "@/lib/utils";

import { api, paths, type RepositoryChoiceView, type RepositoryChoicesView, type TeamEntryView, type TeamPageView } from "../api";
import { ErrorBox, Loading } from "../ui";
import { useTeamSave } from "./teamSave";

const entryLabel = (e: { instance: string; project: string }) => (e.project === "" ? `All repositories of ${e.instance}` : e.project);
const asRead = (e: TeamEntryView) => ({ instance: e.instance, project: e.project, owner: e.owner });

// row is one line of a list in a card: an entry and what can be done to it.
const row = "flex flex-wrap items-center gap-3 border-b px-4 py-3 text-sm last:border-b-0";
// muted is the small grey text after a name.
const muted = "text-muted-foreground text-xs";
// checkbox is a plain tick box: there is no shadcn one on this page.
const checkbox = "accent-primary size-4";

// TeamRepositories is the team page's Repositories tab: the team's entries,
// and for an admin the owner tick, Remove, and a search to add a repository.
// Each change is one save of the team with the rest as read.
export default function TeamRepositories({ view }: { view: TeamPageView }) {
  const save = useTeamSave(view);
  const [adding, setAdding] = useState(false);
  // The entries that stay: one that is removed is dropped by the next save.
  const live = view.repos.filter((r) => !r.removed).map(asRead);
  const same = (a: { instance: string; project: string }, b: { instance: string; project: string }) =>
    a.instance === b.instance && a.project === b.project;
  return (
    <Card role="region" aria-label="Team repositories" className="gap-0 py-0">
      <CardHeader className="border-b py-4">
        <CardTitle>Repositories</CardTitle>
      </CardHeader>
      {view.repos.length === 0 && <div className="text-muted-foreground px-4 py-3 text-sm">This team covers no repository yet.</div>}
      {view.repos.map((r) => (
        <div className={row} key={`${r.instance}/${r.project}`}>
          <div className="min-w-0 grow">
            {r.project === "" ? (
              <span>{entryLabel(r)}</span>
            ) : (
              <Link className="text-foreground font-medium hover:underline" to={paths.repo(r.instance, r.project)}>
                {r.project}
              </Link>
            )}{" "}
            <span className={muted}>{r.instance}</span>
            {r.owner && (
              <>
                {" "}
                <Badge variant="secondary">owner</Badge>
              </>
            )}
            {r.paused && (
              <span className={muted}>
                {" "}
                (<Link to={paths.repo(r.instance, r.project)}>paused</Link>)
              </span>
            )}
            {r.removed && (
              <>
                {" "}
                <span className={muted}>(removed)</span> <span className={muted}>The next save drops it.</span>
              </>
            )}
          </div>
          {view.canEdit && !r.removed && (
            <>
              <Label className="font-normal">
                <input
                  type="checkbox"
                  className={checkbox}
                  aria-label={`Owner of ${entryLabel(r)}`}
                  checked={r.owner}
                  disabled={save.isPending}
                  onChange={(e) => save.mutate({ repos: live.map((x) => (same(x, r) ? { ...x, owner: e.target.checked } : x)) })}
                />
                Owner
              </Label>
              <Button
                type="button"
                variant="destructive-outline"
                size="sm"
                aria-label={`Remove ${entryLabel(r)}`}
                disabled={save.isPending}
                onClick={() => {
                  // It saves at once: ask first, and say what the team stops giving.
                  if (window.confirm(`Remove ${entryLabel(r)} from this team? Its members lose the access it gives, and it stops using this team's models.`)) {
                    save.mutate({ repos: live.filter((x) => !same(x, r)) });
                  }
                }}
              >
                Remove
              </Button>
            </>
          )}
        </div>
      ))}
      {save.isError && (
        <div className="px-4 py-3">
          <ErrorBox error={save.error} />
        </div>
      )}
      {view.canEdit && (
        <CardFooter className="bg-muted/40 justify-end gap-2 border-t py-3">
          <Button type="button" variant="outline" size="sm" onClick={() => setAdding(!adding)}>
            Add repository
          </Button>
        </CardFooter>
      )}
      {view.canEdit && adding && <Search view={view} onAdd={(c, owner) => save.mutate({ repos: [...live, { instance: c.instance, project: c.project, owner }] })} busy={save.isPending} />}
    </Card>
  );
}

// Search reads the repositories an admin may add, only once a search is
// sent, 50 at a time (page 1 also holds the whole-instance entries).
function Search({ view, onAdd, busy }: { view: TeamPageView; onAdd: (c: RepositoryChoiceView, owner: boolean) => void; busy: boolean }) {
  const [text, setText] = useState("");
  const [sent, setSent] = useState<string | null>(null);
  const [owners, setOwners] = useState<Record<string, boolean>>({});
  const results = useInfiniteQuery({
    queryKey: ["team-repository-choices", view.team.id, sent],
    queryFn: ({ pageParam }) => api.get<RepositoryChoicesView>(paths.apiTeamRepositoryChoices(view.team.id, sent ?? "", pageParam)),
    initialPageParam: 1,
    getNextPageParam: (last) => (last.hasMore ? last.page + 1 : undefined),
    staleTime: 0,
    refetchOnWindowFocus: false,
    enabled: sent !== null,
  });
  // An entry that came on two pages shows once.
  const items = useMemo(() => {
    const seen = new Set<string>();
    return (results.data?.pages ?? []).flatMap((p) => p.items).filter((c) => {
      const k = `${c.instance}/${c.project}`;
      if (seen.has(k)) return false;
      seen.add(k);
      return true;
    });
  }, [results.data]);
  const submit = (e: FormEvent) => {
    e.preventDefault();
    setSent(text.trim());
  };
  return (
    <section className="border-t" aria-label="Add repository">
      <form className="flex items-center gap-2 px-4 py-3" onSubmit={submit}>
        <Input type="search" aria-label="Search" placeholder="Search by path" maxLength={100} value={text} onChange={(e) => setText(e.target.value)} />
        <Button type="submit" variant="outline" size="sm">
          Search
        </Button>
      </form>
      {sent !== null && results.isPending && <Loading />}
      {results.isError && !results.isFetchNextPageError && (
        <div className="px-4 pb-3">
          <ErrorBox error={results.error} />
        </div>
      )}
      {results.data && items.length === 0 && !results.hasNextPage && <div className={cn(row, "text-muted-foreground")}>Nothing to add.</div>}
      {items.map((c) => {
        const key = `${c.instance}/${c.project}`;
        const label = entryLabel(c);
        return (
          <div className={row} key={key}>
            <div className="min-w-0 grow">
              <span className="font-medium">{label}</span> <span className={muted}>{c.instance}</span>
              {c.paused && <span className={muted}> (paused)</span>}
              {c.ownedBy && <span className={muted}> owned by {c.ownedBy.name}</span>}
            </div>
            <Label className="font-normal">
              <input
                type="checkbox"
                className={checkbox}
                aria-label={`Owner of ${label}`}
                checked={owners[key] ?? false}
                disabled={c.ownedBy !== null}
                onChange={(e) => setOwners({ ...owners, [key]: e.target.checked })}
              />
              Owner
            </Label>
            <Button type="button" variant="outline" size="sm" aria-label={`Add ${label}`} disabled={busy} onClick={() => onAdd(c, owners[key] ?? false)}>
              Add
            </Button>
          </div>
        );
      })}
      {results.hasNextPage && (
        <div className="bg-muted/40 flex justify-end gap-2 border-t px-4 py-3">
          <Button type="button" variant="outline" size="sm" disabled={results.isFetchingNextPage} onClick={() => results.fetchNextPage()}>
            Load more
          </Button>
        </div>
      )}
      {results.isFetchNextPageError && (
        <div className="px-4 pb-3">
          <ErrorBox error={results.error} />
        </div>
      )}
    </section>
  );
}
