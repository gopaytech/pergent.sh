import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { NavLink, Outlet, useLocation } from "react-router";

import { api, type Me } from "../api";
import ChangePassword from "../pages/ChangePassword";
import Waiting from "../pages/Waiting";
import { ErrorBox, Loading, NotFound } from "../ui";
import { MeContext, inAdminArea, useMe } from "./me";
import SignOut from "./SignOut";

type NavItem = { to: string; label: string; icon: string; end?: boolean; badge?: string };

// The user area: what a user sees, and an admin too.
const userItems: NavItem[] = [
  { to: "/", label: "Overview", icon: "🏠", end: true },
  { to: "/repos", label: "Repositories", icon: "📁" },
  { to: "/reviews", label: "Reviews", icon: "🔍" },
  { to: "/skills", label: "Skills", icon: "🧩" },
];

// adminItems is the admin area; Users says how many accounts wait for a
// team.
const adminItems = (waitingUsers: number): NavItem[] => [
  { to: "/admin", label: "Admin", icon: "🛡️", end: true },
  { to: "/admin/users", label: "Users", icon: "👤", badge: waitingUsers > 0 ? `${waitingUsers} waiting` : undefined },
  { to: "/admin/teams", label: "Teams", icon: "👥" },
  { to: "/admin/integrations", label: "Integrations", icon: "🔌" },
  { to: "/admin/settings", label: "Settings", icon: "⚙️" },
  { to: "/admin/audit", label: "Audit log", icon: "📜" },
];

// roleLabel is what the menu calls the signed-in account.
const roleLabel = (me: Me) => (me.admin ? "admin" : "user");

// waitingRefetch is how often a waiting account asks whether it was added to
// a team, in ms.
const waitingRefetch = 30_000;

function Nav({ items }: { items: NavItem[] }) {
  return (
    <>
      {items.map((item) => (
        <NavLink
          key={item.to}
          to={item.to}
          end={item.end}
          // The badge is part of the name, set apart from the label.
          aria-label={item.badge ? `${item.label}, ${item.badge}` : undefined}
        >
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
// the sign-in page (see App). The menu shows the area the path is in: the
// user pages, or, for an admin under /admin, the admin pages, each with a link
// to the other. Hiding a link is not the check: the server holds the admin
// data back from a user, and AdminOnly shows a user the not-found page.
export default function Layout() {
  const { pathname } = useLocation();
  const me = useQuery({
    queryKey: ["me"],
    queryFn: () => api.get<Me>("/api/me"),
    staleTime: 60_000,
    // A waiting account asks again, so one added to a team gets in without
    // a reload.
    refetchInterval: (q) => (q.state.data?.waiting ? waitingRefetch : false),
    // With JWT sign-in, reading /api/me again signs the person in again if the
    // proxy still vouches for them: a tab left open after signing out in
    // another must not do that by itself.
    refetchOnWindowFocus: (q) => q.state.data?.signIn !== "jwt",
  });
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
  const admin = user.admin && inAdminArea(pathname);
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
          {admin && <div className="org">Admin area</div>}
          <nav className="nav" aria-label="Main">
            {admin ? (
              <>
                <Nav items={adminItems(user.waitingUsers)} />
                <div className="section-label">Back to</div>
                <Nav items={[{ to: "/", label: "User pages", icon: "↩️", end: true }]} />
              </>
            ) : (
              <>
                <Nav items={userItems} />
                {user.admin && (
                  <>
                    <div className="section-label">Admin</div>
                    <Nav
                      items={[
                        {
                          to: "/admin",
                          label: "Admin area",
                          icon: "🛡️",
                          end: true,
                          badge: user.waitingUsers > 0 ? `${user.waitingUsers} waiting` : undefined,
                        },
                      ]}
                    />
                  </>
                )}
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
