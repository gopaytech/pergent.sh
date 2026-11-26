import { useQuery } from "@tanstack/react-query";
import { useEffect, useState, type ReactNode } from "react";
import { NavLink, Outlet, useLocation } from "react-router";

import {
  ChartColumn,
  FolderGit2,
  GitPullRequest,
  KeyRound,
  LayoutDashboard,
  Menu,
  Plug,
  Puzzle,
  Ruler,
  ScrollText,
  Shield,
  SlidersHorizontal,
  Undo2,
  User,
  Users,
  X,
  type LucideIcon,
} from "lucide-react";

import { cn } from "@/lib/utils";

import { api, type Me } from "../api";
import ChangePassword from "../pages/ChangePassword";
import Waiting from "../pages/Waiting";
import { ErrorBox, Loading, Logo, NotFound } from "../ui";
import { MeContext, inAdminArea, useMe } from "./me";
import SignOut from "./SignOut";

type NavItem = { to: string; label: string; icon: LucideIcon; end?: boolean; badge?: string };

// The user area: what a user sees, and an admin too.
const userItems: NavItem[] = [
  { to: "/", label: "Overview", icon: LayoutDashboard, end: true },
  { to: "/repos", label: "Repositories", icon: FolderGit2 },
  { to: "/reviews", label: "Reviews", icon: GitPullRequest },
  { to: "/skills", label: "Skills", icon: Puzzle },
  { to: "/rules", label: "Rules", icon: Ruler },
  // Every account past the waiting page is on a team (an admin sees them all).
  { to: "/teams", label: "Teams", icon: Users },
];

// withAPIClients puts API clients right after Repositories: the review API's
// clients are an admin's, so a user's menu never has the link.
const withAPIClients = (items: NavItem[]): NavItem[] =>
  items.flatMap((item) => (item.to === "/repos" ? [item, { to: "/api-clients", label: "API clients", icon: KeyRound }] : [item]));

// adminItems is the admin area; Users says how many accounts wait for a
// team.
const adminItems = (waitingUsers: number): NavItem[] => [
  { to: "/admin", label: "Admin", icon: Shield, end: true },
  { to: "/admin/users", label: "Users", icon: User, badge: waitingUsers > 0 ? `${waitingUsers} waiting` : undefined },
  { to: "/admin/teams", label: "Teams", icon: Users },
  { to: "/admin/integrations", label: "Integrations", icon: Plug },
  { to: "/admin/settings", label: "Settings", icon: SlidersHorizontal },
  { to: "/admin/usage", label: "Usage", icon: ChartColumn },
  { to: "/admin/audit", label: "Audit log", icon: ScrollText },
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
          className={({ isActive }) =>
            cn(
              "text-sidebar-foreground flex h-8 items-center gap-2 rounded-md px-2 text-sm outline-none transition-colors focus-visible:ring-[3px] focus-visible:ring-sidebar-ring/50",
              "hover:bg-sidebar-accent hover:text-sidebar-accent-foreground",
              isActive && "active bg-card text-sidebar-accent-foreground font-medium shadow-xs ring-1 ring-sidebar-border",
            )
          }
        >
          <span className="icon" aria-hidden="true">
            <item.icon className="size-4" />
          </span>
          <span className="truncate">{item.label}</span>
          {item.badge && (
            <span className="badge bg-warn-soft text-warn ml-auto rounded-full px-1.5 py-px text-[11px] font-medium">
              {item.badge}
            </span>
          )}
        </NavLink>
      ))}
    </>
  );
}

function initials(name: string) {
  return name.replace(/[^a-zA-Z0-9]/g, "").slice(0, 2).toUpperCase();
}

// Layout is the dashboard's shell: the light menu on the left, the page on
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
      <div className={cn("shell min-h-svh md:grid md:grid-cols-[232px_minmax(0,1fr)]", menuOpen && "menu-open")}>
        {/* The bar a phone shows instead of the menu; hidden on a wide screen. */}
        <header className="topbar bg-background sticky top-0 z-30 flex h-12 items-center gap-2 border-b px-3 md:hidden">
          <button
            type="button"
            className="menu-btn hover:bg-accent inline-flex h-8 items-center gap-1.5 rounded-md px-2 text-sm font-medium"
            aria-expanded={menuOpen}
            aria-controls="side-menu"
            onClick={() => setMenuOpen(!menuOpen)}
          >
            {menuOpen ? <X className="size-5" aria-hidden="true" /> : <Menu className="size-5" aria-hidden="true" />}
            <span>Menu</span>
          </button>
          <div className="brand flex items-center gap-2 text-[15px] font-semibold">
            <Logo />
            pergent.sh
          </div>
        </header>
        {menuOpen && <div className="fixed inset-0 z-30 bg-black/30 md:hidden" aria-hidden="true" onClick={() => setMenuOpen(false)} />}
        <aside
          id="side-menu"
          className={cn(
            "bg-sidebar text-sidebar-foreground border-sidebar-border flex-col gap-4 border-r p-3",
            "md:sticky md:top-0 md:flex md:h-svh",
            menuOpen ? "fixed inset-y-0 left-0 z-40 flex w-64 overflow-y-auto shadow-lg" : "hidden",
          )}
        >
          <div className="brand text-foreground flex h-9 items-center gap-2 px-2 text-[15px] font-semibold max-md:hidden">
            <Logo />
            pergent.sh
          </div>
          {admin && (
            <div className="org bg-sidebar-accent text-sidebar-accent-foreground rounded-md px-2 py-1.5 text-xs font-medium">
              Admin area
            </div>
          )}
          <nav className="nav flex flex-1 flex-col gap-0.5" aria-label="Main">
            {admin ? (
              <>
                <Nav items={adminItems(user.waitingUsers)} />
                <div className="section-label text-muted-foreground mt-4 mb-1 px-2 text-xs font-medium">Back to</div>
                <Nav items={[{ to: "/", label: "User pages", icon: Undo2, end: true }]} />
              </>
            ) : (
              <>
                <Nav items={user.admin ? withAPIClients(userItems) : userItems} />
                {user.admin && (
                  <>
                    <div className="section-label text-muted-foreground mt-4 mb-1 px-2 text-xs font-medium">Admin</div>
                    <Nav
                      items={[
                        {
                          to: "/admin",
                          label: "Admin area",
                          icon: Shield,
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
          <section className="me border-sidebar-border flex items-center gap-2 border-t px-1 pt-3" aria-label="Account">
            <div
              className="avatar bg-primary text-primary-foreground grid size-8 shrink-0 place-items-center rounded-full text-xs font-semibold"
              aria-hidden="true"
            >
              {initials(user.username)}
            </div>
            <div className="who min-w-0 flex-1">
              <div className="name text-foreground truncate text-sm font-medium" title={user.username}>
                {user.username}
              </div>
              <div className="meta-line text-muted-foreground flex items-center gap-1.5 text-xs">
                <span className="role">{roleLabel(user)}</span>
                <span aria-hidden="true">·</span>
                <SignOut />
              </div>
            </div>
          </section>
        </aside>
        <main className="mx-auto w-full max-w-[1280px] min-w-0 px-4 py-6 md:px-8 md:py-8">
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
