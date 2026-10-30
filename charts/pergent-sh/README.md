# pergent-sh

pergent.sh runs pergent code reviews on GitLab merge requests and GitHub pull requests as Kubernetes Jobs

![Version: 0.1.0](https://img.shields.io/badge/Version-0.1.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: v0.1.0](https://img.shields.io/badge/AppVersion-v0.1.0-informational?style=flat-square)

pergent.sh runs [pergent](https://github.com/gopaytech/pergent) code reviews on GitLab merge requests and GitHub pull requests, one Kubernetes Job per review. Postgres is not part of this chart. The [repository README](../../README.md) explains the setup: the Secret, the config, the skills and what the chart creates.

## Installing

```console
helm install pergent-sh charts/pergent-sh -n pergent-sh -f values.yaml
```

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| affinity | object | `{}` | Affinity of the pod. |
| config | object | `{"gitlab":[{"approve_token_env":"GITLAB_APPROVE_TOKEN","name":"main","review_token_env":"GITLAB_REVIEW_TOKEN","url":"https://gitlab.example.com","webhook_secret_env":"GITLAB_WEBHOOK_SECRET"}],"jobs":{"namespace":"pergent"},"listen":":8080","llm":{"api_key_env":"OPENCODE_API_KEY","model":"claude-sonnet-4","provider":"anthropic"},"public_url":"https://pergent.example.com","repositories":[],"skills_dir":"/etc/pergent-sh/skills"}` | Rendered verbatim to /etc/pergent-sh/config.yaml. See the repository README for every field and for the GitHub example (config.github). listen and skills_dir are fixed by the chart: leave them as they are. Set config.gitlab to [] when you only use GitHub. |
| existingSecret | string | `""` | Name of an existing Secret in the release namespace (required). It is mounted with envFrom and holds: DATABASE_URL (PostgreSQL connection URL); PERGENT_SH_SECRET_KEY (32 random bytes, standard base64); OPENCODE_API_KEY (the LLM key, the env var named by config.llm.api_key_env); every env var named by config.gitlab[*].review_token_env, approve_token_env and webhook_secret_env; every env var named by config.github[*].review_app.private_key_env, approve_app.private_key_env (or approve_token_env) and webhook_secret_env. A private key is the App's whole PEM file, e.g. --from-file=GITHUB_REVIEW_APP_KEY=review-app.private-key.pem |
| fullnameOverride | string | `""` | Overrides the full resource name. |
| image.pullPolicy | string | `"Always"` | Image pull policy. |
| image.repository | string | `"ghcr.io/gopaytech/pergent.sh"` | Image repository. |
| image.tag | string | `"main"` | Image tag. "main" is the newest build of the main branch (it moves, hence pullPolicy Always). For production pin "sha-<short sha>" or a release tag such as v0.1.0 (then IfNotPresent is enough). Empty means the chart appVersion. Releasing the chart: set this to the release version (and pullPolicy to IfNotPresent); "main" must not ship in a released chart. |
| imagePullSecrets | list | `[]` | Secrets for pulling the image from a private registry. |
| ingress.annotations | object | `{}` | Ingress annotations. |
| ingress.className | string | `""` | Ingress class name. |
| ingress.enabled | bool | `false` | Create an Ingress. |
| ingress.host | string | `"pergent.example.com"` | Ingress host. |
| ingress.paths | list | `["/webhooks/","/runs/"]` | Paths exposed. GitLab posts to /webhooks/gitlab/<instance>, GitHub to /webhooks/github/<instance>; Jobs post to /runs/<id>/result. |
| ingress.tls | list | `[]` | Ingress TLS, e.g. `[{secretName: pergent-example-com-tls, hosts: [pergent.example.com]}]`. |
| jobEgress.cidrs | list | `["0.0.0.0/0"]` | CIDRs the Jobs may reach on TCP 443 and 80. The default 0.0.0.0/0 lets a Job reach ANY address on 80/443, including in-cluster services and the cloud metadata endpoint: narrow it to GitLab or GitHub and the LLM provider. An EMPTY list allows no external egress at all: Jobs can then only use DNS and the pergent.sh callback (on 8080). |
| jobEgress.except | list | `[]` | Optional ranges carved out of EVERY cidr above (NetworkPolicy "except"), e.g. 169.254.169.254/32. Kubernetes rejects an entry that lies outside its cidr, so use this with broad cidrs such as the default 0.0.0.0/0. |
| jobsNamespace | string | `"pergent"` | The namespace the review Jobs run in. It must already exist. The chart creates the Role, RoleBinding and NetworkPolicy there. Must equal config.jobs.namespace. |
| nameOverride | string | `""` | Overrides the chart name in resource names. |
| nodeSelector | object | `{}` | Node selector of the pod. |
| podAnnotations | object | `{}` | Annotations of the pod. |
| podLabels | object | `{}` | Extra labels of the pod. |
| resources | object | `{"limits":{"memory":"512Mi"},"requests":{"cpu":"100m","memory":"128Mi"}}` | Resources of the pergent.sh pod. |
| service.port | int | `8080` | Service port. |
| service.type | string | `"ClusterIP"` | Service type. |
| serviceAccount.annotations | object | `{}` | Annotations of the ServiceAccount. |
| serviceAccount.create | bool | `true` | Create the ServiceAccount. |
| serviceAccount.name | string | `""` | ServiceAccount name. Defaults to the release's full name. |
| skills | object | `{}` | The skill library: file name (name.md) -> content. Mounted at /etc/pergent-sh/skills. A ConfigMap key can't hold a "/", so folder-style skills (<name>/SKILL.md) aren't possible here. |
| tolerations | list | `[]` | Tolerations of the pod. |

----------------------------------------------
Autogenerated from chart metadata using [helm-docs v1.14.2](https://github.com/norwoodj/helm-docs/releases/v1.14.2)
