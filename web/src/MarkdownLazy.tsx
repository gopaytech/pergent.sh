import { lazy, Suspense } from "react";

// The markdown renderer is about 50 KB gzipped, and only the skill, rule
// and instruction views need it: it is loaded when one of them first shows,
// in its own file. Until then the text shows as written.
const Inner = lazy(() => import("./Markdown"));

export default function Markdown({ text }: { text: string }) {
  return (
    <Suspense
      fallback={
        <div className="md-doc">
          <pre className="doc">{text}</pre>
        </div>
      }
    >
      <Inner text={text} />
    </Suspense>
  );
}
