import { useState } from "react";
import ReactMarkdown, { type Components } from "react-markdown";
import remarkGfm from "remark-gfm";

import { safeHref } from "./api";

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
// button to show the text as it is written.
export default function Markdown({ text }: { text: string }) {
  const [source, setSource] = useState(false);
  const { front, body } = splitFrontmatter(text);
  return (
    <div className="md-doc">
      <div className="md-bar">
        <button type="button" className="btn small" aria-pressed={source} onClick={() => setSource(!source)}>
          {source ? "Show rendered" : "Show source"}
        </button>
      </div>
      {source ? (
        <pre className="doc">{text}</pre>
      ) : (
        <>
          {front && (
            <pre className="md-front" aria-label="Front matter">
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
