import { useQuery } from "@tanstack/react-query";

import { api, type AdminSettingsView } from "../api";
import { ErrorBox, Loading } from "../ui";

// minutes writes a timeout in seconds the way people read it.
function minutes(secs: number) {
  return secs % 60 === 0 ? `${secs / 60} min` : `${secs} s`;
}

// AdminSettings shows the defaults a repository gets, the model reviews
// use. Read-only; never a key.
export default function AdminSettings() {
  const data = useQuery({ queryKey: ["admin-settings"], queryFn: () => api.get<AdminSettingsView>("/api/admin/settings") });
  return (
    <div className="narrow">
      <div className="top">
        <div>
          <h1>Settings</h1>
          <div className="sub">pergent.sh's config for this instance, read-only. Change it in the config file.</div>
        </div>
      </div>
      {data.isPending && <Loading />}
      {data.isError && <ErrorBox error={data.error} />}
      {data.isSuccess && <Body v={data.data} />}
    </div>
  );
}

function Item({ k, v }: { k: string; v: string }) {
  return (
    <div className="kv">
      <dt>{k}</dt>
      <dd>{v}</dd>
    </div>
  );
}

function Body({ v }: { v: AdminSettingsView }) {
  const d = v.defaults;
  return (
    <>
      <section className="card" aria-label="Repository defaults">
        <h2>Repository defaults</h2>
        <dl className="kvs">
          <Item k="Approve clean reviews" v={d.approve ? "On" : "Off"} />
          <Item k="Review drafts" v={d.reviewDrafts ? "On" : "Off"} />
          <Item k="Status when a review fails" v={d.failureStatus} />
          <Item k="Timeout" v={minutes(d.jobTimeoutSeconds)} />
          <Item k="Memory limit" v={d.jobMemory || "none"} />
        </dl>
      </section>
      <section className="card" aria-label="Model">
        <h2>Model</h2>
        <dl className="kvs">
          <Item k="Provider" v={v.llm.provider} />
          <Item k="Model" v={v.llm.model} />
        </dl>
      </section>
    </>
  );
}
