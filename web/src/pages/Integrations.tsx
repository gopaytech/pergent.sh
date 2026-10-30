import { useQuery } from "@tanstack/react-query";

import { api, type IntegrationView } from "../api";
import { ErrorBox, Loading, Pill, platformName } from "../ui";

// Integrations shows the admin's own instance: whether pergent is
// connected, how it approves, and each repository's installation.
export default function Integrations() {
  const data = useQuery({ queryKey: ["integrations"], queryFn: () => api.get<IntegrationView>("/api/integrations") });
  return (
    <div className="narrow">
      <div className="top">
        <div>
          <h1>Integrations</h1>
          <div className="sub">The instance you signed in with, read-only</div>
        </div>
      </div>
      {data.isPending && <Loading />}
      {data.isError && <ErrorBox error={data.error} />}
      {data.isSuccess && <Body v={data.data} />}
    </div>
  );
}

function Body({ v }: { v: IntegrationView }) {
  const platform = platformName(v.platform);
  const approver = v.approver === "app" ? "approve App" : "bot user token";
  return (
    <>
      <section className="card" aria-label={`${platform} · ${v.instance}`}>
        <h2>
          {platform} · {v.instance}{" "}
          {v.connected ? <Pill tone="ok">Connected</Pill> : <Pill tone="bad">Not connected</Pill>}
        </h2>
        <div className="facts">
          <div>Approver: {approver}</div>
          {v.tokenExpiry && <div className="warn-text">Token expires {v.tokenExpiry.slice(0, 10)}</div>}
        </div>
      </section>
      <section className="card scroll" aria-label="Repositories">
        <h2>Repositories</h2>
        {v.repos.length === 0 ? (
          <div className="row empty-row">No repositories are switched on.</div>
        ) : (
          <table className="table">
            <thead>
              <tr>
                <th>Repository</th>
                <th>{v.platform === "github" ? "Review App" : "Webhook and deploy token"}</th>
                <th>Approver</th>
              </tr>
            </thead>
            <tbody>
              {v.repos.map((r) => (
                <tr key={r.project}>
                  <td className="mr">{r.project}</td>
                  <td>{r.installed ? <Pill tone="ok">Installed</Pill> : <Pill tone="bad">Not installed</Pill>}</td>
                  <td>{r.approverReachable ? <Pill tone="ok">Can approve</Pill> : <Pill tone="bad">Can't approve</Pill>}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>
    </>
  );
}
