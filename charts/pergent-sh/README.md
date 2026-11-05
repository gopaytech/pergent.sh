# pergent-sh

pergent.sh runs pergent code reviews on GitLab merge requests and GitHub pull requests as Kubernetes Jobs

![Version: 0.7.0](https://img.shields.io/badge/Version-0.7.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: v0.7.0](https://img.shields.io/badge/AppVersion-v0.7.0-informational?style=flat-square)

pergent.sh runs [pergent](https://github.com/gopaytech/pergent) code reviews on GitLab merge requests and GitHub pull requests, one Kubernetes Job per review. Postgres is not part of this chart. The [repository README](../../README.md) explains the setup: the Secret, the config, the skill library and what the chart creates.

The pergent.sh image and the default pergent image for the review Jobs are built for `linux/amd64` and `linux/arm64`.

## Installing

```console
helm repo add pergent-sh https://gopaytech.github.io/pergent.sh/charts/releases
helm repo update
helm install pergent-sh pergent-sh/pergent-sh -n pergent-sh -f values.yaml
```

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| adminPassword.existingSecret | string | `""` | Name of an existing Secret holding the first password admin's password (at least 12 characters). The chart passes it to the pod as the env var named by config.dashboard.admin_password_env, which must then be set. Optional: without it pergent.sh generates the password and logs it once at the first start ("initial admin password"). Either way the admin must change it at the first sign-in, and the value is read only on the start that creates the admin. |
| adminPassword.key | string | `"password"` | Key of that Secret that holds the password. |
| affinity | object | `{}` | Affinity of the pod. |
| config | object | `{"jobs":{"namespace":"pergent"},"listen":":8080","public_url":"https://pergent.example.com","repositories":[]}` | Rendered verbatim to /etc/pergent-sh/config.yaml. See the repository README for every field and for examples of config.gitlab, config.github, config.llm and config.dashboard ("Config reference", "Instances and the LLM in the config file"). No instance and no LLM are set by default: without config.dashboard.url, pergent.sh needs at least one instance and an LLM here. listen is fixed by the chart: leave it as it is. skills_dir was removed: the skill library lives in the dashboard. |
| existingSecret | string | `""` | Name of an existing Secret in the release namespace (required). It is mounted with envFrom and always holds: DATABASE_URL (PostgreSQL connection URL); PERGENT_SH_SECRET_KEY (32 random bytes, standard base64). Instances, tokens, App keys and the LLM key can be entered in the dashboard instead. Only when they are in the config file (config.gitlab, config.github, config.llm) does the Secret also hold: OPENCODE_API_KEY (the LLM key, the env var named by config.llm.api_key_env); every env var named by config.gitlab[*].review_token_env, approve_token_env and webhook_secret_env; every env var named by config.github[*].review_app.private_key_env, approve_app.private_key_env (or approve_token_env; none with approve_with_review_app) and webhook_secret_env; every env var named by config.gitlab[*] and config.github[*] oauth_client_id_env and oauth_client_secret_env. A private key is the App's whole PEM file, e.g. --from-file=GITHUB_REVIEW_APP_KEY=review-app.private-key.pem |
| fullnameOverride | string | `""` | Overrides the full resource name. |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. |
| image.repository | string | `"ghcr.io/gopaytech/pergent.sh"` | Image repository. |
| image.tag | string | `"v0.7.0"` | Image tag. A released chart pins its own release. "main" is the newest build of the main branch (it moves, so use pullPolicy Always with it); "sha-<short sha>" pins one build. Empty means the chart appVersion. Releasing the chart: set this to the release version (and pullPolicy to IfNotPresent); "main" must not ship in a released chart. |
| imagePullSecrets | list | `[]` | Secrets for pulling the image from a private registry. |
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
| tolerations | list | `[]` | Tolerations of the pod. |

----------------------------------------------
Autogenerated from chart metadata using [helm-docs v1.14.2](https://github.com/norwoodj/helm-docs/releases/v1.14.2)
