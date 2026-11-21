import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router";

import { api, paths, type MyTeamView, type TeamRepoView } from "../api";
import { ErrorBox, Loading } from "../ui";

const myTeamsKey = ["my-teams"];

// coverText names what a team covers, and says which entries it owns.
const coverText = (r: TeamRepoView) => `${r.project === "" ? `All repositories of ${r.instance}` : r.project}${r.owner ? " (owner)" : ""}`;

// MyTeams lists the session's teams in the user area (every team, for an
// admin), each linked to its page with the team's models and chain.
export default function MyTeams() {
  const teams = useQuery({ queryKey: myTeamsKey, queryFn: () => api.get<MyTeamView[]>("/api/me/teams") });
  return (
    <div className="narrow">
      <div className="top">
        <div>
          <h1>Teams</h1>
          <div className="sub">
            The teams you are on. A team that owns a repository can give its reviews its own models and keys.
          </div>
        </div>
      </div>
      {teams.isPending && <Loading />}
      {teams.isError && <ErrorBox error={teams.error} />}
      {teams.isSuccess &&
        (teams.data.length === 0 ? (
          <div className="card empty">No team yet.</div>
        ) : (
          teams.data.map((t) => (
            <section className="card" aria-label={t.name} key={t.id}>
              <h2>
                <Link to={paths.team(t.id)}>{t.name}</Link>
              </h2>
              <div className="facts">
                <div>Your role: {t.role}</div>
                <div>Repositories: {t.repos.length === 0 ? "none" : t.repos.map(coverText).join(", ")}</div>
              </div>
            </section>
          ))
        ))}
    </div>
  );
}
