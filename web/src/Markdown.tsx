import { useState } from "react";
import ReactMarkdown, { type Components } from "react-markdown";
import remarkGfm from "remark-gfm";

import { Button } from "@/components/ui/button";

import { safeHref } from "./api";

// sourceBox is the box the text as written is shown in. MarkdownLazy draws
// the same box while the renderer loads.
const sourceBox = "bg-muted/50 rounded-md border px-3 py-2.5 font-mono text-xs break-words whitespace-pre-wrap";

// splitFrontmatter takes a leading "---" block (a skill's name and
// description) off the text: rendered, it would read as a rule and a run of
// plain lines.
export function splitFrontmatter(text: string): { front: string; body: string } {
  const m = /^---\r?\n([\s\S]*?)\r?\n---[ \t]*(?:\r?\n|$)/.exec(text);
  return m ? { front: m[1], body: text.slice(m[0].length) } : { front: "", body: text };
}

// htmlAsText turns raw HTML in the markdown into plain text: it is shown as
// written, as everywhere else in the dashboard, and never becomes HTML.
type MdNode = { type: string; children?: MdNode[] };
function htmlAsText() {
  return (tree: MdNode) => {
    const walk = (n: MdNode) => {
      if (n.type === "html") n.type = "text";
      n.children?.forEach(walk);
    };
    walk(tree);
  };
}

// The text comes from repositories and the library, so it is never trusted:
// raw HTML in it is shown as text (htmlAsText), a link opens only when it is
// http(s) and in a new tab, and an image is shown as a link to it (the
// page's CSP loads no image from another host anyway).
const components: Components = {
  a: ({ href, children }) => {
    const safe = safeHref(href);
    return safe ? (
      <a href={safe} target="_blank" rel="noopener noreferrer">
        {children}
      </a>
    ) : (
      <span>{children}</span>
    );
  },
  img: ({ src, alt }) => {
    const url = typeof src === "string" ? src : undefined;
    const label = `[image: ${alt || url || "no name"}]`;
    const safe = safeHref(url);
    return safe ? (
      <a href={safe} target="_blank" rel="noopener noreferrer">
        {label}
      </a>
    ) : (
      <span>{label}</span>
    );
  },
};

// Markdown shows a skill, an instructions file or a rule as rendered
// markdown (with GitHub's tables, task lists and strikethrough), with a
// button to show the text as it is written. md-doc and md stay as class
// names: the rendered markdown's own styles hang on them.
export default function Markdown({ text }: { text: string }) {
  const [source, setSource] = useState(false);
  const { front, body } = splitFrontmatter(text);
  return (
    <div className="md-doc mx-4 mb-3">
      <div className="mb-1.5 flex justify-end">
        <Button type="button" variant="outline" size="sm" aria-pressed={source} onClick={() => setSource(!source)}>
          {source ? "Show rendered" : "Show source"}
        </Button>
      </div>
      {source ? (
        <pre className={sourceBox}>{text}</pre>
      ) : (
        <>
          {front && (
            <pre className="bg-muted/50 text-muted-foreground mb-2.5 rounded-md border px-3 py-2 font-mono text-xs whitespace-pre-wrap" aria-label="Front matter">
              {front}
            </pre>
          )}
          <div className="md">
            <ReactMarkdown remarkPlugins={[remarkGfm, htmlAsText]} components={components}>
              {body}
            </ReactMarkdown>
          </div>
        </>
      )}
    </div>
  );
}
