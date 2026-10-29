# pergent.sh -- Vision and Roadmap

**Date:** 2026-10-29
**Status:** Agreed in brainstorming; each piece gets its own design before it is built.

---

## What it is

pergent.sh is a **self-hosted web application** (backend + frontend) that runs [pergent](https://github.com/gopaytech/pergent) code reviews on merge requests automatically. pergent is an open-source CLI that reviews a change with an LLM agent and reports per-skill verdicts (`no_blocking_issues`, `has_blocking_issues`, `failed`), findings, summaries and checklist results.

Today, pergent runs as a CI job that each repository adds to its pipeline. pergent.sh replaces that with one installation that:

- connects to GitHub and GitLab;
- reviews every MR in the repositories it has been switched on for;
- posts the review;
- approves MRs that have no blocking issues;
- helps each repository become easier to review well.

## Who uses it

**Platform engineers, developers, admins and DevOps engineers who onboard their repositories.** They open pergent.sh to set up reviews for a repository, tune how it is reviewed, and act on what pergent.sh suggests. Reading reviews still happens in the MR itself.

## Goals

- Onboarding a repository takes minutes: connect a platform, set up the bots, switch the repository on.
- Every MR in an enabled repository gets a pergent review without any CI changes in the repository.
- MRs with no blocking issues get approved by a bot; everything else goes to a human; merging is always human.
- pergent.sh actively improves review quality per repository:
  - it scans what helps reviews and drafts what is missing;
  - it improves skills from what it learns.
- Untrusted MR code never runs anywhere that holds a token that can comment, approve or write.

## Non-Goals (for the first version)

- **Analytics** (acceptance rates, findings over time, per-author stats). Deferred.
- **Applying skill improvements automatically.** In version 1, every improvement waits for human approval; automatic application is a later phase.
- **Merging, or enabling auto-merge.** Never.
- **Changing a repository's approval rules or branch protection.** pergent.sh advises; humans change settings.
- **Running anywhere but Kubernetes.** No Docker / Compose mode.

## Anti-goals

- **An MR loosening its own review rules.** It must not be able to edit team instructions, skills or approval advice and then get itself approved.
- **A bot approval that silently becomes the only approval** needed to merge, without the admin being told.
- **Duplicate or nagging MRs** from pergent.sh, for example re-proposing an `AGENTS.md` the team already closed.
- **A failed or partial review counted as "no blocking issues".**

---

## Decisions

### 1. Onboarding

1. **Log in.**
2. **Connect a platform:**
   - a GitHub organization, through a GitHub App installation;
   - a self-managed GitLab instance, through its instance URL and a bot token, with pergent.sh creating the webhooks itself.

   *Builder's call, to confirm in the core design:* whether GitHub Enterprise Server and GitLab.com are in version 1.
3. **Set up the bots:** a **review bot** that posts comments, and an **approve bot** that approves. The bots also need permission to create branches and MRs, for drafted files (section 4). Onboarding says which permissions each bot needs and why.
4. **Switch repositories on or off** for review.

### 2. Configuration, in three layers

| Layer | What it holds | Where it lives | Who changes it |
|---|---|---|---|
| **Scanned facts** | CODEOWNERS, approval rules, branch protection, required checks, and the readiness scan (section 4) | pergent.sh database, re-scanned automatically and on demand | pergent.sh |
| **Team instructions** | Repo-specific review guidance, e.g. "payments/ is high risk" | An optional file in the repository, **read from the MR's target branch** | The team, through MRs |
| **Overrides** | Admin settings per repository | pergent.sh database | Admins in the UI, shown with "overridden here" badges |

Reading repository files from the target branch means an MR cannot change the rules it is reviewed by.

### 3. Approval

- **When it approves:** the approve bot approves only when **every skill** finishes `no_blocking_issues`. Failed or partial reviews and blocking findings go to a human.
- **How it approves:** only through the platform's own approval feature (a GitLab MR approval, a GitHub "Approve" review). Where a repository has no approval step, the bot does nothing beyond the review.
- **Merging is always human.**
- **Per-repository approval advice:** from the scanned facts, pergent.sh tells admins how the bot's approval interacts with each repository's rules. For example:
  - "1 approval is required and the bot is an eligible approver: its approval alone unlocks merge";
  - "CODEOWNERS requires the security team for `auth/`: the bot cannot satisfy that".

  pergent.sh never changes those settings itself.

### 4. Repository readiness scan

When a repository is switched on, and again later, pergent.sh checks what helps pergent review it well:

- **project context:** `AGENTS.md`, `CLAUDE.md`, README, contributing docs;
- **ownership and process:** CODEOWNERS, approval rules, branch protection, required checks;
- **conventions:** linter and formatter configs, `.editorconfig`, conventions documents;
- **skill fit:** languages and folders, to suggest which skills and `paths` suit the repository;
- **size and noise:** generated, vendored or very large files worth excluding.

Each result shows on the repository page. When something is missing (e.g. no `AGENTS.md`), pergent.sh **drafts the file and opens an MR**:

- the draft is produced by a read-only Kubernetes Job (section 5);
- pergent.sh itself creates the branch, commit and MR;
- it remembers MRs it opened, never opens a second one while one is open, and doesn't re-propose a closed one unless asked from the repository page.

### 5. Running reviews: Kubernetes Jobs

- Every review, and every draft, runs as a **Kubernetes Job**. pergent.sh is installed on Kubernetes (Helm chart; builder's call).
- The job runs pergent in a new **agent mode** (piece 1): it reviews and writes its result as JSON, and **never talks to GitHub or GitLab**.
- The job receives only:
  - the LLM key;
  - a short-lived read-only token to clone;
  - the skills and instructions pergent.sh resolved for the repository.
- The job sends its result back to pergent.sh with a **one-time token** valid only for that run.
- pergent.sh then posts the comment, sets the commit status and approves. The tokens that can comment, approve or write never enter the job.
- Jobs get CPU/memory limits, are cleaned up after they finish, and are limited by a NetworkPolicy to the LLM endpoint and the git host. pergent.sh caps how many run at once.

### 6. MR commands and dashboard buttons

Commands are read from MR comments through the platform's comment webhook:

| Command | Effect |
|---|---|
| `/pergent review` | Re-run the full review on the latest commit |
| `/pergent review <skill>` | Re-run one skill |
| `/pergent retry` | Re-run only the skills or batches that failed; keep the rest |
| `/pergent dismiss <finding-id>` | Mark a finding as not a real problem; recompute the verdict if it was blocking; counts as a "too noisy" signal (section 7) |
| `/pergent pause` / `/pergent resume` | Stop or restart automatic reviews on new pushes to this MR |
| `/pergent help` | Reply with the command list |

- **Who may run commands:** users with at least developer access to the repository. Others get a refusal.
- **Feedback:** pergent.sh reacts to the command comment (👀 when it starts, ✅ or ❌ when it finishes) and updates the one review comment instead of posting new ones.
- **Finding IDs:** each finding shows a short ID in the comment, from pergent's existing fingerprints.
- **Dashboard buttons:** every command also exists as a button on the MR's page in the dashboard.

### 7. Skills

- **Library skills** live in pergent.sh: listed, added, edited and deleted in the UI, stored in the database, and assigned to an organization, a group or specific repositories.
- **Repository skills** live in the repository (e.g. a `.pergent/skills/` folder) and are read from the target branch. The repository page lists them read-only, with a link to the file.
- **Same name in both:** the repository's skill wins for that repository, and the page shows "overridden by repo".
- **Improvements** come from two signals seen across reviews:
  - **too noisy:** findings developers keep dismissing or not addressing;
  - **a gap:** problems human reviewers raise in MR comments that pergent missed.
- **Where improvements go:**
  - a library skill gets a proposed change in pergent.sh (before/after diff, accept or reject);
  - a repository skill gets an MR to the repository.
- **Every improvement waits for human approval in version 1.**

### 8. Organization

Members, roles, SSO and an audit log. A simple admin login comes first (piece 2); full organization management is piece 5.

### 9. Menu (target)

**Repositories · Configuration · Reviews · Skills · Integrations · Organization.** This is derived from the common pattern across hosted review products; Analytics is deferred.

Each MR's page in Reviews shows the runs (per skill and batch), their status and cause, and the command buttons. The research found this per-run history to be rare among existing products.

---

## Pieces and order

Each piece gets its own design (and plan) before it is built, and each works on its own once it lands.

1. **pergent agent mode** *(in the pergent repository)*: `pergent agent --result <path>`, which reviews and writes the JSON result, never talks to a platform, and takes skills and instructions from paths it is given. Everything else depends on it.
2. **pergent.sh core:**
   - login, platform connections, the two bots, switching repositories on and off;
   - the three configuration layers;
   - webhooks, Kubernetes Jobs, the result callback;
   - posting the comment and commit status;
   - the approve bot with per-repository advice;
   - MR commands and dashboard buttons;
   - a simple admin login.

   After this piece, MRs are reviewed and approved through pergent.sh.

   Piece 2 must also implement the agent-mode contract (pergent `docs/superpowers/specs/2026-10-29-pergent-agent-mode-design.md`, section 8):
   - **Tokens:** a Secret per Job, with the git token only in the init container, passed through env-based git config and never in a URL or `.git/config`. Hooks and pushes are disabled in the checkout.
   - **Cache mounts:** only the repo's own cache folder, read-only, in both containers.
   - **Fetching:** verify fetching the merge base by SHA (including forks), with a shallow target-branch fallback.
   - **Deadline:** `activeDeadlineSeconds` = `--timeout` + 2 min + a measured clone allowance, with `terminationGracePeriodSeconds: 30`.
   - **Head re-check:** re-check the MR head before approving.
   - **Failure status:** a per-repository commit-status setting for failed reviews, `failed` by default or `neutral`.
3. **Repository readiness scan:** the checks, the drafted MRs, and the scan feeding the approval advice.
4. **Skills:** the library, repository skills, assignment, the two improvement signals, and proposals with human approval.
5. **Organization:** members, roles, SSO, the audit log.

Later, not version 1:
- analytics;
- automatic skill improvements;
- an LLM proxy that holds the provider key, with per-Job credentials and cost caps;
- skipping re-reviews of pushes that don't change the patch;
- a hardened pod profile: restricted security profile, fail-closed egress policy, optional gVisor/Kata.

## Stack (builder's defaults, not yet decided with the user)

- **Backend:** Go, matching pergent.
- **Database:** PostgreSQL.
- **Frontend:** a TypeScript single-page app.
- **Install:** Helm chart; Kubernetes is the only supported runtime.

## Constraints

- **Public repository:** no employer, internal hostnames, internal repositories, people, or screenshots of internal systems in code, docs, tests or commit messages.
- **pergent's guarantees carry over:**
  - reviews are read-only;
  - a failed review never counts as a pass;
  - secrets are redacted from everything posted.

## Background research

A survey of hosted AI review products (onboarding, menus, configuration layering, analytics, review history, learnings, merge gating, self-hosted architectures) informed these decisions. It is kept in the pergent repository under `docs/research/2026-10-29-hosted-review-product-surfaces.md`.
