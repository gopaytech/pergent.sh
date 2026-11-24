import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router";

import { api, paths, type MyTeamView } from "../api";
import { ErrorBox, Loading } from "../ui";
import { membersText, teamCountsText } from "./teamText";

const myTeamsKey = ["my-teams"];

// MyTeams lists the session's teams in the user area (every team, for an
// admin), each linked to its page, with counts and no repository names.
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
          <div className="card empty">You are on no team yet, so you see no repositories. Ask an admin to add you to one.</div>
        ) : (
          teams.data.map((t) => (
            <section className="card" aria-label={t.name} key={t.id}>
              <h2>
                <Link to={paths.team(t.id)}>{t.name}</Link>
              </h2>
              <div className="facts">
                <div>Your role: {t.role}</div>
                <div>{teamCountsText(t)}</div>
                <div>{membersText(t.memberCount)}</div>
              </div>
            </section>
          ))
        ))}
    </div>
  );
}
