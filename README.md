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

- [Install](docs/setup/install.md), [GitHub setup](docs/setup/github.md), [GitLab settings](docs/setup/gitlab.md)
- [Dashboard](docs/concepts/dashboard.md), [Skills](docs/concepts/skills.md), [Commands](docs/concepts/commands.md), [Approval](docs/concepts/approval.md)
- [Config reference](docs/reference/configuration.md), [Upgrading](docs/upgrading.md), [Development](docs/development.md)
- [All docs](docs/README.md)

## License

MIT. See [LICENSE](LICENSE).
