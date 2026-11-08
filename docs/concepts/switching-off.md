# 📴 Switching a repository off

A repository is on when it is listed in the config or switched on in the [dashboard](dashboard.md). To switch a config repository off, remove it from `repositories` and redeploy; a dashboard repository is switched off with its switch. On the next reconcile (at startup and then every hour) pergent.sh deletes its webhook and revokes its deploy token. Runs still queued for it are dropped when they come up for dispatch; a review that was already running is still posted, but never approved, as with approval turned off. Its dismissals and the comment commands waiting for it are deleted with its record, and commands written there afterwards are ignored.

A GitHub repository has nothing to delete: removing it from the config deletes only pergent.sh's own record of it. Installing an App on a repository never switches it on; only the config list or the dashboard does.

If you remove a whole GitLab instance from the config, pergent.sh can no longer reach its projects: it forgets them with a warning in the log, leaves the webhooks in place (pergent.sh answers them with 404, as the instance is unknown) and the deploy tokens expire on their own within 30 days. An instance added in the dashboard is removed there instead, which takes its webhooks and deploy tokens off the platform first ([Instances and secrets in the dashboard](dashboard.md#instances-and-secrets-in-the-dashboard)).

Deploy tokens are created per project with the name `pergent-sh`, valid for 30 days, and replaced when fewer than 7 days remain, or at once when the stored token can't be decrypted because `PERGENT_SH_SECRET_KEY` changed.
