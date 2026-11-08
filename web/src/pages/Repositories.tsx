import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useMemo, useState, type FormEvent } from "react";
import { Link, useNavigate, useSearchParams } from "react-router";

import {
  ApiError,
  api,
  paths,
  type AvailableView,
  type InstanceAdminView,
  type LibrarySkill,
  type RepositoryDetailView,
  type RepositoryView,
} from "../api";
import { useMe } from "../layout/me";
import { useWrite } from "../layout/write";
import { ErrorBox, Loading, Pill, Switch, platformName } from "../ui";

// Repositories lists the repositories pergent reviews that the user can
// see. Admins switch them on (the picker) and off (the switch).
export default function Repositories() {
  const me = useMe();
  const [params, setParams] = useSearchParams();
  const adding = me.admin && params.get("add") === "1";
  const repos = useQuery({ queryKey: ["repositories"], queryFn: () => api.get<RepositoryView[]>("/api/repositories") });
  return (
    <>
      <div className="top">
        <div>
          <h1>Repositories</h1>
          <div className="sub">The repositories pergent reviews that you can see</div>
        </div>
        {me.admin && (
          <button type="button" className="btn primary" onClick={() => setParams(adding ? {} : { add: "1" })}>
            + Add repository
          </button>
        )}
      </div>
      {adding && <Picker />}
      {repos.isPending && <Loading />}
      {repos.isError && <ErrorBox error={repos.error} />}
      {repos.isSuccess &&
        (repos.data.length === 0 ? (
          <div className="card empty">No repositories yet.{me.admin && " Use + Add repository to switch one on."}</div>
        ) : (
          <div className="card scroll">
            <table className="table">
              <thead>
                <tr>
                  <th>Repository</th>
                  <th>Platform</th>
                  {me.admin && <th>Review</th>}
                  <th className="num">Reviews (7 days)</th>
                  <th className="num">Approved</th>
                  <th className="num">Failed</th>
                  <th>Health</th>
                </tr>
              </thead>
              <tbody>
                {repos.data.map((r) => (
                  <Row key={`${r.instance}/${r.project}`} repo={r} admin={me.admin} />
                ))}
              </tbody>
            </table>
          </div>
        ))}
    </>
  );
}

function Row({ repo, admin }: { repo: RepositoryView; admin: boolean }) {
  const client = useQueryClient();
  const write = useWrite();
  const off = useMutation({
    mutationFn: () => write<RepositoryDetailView>("DELETE", paths.apiRepo(repo.instance, repo.project)),
    onSuccess: () => client.invalidateQueries({ queryKey: ["repositories"] }),
  });
  const config = repo.source === "config";
  const switchOff = () => {
    if (window.confirm(`Stop reviewing ${repo.project}? pergent removes its webhook and clone credentials.`)) off.mutate();
  };
  return (
    <tr>
      <td>
        <Link className="mr" to={paths.repo(repo.instance, repo.project)}>
          {repo.project}
        </Link>
        {config && <span className="badge cfg">managed by config</span>}
        {off.isError && <ErrorBox error={off.error} />}
      </td>
      <td>
        <span className="plat">{platformName(repo.platform)}</span>
      </td>
      {admin && (
        <td>
          <Switch
            label={`Review ${repo.project}`}
            checked={repo.on && !off.isSuccess}
            disabled={config || off.isPending || !repo.on}
            onChange={(on) => !on && switchOff()}
          />
        </td>
      )}
      <td className="num">{repo.last7.reviews}</td>
      <td className="num">{repo.last7.approved}</td>
      <td className="num">{repo.last7.failed}</td>
      <td>{repo.health === "ok" ? <Pill tone="ok">OK</Pill> : <Pill tone="warn">Needs attention</Pill>}</td>
    </tr>
  );
}

function InstanceChoice({ instance, onChange }: { instance: string; onChange: (name: string) => void }) {
  const instances = useQuery({
    queryKey: ["admin-instances"],
    queryFn: () => api.get<InstanceAdminView[]>("/api/admin/instances"),
  });
  if (instances.isPending) return <Loading />;
  if (instances.isError) return <ErrorBox error={instances.error} />;
  // One being removed has nothing to switch on.
  const choices = instances.data.filter((i) => !i.removing);
  if (choices.length === 0) return <div className="row empty-row">No instance yet. Add one under Integrations.</div>;
  return (
    <div className="pad search">
      <label htmlFor="picker-instance">Instance</label>
      <select id="picker-instance" value={instance} onChange={(e) => onChange(e.target.value)}>
        <option value="">Choose an instance</option>
        {choices.map((i) => (
          <option key={i.name} value={i.name}>
            {i.name}
          </option>
        ))}
      </select>
    </div>
  );
}

// Picker lists the repositories the review bot or App of an instance can
// reach, to switch one on. The admin names the instance first.
function Picker() {
  const [instance, setInstance] = useState("");
  const choice = <InstanceChoice instance={instance} onChange={setInstance} />;
  const write = useWrite();
  const navigate = useNavigate();
  const client = useQueryClient();
  const [text, setText] = useState("");
  const [search, setSearch] = useState("");
  const chosen = instance !== "";
  // A repository switched on here gets the default skills: with none set, it
  // has no skills and isn't reviewed. A library that can't be read says nothing.
  const library = useQuery({ queryKey: ["skills"], queryFn: () => api.get<LibrarySkill[]>("/api/skills") });
  const noDefaults = library.isSuccess && !library.data.some((s) => s.inDefaults);
  const base = `/api/admin/instances/${encodeURIComponent(instance)}/repositories`;
  // The list comes 50 at a time; Load more reads the next page. Pages stay
  // while one is refetched, so a window focus doesn't read them all again.
  const available = useInfiniteQuery({
    queryKey: ["available", instance, search],
    queryFn: ({ pageParam }) =>
      api.get<AvailableView>(`${base}?search=${encodeURIComponent(search)}${pageParam > 1 ? `&page=${pageParam}` : ""}`),
    initialPageParam: 1,
    getNextPageParam: (last) => (last.hasMore ? last.page + 1 : undefined),
    staleTime: 0,
    refetchOnWindowFocus: false,
    enabled: chosen,
  });
  // A repository that came on two pages (the list moved between requests)
  // shows once.
  const items = useMemo(() => {
    const seen = new Set<string>();
    const out: AvailableView["items"] = [];
    for (const r of (available.data?.pages ?? []).flatMap((p) => p.items)) {
      const key = `${r.instance}/${r.project}`;
      if (seen.has(key)) continue;
      seen.add(key);
      out.push(r);
    }
    return out;
  }, [available.data]);
  const truncated = available.data?.pages.some((p) => p.truncated) ?? false;
  const on = useMutation({
    mutationFn: (project: string) =>
      write<RepositoryDetailView>("POST", "/api/repositories", { project, instance }),
    onSuccess: (detail) => {
      client.invalidateQueries({ queryKey: ["repositories"] });
      client.invalidateQueries({ queryKey: ["available"] });
      navigate(paths.repo(detail.repo.instance, detail.repo.project));
    },
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    setSearch(text.trim());
  };
  return (
    <section className="card picker" aria-label="Add repository">
      <h2>Add repository</h2>
      {noDefaults && (
        <div className="alert" role="note">
          <span aria-hidden="true">⚠️</span>
          <span className="prose">
            No default skills are set. A repository you switch on gets no skills and is not reviewed, unless it has its own in
            .pergent/skills. <Link to="/admin/settings">Set the default skills</Link>
          </span>
        </div>
      )}
      {choice}
      {chosen && (
        <form className="pad search" onSubmit={submit}>
          <input
            type="search"
            aria-label="Search"
            placeholder="Search by path"
            maxLength={100}
            value={text}
            onChange={(e) => setText(e.target.value)}
          />
          <button type="submit" className="btn">
            Search
          </button>
        </form>
      )}
      {on.isError &&
        (on.error instanceof ApiError && on.error.status === 404 ? (
          <div className="error" role="alert">
            The review bot can't reach this repository now. It may have been renamed or moved. Reload the list.
          </div>
        ) : (
          <ErrorBox error={on.error} />
        ))}
      {chosen && available.isPending && <Loading />}
      {available.isError && !available.isFetchNextPageError && <ErrorBox error={available.error} />}
      {chosen && available.data && (
        <>
          {truncated && <div className="pad meta">The list is too long to read whole. Search to find others.</div>}
          {items.length === 0 && !available.hasNextPage && <div className="row empty-row">No repository to switch on.</div>}
          {items.map((r) => (
            <div className="row" key={`${r.instance}/${r.project}`}>
              <div className="grow mr">{r.project}</div>
              <button
                type="button"
                className="btn small"
                aria-label={`Switch on ${r.project}`}
                disabled={on.isPending}
                onClick={() => on.mutate(r.project)}
              >
                Switch on
              </button>
            </div>
          ))}
          {available.hasNextPage && available.data.pages.at(-1)?.items.length === 0 && (
            <div className="row empty-row">Everything on this page is already switched on.</div>
          )}
          {available.hasNextPage && (
            <div className="foot">
              <button type="button" className="btn" disabled={available.isFetchingNextPage} onClick={() => available.fetchNextPage()}>
                Load more
              </button>
            </div>
          )}
          {available.isFetchNextPageError && <ErrorBox error={available.error} />}
        </>
      )}
    </section>
  );
}
