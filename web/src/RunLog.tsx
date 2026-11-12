import { useQuery } from "@tanstack/react-query";

import { api, isStatus, type DiagnosticsView, type LogView } from "./api";
import { ErrorBox, Loading } from "./ui";

// RunLog is a run's log and, below it, what was kept of its Job. The paths
// say which API answers: a merge request's run and an API review's run have
// their own, and the same views.
export default function RunLog({ logPath, diagnosticsPath }: { logPath: string; diagnosticsPath: string }) {
  return (
    <>
      <ReviewLog path={logPath} />
      <Diagnostics path={diagnosticsPath} />
    </>
  );
}

function ReviewLog({ path }: { path: string }) {
  const log = useQuery({ queryKey: ["log", path], queryFn: () => api.get<LogView>(path) });
  if (isStatus(log.error, 404)) return <div className="pad meta">No log was kept for this run.</div>;
  if (log.isPending) return <Loading />;
  if (log.isError) return <ErrorBox error={log.error} />;
  return <pre className="log">{log.data.text}</pre>;
}

// Diagnostics is what was kept of a failed run's Job: the clone log, and for
// admins the pod's state. A run with none kept (404) shows nothing here.
function Diagnostics({ path }: { path: string }) {
  const diag = useQuery({ queryKey: ["diagnostics", path], queryFn: () => api.get<DiagnosticsView>(path) });
  if (diag.isPending || isStatus(diag.error, 404)) return null;
  if (diag.isError) return <ErrorBox error={diag.error} />;
  const { cloneLog, podState } = diag.data;
  return (
    <>
      {cloneLog && (
        <>
          <h3 className="group">Clone log</h3>
          <pre className="log">{cloneLog}</pre>
        </>
      )}
      {podState && (
        <>
          <h3 className="group">Pod state</h3>
          <pre className="log">{podState}</pre>
        </>
      )}
    </>
  );
}
