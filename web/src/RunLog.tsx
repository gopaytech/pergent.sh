import { useQuery } from "@tanstack/react-query";

import { api, isStatus, type DiagnosticsView, type LogView } from "./api";
import { ErrorBox, Loading } from "./ui";

// groupHead is the small heading over the clone log and the pod state.
const groupHead = "text-muted-foreground mx-4 mt-4 mb-1.5 text-[13px] font-semibold";

// RunLog is a run's log and, below it, what was kept of its Job. The paths
// say which API answers: a merge request's run, a multi review reviewer and an
// API review's run have their own, and the same views. what names whose log
// it is when none was kept ("run" when not given).
export default function RunLog({ logPath, diagnosticsPath, what = "run" }: { logPath: string; diagnosticsPath: string; what?: string }) {
  return (
    <>
      <ReviewLog path={logPath} what={what} />
      <Diagnostics path={diagnosticsPath} />
    </>
  );
}

function ReviewLog({ path, what }: { path: string; what: string }) {
  const log = useQuery({ queryKey: ["log", path], queryFn: () => api.get<LogView>(path) });
  if (isStatus(log.error, 404)) return <div className="text-muted-foreground px-4 py-3 text-sm">No log was kept for this {what}.</div>;
  if (log.isPending) return <Loading />;
  if (log.isError) return <ErrorBox error={log.error} />;
  // "log" stays: the run log's box is content CSS, as it was.
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
          <h3 className={groupHead}>Clone log</h3>
          <pre className="log">{cloneLog}</pre>
        </>
      )}
      {podState && (
        <>
          <h3 className={groupHead}>Pod state</h3>
          <pre className="log">{podState}</pre>
        </>
      )}
    </>
  );
}
