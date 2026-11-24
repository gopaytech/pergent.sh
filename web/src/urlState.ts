import { useSearchParams } from "react-router";

// useUrlParam keeps one choice of a page (a tab, a period) in the address,
// so a reload, Back or a shared link opens the page as it was. The fallback
// is left out of the address; a value not in allowed reads as the fallback.
// A change is a new history entry: Back goes to the choice before.
export function useUrlParam<T extends string | number>(name: string, fallback: T, allowed: readonly T[]): [T, (v: T) => void] {
  const [params, setParams] = useSearchParams();
  const raw = params.get(name);
  const read = (typeof fallback === "number" ? Number(raw) : raw) as T;
  const value = raw !== null && allowed.includes(read) ? read : fallback;
  const set = (v: T) =>
    setParams((old) => {
      const next = new URLSearchParams(old);
      if (v === fallback) next.delete(name);
      else next.set(name, String(v));
      return next;
    });
  return [value, set];
}
