import type { MarkView } from "./api";
import { utcText } from "./ui";

// markText says why reviews skip an entry, and until when.
export function markText(m: MarkView): string {
  const until = m.until ? `until ${utcText(m.until)}` : "";
  switch (m.class) {
    case "quota":
      return `out of quota ${until}`.trim();
    case "billing":
      return `out of credit ${until}`.trim();
    case "auth":
      return "the key was rejected, until someone changes the key";
  }
  return m.class;
}

// MarkBanner names the entries the provider refused, while any has a live
// mark: reviews skip them until the mark ends or someone clears it. Nothing
// shows while none has one.
export default function MarkBanner({ entries }: { entries: { name: string; mark: MarkView | null }[] }) {
  const marked = entries.filter((e) => e.mark?.live);
  if (marked.length === 0) return null;
  return (
    <div className="alert" role="note" aria-label="Marked entries">
      <span className="prose">
        <strong>Reviews skip these entries until the mark ends or someone clears it.</strong>
        <ul>
          {marked.map((e) => (
            <li key={e.name}>
              {e.name}: {markText(e.mark!)}
            </li>
          ))}
        </ul>
      </span>
    </div>
  );
}
