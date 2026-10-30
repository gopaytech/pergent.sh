import { NavLink, Route, Routes } from "react-router";

import Placeholder from "./Placeholder";

type NavItem = { to: string; label: string; end?: boolean };

const main: NavItem[] = [
  { to: "/", label: "Overview", end: true },
  { to: "/repos", label: "Repositories" },
  { to: "/reviews", label: "Reviews" },
  { to: "/skills", label: "Skills" },
];

const admin: NavItem[] = [
  { to: "/integrations", label: "Integrations" },
  { to: "/settings", label: "Settings" },
  { to: "/organization", label: "Organization" },
];

function Nav({ items }: { items: NavItem[] }) {
  return (
    <>
      {items.map((item) => (
        <NavLink key={item.to} to={item.to} end={item.end}>
          {item.label}
        </NavLink>
      ))}
    </>
  );
}

// App is the dashboard's shell: the dark menu on the left, the page on the right.
export default function App() {
  return (
    <div className="shell">
      <aside>
        <div className="brand">
          <div className="logo" aria-hidden="true">
            p
          </div>
          pergent.sh
        </div>
        <nav className="nav" aria-label="Main">
          <Nav items={main} />
          <div className="section-label">Admin</div>
          <Nav items={admin} />
        </nav>
      </aside>
      <main>
        <Routes>
          <Route path="/" element={<Placeholder title="Overview" />} />
          <Route path="*" element={<Placeholder title="Coming soon" />} />
        </Routes>
      </main>
    </div>
  );
}
