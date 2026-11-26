import { TriangleAlertIcon } from "lucide-react";
import { Link } from "react-router";

import { Alert } from "@/components/ui/alert";
import { Card } from "@/components/ui/card";

import { useMe } from "../layout/me";
import { PageHeader } from "../ui";

const areas: [string, string, string][] = [
  ["/admin/users", "Users", "Who has signed in, who is an admin, and who waits for a team."],
  ["/admin/teams", "Teams", "Which repositories each team covers, and its members' roles."],
  ["/admin/integrations", "Integrations", "The GitLab and GitHub instances, and how people sign in."],
  ["/admin/settings", "Settings", "The model, the defaults and the skill library's default list."],
  ["/admin/usage", "Usage", "Where model tokens go."],
  ["/admin/audit", "Audit log", "What admins and the bot did."],
];

// AdminHome is the admin area's first page: where each admin task lives.
export default function AdminHome() {
  const me = useMe();
  return (
    <>
      <PageHeader title="Admin" description="Manage who can sign in, what pergent.sh is connected to, and how it behaves." />
      {me.waitingUsers > 0 && (
        <Alert variant="warn" role="note" className="mb-4">
          <TriangleAlertIcon aria-hidden="true" />
          <span className="prose">
            {me.waitingUsers} {me.waitingUsers === 1 ? "user is" : "users are"} waiting for a team.{" "}
            <Link className="font-medium underline underline-offset-4" to="/admin/users">
              Open Users
            </Link>
          </span>
        </Alert>
      )}
      <Card role="region" aria-label="Admin pages" className="gap-0 py-0">
        {areas.map(([to, label, text]) => (
          <div className="border-b px-4 py-3 last:border-b-0" key={to}>
            <Link className="text-primary text-sm font-medium underline-offset-4 hover:underline" to={to}>
              {label}
            </Link>
            <div className="text-muted-foreground text-sm">{text}</div>
          </div>
        ))}
      </Card>
    </>
  );
}
