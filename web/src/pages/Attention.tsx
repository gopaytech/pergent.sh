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
  if (items.length === 0) return <div className="text-muted-foreground border-t px-4 py-6 text-sm">Nothing needs attention.</div>;
  return (
    <>
      {items.map((a) => {
        const [tone, label] = kinds[a.kind] ?? ["warn", "Setup"];
        return (
          <div className="flex items-start gap-3 border-t px-4 py-3" key={`${a.instance}/${a.project}/${a.kind}`}>
            <div className="flex min-w-20 shrink-0 pt-0.5">
              <Pill tone={tone}>{label}</Pill>
            </div>
            <div className="min-w-0 grow">
              <div className="truncate text-sm font-medium">{a.project}</div>
              <div className="prose text-muted-foreground mt-0.5 text-sm">{a.message}</div>
            </div>
          </div>
        );
      })}
    </>
  );
}
