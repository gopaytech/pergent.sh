# Documentation

**Start here**

| | |
| --- | --- |
| [How it works](concepts/how-it-works.md) | The flow of a review, and every feature |
| [Install](setup/install.md) | Requirements, the Helm chart, exposing it |
| [Development](development.md) | Build, test, publish the chart |

**Setup**

| | |
| --- | --- |
| [GitHub](setup/github.md) | Create the review App, approvals, branch protection |
| [GitLab project settings](setup/gitlab.md) | Settings that make the approval a real gate |
| [Config reference](reference/configuration.md) | Every config key and environment variable |
| [Troubleshooting a failed review](setup/troubleshooting.md) | Find a review's Job and pod, and what each failed step means |
| [Upgrading](upgrading.md) | What to do for each release, and how to roll back |

**Concepts**

| | |
| --- | --- |
| [Dashboard](concepts/dashboard.md) | Sign-in, teams, repositories, readiness, audit log |
| [Skills and instructions](concepts/skills.md) | How a review picks its skills, and the skill library |
| [Commands](concepts/commands.md) | `@<bot> review`, `review failed`, `dismiss` |
| [How approval works](concepts/approval.md) | When pergent.sh approves, and when it takes it back |
| [Models](concepts/models.md) | The list of models, the default, and a model per repository |
| [Usage](concepts/usage.md) | Where the model tokens go |
| [Metrics](concepts/metrics.md) | Prometheus metrics |
| [Skill results](concepts/skill-results.md) | What became of each skill's findings |
| [Inline findings](concepts/inline-findings.md) | Findings as threads on the diff |
| [Incremental reviews](concepts/incremental-reviews.md) | Review only what changed |
| [Quiet reviews](concepts/quiet-reviews.md) | Fewer nitpicks, shorter comments |
| [Comment layout](concepts/comment-layout.md) | Choose what the review comment shows |
| [Learn from dismissals](concepts/learn-from-dismissals.md) | Teach reviews what isn't a problem |
| [Linked repositories](concepts/linked-repositories.md) | Check a change against the repositories that depend on it |
| [Trusted authors](concepts/trusted-authors.md) | Who gets reviewed without being started |
| [What a Job can and cannot see](concepts/job-isolation.md) | How a review is sandboxed |
| [Fork merge requests](concepts/fork-merge-requests.md) | Changes from forks |
| [Switching a repository off](concepts/switching-off.md) | What's removed and what's kept |

**Reference**

| | |
| --- | --- |
| [HTTP routes](reference/http-routes.md) | Every route and who uses it |
| [Limits](reference/limits.md) | What pergent.sh doesn't do |
| [OpenAPI](../openapi.yaml) | The API spec |
