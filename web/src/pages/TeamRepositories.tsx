import { useInfiniteQuery } from "@tanstack/react-query";
import { useMemo, useState, type FormEvent } from "react";
import { Link } from "react-router";

import { api, paths, type RepositoryChoiceView, type RepositoryChoicesView, type TeamEntryView, type TeamPageView } from "../api";
import { ErrorBox, Loading } from "../ui";
import { useTeamSave } from "./teamSave";

const entryLabel = (e: { instance: string; project: string }) => (e.project === "" ? `All repositories of ${e.instance}` : e.project);
const asRead = (e: TeamEntryView) => ({ instance: e.instance, project: e.project, owner: e.owner });

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
    <section className="card" aria-label="Team repositories">
      <h2>Repositories</h2>
      {view.repos.length === 0 && <div className="pad meta">This team covers no repository yet.</div>}
      {view.repos.map((r) => (
        <div className="row" key={`${r.instance}/${r.project}`}>
          <div className="grow">
            {r.project === "" ? (
              <span>{entryLabel(r)}</span>
            ) : (
              <Link className="mr" to={paths.repo(r.instance, r.project)}>
                {r.project}
              </Link>
            )}{" "}
            <span className="meta">{r.instance}</span>
            {r.owner && <span className="badge"> owner</span>}
            {r.paused && (
              <span className="meta">
                {" "}
                (<Link to={paths.repo(r.instance, r.project)}>paused</Link>)
              </span>
            )}
            {r.removed && (
              <>
                {" "}
                <span className="meta">(removed)</span> <span className="meta">The next save drops it.</span>
              </>
            )}
          </div>
          {view.canEdit && !r.removed && (
            <>
              <label className="check">
                <input
                  type="checkbox"
                  aria-label={`Owner of ${entryLabel(r)}`}
                  checked={r.owner}
                  disabled={save.isPending}
                  onChange={(e) => save.mutate({ repos: live.map((x) => (same(x, r) ? { ...x, owner: e.target.checked } : x)) })}
                />{" "}
                Owner
              </label>
              <button
                type="button"
                className="btn small"
                aria-label={`Remove ${entryLabel(r)}`}
                disabled={save.isPending}
                onClick={() => save.mutate({ repos: live.filter((x) => !same(x, r)) })}
              >
                Remove
              </button>
            </>
          )}
        </div>
      ))}
      {save.isError && <ErrorBox error={save.error} />}
      {view.canEdit && (
        <div className="foot">
          <button type="button" className="btn" onClick={() => setAdding(!adding)}>
            Add repository
          </button>
        </div>
      )}
      {view.canEdit && adding && <Search view={view} onAdd={(c, owner) => save.mutate({ repos: [...live, { instance: c.instance, project: c.project, owner }] })} busy={save.isPending} />}
    </section>
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
    <section className="picker" aria-label="Add repository">
      <form className="pad search" onSubmit={submit}>
        <input type="search" aria-label="Search" placeholder="Search by path" maxLength={100} value={text} onChange={(e) => setText(e.target.value)} />
        <button type="submit" className="btn">
          Search
        </button>
      </form>
      {sent !== null && results.isPending && <Loading />}
      {results.isError && !results.isFetchNextPageError && <ErrorBox error={results.error} />}
      {results.data && items.length === 0 && !results.hasNextPage && <div className="row empty-row">Nothing to add.</div>}
      {items.map((c) => {
        const key = `${c.instance}/${c.project}`;
        const label = entryLabel(c);
        return (
          <div className="row" key={key}>
            <div className="grow">
              <span className="mr">{label}</span> <span className="meta">{c.instance}</span>
              {c.paused && <span className="meta"> (paused)</span>}
              {c.ownedBy && <span className="meta"> owned by {c.ownedBy.name}</span>}
            </div>
            <label className="check">
              <input
                type="checkbox"
                aria-label={`Owner of ${label}`}
                checked={owners[key] ?? false}
                disabled={c.ownedBy !== null}
                onChange={(e) => setOwners({ ...owners, [key]: e.target.checked })}
              />{" "}
              Owner
            </label>
            <button type="button" className="btn small" aria-label={`Add ${label}`} disabled={busy} onClick={() => onAdd(c, owners[key] ?? false)}>
              Add
            </button>
          </div>
        );
      })}
      {results.hasNextPage && (
        <div className="foot">
          <button type="button" className="btn" disabled={results.isFetchingNextPage} onClick={() => results.fetchNextPage()}>
            Load more
          </button>
        </div>
      )}
      {results.isFetchNextPageError && <ErrorBox error={results.error} />}
    </section>
  );
}
