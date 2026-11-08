# 🐙 GitHub

pergent.sh reaches GitHub.com through a GitHub App that you create (pergent.sh is self-hosted, so there is no shared App). **One App is enough and is the simplest setup:**

- the **review App** receives the `pull_request`, `issue_comment` and `pull_request_review_comment` webhooks, reads the pull request and the repository, posts the comment and the finding threads, sets the `pergent` Check Run, reacts to and answers [command](../concepts/commands.md) comments, looks up a commenter's repository permission and mints each Job's clone token. It can also approve, and dismiss its own approvals when they go stale (`approve_with_review_app`, the default in the dashboard). For that it needs **Pull requests: Read and write**, and the pull request shows the same bot commenting and approving.

Who approves is a choice of three:

- **the review App** (one App, as above);
- **a separate approve App**, optional, for teams that want the identity that writes the review never to be the one that approves it. It only approves, and dismisses its own approvals when they go stale. It also keeps the review App's permissions smaller: people sign in to the dashboard with the review App, and with `Pull requests: Read and write` on it their stored sign-in tokens can write pull requests too (as that person, limited by what they may do), which pergent.sh never uses;
- **a bot user's token**, for when GitHub doesn't count an App's approval toward required reviews; see [Approving as a bot user](#approving-as-a-bot-user).

## 1. Create the review App

In the organization's settings, **Developer settings → GitHub Apps → New GitHub App**:

- **Webhook:** active. URL `<public_url>/webhooks/github/<instance name>`, for example `https://pergent.example.com/webhooks/github/gh`. Once the instance is added, its card under **Integrations** shows this URL with a copy button. Secret: a random string (`openssl rand -hex 32`); you enter it as the instance's webhook secret in the dashboard, or, for an instance in the config file, put it in the Secret as the instance's `webhook_secret_env`.
- **Repository permissions:**
  - Pull requests: **Read and write** when this App approves too (the one-App setup) or posts [inline findings](../concepts/inline-findings.md), else **Read-only**. With Read-only, inline findings are off for that instance: findings stay in the comment, and the Integrations page says so.
  - Contents: **Read-only**
  - Checks: **Read and write**
  - Issues: **Read and write** (pull request comments are issue comments; this also covers the 👀, 👍 and 👎 reactions on a command comment)
  - Metadata: **Read-only** (always on; this also covers reading a commenter's repository permission)
  - Administration: **Read-only** (optional). It lets pergent.sh read the classic branch protection's conversation-resolution rule for [inline findings](../concepts/inline-findings.md); without it only rulesets are read, and the classic rule is treated as off. It also lets the [readiness scan](../concepts/dashboard.md#readiness-scan) read classic branch protection. Without it, on a branch that has classic protection, 3 rows of the scan can say "Couldn't check" and name this permission; nothing else changes.
- **Subscribe to events:** **Pull request**, **Issue comment** and **Pull request review comment**. The Issue comment event delivers the comments that commands are read from, and the Pull request review comment event the replies in a finding's thread.
- **Where can this GitHub App be installed:** "Only on this account", or "Any account" to install it on several organizations.

Create it, note its **App ID** and generate a **private key** (a `.pem` file).

**An existing review App** that should also approve needs **Pull requests: Read and write**: change it under **Permissions & events**, save, and accept the new permission on each installation (GitHub asks the organization's owner to approve it). Until that is accepted, pergent.sh finds (at startup and in the hourly pass) that the App can't approve on that repository: a clean review is published as not approved, with a note that the review App lacks Pull requests: write, and the dashboard shows "Can't approve".

Changing who approves (another App, the review App, a bot user) doesn't withdraw approvals the old approver left. Turn approval off for the repositories first and wait until their approvals are withdrawn, or dismiss them by hand.

**An existing review App** needs one change to run commands: under **Permissions & events → Subscribe to events**, tick **Issue comment** and save. It asks for no new permission, as it already has the ones above. Without the event the App still reviews, but no command is ever received.

**Administration: Read** is optional too. To turn it on, open **Permissions & events → Repository permissions**, set **Administration** to **Read-only** and save; the owner of each installation then has to accept the new permission. The [readiness scan](../concepts/dashboard.md#readiness-scan) uses it for the rows "Approvals needed to merge", "Default branch protected" and "Required checks before merge". Reviews never need it.

## 2. Create the approve App (optional)

Skip this when the review App approves. For a separate approve App, create it the same way, with:

- **Webhook:** not active (uncheck "Active").
- **Repository permissions:** Pull requests: **Read and write**; Metadata: **Read-only**.
- No events.

Note its App ID and generate a private key.

## 3. Install the App or Apps

Install the App (both Apps, with a separate approve App) on the organization, on all repositories or on the ones you will review. Installing an App switches nothing on: only the repositories listed in the config or switched on in the [dashboard](../concepts/dashboard.md) are reviewed. One `github:` instance covers every organization the Apps are installed on; pergent.sh looks up the installation per repository.

## 4. Add the instance

In the dashboard, an admin adds the instance under **Integrations → Add GitHub instance**: the API URL, the review App's ID and private key (pasted whole), **Approve with** (the review App, the default; a separate approve App, which asks for its ID and private key too; or a bot user's token), the webhook secret ([Instances and secrets in the dashboard](../concepts/dashboard.md#instances-and-secrets-in-the-dashboard)).

To keep the instance in the config file instead, put the keys in the Secret and name them in the config:

```bash
kubectl -n pergent-sh create secret generic pergent-sh-env \
  --from-literal=DATABASE_URL='postgres://pergent:CHANGE_ME@postgres.example.com:5432/pergent?sslmode=require' \
  --from-literal=PERGENT_SH_SECRET_KEY="$(openssl rand -base64 32)" \
  --from-literal=OPENCODE_API_KEY='CHANGE_ME' \
  --from-file=GITHUB_REVIEW_APP_KEY=review-app.private-key.pem \
  --from-literal=GITHUB_WEBHOOK_SECRET='the secret you gave the review App'
```

```yaml
config:
  public_url: https://pergent.example.com
  github:
    - name: gh
      review_app:
        app_id: 123456
        private_key_env: GITHUB_REVIEW_APP_KEY
      approve_with_review_app: true
      webhook_secret_env: GITHUB_WEBHOOK_SECRET
  repositories:
    - github: gh
      project: acme/widgets
```

With a separate approve App, add its key to the Secret (`--from-file=GITHUB_APPROVE_APP_KEY=approve-app.private-key.pem`) and replace `approve_with_review_app: true` with:

```yaml
      approve_app:
        app_id: 123457
        private_key_env: GITHUB_APPROVE_APP_KEY
```

Exactly one of `approve_with_review_app`, `approve_app` and `approve_token_env` is set. `approve_app` can't name the review App's ID or key; say `approve_with_review_app: true` for that.

At startup pergent.sh reads each App's slug (`GET /app`) and its bot user (`GET /users/<slug>[bot]`), and, for an instance in the config file, stops with an error naming the instance if a key doesn't work (an instance added in the dashboard is logged and recorded as "Needs attention" instead). The bot user lookup is unauthenticated (GitHub allows 60 an hour per address), which is plenty unless the pod restarts in a loop. Then, at startup and every hour, it looks each listed repository up: the review App's installation, the repository's ID and clone URL, and whether the approve side can reach it. A listed repository the review App isn't installed on is logged as an error on every pass and can't be reviewed.

## The `pergent` Check Run

The status of a review is a Check Run named `pergent` on the reviewed head commit, created by the review App:

- `in_progress` ("pergent review running") when the Job starts;
- then `completed` with the conclusion `success`, `failure`, `neutral` (a failed review with `failure_status: neutral`) or `cancelled` (the run was superseded: a newer push or a new target branch replaced it, or the pull request was closed). A review that couldn't be posted after 10 attempts ends as `failure`, titled "pergent couldn't post its review".

Its title is the status description from [How approval works](../concepts/approval.md) and its summary points to the pull request comment; the findings are in the comment and, when [inline findings](../concepts/inline-findings.md) are on, in threads on the diff. Every change is a new Check Run with the same name, and GitHub shows the latest. To make merging wait for the review, add `pergent` as a required status check in the branch's protection rule or ruleset, and **pin it to the review App**: otherwise GitHub accepts a check named `pergent` from any source, and a workflow or another App could satisfy it. In a branch protection rule, set the check's source to the review App instead of "Any source"; in a ruleset, choose the review App as the check's integration. See [Branch protection](#branch-protection).

## Branch protection

- **Turn on "Dismiss stale pull request approvals when new commits are pushed".** pergent.sh dismisses its approval on every push, but this makes GitHub do it too, as a second layer for a webhook pergent.sh missed.
- **Make a human approval necessary.** If a branch needs one approval and the approver's approval counts, the bot alone unlocks the merge. Require two approvals, or a code owner review where the approver is not a code owner. At startup and every hour, for each repository the approve side can reach, pergent.sh reads the required approving review count of the default branch from its rulesets (`GET /repos/{owner}/{repo}/rules/branches/{branch}`, which the review App's metadata permission allows) and from its classic branch protection (`.../branches/{branch}/protection/required_pull_request_reviews`, which needs the Administration read permission the review App doesn't ask for), takes the higher, and logs a warning naming the repository when it is one or none: "one approval is enough to merge here, so pergent's approval alone can unlock it; add a rule requiring a human approval". When no ruleset requires reviews and the classic rule can't be read (a 403 or 404 without the Administration permission, or any other error), it logs once at info level that it couldn't check; the check never stops a review. It doesn't check code owner reviews.
- **Require the `pergent` check from the review App only.** Add `pergent` as a required status check and set its source to the review App: in a branch protection rule, pick the review App as the check's expected source instead of "Any source"; in a ruleset, choose the review App as the integration of the `pergent` check. With any source allowed, any Actions job or App that posts a check named `pergent` can satisfy the requirement.
- If you use **"Restrict who can dismiss pull request reviews"**, allow the App that approves (or the bot user). Otherwise GitHub refuses its dismissals. pergent.sh keeps the approval on record, logs the failure and tries again after a backoff: 30 seconds after the first failure, doubling after each one, at most 5 minutes. The 5-minute approval sweep tries it too. The stale approval stays until a dismissal works or GitHub dismisses it itself.
- **Check that the approval counts.** GitHub may not count a GitHub App's approving review toward a rule's required approvals; this release has not verified it. Use a test branch whose protection requires exactly **one** approval (with more, the result is ambiguous). Open a test pull request into it, let pergent.sh approve it, and look at the merge box: if the App's approval is listed but the required approvals are still missing, switch to a bot user.

## Approving as a bot user

Use this instead of an App (the review App or a separate approve App) when your branch protection doesn't count the App's approval.

Changing who approves (another App, the review App, a bot user) doesn't withdraw approvals the old approver left. Turn approval off for the repositories first and wait until their approvals are withdrawn, or dismiss them by hand.

1. Create a GitHub user for the bot and give it **Write** access to the reviewed repositories (GitHub counts approvals only from people with write access).
2. As that user, create a **fine-grained personal access token**: resource owner the organization, repository access the reviewed repositories, repository permission **Pull requests: Read and write** (Metadata read-only is included). If the organization requires approval of fine-grained tokens, approve it.
3. Put it in the Secret (for example `GITHUB_APPROVE_TOKEN`) and use `approve_token_env` instead of `approve_with_review_app` or `approve_app`:

   ```yaml
   github:
     - name: gh
       review_app:
         app_id: 123456
         private_key_env: GITHUB_REVIEW_APP_KEY
       approve_token_env: GITHUB_APPROVE_TOKEN
       webhook_secret_env: GITHUB_WEBHOOK_SECRET
   ```

At startup pergent.sh checks the token by asking whose it is (`GET /user`). A repository the token can't see (404) gets the "approve bot can't reach this repository" note; any other refusal is logged as an error by the hourly pass, which keeps its earlier answer.

The token is rotated by hand: create a new token, update the Secret, restart pergent.sh, then delete the old token. When GitHub reports (in the `GitHub-Authentication-Token-Expiration` header) that the token expires within 14 days, pergent.sh logs the warning "the GitHub approve token expires soon; rotate it", at most once an hour.

## When the approve App isn't installed, or can't approve, on a repository

With a separate approve App, reviews still run and are posted with their comment and Check Run. A clean review is not approved: the comment says `**Not approved:** the pergent approve App isn't installed on this repository or lacks Pull requests: write.` (for the review App: `the pergent review App lacks Pull requests: write on this repository`) and the Check Run title says why. This is not retried. pergent.sh notices an installation at startup and in the hourly pass; from then on the next push or ready-for-review is approved. There is no catch-up pass over pull requests that were already reviewed. When the review App approves there is no separate installation to miss; what can be missing is its **Pull requests: Read and write** permission, which gets the same treatment. The hourly pass reads the installation's permissions, so the note goes away after the permission is accepted.

## Rotating an App's private key

A GitHub App can have several private keys at once:

1. In the App's settings, generate a new private key.
2. For an instance added in the dashboard, paste the new `.pem` file into its private key field under **Integrations**: it is checked before it is saved and used at once, with no restart. For an instance in the config file, update the Secret with the new `.pem` file and restart pergent.sh (`kubectl -n pergent-sh rollout restart deployment/pergent-sh`), then check that it started.
3. Delete the old key in the App's settings.

Installation tokens minted with the old key expire within an hour.
