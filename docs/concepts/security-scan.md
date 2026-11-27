# 🛡️ Security scan

A review looks at one change. A **security scan** looks at a whole repository for security problems, such as an API with no auth check, an injection in old code, or a secret written to a log. A full audit costs a lot of tokens, so it never runs by itself.

## What a scan is, and is not

- It scans the **default branch** of one repository, at its newest commit.
- **A person starts it by hand.** A maintainer of the repository or an admin presses **Start scan**. A webhook, a push, a schedule or a comment never starts a scan.
- It never reviews a merge request, never posts a comment, never sets a status and never approves. A finding **never blocks a merge**.
- It does not scan other branches, and it does not upload to GitHub code scanning. pergent.sh sends no message when a scan ends.
- A paused repository, and one that is not switched on, cannot start a scan. One repository runs one scan at a time.

## Who can do what

| Action | Viewer | Maintainer | Admin |
| --- | --- | --- | --- |
| See the scans, the findings and the SARIF | yes | yes | yes |
| Start or cancel a scan | no | yes | yes |
| Dismiss or restore a finding | no | yes | yes |
| Change the scan settings | no | yes | yes |

An admin may start a scan past a used-up quota, as for a review. The audit log records `security_scan_started`, `security_finding_dismissed` and `security_finding_restored`.

## The image and the skill

The scan Job runs `jobs.security_image` (`ghcr.io/gopaytech/pergent-security`), which pergent releases with the review image. It holds one built-in skill, pinned to one commit, with its licence. The pergent developers choose the skill: **users cannot choose one**, and pergent.sh has no security prompt of its own. A new skill version is a new image release. Each scan stores the skill's name and commit.

## How the code reaches the scan pod

The scan pod **never talks to the git host and never holds a git token**. A NetworkPolicy covers the whole pod, so a clone step would need the git host in the egress rule, and injected code could then send data to the attacker's own repository on that host.

1. When a person presses **Start scan**, pergent.sh asks the platform for the default branch's newest commit, with the review bot or App it already has, and stores the commit on the scan.
2. The scan Job has no clone container. `pergent scan` downloads the code from pergent.sh: `GET /security-scans/{id}/source`, with the scan's own token. The route answers only while that scan runs.
3. pergent.sh passes the platform's archive of that commit on as a stream. It stores nothing.
4. pergent unpacks it into the work folder and refuses absolute paths, `..`, links that point outside, device files, and too many files or bytes. There is no `.git` folder.

**What is not in the archive:** Git LFS files (you get the pointer text) and submodules. A scan does not see them.

**Size.** `security.max_source_bytes` (default 500 MiB, range 1 MiB to 10 GiB) is the most pergent.sh passes on. Above it the scan fails with `the source is over the size limit`. pergent itself refuses an archive over 500 MiB, so a value above 500 MiB has no effect today.

## The network rule

A scan runs shell commands and sub-agents, so its pod can read its own environment, including the model key. Removing `curl` is not enough. The real guard is the network:

- A scan Job has **its own NetworkPolicy**, built from `securityJobEgress.cidrs`. There is **no default**. Set it to the LLM provider's addresses only. **Never add the GitLab or GitHub host**: the code comes from pergent.sh.
- Until an admin sets it, **Start scan** is off and the tab says why. A scan never falls back to `jobEgress.cidrs`, and the review Jobs' policy leaves scan pods out.
- The pod can reach DNS (the cluster DNS, or `securityJobEgress.dns`) and the pergent.sh pods on port 8080. That is why a scan needs `callback_url` to be the chart's in-cluster Service address.
- A scan Job gets **no proxy variables**, unless an admin sets `security.proxy` on purpose. A proxy is an allowed address that can reach any other, so it undoes the rule: its address goes in `securityJobEgress.cidrs` and its port in `securityJobEgress.ports`.

**Remaining risk.** DNS queries through a recursive resolver can still carry data out in the names they ask for. And a prompt injection in the scanned code can make the model post **its own result** for the scan, such as "no findings", using the result token it reads from its environment. That is accepted: the token is tied to that one scan, pergent.sh takes one result per scan, and the next scan finds the problems again.

pergent.sh withholds every text of a result that holds a secret it knows: the finding texts, the reason, the state and the SARIF.

## Findings

One row per repository and fingerprint, stored in a SARIF 2.1.0 shape. A finding has a rule (CWE), a severity (critical, high, medium or low), a title, a description, an exploit scenario, a fix hint, a file and lines, and the first, last and fixed scan. A finding the skill marks `needs_validation` is open, with a **Needs validation** label. At most 5000 findings of one scan are kept.

| State | How |
| --- | --- |
| **Open** | The first scan that finds it. A fixed finding that a scan finds again is open again. |
| **Dismissed** | A maintainer or an admin, with a reason: false positive, won't fix, used in tests, mitigated or acceptable risk, and an optional comment. Only an open finding can be dismissed. A later scan keeps it dismissed. Restore opens it again. |
| **Fixed** | A **complete** scan does not find it. A partial or failed scan never sets it. A dismissed finding is never fixed. |

**A skill update.** When the skill's name or commit differs from the last complete scan's, the first complete scan after it **fixes nothing** and the page says `Skill updated: some findings may show as new`, because a new version may build fingerprints in a new way. A complete scan that names no skill also fixes nothing.

The next scan gets the skill's state from the last **complete** scan, and the repository's dismissed findings with the person's reason, so the skill does not report them again.

## Retention

Scan rows and findings, with their history, are kept for good. The raw SARIF and the state are kept for the **last 10 scans** of a repository, and for its last complete scan. Older scans lose both, so **Download SARIF** is gone for them.

## Usage, quotas and limits

A scan's tokens count like a review's: in the [Usage](usage.md) pages (skill `security-scan`), in the [monthly token quota](token-quota.md), and in the model and team limits. A scan is **never counted as a review**: it is not in the review counts, the review lists or the review metrics.

**The token budget.** There is no setting for it. When a scan starts, pergent.sh works out the tokens left under each quota and limit that applies, and gives the smallest number to the Job. When the scan reaches it, pergent stops the model and sends what it has, as a partial scan with the reason `token budget reached`. A scan can pass the budget by the tokens of one 30-second check. An admin's start goes past the quotas, as for a review.

**The model.** A scan uses the first usable entry of its model chain when it starts. If that model fails during the scan, pergent.sh does not start it again on the next entry. The scan ends partial or failed, and a person starts it again.

## Cancel

A maintainer or an admin can **Cancel** a queued or running scan. A queued scan ends now. A running scan gets a stop signal and sends what it has, so it ends partial with the reason `cancelled`. A scan whose repository is switched off is cancelled too.

## Settings

A **Security scan** card on the repository's **Settings** page.

| Setting | Default | Range |
| --- | --- | --- |
| Scan model | The repository's model chain | Any chain |
| Max steps, main agent | 600 | 5 to 1000 |
| Max steps, each sub-agent | 100 | 5 to 300 |
| Scan timeout | 3 hours | 10 minutes to 6 hours |
| Use security findings in reviews | Off | On or off |

Instance level: `security.max_running` (default 1, up to 20) keeps scans from taking the Job slots reviews need. See the [config reference](../reference/configuration.md).

## Findings in reviews

With **Use security findings in reviews** on, a review of the repository gets the **open** findings in the files the change touches, as data (`security-findings.md`), up to about 32 KiB. This is off by default.

- The review reads them as data. They are not instructions.
- It is dropped first when a review's inputs are too large, and the review goes on without it.
- It never blocks a merge, and a review never changes a finding's state.
- A review Job from an older image ignores it.

## Why a scan ends partial or failed

The scan page shows the reason.

| Status | Reason |
| --- | --- |
| partial | `cancelled`: a person cancelled it. |
| partial | `token budget reached`. |
| partial | A step limit or the scan timeout stopped the model. |
| partial | `more than 5000 findings: the first 5000 are kept`. |
| failed | `no model of the chain can be used`. |
| failed | `security scans need a scan image (jobs.security_image)`. |
| failed | `couldn't start the scan Job; see the pergent.sh log`. |
| failed | `the scan stopped before its Job started; start it again` (pergent.sh restarted). |
| failed | `the scan took longer than its timeout`. |
| failed | `the source is over the size limit`. |
| failed | `the scan Job sent a result over 12 MiB`. |
| failed | `pergent sent a result pergent.sh couldn't use: <why>`. |

A partial scan keeps its findings and its tokens, and fixes nothing. A scan that failed before its Job ran, or that ended with no result (a timeout, a launch failure, a source over the size limit, a body over 12 MiB), has no tokens. So has a scan whose Job sent a body that is not a valid envelope. When the envelope can be read but its SARIF or a finding cannot be used, the scan fails, keeps the tokens its Job reported, and keeps no findings.

## Routes and metrics

The Job's routes are `GET /security-scans/{id}/source` and `POST /security-scans/{id}/result`: see [HTTP routes](../reference/http-routes.md). The metrics are `pergent_security_scans_total` and `pergent_security_findings_open`: see [Metrics](metrics.md). The isolation of the Job is in [What a Job can and cannot see](job-isolation.md).
