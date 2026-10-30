import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { NavLink, Outlet, useNavigate } from "react-router";

import { api, type Me } from "../api";
import { ErrorBox, Loading, NotFound, platformName } from "../ui";
import { MeContext, useMe } from "./me";

type NavItem = { to: string; label: string; icon: string; end?: boolean };

const main: NavItem[] = [
  { to: "/", label: "Overview", icon: "🏠", end: true },
  { to: "/repos", label: "Repositories", icon: "📁" },
  { to: "/reviews", label: "Reviews", icon: "🔍" },
  { to: "/skills", label: "Skills", icon: "🧩" },
];

const admin: NavItem[] = [
  { to: "/integrations", label: "Integrations", icon: "🔌" },
  { to: "/settings", label: "Settings", icon: "⚙️" },
];

function Nav({ items }: { items: NavItem[] }) {
  return (
    <>
      {items.map((item) => (
        <NavLink key={item.to} to={item.to} end={item.end}>
          <span className="icon" aria-hidden="true">
            {item.icon}
          </span>
          {item.label}
        </NavLink>
      ))}
    </>
  );
}

function initials(name: string) {
  return name.replace(/[^a-zA-Z0-9]/g, "").slice(0, 2).toUpperCase();
}

function SignOut({ me }: { me: Me }) {
  const navigate = useNavigate();
  const client = useQueryClient();
  const out = useMutation({
    mutationFn: () => api.post<void>("/auth/logout", undefined, me.csrfToken),
    onSettled: () => {
      client.clear();
      navigate("/signin", { replace: true });
    },
  });
  return (
    <button type="button" className="linkish" onClick={() => out.mutate()} disabled={out.isPending}>
      Sign out
    </button>
  );
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
            {platformName(user.platform)} · {user.instance}
          </div>
          <nav className="nav" aria-label="Main">
            <Nav items={main} />
            {user.isAdmin && (
              <>
                <div className="section-label">Admin</div>
                <Nav items={admin} />
              </>
            )}
          </nav>
          <div className="me">
            <div className="avatar" aria-hidden="true">
              {initials(user.username)}
            </div>
            <span className="grow">
              {user.username} · {user.isAdmin ? "admin" : "developer"}
            </span>
            <SignOut me={user} />
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
  return me.isAdmin ? <>{children}</> : <NotFound />;
}
