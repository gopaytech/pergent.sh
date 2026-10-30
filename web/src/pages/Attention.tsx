import { Link } from "react-router";

import { paths, type AttentionView } from "../api";
import { Pill, platformName, type Tone } from "../ui";

const kinds: Record<string, [Tone, string]> = {
  single_approval: ["warn", "Setup"],
  approver_unreachable: ["warn", "Setup"],
  out_of_memory: ["bad", "Failing"],
  token_expiry: ["draft", "Token"],
};

// AttentionRows lists setup problems: a repository's, or (project "") the
// whole instance's.
export default function AttentionRows({
  items,
  platform,
  linkRepos = true,
}: {
  items: AttentionView[];
  platform: string;
  linkRepos?: boolean;
}) {
  if (items.length === 0) return <div className="row empty-row">Nothing needs attention.</div>;
  return (
    <>
      {items.map((a) => {
        const [tone, label] = kinds[a.kind] ?? ["warn", "Setup"];
        return (
          <div className="row" key={`${a.instance}/${a.project}/${a.kind}`}>
            <Pill tone={tone}>{label}</Pill>
            <div className="grow">
              <div className="title">
                {a.project === "" ? (
                  `${platformName(platform)} · ${a.instance}`
                ) : linkRepos ? (
                  <Link to={`${paths.repo(a.instance, a.project)}?tab=setup`}>{a.project}</Link>
                ) : (
                  a.project
                )}
              </div>
              <div className="why prose">{a.message}</div>
            </div>
          </div>
        );
      })}
    </>
  );
}
