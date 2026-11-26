import { useSearchParams } from "react-router";

import { TableHead } from "@/components/ui/table";
import { cn } from "@/lib/utils";

// SortState is the column a table is sorted by and which way.
export type SortState<K extends string> = { key: K; desc: boolean };

const DESC = "-desc";

// useTableSort is the sort of a table, kept in the address (?sort=reviews, or
// ?sort=reviews-desc), so a reload or a shared link opens the same order. An
// unknown value is the fallback column, ascending. A number column starts
// with the biggest first; a text column starts A to Z. Choosing the column
// that is already sorted flips it.
export function useTableSort<K extends string>(
  keys: readonly K[],
  fallback: K,
  numeric: readonly K[],
): [SortState<K>, (key: K) => void] {
  const [params, setParams] = useSearchParams();
  const raw = params.get("sort") ?? "";
  const desc = raw.endsWith(DESC);
  const named = (desc ? raw.slice(0, -DESC.length) : raw) as K;
  const sort: SortState<K> = keys.includes(named) ? { key: named, desc } : { key: fallback, desc: false };

  const onSort = (key: K) => {
    const nextDesc = key === sort.key ? !sort.desc : numeric.includes(key);
    setParams(
      (old) => {
        const next = new URLSearchParams(old);
        if (key === fallback && !nextDesc) next.delete("sort");
        else next.set("sort", nextDesc ? key + DESC : key);
        return next;
      },
      { replace: true },
    );
  };
  return [sort, onSort];
}

// SortHeader is a column heading that sorts the table when clicked. It
// shows an arrow only on the sorted column.
export function SortHeader<K extends string>({
  label,
  sortKey,
  sort,
  onSort,
  num,
}: {
  label: string;
  sortKey: K;
  sort: SortState<K>;
  onSort: (k: K) => void;
  num?: boolean;
}) {
  const on = sort.key === sortKey;
  const aria = on ? (sort.desc ? "descending" : "ascending") : "none";
  return (
    <TableHead className={cn(num && "num text-right")} aria-sort={aria}>
      <button
        type="button"
        className={cn(
          "hover:text-foreground focus-visible:ring-ring/50 inline-flex cursor-pointer items-center gap-1 rounded-sm font-medium outline-none focus-visible:ring-[3px]",
          on && "text-foreground",
        )}
        onClick={() => onSort(sortKey)}
      >
        {label}
        {on && (
          <span className="text-[11px]" aria-hidden="true">
            {sort.desc ? "▼" : "▲"}
          </span>
        )}
      </button>
    </TableHead>
  );
}

// compareRows orders two rows by a value, up or down. Rows with an equal
// value come in name order, A to Z, so the order is the same on each load.
export function compareRows<T>(
  a: T,
  b: T,
  value: (r: T) => string | number,
  desc: boolean,
  name: (r: T) => string,
): number {
  const x = value(a);
  const y = value(b);
  let by = 0;
  if (typeof x === "number" && typeof y === "number") by = x - y;
  else by = String(x).localeCompare(String(y));
  if (by !== 0) return desc ? -by : by;
  return name(a).localeCompare(name(b));
}
