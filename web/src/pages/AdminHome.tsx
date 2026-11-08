import { Link } from "react-router";

import { useMe } from "../layout/me";

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
      <div className="top">
        <div>
          <h1>Admin</h1>
          <div className="sub">Manage who can sign in, what pergent.sh is connected to, and how it behaves.</div>
        </div>
      </div>
      {me.waitingUsers > 0 && (
        <div className="alert" role="note">
          <span aria-hidden="true">⚠️</span>
          <span className="prose">
            {me.waitingUsers} {me.waitingUsers === 1 ? "user is" : "users are"} waiting for a team. <Link to="/admin/users">Open Users</Link>
          </span>
        </div>
      )}
      <section className="card" aria-label="Admin pages">
        {areas.map(([to, label, text]) => (
          <div className="row" key={to}>
            <div className="grow">
              <Link to={to}>{label}</Link>
              <div className="meta">{text}</div>
            </div>
          </div>
        ))}
      </section>
    </>
  );
}
