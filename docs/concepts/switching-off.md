# 📴 Pausing and removing a repository

A repository is on when it is listed in the config or switched on in the [dashboard](dashboard.md). A dashboard repository is paused and resumed with its switch on **Repositories** (admins) or from the repository page (maintainers), and removed for good from the Settings tab of the repository page:

| Action | Who | Where | Asks first | What happens |
|---|---|---|---|---|
| Pause | admin, maintainer of the owner team | an admin: the switch on **Repositories**. A maintainer: **Pause** on the repository page | no | The repository stays in the list with every setting. On the next reconcile pergent.sh deletes its webhook and revokes its deploy token. Its readiness scans and setup problems are deleted. Its dismissals, comment commands, inline threads, finding fates, [code graph](code-graph.md) and review history stay. |
| Resume | admin, maintainer of the owner team | an admin: the same switch. A maintainer: **Resume** on the **Paused** banner of the repository page, or in the picker | no | pergent.sh creates a new webhook and deploy token and queues a readiness scan. The settings, dismissals and review history are the ones from before the pause. |
| Remove | admin, maintainer of the owner team | **Remove repository** on the Settings tab of the repository page | yes | The repository, its settings and its team links are deleted. Its webhook and deploy token are removed, and so are its dismissals, comment commands, inline threads, finding fates and [code graph](code-graph.md). It can be added again with **+ Add repository**, with every setting at its default. |

The reconcile of that one repository starts within seconds of the switch. The hourly reconcile also covers every repository.

**While a repository is paused:**

- It stays in the list, with its switch off and `Paused` in the Health column. It is never marked `Needs attention`, whatever setup problems are left.
- Whoever could see it before still sees it, and its reviews. Its settings stay editable by the same roles, and its page says `Paused` at the top.
- Nothing starts a Job or acts on the platform for it. **Scan now** and **Start review** are hidden, and the API answers them and the review buttons (re-run, dismiss, restore) `409` with `the repository is paused`. A webhook for it is ignored and a comment command is dropped, as for a repository that is not switched on. Runs still queued for it are dropped when they come up for dispatch; a review that was already running is still posted, but never approved, as with approval turned off.
- The bot's approvals on open merge requests and pull requests stay as they are. Nothing is posted on them.
- **+ Add repository** doesn't list it. Adding it anyway (`POST /api/repositories`) resumes it, with its settings.
- Resume is refused while its instance is being removed.
- A library skill or a model it names can't be deleted, as for a repository that is on.

The API: `PUT /api/admin/repositories/{instance}/{project}` with `{"enabled": false}` pauses and `{"enabled": true}` resumes (admins, and maintainers of the owner team: see [Roles](roles.md)). `DELETE /api/repositories/{instance}/{project}` removes.

A config repository has no switch and no **Remove repository**. To switch it off, remove it from `repositories` and redeploy: on the next reconcile (at startup and then every hour) pergent.sh deletes its webhook and revokes its deploy token, and its dismissals and the comment commands waiting for it are deleted with its record. If the config lists a repository that is paused in the dashboard, the config wins and the paused record is deleted.

A GitHub repository has no webhook or deploy token of its own to delete: pausing it keeps pergent.sh's record as it is, and removing it deletes only that record. Installing an App on a repository never switches it on; only the config list or the dashboard does.

If you remove a whole GitLab instance from the config, pergent.sh can no longer reach its projects: it forgets them with a warning in the log, leaves the webhooks in place (pergent.sh answers them with 404, as the instance is unknown) and the deploy tokens expire on their own within 30 days. An instance added in the dashboard is removed there instead, which takes its webhooks and deploy tokens off the platform first ([Instances and secrets in the dashboard](dashboard.md#instances-and-secrets-in-the-dashboard)).

Deploy tokens are created per project with the name `pergent-sh`, valid for 30 days, and replaced when fewer than 7 days remain, or at once when the stored token can't be decrypted because `PERGENT_SH_SECRET_KEY` changed.
