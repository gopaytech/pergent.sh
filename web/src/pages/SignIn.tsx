import { useQuery } from "@tanstack/react-query";

import { api, paths, type InstanceView } from "../api";
import { ErrorBox, Loading } from "../ui";

const platforms = new Set(["gitlab", "github"]);

// SignIn has one button per instance with sign-in. Each goes to the
// server's login route, which sends the browser on to GitLab or GitHub.
export default function SignIn() {
  const instances = useQuery({
    queryKey: ["instances"],
    queryFn: () => api.get<InstanceView[]>("/api/instances"),
  });
  const usable = (instances.data ?? []).filter((i) => platforms.has(i.platform));
  return (
    <div className="signin">
      <div className="card signin-card">
        <div className="brand dark">
          <div className="logo" aria-hidden="true">
            🦉
          </div>
          pergent.sh
        </div>
        <h1>Sign in to pergent.sh</h1>
        <p className="sub">See why a merge request or pull request wasn't approved, and manage the repositories pergent reviews.</p>
        {instances.isPending && <Loading />}
        {instances.isError && <ErrorBox error={instances.error} />}
        {instances.isSuccess && usable.length === 0 && (
          <div className="empty">No sign-in is configured. Ask the person who runs pergent.sh to set one up.</div>
        )}
        <div className="signin-buttons">
          {usable.map((i) => (
            <a key={`${i.platform}/${i.instance}`} className="btn primary" href={paths.login(i.platform, i.instance)}>
              Sign in with {i.label}
            </a>
          ))}
        </div>
      </div>
    </div>
  );
}
