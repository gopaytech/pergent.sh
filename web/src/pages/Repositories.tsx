import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent, type ReactNode } from "react";
import { Link, useNavigate, useSearchParams } from "react-router";

import {
  api,
  paths,
  type AvailableView,
  type InstanceAdminView,
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
          <div className="sub">The repositories pergent reviews that you can see{me.platform && ` on ${platformName(me.platform)}`}</div>
        </div>
        {me.admin && (
          <button type="button" className="btn primary" onClick={() => setParams(adding ? {} : { add: "1" })}>
            + Add repository
          </button>
        )}
      </div>
      {adding && (me.operator ? <OperatorPicker /> : <Picker />)}
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

// OperatorPicker is the picker for an operator, who has no platform account
// and no instance of their own: it asks for the instance first.
function OperatorPicker() {
  const [instance, setInstance] = useState("");
  return <Picker instance={instance} choice={<InstanceChoice instance={instance} onChange={setInstance} />} />;
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

// Picker lists the repositories the admin and pergent can both reach, to
// switch one on. An admin has its own instance (instance is undefined); an
// operator names one, and "" is not yet chosen.
function Picker({ instance, choice }: { instance?: string; choice?: ReactNode }) {
  const write = useWrite();
  const navigate = useNavigate();
  const client = useQueryClient();
  const [text, setText] = useState("");
  const [search, setSearch] = useState("");
  const chosen = instance !== "";
  const base =
    instance === undefined ? "/api/repositories/available" : `/api/admin/instances/${encodeURIComponent(instance)}/repositories`;
  const available = useQuery({
    queryKey: ["available", instance ?? "", search],
    queryFn: () => api.get<AvailableView>(`${base}?search=${encodeURIComponent(search)}`),
    staleTime: 0,
    enabled: chosen,
  });
  const on = useMutation({
    mutationFn: (project: string) =>
      write<RepositoryDetailView>("POST", "/api/repositories", instance === undefined ? { project } : { project, instance }),
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
      {on.isError && <ErrorBox error={on.error} />}
      {chosen && available.isPending && <Loading />}
      {available.isError && <ErrorBox error={available.error} />}
      {chosen && available.isSuccess && (
        <>
          {available.data.truncated && (
            <div className="pad meta">Showing the first 500 repositories. Search to find others.</div>
          )}
          {available.data.items.length === 0 && <div className="row empty-row">No repository to switch on.</div>}
          {available.data.items.map((r) => (
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
        </>
      )}
    </section>
  );
}
