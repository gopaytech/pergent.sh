import { useInfiniteQuery } from "@tanstack/react-query";
import { Link } from "react-router";

import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";

import { api, query, type ReviewListView } from "../api";
import { ErrorBox, Loading } from "../ui";
import ReviewRows from "./ReviewRows";

export type ReviewFilter = { project?: string; outcome?: string; days: number; team?: string };

// ReviewList is a page of reviews matching filter, with "Load more" while
// the API has a next page.
// inRepo is a repository's own list (see ReviewRows). clearTo, when the list
// is filtered, is where "Clear the filters" goes when nothing matches.
export default function ReviewList({ filter, label, inRepo, clearTo }: { filter: ReviewFilter; label: string; inRepo?: boolean; clearTo?: string }) {
  const list = useInfiniteQuery({
    queryKey: ["reviews", filter.project ?? "", filter.outcome ?? "", filter.days, filter.team ?? ""],
    initialPageParam: "",
    queryFn: ({ pageParam }) =>
      api.get<ReviewListView>(
        "/api/reviews" +
          query([
            ["project", filter.project],
            ["outcome", filter.outcome],
            ["days", filter.days],
            ["team", filter.team],
            ["cursor", pageParam],
          ]),
      ),
    getNextPageParam: (last) => last.nextCursor || undefined,
  });
  if (list.isPending) return <Loading />;
  if (list.isError) return <ErrorBox error={list.error} onRetry={() => void list.refetch()} />;
  const items = list.data.pages.flatMap((p) => p.items);
  return (
    // The list has no header, so its first row draws no top line.
    <Card role="region" aria-label={label} className="gap-0 overflow-hidden py-0 [&>:first-child]:border-t-0">
      {items.length === 0 && clearTo ? (
        <div className="text-muted-foreground px-4 py-6 text-sm">
          No reviews match these filters.{" "}
          <Link className="text-primary font-medium hover:underline" to={clearTo}>
            Clear the filters
          </Link>
        </div>
      ) : (
        <ReviewRows items={items} empty="No reviews match." inRepo={inRepo} />
      )}
      {list.hasNextPage && (
        <div className="bg-muted/40 flex justify-end border-t px-4 py-3">
          <Button type="button" variant="outline" size="sm" onClick={() => list.fetchNextPage()} disabled={list.isFetchingNextPage}>
            Load more
          </Button>
        </div>
      )}
    </Card>
  );
}
