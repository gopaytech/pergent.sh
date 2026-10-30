import { useInfiniteQuery } from "@tanstack/react-query";

import { api, query, type ReviewListView } from "../api";
import { ErrorBox, Loading } from "../ui";
import ReviewRows from "./ReviewRows";

export type ReviewFilter = { project?: string; outcome?: string; days: number };

// ReviewList is a page of reviews matching filter, with "Load more" while
// the API has a next page.
export default function ReviewList({ filter, label }: { filter: ReviewFilter; label: string }) {
  const list = useInfiniteQuery({
    queryKey: ["reviews", filter.project ?? "", filter.outcome ?? "", filter.days],
    initialPageParam: "",
    queryFn: ({ pageParam }) =>
      api.get<ReviewListView>(
        "/api/reviews" +
          query([
            ["project", filter.project],
            ["outcome", filter.outcome],
            ["days", filter.days],
            ["cursor", pageParam],
          ]),
      ),
    getNextPageParam: (last) => last.nextCursor || undefined,
  });
  if (list.isPending) return <Loading />;
  if (list.isError) return <ErrorBox error={list.error} />;
  const items = list.data.pages.flatMap((p) => p.items);
  return (
    <section className="card" aria-label={label}>
      <ReviewRows items={items} empty="No reviews match." />
      {list.hasNextPage && (
        <div className="foot">
          <button type="button" className="btn" onClick={() => list.fetchNextPage()} disabled={list.isFetchingNextPage}>
            Load more
          </button>
        </div>
      )}
    </section>
  );
}
