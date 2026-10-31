import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { NavLink, Outlet } from "react-router";

import { api, type Me } from "../api";
import ChangePassword from "../pages/ChangePassword";
import Waiting from "../pages/Waiting";
import { ErrorBox, Loading, NotFound, platformName } from "../ui";
import { MeContext, useMe } from "./me";
import SignOut from "./SignOut";

type NavItem = { to: string; label: string; icon: string; end?: boolean; badge?: string };

const main: NavItem[] = [
  { to: "/", label: "Overview", icon: "🏠", end: true },
  { to: "/repos", label: "Repositories", icon: "📁" },
  { to: "/reviews", label: "Reviews", icon: "🔍" },
  { to: "/skills", label: "Skills", icon: "🧩" },
];

// adminItems is the Admin section; Users says how many accounts wait for a
// team.
const adminItems = (waitingUsers: number): NavItem[] => [
  { to: "/users", label: "Users", icon: "👤", badge: waitingUsers > 0 ? `${waitingUsers} waiting` : undefined },
  { to: "/teams", label: "Teams", icon: "👥" },
  { to: "/integrations", label: "Integrations", icon: "🔌" },
  { to: "/settings", label: "Settings", icon: "⚙️" },
  { to: "/audit", label: "Audit log", icon: "📜" },
];

// roleLabel is what the menu calls the signed-in account.
const roleLabel = (me: Me) => (me.operator ? "operator" : me.admin ? "admin" : "member");

function Nav({ items }: { items: NavItem[] }) {
  return (
    <>
      {items.map((item) => (
        <NavLink key={item.to} to={item.to} end={item.end}>
          <span className="icon" aria-hidden="true">
            {item.icon}
          </span>
          {item.label}
          {item.badge && <span className="badge">{item.badge}</span>}
        </NavLink>
      ))}
    </>
  );
}

function initials(name: string) {
  return name.replace(/[^a-zA-Z0-9]/g, "").slice(0, 2).toUpperCase();
}

// Layout is the dashboard's shell: the dark menu on the left, the page on
// the right. It loads the signed-in user first; a 401 sends the browser to
// the sign-in page (see App).
export default function Layout() {
  const me = useQuery({ queryKey: ["me"], queryFn: () => api.get<Me>("/api/me"), staleTime: 60_000 });
  if (me.isPending) return <Loading />;
  if (me.isError) {
    return (
      <main>
        <ErrorBox error={me.error} />
      </main>
    );
  }
  const user = me.data;
  // The server answers an account in either state nothing but who it is
  // (and a sign-out, and the password change), so nothing else is shown.
  if (user.mustChangePassword || user.waiting) {
    return <MeContext.Provider value={user}>{user.mustChangePassword ? <ChangePassword /> : <Waiting />}</MeContext.Provider>;
  }
  return (
    <MeContext.Provider value={user}>
      <div className="shell">
        <aside>
          <div className="brand">
            <div className="logo" aria-hidden="true">
              🦉
            </div>
            pergent.sh
          </div>
          <div className="org" title="The instance you signed in with">
            {user.operator ? "All instances" : `${platformName(user.platform)} · ${user.instance}`}
          </div>
          <nav className="nav" aria-label="Main">
            <Nav items={main} />
            {user.admin && (
              <>
                <div className="section-label">Admin</div>
                <Nav items={adminItems(user.waitingUsers)} />
              </>
            )}
          </nav>
          <div className="me">
            <div className="avatar" aria-hidden="true">
              {initials(user.username)}
            </div>
            <span className="grow">
              {user.username} · {roleLabel(user)}
            </span>
            <SignOut />
          </div>
        </aside>
        <main>
          <Outlet />
        </main>
      </div>
    </MeContext.Provider>
  );
}

// AdminOnly shows children to admins; anyone else gets the not-found page,
// and the admin API is never called for them.
export function AdminOnly({ children }: { children: ReactNode }) {
  const me = useMe();
  return me.admin ? <>{children}</> : <NotFound />;
}
