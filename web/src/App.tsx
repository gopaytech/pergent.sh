import { useQueryClient } from "@tanstack/react-query";
import { useEffect } from "react";
import { Route, Routes, useNavigate } from "react-router";

import { onGateChange, onUnauthorized } from "./api";
import Layout, { AdminOnly } from "./layout/Layout";
import AdminSettings from "./pages/AdminSettings";
import AuditLog from "./pages/AuditLog";
import Integrations from "./pages/Integrations";
import Overview from "./pages/Overview";
import Repositories from "./pages/Repositories";
import RepositoryDetail from "./pages/RepositoryDetail";
import ReviewDetail from "./pages/ReviewDetail";
import Reviews from "./pages/Reviews";
import SignIn from "./pages/SignIn";
import Skills from "./pages/Skills";
import Teams from "./pages/Teams";
import Users from "./pages/Users";
import { NotFound } from "./ui";

// App routes the pages. A 401 from any call drops what was loaded and
// sends the browser to the sign-in page; a 403 that says the account waits
// for a team or must change its password reads /api/me again, so the layout
// shows the page for that state.
export default function App() {
  const navigate = useNavigate();
  const client = useQueryClient();
  useEffect(
    () =>
      onUnauthorized(() => {
        client.clear();
        navigate("/signin", { replace: true });
      }),
    [client, navigate],
  );
  useEffect(() => onGateChange(() => void client.invalidateQueries({ queryKey: ["me"] })), [client]);
  return (
    <Routes>
      <Route path="/signin" element={<SignIn />} />
      <Route element={<Layout />}>
        <Route index element={<Overview />} />
        <Route path="reviews" element={<Reviews />} />
        {/* The project path has slashes: the rest is "{project}/{number}". */}
        <Route path="reviews/:instance/*" element={<ReviewDetail />} />
        <Route path="repos" element={<Repositories />} />
        <Route path="repos/:instance/*" element={<RepositoryDetail />} />
        <Route path="skills" element={<Skills />} />
        <Route
          path="integrations"
          element={
            <AdminOnly>
              <Integrations />
            </AdminOnly>
          }
        />
        <Route
          path="settings"
          element={
            <AdminOnly>
              <AdminSettings />
            </AdminOnly>
          }
        />
        <Route
          path="users"
          element={
            <AdminOnly>
              <Users />
            </AdminOnly>
          }
        />
        <Route
          path="teams"
          element={
            <AdminOnly>
              <Teams />
            </AdminOnly>
          }
        />
        <Route
          path="audit"
          element={
            <AdminOnly>
              <AuditLog />
            </AdminOnly>
          }
        />
        <Route path="*" element={<NotFound />} />
      </Route>
    </Routes>
  );
}
