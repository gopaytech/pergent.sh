import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { Link, useSearchParams } from "react-router";

import { Badge } from "@/components/ui/badge";
import { Button, buttonVariants } from "@/components/ui/button";
import { Card, CardHeader, CardTitle } from "@/components/ui/card";
import { Dialog, DialogClose, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle, DialogTrigger } from "@/components/ui/dialog";
import { Textarea } from "@/components/ui/textarea";

import { api, dismissReasonText, paths, safeHref, type DismissReason, type SecurityEventView, type SecurityFindingView } from "../api";
import { useWrite } from "../layout/write";
import { Ago, ErrorBox, Loading } from "../ui";
import { SeverityPill } from "./SecurityFindings";

const cardClass = "mb-4 gap-0 overflow-hidden py-0";
const headClass = "border-b py-3 [.border-b]:pb-3";
const bodyClass = "px-4 py-3 text-sm whitespace-pre-wrap break-words";
const reasons = Object.keys(dismissReasonText) as DismissReason[];
const maxComment = 1000;
const stateText = { open: "Open", dismissed: "Dismissed", fixed: "Fixed" } as const;

const eventText: Record<string, string> = {
  found: "Found",
  reopened: "Reopened",
  fixed: "Fixed",
  dismissed: "Dismissed",
  restored: "Restored",
};

// eventLine is one history entry as words: what happened, in which scan or by
// whom, the reason and the comment of a dismissal.
function eventParts(e: SecurityEventView): string[] {
  const what = eventText[e.kind] ?? e.kind;
  const head = e.scan > 0 ? `${what} in scan ${e.scan}` : e.actor !== "" ? `${what} by ${e.actor}` : what;
  const reason = e.reason in dismissReasonText ? dismissReasonText[e.reason as DismissReason] : e.reason;
  return [head, reason, e.comment].filter((p) => p !== "");
}

// SecurityFinding is a finding's page (?tab=security&finding=<id>): its texts,
// the code link, its history and, for a maintainer, Dismiss or Restore. Every
// text is the model's or a person's and is shown as plain text, never as
// markdown or HTML. paused: the repository is paused, so nothing can change;
// the buttons are left out and no error is shown.
export default function SecurityFinding({ instance, project, id, paused = false }: { instance: string; project: string; id: number; paused?: boolean }) {
  const key = ["security-finding", instance, project, id];
  const client = useQueryClient();
  const [params] = useSearchParams();
  // Back keeps the filters of the list the finding was opened from.
  const list = new URLSearchParams(params);
  list.delete("finding");
  list.delete("scan");
  list.set("tab", "security");
  const finding = useQuery({ queryKey: key, queryFn: () => api.get<SecurityFindingView>(paths.apiRepoSecurityFinding(instance, project, id)) });
  const back = (
    <Link className="text-muted-foreground mb-3 inline-block text-sm hover:underline" to={`${paths.repo(instance, project)}?${list.toString()}`}>
      Back to findings
    </Link>
  );
  if (finding.isPending) return <Loading />;
  if (finding.isError)
    return (
      <>
        {back}
        <ErrorBox error={finding.error} onRetry={() => void finding.refetch()} />
      </>
    );
  const v = finding.data;
  // The answer of a change is the finding as it is now.
  const changed = (next: SecurityFindingView) => {
    client.setQueryData(key, next);
    void client.invalidateQueries({ queryKey: ["security-findings", instance, project] });
    void client.invalidateQueries({ queryKey: ["security", instance, project] });
  };
  const f = v.finding;
  const place = `${f.file}${f.startLine > 0 ? `:${f.startLine}` : ""}`;
  const href = safeHref(v.codeUrl);
  const manage = v.canManage && !paused;
  return (
    <>
      {back}
      <h2 className="mb-3 text-lg font-semibold break-words">{f.title}</h2>
      <Card className={cardClass}>
        <div className={bodyClass + " flex flex-wrap items-center gap-2 whitespace-normal"}>
          <SeverityPill severity={f.severity} />
          <Badge variant="outline">{stateText[f.state]}</Badge>
          {f.needsValidation && (
            <Badge variant="outline" className="text-muted-foreground">
              Needs validation
            </Badge>
          )}
          <span>{f.ruleId}</span>
          {f.cwe !== "" && <span className="text-muted-foreground">{f.cwe}</span>}
          {v.tags.map((t) => (
            <Badge key={t} variant="outline">
              {t}
            </Badge>
          ))}
          <span className="text-muted-foreground">Confidence: {v.confidence}</span>
        </div>
        <div className={bodyClass + " border-t whitespace-normal break-all"}>
          Code:{" "}
          {href ? (
            <a className="underline" href={href} target="_blank" rel="noopener noreferrer">
              {place}
            </a>
          ) : (
            <span>{place}</span>
          )}
        </div>
        {v.dismissal && (
          <div className={bodyClass + " border-t"}>
            Dismissed by {v.dismissal.by}: {dismissReasonText[v.dismissal.reason] ?? v.dismissal.reason}
            {v.dismissal.comment !== "" && `\n${v.dismissal.comment}`}
          </div>
        )}
        {manage && (
          <div className="flex flex-wrap items-center gap-3 border-t px-4 py-3">
            {f.state === "open" && <DismissButton instance={instance} project={project} id={id} changed={changed} />}
            {f.state === "dismissed" && <RestoreButton instance={instance} project={project} id={id} changed={changed} />}
          </div>
        )}
      </Card>
      {[
        ["Description", v.description],
        ["Exploit scenario", v.exploit],
        ["Fix hint", v.fixHint],
      ].map(
        ([title, text]) =>
          text !== "" && (
            <Card key={title} className={cardClass}>
              <CardHeader className={headClass}>
                <CardTitle>{title}</CardTitle>
              </CardHeader>
              <div className={bodyClass}>{text}</div>
            </Card>
          ),
      )}
      <Card className={cardClass}>
        <CardHeader className={headClass}>
          <CardTitle>History</CardTitle>
        </CardHeader>
        <ul aria-label="History">
          {v.history.map((e, i) => (
            <li key={i} className="border-b px-4 py-3 text-sm whitespace-pre-wrap break-words last:border-b-0">
              {eventParts(e).join(" · ")} · <Ago t={e.at} />
            </li>
          ))}
        </ul>
      </Card>
    </>
  );
}

type Changed = (next: SecurityFindingView) => void;

function RestoreButton({ instance, project, id, changed }: { instance: string; project: string; id: number; changed: Changed }) {
  const write = useWrite();
  const restore = useMutation({
    mutationFn: () => write<SecurityFindingView>("POST", paths.apiRepoSecurityFindingAction(instance, project, id, "restore"), {}),
    onSuccess: changed,
  });
  return (
    <>
      <Button type="button" variant="outline" size="sm" disabled={restore.isPending} onClick={() => restore.mutate()}>
        Restore
      </Button>
      {restore.isError && (
        <div className="min-w-0 grow">
          <ErrorBox error={restore.error} />
        </div>
      )}
    </>
  );
}

// DismissButton is Dismiss and its box: a reason is needed, a comment is not.
// The trigger is Radix's own button: Button does not pass the ref the focus
// return needs on React 18.
function DismissButton({ instance, project, id, changed }: { instance: string; project: string; id: number; changed: Changed }) {
  const write = useWrite();
  const [open, setOpen] = useState(false);
  const [reason, setReason] = useState<DismissReason | "">("");
  const [comment, setComment] = useState("");
  const dismiss = useMutation({
    mutationFn: () =>
      write<SecurityFindingView>("POST", paths.apiRepoSecurityFindingAction(instance, project, id, "dismiss"), { reason, comment: comment.trim() }),
    onSuccess: (next) => {
      setOpen(false);
      changed(next);
    },
  });
  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        setOpen(o);
        if (o) {
          setReason("");
          setComment("");
          dismiss.reset();
        }
      }}
    >
      <DialogTrigger className={buttonVariants({ variant: "outline", size: "sm" })}>Dismiss</DialogTrigger>
      <DialogContent showCloseButton={false}>
        <DialogHeader>
          <DialogTitle>Dismiss this finding</DialogTitle>
          <DialogDescription>A dismissed finding leaves the open list. You can restore it later.</DialogDescription>
        </DialogHeader>
        <fieldset className="grid gap-1.5 text-sm">
          <legend className="mb-1.5 font-medium">Reason</legend>
          {reasons.map((r) => (
            <label key={r} className="flex items-center gap-2">
              <input type="radio" name="dismiss-reason" value={r} checked={reason === r} onChange={() => setReason(r)} />
              {dismissReasonText[r]}
            </label>
          ))}
        </fieldset>
        <label className="grid gap-1.5 text-sm font-medium">
          Comment (optional)
          <Textarea value={comment} maxLength={maxComment} onChange={(e) => setComment(e.target.value)} />
        </label>
        {dismiss.isError && <ErrorBox error={dismiss.error} />}
        <DialogFooter>
          <DialogClose className={buttonVariants({ variant: "outline" })}>Close</DialogClose>
          <Button type="button" disabled={reason === "" || dismiss.isPending} onClick={() => dismiss.mutate()}>
            Dismiss finding
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
