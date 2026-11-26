import { useMutation } from "@tanstack/react-query";
import { useState } from "react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";

import { useWrite } from "./layout/write";
import { limitProblem, parseQuota, quotaText } from "./quota";
import { ErrorBox } from "./ui";

// LimitField sets one monthly token limit at path: a number is a PUT of
// {monthlyTokens}, empty a DELETE (no limit). A value the API would refuse
// is caught before any request. label names the input and saveLabel its
// button; onSaved runs once the server took it. Give it a key of the limit,
// so a new value from the server starts it again.
export default function LimitField({
  label,
  saveLabel,
  hint,
  limit,
  path,
  onSaved,
}: {
  label: string;
  saveLabel: string;
  hint?: string;
  limit: number | null;
  path: string;
  onSaved: () => void;
}) {
  const write = useWrite();
  const [text, setText] = useState(quotaText(limit));
  const [problem, setProblem] = useState("");
  const save = useMutation({
    mutationFn: (n: number | null) => (n === null ? write<void>("DELETE", path) : write<void>("PUT", path, { monthlyTokens: n })),
    onSuccess: () => onSaved(),
  });
  const submit = () => {
    const n = parseQuota(text);
    if (n === undefined) {
      save.reset();
      setProblem(limitProblem);
      return;
    }
    setProblem("");
    save.mutate(n);
  };
  // Save waits for a change; Enter in the input saves too.
  const changed = text.trim() !== quotaText(limit);
  return (
    <div className="grid gap-1.5">
      <div className="text-sm font-medium">Monthly token limit</div>
      <div className="flex items-center gap-2">
        <Input
          className="max-w-48"
          type="text"
          inputMode="numeric"
          aria-label={label}
          placeholder="no limit"
          value={text}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              e.preventDefault();
              if (changed) submit();
            }
          }}
        />
        <Button type="button" variant="outline" size="sm" aria-label={saveLabel} disabled={save.isPending || !changed} onClick={submit}>
          Save
        </Button>
      </div>
      {hint && <div className="text-muted-foreground text-sm">{hint}</div>}
      {problem && <div className="text-bad text-sm">{problem}</div>}
      {save.isError && <ErrorBox error={save.error} />}
    </div>
  );
}
