# 📏 Limits

This release is deliberately small:

- Commands are `review`, `review failed`, `dismiss` and `help`, and the dashboard has the matching buttons plus Restore. No chat, no questions, no asking the bot to explain or fix code, no commands that change settings, no questions in a finding's thread (only `dismiss`), and no dismissing a finding for every merge request of a repository: a dismissal belongs to one merge request. Restoring is a button only.
- Dashboard sign-in is a password (local admins), OIDC against one provider (Google Workspace is the one built and tested), or a JWT checked behind a proxy. Signing in with GitLab or GitHub is gone. No SAML or LDAP, no groups or roles read from the provider, no two-factor authentication of our own, and no API tokens for scripts.
- Teams are kept in pergent.sh: they are not synced from GitLab groups or GitHub teams.
- A GitLab and a GitHub account of one person are two accounts, with no linking.
- `PERGENT_SH_SECRET_KEY` can't be rotated: see [Environment variables](configuration.md).
- No advice on whether a human approval is still needed beyond the hourly warning for a repository that one approval can merge.
- No repository cache: every checkout is a fresh `git init` plus a shallow fetch.
- Skills: one library for the whole installation, with a default list and per-repository lists; no per-team or per-instance lists. Installed skills change only when an admin presses **Update**, never by themselves or from a webhook on the source. Only the `SKILL.md` text of a folder skill is used. Repository skills can't be edited in the dashboard. A skill's text has no preview and the history has no diff or restore for written skills. A URL source must be public and inside `jobEgress.cidrs`. No chat or API token to manage the library from scripts.
- Readiness scan: advice only, no drafting of `AGENTS.md` or CODEOWNERS and no merge requests. It reads the default branch, on switch-on, on request and once after the upgrade, never on a schedule. The limits of a scan Job (5 minutes, 2 GiB, 512Mi, 2 at once) are fixed. On GitHub, for a branch with classic protection, the approvals row (when no ruleset requires reviews), the protected row (when no ruleset stops pushes and no required check is shown) and the required checks row (when no required check is shown) say Couldn't check unless the review App has the optional Administration: Read permission.
- One replica, and PostgreSQL is external.
- GitHub.com only. `github[].api_url` accepts another URL, but GitHub Enterprise Server is untested.
- On GitHub, approving after the approve App is installed starts with the next push or ready-for-review; there is no catch-up pass over open pull requests.
