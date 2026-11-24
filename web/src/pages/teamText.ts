// teamCountsText says how much a team covers: repositories, whole instances
// and how many it owns. A part that is 0 is left out, unless all are 0.
export function teamCountsText(c: { repoCount: number; instanceCount: number; ownedCount: number }): string {
  const parts: string[] = [];
  if (c.repoCount > 0) parts.push(`${c.repoCount} ${c.repoCount === 1 ? "repository" : "repositories"}`);
  if (c.instanceCount > 0) parts.push(`${c.instanceCount} whole ${c.instanceCount === 1 ? "instance" : "instances"}`);
  if (c.ownedCount > 0) parts.push(`owner of ${c.ownedCount}`);
  return parts.length === 0 ? "0 repositories" : parts.join(", ");
}

// membersText gives a member count with its noun.
export const membersText = (n: number): string => `${n} ${n === 1 ? "member" : "members"}`;
