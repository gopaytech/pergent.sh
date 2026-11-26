import { useQuery } from "@tanstack/react-query";

import { Card } from "@/components/ui/card";

import { api, type MyTeamView } from "../api";
import { ErrorBox, Loading, PageHeader } from "../ui";
import TeamsTable from "./TeamsTable";

const myTeamsKey = ["my-teams"];

// MyTeams lists the session's teams in the user area (every team, for an
// admin), each linked to its page, with counts and no repository names.
export default function MyTeams() {
  const teams = useQuery({ queryKey: myTeamsKey, queryFn: () => api.get<MyTeamView[]>("/api/me/teams") });
  return (
    <div className="max-w-[1100px]">
      <PageHeader
        title="Teams"
        description="The teams you are on. A team that owns a repository can give its reviews its own models and keys."
      />
      {teams.isPending && <Loading />}
      {teams.isError && <ErrorBox error={teams.error} />}
      {teams.isSuccess &&
        (teams.data.length === 0 ? (
          <Card className="text-muted-foreground px-4 py-8 text-center text-sm">
            You are on no team yet, so you see no repositories. Ask an admin to add you to one.
          </Card>
        ) : (
          <TeamsTable rows={teams.data} showRole />
        ))}
    </div>
  );
}
