import { Link } from "react-router";

import { paths, type AttentionView } from "../api";
import { Pill, platformName, type Tone } from "../ui";

const kinds: Record<string, [Tone, string]> = {
  single_approval: ["warn", "Setup"],
  approver_unreachable: ["warn", "Setup"],
  out_of_memory: ["bad", "Failing"],
  clone_failing: ["bad", "Failing"],
  token_expiry: ["draft", "Token"],
  no_skill_coverage: ["warn", "Skills"],
  inputs_over_budget: ["bad", "Failing"],
  no_project_context: ["warn", "Context"],
  callback_failing: ["bad", "Failing"],
};

// The kinds the readiness scan raises: their rows open the Readiness tab.
const readinessKinds = new Set(["no_skill_coverage", "inputs_over_budget", "no_project_context"]);

// AttentionRows lists setup problems: a repository's, (project "") the whole
// instance's, or (client set) an API client's.
export default function AttentionRows({
  items,
  platform,
  linkRepos = true,
}: {
  items: AttentionView[];
  // platform is the platform of the repository the rows are about, when
  // there is one: an instance-wide row then reads "GitLab · main".
  platform?: string;
  linkRepos?: boolean;
}) {
  if (items.length === 0) return <div className="row empty-row">Nothing needs attention.</div>;
  return (
    <>
      {items.map((a) => {
        const [tone, label] = kinds[a.kind] ?? ["warn", "Setup"];
        return (
          <div className="row" key={`${a.instance}/${a.project}/${a.client ?? ""}/${a.kind}`}>
            <Pill tone={tone}>{label}</Pill>
            <div className="grow">
              <div className="title">
                {a.client ? (
                  <Link to={paths.clientPage(a.client)}>API client {a.client}</Link>
                ) : a.project === "" ? (
                  platform ? `${platformName(platform)} · ${a.instance}` : a.instance
                ) : linkRepos ? (
                  <Link to={`${paths.repo(a.instance, a.project)}?tab=${readinessKinds.has(a.kind) ? "readiness" : "setup"}`}>{a.project}</Link>
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
