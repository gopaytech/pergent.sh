# 🔁 How it works

When someone opens or pushes to a merge request (or pull request) in a repository you listed in the config:

1. GitLab sends a merge request webhook, or GitHub a `pull_request` webhook, to pergent.sh.
2. pergent.sh starts a Kubernetes Job. The Job checks out the change, runs `pergent agent` over it and posts the result back.
3. pergent.sh posts one comment on the merge request, updated in place on every push, and sets a status named `pergent`: a commit status on GitLab, a Check Run on GitHub. It also posts each finding as a thread on its lines ([Inline findings](inline-findings.md)).
4. If the review found no blocking issue (one that people dismissed doesn't count), actually reviewed something and skipped no changed file except under the rules that leave it reviewed (outside a skill's scope, excluded by a skill, no content changes), the approver approves it: a second GitLab user on GitLab, on GitHub the review App itself (one App is enough), a second GitHub App or a bot user. A draft is reviewed the same way but is approved only once it is marked ready.

People can also act on a review where they already are, in a comment on the merge request (`@<bot> review`, `@<bot> review failed`, `@<bot> dismiss <id> <reason>`) or with the buttons on the dashboard's review page: run the review again, run only the skills that failed, and set aside a finding that isn't a real problem. See [Commands](commands.md).

Everything that can comment, set statuses or approve stays inside pergent.sh. A Job only ever holds a read-only clone token, the key of its model and a one-time token to hand in its result.

This release covers GitLab (gitlab.com or any self-managed instance) and GitHub.com; see [GitHub](../setup/github.md) for the GitHub setup. An optional web [dashboard](dashboard.md) shows every review, lets admins manage teams, write and install skills, switch repositories on and change their settings, and lets admins add GitLab and GitHub instances, their secrets and the models without editing the config file. An instance added in the dashboard never blocks startup: when pergent.sh can't reach it, it starts anyway and the dashboard shows what to fix. Comment [commands](commands.md) and the dashboard's buttons re-run a review or dismiss a finding; they never change settings, answer questions or run code (see [Limits](../reference/limits.md)).

In these docs a "merge request" is also a GitHub pull request, and the "review bot" and "approve bot" are the GitHub review App and approver, unless a section says otherwise.

## ✨ Features

- **A Job per review**: each review runs as its own Kubernetes Job in your cluster, started by a merge request or pull request webhook, so reviews run in parallel up to `jobs.max_running`.
- **One comment, one status**: pergent.sh posts a single comment, updated in place on every push, and sets a status named `pergent`: a commit status on GitLab, a Check Run on GitHub.
- **Inline findings**: each finding is also posted as a thread on its lines, with a one-click fix when it is small, and resolved when a later push fixes it. See [Inline findings](inline-findings.md).
- **Incremental reviews**: a push reviews only the files it changed and carries the rest over from the last review. See [Incremental reviews](incremental-reviews.md).
- **Learn from dismissals**: a review is told what people rejected earlier in the repository, and why, so a false positive stops coming back. See [Learn from dismissals](learn-from-dismissals.md).
- **Quiet reviews**: a repository can ask for a review that says less: fewer nitpicks, no suggestion threads and a shorter comment, with everything that blocks still there. Off by default. See [Quiet reviews](quiet-reviews.md).
- **Deleted and renamed files**: a repository can ask for the lines a change deletes to be reviewed, and for a check of what still uses deleted files and renamed files' old paths. Off by default. See [Deleted and renamed files](deleted-files.md).
- **Comment layout**: a repository can choose which blocks of the review comment it shows (summary, findings, dismissed, checks, files), in what order, and whether each starts open or collapsed. See [Comment layout](comment-layout.md).
- **Models**: admins keep a list of models, pick the default, and can give a repository a model of its own. See [Models](models.md).
- **Skill results**: a tab for each repository shows how many of each skill's findings people fixed, dismissed or left open, so you can fix or drop the weak skills. See [Skill results](skill-results.md).
- **Approval with a human guard**: a clean review that actually reviewed something and skipped no file outside the allowed rules is approved by the approver; on GitLab, pergent.sh logs a warning for every project where one approval alone could merge.
- **GitLab and GitHub**: GitLab (gitlab.com or self-managed) and GitHub.com; on GitHub one App is enough to review and approve.
- **Dashboard**: every review, finding, file and log in the browser, with teams and roles for who sees what, an audit log, and instances, secrets and the list of models managed without editing the config file.
- **Skill library**: skills written in the dashboard or installed from a git repository, plus repository skills and instructions read from the target branch.
- **Comment commands**: `@<bot> review`, `@<bot> review failed` and `@<bot> dismiss <id> <reason>` in a comment, with matching buttons on the dashboard's review page.
- **Readiness scan**: advice, per repository, on what is missing for a review to do well, without changing anything and without the LLM.
- **Job isolation**: a Job holds only a read-only clone token, the key of its model and a one-time result token, never the platform token, and runs under a NetworkPolicy.
- **A small Helm chart**: it creates a Service and no Ingress, so you route to it with whatever your cluster uses; PostgreSQL stays outside the chart.
