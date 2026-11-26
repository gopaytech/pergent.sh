import { lazy, Suspense } from "react";

// The markdown renderer is about 50 KB gzipped, and only the skill, rule
// and instruction views need it: it is loaded when one of them first shows,
// in its own file. Until then the text shows as written.
const Inner = lazy(() => import("./Markdown"));

export default function Markdown({ text }: { text: string }) {
  return (
    <Suspense
      fallback={
        <div className="md-doc mx-4 mb-3">
          {/* The same box as Markdown's source view. Importing its class from
              ./Markdown would load the renderer up front. */}
          <pre className="bg-muted/50 rounded-md border px-3 py-2.5 font-mono text-xs break-words whitespace-pre-wrap">{text}</pre>
        </div>
      }
    >
      <Inner text={text} />
    </Suspense>
  );
}
