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
  type MyTeamView,
  type RepositoryDetailView,
  type RepositoryView,
  type TeamView,
} from "../api";
import { useMe } from "../layout/me";
import { useWrite } from "../layout/write";
import { ErrorBox, Loading, Pill, Switch, platformName } from "../ui";

// Repositories lists the repositories pergent reviews, or has paused, that
// the user can see. Admins add them (the picker) and pause and resume them
// (the switch); maintainers of a team add and claim them on an instance that
// lets teams onboard. Remove is on a repository's page.
export default function Repositories() {
  const me = useMe();
  const [params, setParams] = useSearchParams();
  const canAdd = me.admin || me.onboardInstances.length > 0;
  const adding = canAdd && params.get("add") === "1";
  const repos = useQuery({ queryKey: ["repositories"], queryFn: () => api.get<RepositoryView[]>("/api/repositories") });
  // An instance being removed can't have a repository resumed: its switch is
  // drawn disabled. Only admins read the instances.
  const instances = useQuery({
    queryKey: ["admin-instances"],
    queryFn: () => api.get<InstanceAdminView[]>("/api/admin/instances"),
    enabled: me.admin,
  });
  const removing = new Set((instances.data ?? []).filter((i) => i.removing).map((i) => i.name));
  return (
    <>
      <div className="top">
        <div>
          <h1>Repositories</h1>
          <div className="sub">The repositories pergent reviews that you can see</div>
        </div>
        {canAdd && (
          <button type="button" className="btn primary" onClick={() => setParams(adding ? {} : { add: "1" })}>
            + Add repository
          </button>
        )}
      </div>
      {adding && <Picker />}
      {repos.isPending && <Loading />}
      {repos.isError && <ErrorBox error={repos.error} onRetry={() => void repos.refetch()} />}
      {repos.isSuccess &&
        (repos.data.length === 0 ? (
          <div className="card empty">No repositories yet.{canAdd && " Use + Add repository to switch one on."}</div>
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
                  <Row key={`${r.instance}/${r.project}`} repo={r} admin={me.admin} removing={removing.has(r.instance)} />
                ))}
              </tbody>
            </table>
          </div>
        ))}
    </>
  );
}

function Row({ repo, admin, removing }: { repo: RepositoryView; admin: boolean; removing: boolean }) {
  const client = useQueryClient();
  const write = useWrite();
  // The switch pauses and resumes: nothing is lost, so it asks nothing.
  const toggle = useMutation({
    mutationFn: (enabled: boolean) => write<RepositoryDetailView>("PUT", paths.apiAdminRepo(repo.instance, repo.project), { enabled }),
    onSuccess: () => client.invalidateQueries({ queryKey: ["repositories"] }),
  });
  const config = repo.source === "config";
  return (
    <tr>
      <td>
        <Link className="mr" to={paths.repo(repo.instance, repo.project)}>
          {repo.project}
        </Link>
        {config && <span className="badge cfg">managed by config</span>}
        {toggle.isError && <ErrorBox error={toggle.error} />}
      </td>
      <td>
        <span className="plat">{platformName(repo.platform)}</span>
      </td>
      {admin && (
        <td>
          <Switch
            label={`Review ${repo.project}`}
            checked={toggle.isPending ? (toggle.variables ?? repo.on) : repo.on}
            disabled={config || toggle.isPending || removing}
            onChange={(on) => toggle.mutate(on)}
          />
        </td>
      )}
      <td className="num">{repo.last7.reviews}</td>
      <td className="num">{repo.last7.approved}</td>
      <td className="num">
        {repo.last7.failed > 0 ? (
          <Link to={`/reviews?project=${encodeURIComponent(repo.project)}&outcome=failed&days=7`} aria-label={`${repo.last7.failed} failed reviews of ${repo.project}`}>
            {repo.last7.failed}
          </Link>
        ) : (
          repo.last7.failed
        )}
      </td>
      <td>
        {repo.health === "attention" ? (
          // The problem is on the repository's Setup checks tab.
          <Link to={`${paths.repo(repo.instance, repo.project)}?tab=setup`} aria-label={`Setup checks of ${repo.project}`}>
            <Health health={repo.health} />
          </Link>
        ) : (
          <Health health={repo.health} />
        )}
      </td>
    </tr>
  );
}

function Health({ health }: { health: RepositoryView["health"] }) {
  if (health === "paused") return <Pill tone="grey">Paused</Pill>;
  return health === "ok" ? <Pill tone="ok">OK</Pill> : <Pill tone="warn">Needs attention</Pill>;
}

function InstanceSelect({ instance, names, onChange }: { instance: string; names: string[]; onChange: (name: string) => void }) {
  return (
    <div className="pad search">
      <label htmlFor="picker-instance">Instance</label>
      <select id="picker-instance" value={instance} onChange={(e) => onChange(e.target.value)}>
        <option value="">Choose an instance</option>
        {names.map((n) => (
          <option key={n} value={n}>
            {n}
          </option>
        ))}
      </select>
    </div>
  );
}

// InstanceChoice is the admin's: every instance not being removed.
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
  return <InstanceSelect instance={instance} names={choices.map((i) => i.name)} onChange={onChange} />;
}

// Picker lists the repositories the review bot or App of an instance can
// reach, to switch one on. The person names the instance first. An admin
// switches repositories on; a maintainer onboards, claims or resumes the ones
// of a team they maintain (the buttons follow the row's state and owner).
function Picker() {
  const me = useMe();
  const [instance, setInstance] = useState("");
  const choice = me.admin ? (
    <InstanceChoice instance={instance} onChange={setInstance} />
  ) : (
    <InstanceSelect instance={instance} names={me.onboardInstances} onChange={setInstance} />
  );
  const write = useWrite();
  const navigate = useNavigate();
  const client = useQueryClient();
  const [text, setText] = useState("");
  const [search, setSearch] = useState("");
  // ?team=<id> (from a team's + Add repository) pre-selects the owner team.
  const [params] = useSearchParams();
  const [ownerTeam, setOwnerTeam] = useState(params.get("team") ?? "");
  const chosen = instance !== "";
  // The owner team is optional. A list that can't be read leaves the select out.
  const teams = useQuery({ queryKey: ["teams"], queryFn: () => api.get<TeamView[]>("/api/teams"), enabled: me.admin });
  // A maintainer owns through the teams they maintain.
  const myTeams = useQuery({ queryKey: ["my-teams"], queryFn: () => api.get<MyTeamView[]>("/api/me/teams"), enabled: !me.admin });
  const maintained = (myTeams.data ?? []).filter((t) => t.role === "maintainer");
  const today = chosen ? teams.data?.find((t) => t.repos.some((r) => r.instance === instance && r.project === "" && r.owner)) : undefined;
  // A repository switched on here gets the default skills: with none set, it
  // has no skills and isn't reviewed. A library that can't be read says nothing.
  const library = useQuery({ queryKey: ["skills"], queryFn: () => api.get<LibrarySkill[]>("/api/skills") });
  const noDefaults = library.isSuccess && !library.data.some((s) => s.inDefaults);
  const base = me.admin
    ? `/api/admin/instances/${encodeURIComponent(instance)}/repositories`
    : `/api/repositories/available?instance=${encodeURIComponent(instance)}`;
  const sep = me.admin ? "?" : "&";
  // The list comes 50 at a time; Load more reads the next page. Pages stay
  // while one is refetched, so a window focus doesn't read them all again.
  const available = useInfiniteQuery({
    queryKey: ["available", instance, search],
    queryFn: ({ pageParam }) =>
      api.get<AvailableView>(`${base}${sep}search=${encodeURIComponent(search)}${pageParam > 1 ? `&page=${pageParam}` : ""}`),
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
  // instanceOwner is the team that owns the whole instance ("" for none): a
  // maintainer's choice of team is made for them, as the owner team.
  const instanceOwner = me.admin ? "" : (available.data?.pages[0]?.instanceOwner ?? "");
  const mine = new Set(maintained.map((t) => t.name));
  // The team a maintainer acts for: the instance's owner, their only team,
  // or the one they chose. An admin's is the select's, "" for none.
  const team = me.admin
    ? ownerTeam
    : instanceOwner !== ""
      ? String(maintained.find((t) => t.name === instanceOwner)?.id ?? "")
      : maintained.length === 1
        ? String(maintained[0].id)
        : maintained.some((t) => String(t.id) === ownerTeam)
          ? ownerTeam
          : "";
  const act = useMutation({
    mutationFn: ({ kind, project }: { kind: "onboard" | "claim" | "resume"; project: string }) => {
      if (kind === "claim") return write<RepositoryDetailView>("POST", paths.apiRepoClaim(instance, project), { team: Number(team) });
      if (kind === "resume") return write<RepositoryDetailView>("PUT", paths.apiAdminRepo(instance, project), { enabled: true });
      return write<RepositoryDetailView>("POST", "/api/repositories", {
        project,
        instance,
        ...(team !== "" && { ownerTeam: Number(team) }),
      });
    },
    onSuccess: (detail) => {
      client.invalidateQueries({ queryKey: ["repositories"] });
      client.invalidateQueries({ queryKey: ["available"] });
      client.invalidateQueries({ queryKey: ["my-teams"] });
      client.invalidateQueries({ queryKey: ["teams"] });
      if (team !== "") client.invalidateQueries({ queryKey: ["team", Number(team)] });
      navigate(paths.repo(detail.repo.instance, detail.repo.project));
    },
  });
  // The button of a row, or the text that says who owns it; null for none.
  const rowAction = (r: AvailableView["items"][number]) => {
    const button = (kind: "onboard" | "claim" | "resume", label: string, needsTeam: boolean) => (
      <button
        type="button"
        className="btn small"
        aria-label={`${label} ${r.project}`}
        disabled={act.isPending || (needsTeam && team === "")}
        onClick={() => act.mutate({ kind, project: r.project })}
      >
        {label}
      </button>
    );
    if (me.admin) {
      if (r.state === "off") return button("onboard", "Switch on", false);
      if (r.state === "paused") return button("resume", "Resume", false);
      return r.ownerTeam !== "" ? <span className="meta">owned by {r.ownerTeam}</span> : null;
    }
    const owned = r.ownerTeam !== "";
    if (owned && !mine.has(r.ownerTeam)) return <span className="meta">owned by {r.ownerTeam}</span>;
    if (r.state === "off") return button("onboard", "Onboard", true);
    if (!owned) return button("claim", "Claim", true);
    return r.state === "paused" ? (
      button("resume", "Resume", false)
    ) : (
      <span className="meta">owned by {r.ownerTeam} (your team)</span>
    );
  };
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
      {chosen && !me.admin && instanceOwner !== "" && (
        <div className="pad search">
          <div className="meta">
            Owner: {instanceOwner} (all repositories of {instance})
          </div>
        </div>
      )}
      {chosen && !me.admin && instanceOwner === "" && myTeams.isSuccess && maintained.length > 1 && (
        <div className="pad search">
          <label htmlFor="picker-owner-team">Owner team</label>
          <select id="picker-owner-team" value={ownerTeam} onChange={(e) => setOwnerTeam(e.target.value)}>
            <option value="">Choose a team</option>
            {maintained.map((t) => (
              <option key={t.id} value={t.id}>
                {t.name}
              </option>
            ))}
          </select>
        </div>
      )}
      {chosen && me.admin && teams.isSuccess && (
        <div className="pad search">
          <label htmlFor="picker-owner-team">Owner team</label>
          <select id="picker-owner-team" value={ownerTeam} onChange={(e) => setOwnerTeam(e.target.value)}>
            <option value="">None</option>
            {teams.data.map((t) => (
              <option key={t.id} value={t.id}>
                {t.name}
              </option>
            ))}
          </select>
          {today && <div className="meta">Owner today: {today.name} (all repositories of {instance})</div>}
        </div>
      )}
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
      {act.isError &&
        (act.error instanceof ApiError && act.error.status === 404 && act.variables?.kind === "onboard" ? (
          <div className="error" role="alert">
            The review bot can't reach this repository now. It may have been renamed or moved. Reload the list.
          </div>
        ) : (
          <ErrorBox error={act.error} />
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
              {rowAction(r)}
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
