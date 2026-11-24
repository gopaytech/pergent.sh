import { useQuery } from "@tanstack/react-query";
import { useEffect, useState, type ReactNode } from "react";
import { NavLink, Outlet, useLocation } from "react-router";

import { api, type Me } from "../api";
import { Icon, type IconName } from "../icons";
import ChangePassword from "../pages/ChangePassword";
import Waiting from "../pages/Waiting";
import { ErrorBox, Loading, NotFound } from "../ui";
import { MeContext, inAdminArea, useMe } from "./me";
import SignOut from "./SignOut";

type NavItem = { to: string; label: string; icon: IconName; end?: boolean; badge?: string };

// The user area: what a user sees, and an admin too.
const userItems: NavItem[] = [
  { to: "/", label: "Overview", icon: "home", end: true },
  { to: "/repos", label: "Repositories", icon: "repos" },
  { to: "/reviews", label: "Reviews", icon: "reviews" },
  { to: "/skills", label: "Skills", icon: "skills" },
  { to: "/rules", label: "Rules", icon: "rules" },
  // Every account past the waiting page is on a team (an admin sees them all).
  { to: "/teams", label: "Teams", icon: "teams" },
];

// withAPIClients puts API clients right after Repositories: the review API's
// clients are an admin's, so a user's menu never has the link.
const withAPIClients = (items: NavItem[]): NavItem[] =>
  items.flatMap((item) => (item.to === "/repos" ? [item, { to: "/api-clients", label: "API clients", icon: "clients" }] : [item]));

// adminItems is the admin area; Users says how many accounts wait for a
// team.
const adminItems = (waitingUsers: number): NavItem[] => [
  { to: "/admin", label: "Admin", icon: "admin", end: true },
  { to: "/admin/users", label: "Users", icon: "user", badge: waitingUsers > 0 ? `${waitingUsers} waiting` : undefined },
  { to: "/admin/teams", label: "Teams", icon: "teams" },
  { to: "/admin/integrations", label: "Integrations", icon: "integrations" },
  { to: "/admin/settings", label: "Settings", icon: "settings" },
  { to: "/admin/usage", label: "Usage", icon: "usage" },
  { to: "/admin/audit", label: "Audit log", icon: "audit" },
];

// roleLabel is what the menu calls the signed-in account.
const roleLabel = (me: Me) => (me.admin ? "Admin" : "User");

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
            <Icon name={item.icon} />
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
  // On a phone the menu is folded behind a button; any move closes it.
  const [menuOpen, setMenuOpen] = useState(false);
  useEffect(() => setMenuOpen(false), [pathname]);
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
  // The browser tab names the page: its h1 (without a status pill), then the
  // product. Pages set their h1 when their data comes, so it is watched.
  const loaded = me.isSuccess;
  useEffect(() => {
    const main = document.querySelector("main");
    if (!main) return;
    const name = () => {
      const h1 = main.querySelector("h1");
      if (!h1) return "";
      const copy = h1.cloneNode(true) as HTMLElement;
      copy.querySelectorAll(".pill").forEach((p) => p.remove());
      return copy.textContent?.trim() ?? "";
    };
    const set = () => {
      const page = name();
      const title = page ? `${page} · pergent.sh` : "pergent.sh";
      if (document.title !== title) document.title = title;
    };
    set();
    const watch = new MutationObserver(set);
    watch.observe(main, { subtree: true, childList: true, characterData: true });
    return () => watch.disconnect();
  }, [loaded]);
  if (me.isPending) return <Loading />;
  if (me.isError) {
    return (
      <main>
        <ErrorBox error={me.error} onRetry={() => void me.refetch()} />
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
      <div className={menuOpen ? "shell menu-open" : "shell"}>
        {/* The bar a phone shows instead of the menu; hidden on a wide screen. */}
        <header className="topbar">
          <button
            type="button"
            className="menu-btn"
            aria-expanded={menuOpen}
            aria-controls="side-menu"
            onClick={() => setMenuOpen(!menuOpen)}
          >
            <Icon name={menuOpen ? "close" : "menu"} size={20} />
            <span>Menu</span>
          </button>
          <div className="brand">
            <div className="logo" aria-hidden="true">
              🦉
            </div>
            pergent.sh
          </div>
        </header>
        <aside id="side-menu">
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
                <Nav items={[{ to: "/", label: "User pages", icon: "back", end: true }]} />
              </>
            ) : (
              <>
                <Nav items={user.admin ? withAPIClients(userItems) : userItems} />
                {user.admin && (
                  <>
                    <div className="section-label">Admin</div>
                    <Nav
                      items={[
                        {
                          to: "/admin",
                          label: "Admin area",
                          icon: "admin",
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
          {/* The name on one line, cut short with the whole of it in the
              tooltip; the role and Sign out below it. */}
          <section className="me" aria-label="Account">
            <div className="avatar" aria-hidden="true">
              {initials(user.username)}
            </div>
            <div className="who">
              <div className="name" title={user.username}>
                {user.username}
              </div>
              <div className="meta-line">
                <span className="role">{roleLabel(user)}</span>
                <span aria-hidden="true">·</span>
                <SignOut />
              </div>
            </div>
          </section>
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
