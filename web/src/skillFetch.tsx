import { useMutation, useQuery } from "@tanstack/react-query";

import { api, paths, type SkillFetch } from "./api";
import { useWrite } from "./layout/write";

// fetchPollMs is how often a read that is still running is asked again.
export const fetchPollMs = 1000;

// FetchStart is what a read of a git source is started with: a repository of
// an instance, or a public URL, at a branch or tag, in a folder ("" for the
// top). fromRef is for a check for updates: the ref the skills came from.
export type FetchStart = {
  instance?: string;
  repo?: string;
  url?: string;
  ref: string;
  path: string;
  purpose: "install" | "update";
  fromRef?: string;
};

// useStartFetch starts a read: POST /api/admin/skill-fetches, which answers
// 202 with the read's id. The body keeps only the fields that are set.
export function useStartFetch(onStarted: (id: string) => void) {
  const write = useWrite();
  return useMutation({
    mutationFn: (start: FetchStart) => {
      const body = Object.fromEntries(Object.entries(start).filter(([, v]) => v !== undefined && v !== ""));
      return write<{ id: string }>("POST", paths.apiSkillFetches, body);
    },
    onSuccess: (r) => onStarted(r.id),
  });
}

// useSkillFetch follows a read until it is done or failed.
export function useSkillFetch(id: string | undefined) {
  return useQuery({
    queryKey: ["skill-fetch", id],
    queryFn: () => api.get<SkillFetch>(paths.apiSkillFetch(id ?? "")),
    enabled: id !== undefined,
    refetchInterval: (q) => (q.state.data?.status === "running" ? fetchPollMs : false),
  });
}

// FetchStatus says a read is running, or why it failed; it is nothing once
// the read is done.
export function FetchStatus({ read }: { read: SkillFetch }) {
  if (read.status === "running") return <div className="meta pad">Reading the source…</div>;
  if (read.status === "failed") {
    return (
      <div className="error" role="alert">
        {read.error || "Reading the source failed."}
      </div>
    );
  }
  return null;
}
