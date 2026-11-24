import { type AttentionView } from "../api";
import { Pill, type Tone } from "../ui";

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

// AttentionRows lists a repository's setup problems, on its Setup checks tab.
export default function AttentionRows({ items }: { items: AttentionView[] }) {
  if (items.length === 0) return <div className="row empty-row">Nothing needs attention.</div>;
  return (
    <>
      {items.map((a) => {
        const [tone, label] = kinds[a.kind] ?? ["warn", "Setup"];
        return (
          <div className="row" key={`${a.instance}/${a.project}/${a.kind}`}>
            <Pill tone={tone}>{label}</Pill>
            <div className="grow">
              <div className="title">{a.project}</div>
              <div className="why prose">{a.message}</div>
            </div>
          </div>
        );
      })}
    </>
  );
}
