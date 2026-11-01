// DiffLine is one line of a line diff: in both texts, only in the new one
// (add), or only in the old one (del).
export type DiffLine = { kind: "same" | "add" | "del"; text: string };

// Past this many cells the table of a longest-common-subsequence diff is not
// worth its memory (a skill is at most 128 KiB, so this is a rare case); the
// changed middle is shown as all removed, then all added.
const maxCells = 4_000_000;

// lines splits text into lines; one trailing newline does not make a line.
const lines = (text: string): string[] => (text === "" ? [] : text.replace(/\n$/, "").split("\n"));

// lineDiff compares two texts line by line. Equal lines at both ends are
// kept and the changed middle is matched by its longest common lines.
export function lineDiff(a: string, b: string): DiffLine[] {
  const x = lines(a);
  const y = lines(b);
  let start = 0;
  while (start < x.length && start < y.length && x[start] === y[start]) start++;
  let endX = x.length;
  let endY = y.length;
  while (endX > start && endY > start && x[endX - 1] === y[endY - 1]) {
    endX--;
    endY--;
  }
  const out: DiffLine[] = x.slice(0, start).map((text) => ({ kind: "same", text }));
  out.push(...middle(x.slice(start, endX), y.slice(start, endY)));
  out.push(...x.slice(endX).map((text): DiffLine => ({ kind: "same", text })));
  return out;
}

function middle(x: string[], y: string[]): DiffLine[] {
  const del = (text: string): DiffLine => ({ kind: "del", text });
  const add = (text: string): DiffLine => ({ kind: "add", text });
  if (x.length === 0 || y.length === 0 || (x.length + 1) * (y.length + 1) > maxCells) {
    return [...x.map(del), ...y.map(add)];
  }
  // len[i][j] is the longest common run of x[i:] and y[j:].
  const w = y.length + 1;
  const len = new Uint32Array((x.length + 1) * w);
  for (let i = x.length - 1; i >= 0; i--) {
    for (let j = y.length - 1; j >= 0; j--) {
      len[i * w + j] = x[i] === y[j] ? len[(i + 1) * w + j + 1] + 1 : Math.max(len[(i + 1) * w + j], len[i * w + j + 1]);
    }
  }
  const out: DiffLine[] = [];
  let i = 0;
  let j = 0;
  while (i < x.length && j < y.length) {
    if (x[i] === y[j]) {
      out.push({ kind: "same", text: x[i] });
      i++;
      j++;
    } else if (len[(i + 1) * w + j] >= len[i * w + j + 1]) {
      out.push(del(x[i++]));
    } else {
      out.push(add(y[j++]));
    }
  }
  while (i < x.length) out.push(del(x[i++]));
  while (j < y.length) out.push(add(y[j++]));
  return out;
}
