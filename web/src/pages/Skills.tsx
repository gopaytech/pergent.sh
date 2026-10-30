import { useQuery } from "@tanstack/react-query";

import { api, type LibrarySkill } from "../api";
import { ErrorBox, Loading } from "../ui";

// Skills lists pergent.sh's skill library, read-only, with each skill's
// content shown as text.
export default function Skills() {
  const skills = useQuery({ queryKey: ["skills"], queryFn: () => api.get<LibrarySkill[]>("/api/skills") });
  return (
    <div className="narrow">
      <div className="top">
        <div>
          <h1>Skills</h1>
          <div className="sub">The skill library a repository's reviews can use. A repository's own skill of the same name replaces it.</div>
        </div>
      </div>
      {skills.isPending && <Loading />}
      {skills.isError && <ErrorBox error={skills.error} />}
      {skills.isSuccess && (
        <section className="card" aria-label="Library skills">
          {skills.data.length === 0 && <div className="row empty-row">The library has no skills.</div>}
          {skills.data.map((s) => (
            <details className="skill-doc" key={s.name}>
              <summary className="skill">
                <b className="grow">{s.name}</b>
                <span className="src">library</span>
              </summary>
              <pre className="doc">{s.content}</pre>
            </details>
          ))}
        </section>
      )}
    </div>
  );
}
