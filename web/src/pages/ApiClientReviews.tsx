import { useInfiniteQuery } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Link } from "react-router";

import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { NativeSelect } from "@/components/ui/native-select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

import { api, paths, query, type APIReviewList } from "../api";
import { ErrorBox, Loading, utcText } from "../ui";
import { StatusPill, VerdictPill } from "./ApiReview";

type Filter = { tenant: string; subject: string; status: string };

const none: Filter = { tenant: "", subject: "", status: "" };

// The statuses a review has: the ones the list takes.
const statuses: [string, string][] = [
  ["queued", "Queued"],
  ["running", "Running"],
  ["completed", "Completed"],
  ["failed", "Failed"],
  ["superseded", "Superseded"],
];

// ClientReviews lists a client's reviews, newest first, filtered by tenant,
// subject and status. The filter is sent when the admin applies it, not on
// every key.
export default function ClientReviews({ client }: { client: string }) {
  const [draft, setDraft] = useState<Filter>(none);
  const [applied, setApplied] = useState<Filter>(none);
  const list = useInfiniteQuery({
    queryKey: ["api-reviews", client, applied.tenant, applied.subject, applied.status],
    initialPageParam: "",
    queryFn: ({ pageParam }) =>
      api.get<APIReviewList>(
        paths.apiAdminClientReviews(client) +
          query([
            ["tenant", applied.tenant],
            ["subject", applied.subject],
            ["status", applied.status],
            ["cursor", pageParam],
          ]),
      ),
    getNextPageParam: (last) => last.nextCursor || undefined,
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    setApplied({ ...draft, tenant: draft.tenant.trim(), subject: draft.subject.trim() });
  };
  const filtered = applied.tenant !== "" || applied.subject !== "" || applied.status !== "";
  const items = list.data?.pages.flatMap((p) => p.items) ?? [];
  return (
    <>
      <form className="mb-4 flex flex-wrap items-center gap-2" aria-label="Filter reviews" onSubmit={submit}>
        <Input
          className="w-auto min-w-40"
          type="text"
          aria-label="Tenant"
          placeholder="Tenant"
          autoComplete="off"
          spellCheck={false}
          value={draft.tenant}
          onChange={(e) => setDraft({ ...draft, tenant: e.target.value })}
        />
        <Input
          className="w-auto min-w-40"
          type="text"
          aria-label="Subject"
          placeholder="Subject"
          autoComplete="off"
          spellCheck={false}
          value={draft.subject}
          onChange={(e) => setDraft({ ...draft, subject: e.target.value })}
        />
        <NativeSelect className="w-auto min-w-40" aria-label="Status" value={draft.status} onChange={(e) => setDraft({ ...draft, status: e.target.value })}>
          <option value="">Any status</option>
          {statuses.map(([value, label]) => (
            <option key={value} value={value}>
              {label}
            </option>
          ))}
        </NativeSelect>
        <Button type="submit" variant="outline">
          Filter
        </Button>
      </form>
      {list.isPending && <Loading />}
      {list.isError && <ErrorBox error={list.error} />}
      {list.isSuccess &&
        (items.length === 0 ? (
          <Card className="text-muted-foreground px-4 py-8 text-center text-sm">{filtered ? "No reviews match." : "No reviews yet."}</Card>
        ) : (
          <Card className="gap-0 overflow-hidden py-0">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Subject</TableHead>
                  <TableHead>Tenant</TableHead>
                  <TableHead>Title</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Verdict</TableHead>
                  <TableHead>Created</TableHead>
                  <TableHead>Finished</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {items.map((r) => (
                  <TableRow key={r.id}>
                    <TableCell>
                      <Link className="text-foreground font-medium no-underline hover:underline" to={paths.clientReviewPage(client, r.id)}>
                        {r.subject}
                      </Link>
                    </TableCell>
                    <TableCell>{r.tenant}</TableCell>
                    <TableCell>{r.title}</TableCell>
                    <TableCell>
                      <StatusPill status={r.status} />
                    </TableCell>
                    <TableCell>{r.verdict && <VerdictPill verdict={r.verdict} />}</TableCell>
                    <TableCell className="text-muted-foreground whitespace-nowrap">{utcText(r.createdAt)}</TableCell>
                    <TableCell className="text-muted-foreground whitespace-nowrap">{r.finishedAt ? utcText(r.finishedAt) : ""}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
            {list.hasNextPage && (
              <div className="flex justify-end border-t px-4 py-3">
                <Button type="button" variant="outline" size="sm" onClick={() => list.fetchNextPage()} disabled={list.isFetchingNextPage}>
                  Load more
                </Button>
              </div>
            )}
          </Card>
        ))}
    </>
  );
}
