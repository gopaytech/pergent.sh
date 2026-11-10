# 🔐 Trusted authors

On a public repository anyone can open a change, usually from a fork, and so start a review on your LLM account, or try to talk the review into a clean result. With **Trusted authors only** on, pergent.sh reviews a change by itself only when its author is trusted: they have write access to the repository (GitLab Developer or above, inherited access included; GitHub `write`, `maintain` or `admin`), or they are on the repository's allowlist. Anyone else's change waits (trust is checked before the [monthly token quota](token-quota.md), so an untrusted author's change waits for trust first):

- no run, no Job and no LLM call;
- one note on the change, posted once: ``Not reviewed automatically: the author isn't a trusted author of this repository. Someone with write access can start a review with `@<bot> review`.``
- a pending status on each head (GitLab commit status `pending`, GitHub a `queued` check run), `Waiting for someone with write access to start the review`, so a required pergent check keeps the change unmergeable;
- the change is listed under **Waiting for a start** on the repository's page in the dashboard.

Someone with write access starts the review with `@<bot> review`, even while **Only the author and maintainers use commands** is on, or an admin or a maintainer of the repository's team presses **Start review**. A start reviews only the head that was there when it was asked for: a push that lands between the comment (or the page the button was pressed on) and its handling isn't started, and the answer says to start it again (`this change was pushed after your comment; start the review again`, or a 409 asking to reload the page). Each new head waits again and needs another start. A started review is a normal review, and a clean one is approved as usual. A start counts against the change's re-run cap (10 a day by default), not the repository's. The author's own `@<bot> review` is refused, whatever else they may do on their change.

**Who is trusted is checked when the run comes up for dispatch, not cached:** someone taken off the allowlist or out of the repository is untrusted from their next push. When the author's access can't be read, pergent.sh tries again for about 10 minutes; then the run waits with the status `Couldn't check the author's access. Someone with write access can start the review with @<bot> review`, and no note.

**The setting** has three values on each instance: *follow visibility* (the default: on for a public repository, off for an internal or private one; a visibility that can't be read counts as public), *always on* and *always off*. It is set under the instance in the config (`trusted_authors_only: follow_visibility | on | off`) or under **Admin → Integrations** for an instance added in the dashboard. A repository can set `on` or `off` itself (**Repository → Settings**, admins only, or `repositories[].trusted_authors_only`), which wins over the instance's.

**The allowlist** belongs to one repository, and only admins change it (a maintainer's **Reset to defaults** leaves it, and the setting, as they are). In the dashboard, type a username: it is looked up when you save and kept by the platform's user ID, so a renamed user stays trusted and someone who takes a freed name doesn't. In the config, give the ID: `trusted_authors: [{id: 123, username: alice}]` (find it with `GET /api/v4/users?username=alice` on GitLab or `GET /users/alice` on GitHub). The allowlist only decides automatic reviews; it gives no command rights.

With the instance on *follow visibility*:

| Author of the change | GitHub public | GitHub private | GitLab public | GitLab internal or private |
| --- | --- | --- | --- | --- |
| No access, or a fork's author | waits | reviewed | waits | reviewed |
| GitHub `read` or `triage`; GitLab Guest or Reporter | waits | reviewed | waits | reviewed |
| On the repository's allowlist | reviewed | reviewed | reviewed | reviewed |
| GitHub `write`, `maintain` or `admin`; GitLab Developer or above | reviewed, and can start others | reviewed | reviewed, and can start others | reviewed |

With *always off* every author is reviewed, as before 0.12.0; with *always on* every "reviewed" for someone without write access and not on the allowlist becomes "waits".
