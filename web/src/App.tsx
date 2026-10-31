import { useQueryClient } from "@tanstack/react-query";
import { useEffect } from "react";
import { Route, Routes, useNavigate } from "react-router";

import { onUnauthorized } from "./api";
import Layout, { AdminOnly } from "./layout/Layout";
import AdminSettings from "./pages/AdminSettings";
import Integrations from "./pages/Integrations";
import Overview from "./pages/Overview";
import Repositories from "./pages/Repositories";
import RepositoryDetail from "./pages/RepositoryDetail";
import ReviewDetail from "./pages/ReviewDetail";
import Reviews from "./pages/Reviews";
import SignIn from "./pages/SignIn";
import Skills from "./pages/Skills";
import { NotFound } from "./ui";

// App routes the pages. A 401 from any call drops what was loaded and
// sends the browser to the sign-in page.
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
        {/* Task 10's pages; until they exist an admin gets a plain notice. */}
        {["users", "teams", "audit"].map((path) => (
          <Route
            key={path}
            path={path}
            element={
              <AdminOnly>
                <div className="empty">This page isn't built yet.</div>
              </AdminOnly>
            }
          />
        ))}
        <Route path="*" element={<NotFound />} />
      </Route>
    </Routes>
  );
}
