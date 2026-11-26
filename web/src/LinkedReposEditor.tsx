import { useEffect, useState } from "react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";

import type { Link, LinkableView } from "./api";
import { ErrorBox, Loading, Switch } from "./ui";

// note is a line of muted text between the rows.
const note = "text-muted-foreground border-b px-4 py-2.5 text-sm last:border-b-0";

// The most links a repository has; the server refuses a fourth.
export const maxLinks = 3;

// LinkableState is the list of repositories the review App reaches, for the
// repository being edited: loading, an error, or the answer.
export type LinkableState = { status: "loading" } | { status: "error"; error: unknown } | { status: "ok"; view: LinkableView };

// tidyLinks is links as they are sent: each glob trimmed, blank lines dropped.
// The editor keeps blank lines while someone types.
export function tidyLinks(links: Link[]): Link[] {
  return links.map((l) => ({
    target: l.target,
    watched: l.watched.map((w) => w.trim()).filter((w) => w !== ""),
    canBlock: l.canBlock,
  }));
}

// LinkedReposEditor edits the impact check across linked repositories: the
// on/off switch first (off by default), then the links, one row each (target,
// watched paths one glob per line, Can block, Remove), then a list to add one
// from, with the reason a repository can't be linked. The links stay editable
// while the switch is off. Every change calls onEnabled or onLinks with the
// whole value; onSearch gets the search text once typing stops.
export default function LinkedReposEditor({
  enabled,
  onEnabled,
  links,
  onLinks,
  linkable,
  onSearch,
  canAdd = true,
  disabled,
}: {
  enabled: boolean;
  onEnabled: (on: boolean) => void;
  links: Link[];
  onLinks: (links: Link[]) => void;
  linkable: LinkableState;
  onSearch: (text: string) => void;
  // canAdd false leaves out the list to add from (the config sets the links).
  canAdd?: boolean;
  disabled?: boolean;
}) {
  const [search, setSearch] = useState("");
  // The list is asked for once typing has stopped.
  useEffect(() => {
    const t = setTimeout(() => onSearch(search.trim()), 300);
    return () => clearTimeout(t);
  }, [search, onSearch]);

  const change = (i: number, edit: Partial<Link>) => onLinks(links.map((l, j) => (j === i ? { ...l, ...edit } : l)));
  const added = new Set(links.map((l) => l.target.toLowerCase()));
  const full = links.length >= maxLinks;

  return (
    <div>
      <div className="grid items-center gap-4 border-b px-4 py-3.5 sm:grid-cols-[1fr_auto]">
        <div className="text-muted-foreground text-sm">
          Off by default. When on, a change to a watched path is also checked against the repositories below, to find callers it
          breaks. A finding can quote code from a linked repository in a comment on this repository's pull requests, and anyone
          who can read those pull requests can read the quote. Link only repositories whose code they may see.
        </div>
        <div>
          <Switch label="Linked repositories" checked={enabled} disabled={disabled} onChange={onEnabled} />
        </div>
      </div>
      {!enabled && links.length > 0 && (
        <div className={note}>The links below are kept, but they are ignored until Linked repositories is on.</div>
      )}

      <div role="list" aria-label="Linked repositories">
        {links.map((l, i) => (
          <div className="grid gap-2 border-b px-4 py-3" role="listitem" key={l.target}>
            <div className="flex flex-wrap items-center gap-3 text-sm">
              <b className="min-w-0 grow font-semibold">{l.target}</b>
              <Label className="shrink-0 font-normal">
                <Switch label={`Can block ${l.target}`} checked={l.canBlock} disabled={disabled} onChange={(on) => change(i, { canBlock: on })} />
                <span>Can block</span>
              </Label>
              <Button
                type="button"
                variant="outline"
                size="sm"
                aria-label={`Remove ${l.target}`}
                disabled={disabled}
                onClick={() => onLinks(links.filter((_, j) => j !== i))}
              >
                Remove
              </Button>
            </div>
            <Label className="grid gap-1.5 font-normal">
              <span className="text-muted-foreground text-sm">Watched paths, one glob per line. Empty: every change counts.</span>
              <Textarea
                className="font-mono"
                aria-label={`Watched paths of ${l.target}`}
                rows={Math.max(2, Math.min(6, l.watched.length + 1))}
                spellCheck={false}
                placeholder="Every change"
                value={l.watched.join("\n")}
                disabled={disabled}
                onChange={(e) => change(i, { watched: e.target.value.split("\n") })}
              />
            </Label>
            {!l.canBlock && <div className="text-muted-foreground text-sm">Findings on this link are only ever suggestions, whatever the review says.</div>}
          </div>
        ))}
      </div>
      {links.length === 0 && <div className={note}>No linked repository yet.</div>}

      {!canAdd ? null : full ? (
        <div className={note}>A repository has at most {maxLinks} linked repositories. Remove one to add another.</div>
      ) : (
        <div>
          <Label className="grid gap-1.5 px-4 py-3 font-normal">
            <span className="text-muted-foreground text-sm">Add a linked repository</span>
            <Input
              type="search"
              aria-label="Search repositories to link"
              placeholder="Search the repositories the review App reaches"
              value={search}
              disabled={disabled}
              onChange={(e) => setSearch(e.target.value)}
            />
          </Label>
          {linkable.status === "loading" && <Loading />}
          {linkable.status === "error" && (
            <div className="px-4 py-3">
              <ErrorBox error={linkable.error} />
            </div>
          )}
          {linkable.status === "ok" && (
            <>
              <div className="max-h-90 overflow-y-auto border-t" role="list" aria-label="Repositories to link">
                {linkable.view.items.map((it) => {
                  const have = added.has(it.repo.toLowerCase());
                  return (
                    <div className="flex items-center gap-2.5 border-b px-4 py-2.5 text-sm last:border-b-0" role="listitem" key={it.repo}>
                      <span className="min-w-0 grow">{it.repo}</span>
                      {it.visibility && (
                        <Badge variant="outline" className="src text-muted-foreground font-normal">
                          {it.visibility}
                        </Badge>
                      )}
                      {have ? (
                        <span className="text-muted-foreground text-xs">added</span>
                      ) : it.linkable ? (
                        <>
                          {!it.checked && <span className="text-muted-foreground text-xs">{it.reason}</span>}
                          <Button
                            type="button"
                            variant="outline"
                            size="sm"
                            aria-label={`Add ${it.repo}`}
                            disabled={disabled}
                            onClick={() => onLinks([...links, { target: it.repo, watched: [], canBlock: true }])}
                          >
                            Add
                          </Button>
                        </>
                      ) : (
                        <span className="text-bad text-xs">{it.reason}</span>
                      )}
                    </div>
                  );
                })}
              </div>
              {linkable.view.items.length === 0 && <div className={note}>No repository found.</div>}
              {linkable.view.truncated && <div className={note}>Only the first 200 are shown. Search to narrow the list.</div>}
            </>
          )}
        </div>
      )}
    </div>
  );
}
