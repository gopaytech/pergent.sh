import { useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef } from "react";
import { Navigate, Route, Routes, useLocation, useNavigate } from "react-router";

import { onGateChange, onUnauthorized, type Me } from "./api";
import Layout, { AdminOnly } from "./layout/Layout";
import { inAdminArea, useMe } from "./layout/me";
import AdminHome from "./pages/AdminHome";
import AdminSettings from "./pages/AdminSettings";
import AdminSignIn from "./pages/AdminSignIn";
import AuditLog from "./pages/AuditLog";
import Integrations from "./pages/Integrations";
import Overview from "./pages/Overview";
import Repositories from "./pages/Repositories";
import RepositoryDetail from "./pages/RepositoryDetail";
import ReviewDetail from "./pages/ReviewDetail";
import Reviews from "./pages/Reviews";
import SignedOut from "./pages/SignedOut";
import SignIn from "./pages/SignIn";
import SkillEditor, { NewSkill } from "./pages/SkillEditor";
import SkillInstall from "./pages/SkillInstall";
import Skills from "./pages/Skills";
import SkillUpdates from "./pages/SkillUpdates";
import Teams from "./pages/Teams";
import Usage from "./pages/Usage";
import UsageRepository from "./pages/UsageRepository";
import Users from "./pages/Users";
import { NotFound } from "./ui";

// App routes the pages. There are two areas: the user pages (Overview,
// Repositories, Reviews, Skills) with their sign-in at /sign-in, and the admin
// pages under /admin with theirs at /admin/sign-in. A 401 from any call drops
// what was loaded and sends the browser to the sign-in page of the area it was
// in; a 403 that says the account waits for a team or must change its password
// reads /api/me again, so the layout shows the page for that state.
//
// The server holds the admin area's data back from a user. What this file does
// for them is the page: an admin page shows a user the not-found page.
export default function App() {
  const navigate = useNavigate();
  const client = useQueryClient();
  const { pathname } = useLocation();
  // The effect below must not restart on every navigation: it reads the path
  // through a ref.
  const here = useRef(pathname);
  useEffect(() => {
    here.current = pathname;
  }, [pathname]);
  useEffect(
    () =>
      onUnauthorized(() => {
        // A local admin has no SSO button on /sign-in: whichever page the 401
        // came on, they go to theirs. The last /api/me is read before the
        // cache is cleared.
        const local = client.getQueryData<Me>(["me"])?.kind === "password";
        client.clear();
        navigate(local || inAdminArea(here.current) ? "/admin/sign-in" : "/sign-in", { replace: true });
      }),
    [client, navigate],
  );
  useEffect(() => onGateChange(() => void client.invalidateQueries({ queryKey: ["me"] })), [client]);
  return (
    <Routes>
      <Route path="/sign-in" element={<SignIn />} />
      <Route path="/admin/sign-in" element={<AdminSignIn />} />
      <Route path="/signed-out" element={<SignedOut />} />
      {/* The sign-in page's old address; the refusal code goes with it. */}
      <Route path="/signin" element={<OldSignIn />} />
      <Route element={<Layout />}>
        <Route index element={<Overview />} />
        <Route path="reviews" element={<Reviews />} />
        {/* The project path has slashes: the rest is "{project}/{number}". */}
        <Route path="reviews/:instance/*" element={<ReviewDetail />} />
        <Route path="repos" element={<Repositories />} />
        <Route path="repos/:instance/*" element={<RepositoryDetail />} />
        <Route path="skills" element={<Skills />} />
        <Route
          path="skills/new"
          element={
            <AdminOnly>
              <NewSkill />
            </AdminOnly>
          }
        />
        <Route
          path="skills/install"
          element={
            <AdminOnly>
              <SkillInstall />
            </AdminOnly>
          }
        />
        <Route
          path="skills/updates"
          element={
            <AdminOnly>
              <SkillUpdates />
            </AdminOnly>
          }
        />
        <Route path="skills/view/:name" element={<SkillEditor />} />
        <Route
          path="admin"
          element={
            <AdminOnly>
              <AdminHome />
            </AdminOnly>
          }
        />
        <Route
          path="admin/integrations"
          element={
            <AdminOnly>
              <Integrations />
            </AdminOnly>
          }
        />
        <Route
          path="admin/settings"
          element={
            <AdminOnly>
              <AdminSettings />
            </AdminOnly>
          }
        />
        <Route
          path="admin/users"
          element={
            <AdminOnly>
              <Users />
            </AdminOnly>
          }
        />
        <Route
          path="admin/teams"
          element={
            <AdminOnly>
              <Teams />
            </AdminOnly>
          }
        />
        <Route
          path="admin/audit"
          element={
            <AdminOnly>
              <AuditLog />
            </AdminOnly>
          }
        />
        <Route
          path="admin/usage"
          element={
            <AdminOnly>
              <Usage />
            </AdminOnly>
          }
        />
        {/* The project path has slashes, as in the repository route. */}
        <Route
          path="admin/usage/:instance/*"
          element={
            <AdminOnly>
              <UsageRepository />
            </AdminOnly>
          }
        />
        {/* The admin pages' old addresses: an admin is sent to the new one, a user finds nothing. */}
        <Route path="users" element={<OldAdminPage to="/admin/users" />} />
        <Route path="teams" element={<OldAdminPage to="/admin/teams" />} />
        <Route path="integrations" element={<OldAdminPage to="/admin/integrations" />} />
        <Route path="settings" element={<OldAdminPage to="/admin/settings" />} />
        <Route path="audit" element={<OldAdminPage to="/admin/audit" />} />
        <Route path="*" element={<NotFound />} />
      </Route>
    </Routes>
  );
}

// OldSignIn sends /signin to /sign-in, keeping ?error=<code>.
function OldSignIn() {
  const { search } = useLocation();
  return <Navigate to={{ pathname: "/sign-in", search }} replace />;
}

// OldAdminPage sends an admin from a page's old address to its new one under
// /admin; anyone else gets the not-found page, as for any admin page.
function OldAdminPage({ to }: { to: string }) {
  const me = useMe();
  return me.admin ? <Navigate to={to} replace /> : <NotFound />;
}
