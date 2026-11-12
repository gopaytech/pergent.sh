<div align="center">
  <img src="docs/assets/logo.png" alt="pergent" width="110">

  <h1>pergent.sh</h1>

  <p><strong>Run pergent code reviews on GitLab merge requests and GitHub pull requests, one Kubernetes Job per review.</strong></p>

  <p>
    <a href="LICENSE"><img alt="License" src="https://img.shields.io/badge/license-MIT-blue.svg"></a>
    <a href="go.mod"><img alt="Go" src="https://img.shields.io/github/go-mod/go-version/gopaytech/pergent.sh"></a>
    <a href="https://github.com/gopaytech/pergent.sh/pkgs/container/pergent.sh"><img alt="Image" src="https://img.shields.io/badge/ghcr.io-pergent.sh-informational"></a>
    <a href="charts/pergent-sh/README.md"><img alt="Helm chart" src="https://img.shields.io/badge/helm-pergent--sh-informational"></a>
  </p>

  <p>
    <a href="docs/setup/install.md">Install</a> ·
    <a href="docs/setup/github.md">GitHub setup</a> ·
    <a href="docs/concepts/dashboard.md">Dashboard</a> ·
    <a href="docs/concepts/skills.md">Skills</a> ·
    <a href="docs/concepts/commands.md">Commands</a> ·
    <a href="docs/reference/configuration.md">Config reference</a> ·
    <a href="docs/README.md">All docs</a>
  </p>
</div>


---

pergent.sh runs [pergent](https://github.com/gopaytech/pergent) code reviews on GitLab merge requests and GitHub pull requests, inside your Kubernetes cluster, with one Job per review.

## Features

**Reviews**

- 🧩 **[Skills](docs/concepts/skills.md):** markdown skills that tell the model what to look for, kept in a shared library.
- 💬 **[Inline findings](docs/concepts/inline-findings.md):** one comment, updated on every push, plus each finding as a thread on its lines.
- ✅ **[Approval](docs/concepts/approval.md):** a clean review approves the change, and pergent.sh takes the approval back when that stops being true.
- ⚡ **[Incremental reviews](docs/concepts/incremental-reviews.md):** a push re-reviews only what it changed.
- 🗑️ **[Deleted and renamed files](docs/concepts/deleted-files.md):** reviews the lines a change deletes, and what still uses them.
- 🔗 **[Linked repositories](docs/concepts/linked-repositories.md):** checks a change against the repositories that depend on it.
- 🍴 **[Fork merge requests](docs/concepts/fork-merge-requests.md):** changes from forks are reviewed too.

**Tuning**

- 🙋 **[Commands](docs/concepts/commands.md):** `@<bot> review` and `dismiss` in a comment, or as buttons on the dashboard.
- 🧠 **[Learn from dismissals](docs/concepts/learn-from-dismissals.md):** reviews are told what people rejected before, and why.
- 🤫 **[Quiet reviews](docs/concepts/quiet-reviews.md):** fewer nitpicks and shorter comments, the same blocking findings.
- 🎛️ **[Comment layout](docs/concepts/comment-layout.md):** each repository chooses what its review comment shows.
- 🤖 **[Models](docs/concepts/models.md):** a list of models, a default, and a model per repository.

**Control and cost**

- 🛡️ **[Trusted authors](docs/concepts/trusted-authors.md):** a change from an author without write access, and not on the allowlist, waits until someone starts its review.
- 💰 **[Monthly token quota](docs/concepts/token-quota.md):** a ceiling on the tokens an instance or a repository spends.
- 🔒 **[Job isolation](docs/concepts/job-isolation.md):** a Job gets only a read-only clone token, and no service account token.

**Insight and operations**

- 🖥️ **[Dashboard](docs/concepts/dashboard.md):** sign-in, teams, repositories, readiness checks and an audit log.
- 📊 **[Usage](docs/concepts/usage.md) and [metrics](docs/concepts/metrics.md):** where the tokens go, Prometheus metrics and a Grafana dashboard.
- 📈 **[Skill results](docs/concepts/skill-results.md):** which skills people act on, and which they set aside.
- 🩺 **[Troubleshooting](docs/setup/troubleshooting.md):** find a failed review's Job and pod, and which step failed.
- 🔌 **[Review API](docs/setup/review-api.md):** other systems can send changes for review with an API key.

## How it works

1. A merge request or pull request webhook reaches pergent.sh.
2. A Kubernetes Job checks out the change, runs the review and posts the result back.
3. pergent.sh posts one comment, updated on every push, with findings as threads on the diff and a status named `pergent`.
4. A clean review is approved. A person can re-run it or dismiss a finding from a comment or the dashboard.

The Job only holds a read-only clone token, the key of its model and a one-time result token. [More on how it works](docs/concepts/how-it-works.md).

## Requirements

- Kubernetes
- PostgreSQL 13 or newer
- A review bot on GitLab, or a GitHub App
- An API key for an LLM provider
- An HTTPS route so GitLab or GitHub can reach pergent.sh

[Full requirements](docs/setup/install.md#-requirements).

## Install

```bash
helm repo add pergent-sh https://gopaytech.github.io/pergent.sh/charts/releases
helm install pergent-sh pergent-sh/pergent-sh -n pergent-sh -f values.yaml
```

You need a Secret and a `values.yaml` first: follow the [install guide](docs/setup/install.md).

## Docs

- [Install](docs/setup/install.md), [GitHub setup](docs/setup/github.md), [GitLab settings](docs/setup/gitlab.md), [Review API](docs/setup/review-api.md)
- [Dashboard](docs/concepts/dashboard.md), [Skills](docs/concepts/skills.md), [Commands](docs/concepts/commands.md), [Approval](docs/concepts/approval.md)
- [Config reference](docs/reference/configuration.md), [Upgrading](docs/upgrading.md), [Development](docs/development.md)
- [All docs](docs/README.md)

## License

MIT. See [LICENSE](LICENSE).
