# pergent-sh

pergent.sh runs pergent code reviews on GitLab merge requests and GitHub pull requests as Kubernetes Jobs

![Version: 0.26.1](https://img.shields.io/badge/Version-0.26.1-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: v0.26.0](https://img.shields.io/badge/AppVersion-v0.26.0-informational?style=flat-square)

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
| adminPassword.existingSecret | string | `""` | Name of an existing Secret holding the first local admin's password (at least 12 characters). The chart passes it to the pod as the env var named by config.dashboard.admin_password_env, which must then be set. Optional: without it pergent.sh generates the password and logs it once at the first start ("initial admin password"). Either way the admin must change it at the first sign-in, and the value is read only on the start that creates the admin. |
| adminPassword.key | string | `"password"` | Key of that Secret that holds the password. |
| affinity | object | `{}` | Affinity of the pod. |
| config | object | `{"jobs":{"namespace":"pergent"},"listen":":8080","public_url":"https://pergent.example.com","repositories":[]}` | Rendered to /etc/pergent-sh/config.yaml. callback_url defaults to this chart's Service, so Jobs post their results inside the cluster; set it to send them elsewhere, but security scans need the in-cluster Service address: a scan gets its source from pergent.sh and posts its result there through callback_url, and the scan NetworkPolicy allows only the pergent.sh pods on 8080, so with a public callback_url every scan fails at the source download. See the repository README for every field and for examples of config.gitlab, config.github, config.llm and config.dashboard ("Config reference", "Instances and the LLM in the config file"). No instance and no LLM are set by default: without config.dashboard.url, pergent.sh needs at least one instance and an LLM here. listen is fixed by the chart: leave it as it is. skills_dir was removed: the skill library lives in the dashboard. |
| existingSecret | string | `""` | Name of an existing Secret in the release namespace (required). It is mounted with envFrom and always holds: DATABASE_URL (PostgreSQL connection URL); PERGENT_SH_SECRET_KEY (32 random bytes, standard base64). Instances, tokens, App keys and the LLM key can be entered in the dashboard instead. Only when they are in the config file (config.gitlab, config.github, config.llm) does the Secret also hold: OPENCODE_API_KEY (the LLM key, the env var named by config.llm.api_key_env); every env var named by config.gitlab[*].review_token_env, approve_token_env (none with approve_with_review_bot) and webhook_secret_env; every env var named by config.github[*].review_app.private_key_env, approve_app.private_key_env (or approve_token_env; none with approve_with_review_app) and webhook_secret_env; the env var named by config.dashboard.sign_in.client_secret_env, when OIDC sign-in is set in the config file (set in the dashboard, the client secret is entered there). A private key is the App's whole PEM file, e.g. --from-file=GITHUB_REVIEW_APP_KEY=review-app.private-key.pem |
| fullnameOverride | string | `""` | Overrides the full resource name. |
| graphs.enabled | bool | `false` | Keep code graphs: a PVC, and the graph build image. Needs the dashboard (config.dashboard.url), where the switch is. |
| graphs.image.repository | string | `"ghcr.io/gopaytech/pergent.sh-graph"` | The graph build image: graphify and the pergent converter, no token. Its Job runs as user 65532. |
| graphs.image.tag | string | `""` | Its tag. Empty means the chart appVersion, as for image.tag. |
| graphs.persistence.size | string | `"5Gi"` | Size of the graphs volume. Only the newest graph of each repository is kept. |
| graphs.persistence.storageClass | string | `""` | Storage class of the graphs volume. Empty uses the cluster's default. |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. |
| image.repository | string | `"ghcr.io/gopaytech/pergent.sh"` | Image repository. |
| image.tag | string | `"v0.26.0"` | Image tag. A released chart pins its own release. "main" is the newest build of the main branch (it moves, so use pullPolicy Always with it); "sha-<short sha>" pins one build. Empty means the chart appVersion. Releasing the chart: set this to the release version (and pullPolicy to IfNotPresent); "main" must not ship in a released chart. |
| imagePullSecrets | list | `[]` | Secrets for pulling the image from a private registry. |
| jobEgress.cidrs | list | `["0.0.0.0/0"]` | CIDRs the Jobs may reach on TCP 443 and 80. The default 0.0.0.0/0 lets a Job reach ANY address on 80/443, including in-cluster services and the cloud metadata endpoint: narrow it to GitLab or GitHub and the LLM provider. An EMPTY list allows no external egress at all: Jobs can then only use DNS and the pergent.sh callback (on 8080). |
| jobEgress.except | list | `[]` | Optional ranges carved out of EVERY cidr above (NetworkPolicy "except"), e.g. 169.254.169.254/32. Kubernetes rejects an entry that lies outside its cidr, so use this with broad cidrs such as the default 0.0.0.0/0. |
| jobsNamespace | string | `"pergent"` | The namespace the review Jobs run in. It must already exist. The chart creates the Role, RoleBinding and NetworkPolicy there. Must equal config.jobs.namespace. |
| metrics.enabled | bool | `false` | Serve /metrics on metrics.port. |
| metrics.port | int | `9090` | Port of the metrics server and its Service. |
| metrics.serviceMonitor.enabled | bool | `false` | Create a ServiceMonitor (needs the Prometheus Operator's CRDs). |
| metrics.serviceMonitor.interval | string | `"60s"` | Scrape interval. |
| metrics.serviceMonitor.labels | object | `{}` | Extra labels, for a Prometheus that selects ServiceMonitors by label. |
| nameOverride | string | `""` | Overrides the chart name in resource names. |
| networkPolicy.enabled | bool | `true` | Create the 2 NetworkPolicies for the Jobs: the review Jobs one (jobEgress) and the security scan Jobs one (securityJobEgress). false creates neither: every Job may reach any address, jobEgress and securityJobEgress are ignored, and security scans are on with no securityJobEgress.cidrs. A scan Job runs a shell, so with false a scanned repository's instructions could send its LLM key anywhere. |
| nodeSelector | object | `{}` | Node selector of the pod. |
| podAnnotations | object | `{}` | Annotations of the pod. |
| podLabels | object | `{}` | Extra labels of the pod. |
| resources | object | `{"limits":{"memory":"512Mi"},"requests":{"cpu":"100m","memory":"128Mi"}}` | Resources of the pergent.sh pod. |
| securityJobEgress.cidrs | list | `["0.0.0.0/0"]` | CIDRs the security scan Jobs may reach on the ports below. The default 0.0.0.0/0 lets a scan Job reach ANY address on 443 and 80, like jobEgress for review Jobs, so scans work out of the box. A scan Job runs a shell, so narrow it to the LLM provider (or the egress proxy, see config.security.proxy) to stop a scanned repository's text from sending the model key elsewhere. Scans need config.callback_url to be the chart's in-cluster Service address (the default): the scan policy allows only the pergent.sh pods on 8080. An EMPTY list allows no external egress: scan Jobs then reach only the DNS servers below and the pergent.sh pods on 8080, and Start scan stays off (the chart sets no PERGENT_SH_SECURITY_EGRESS). Remaining risk: DNS queries through a recursive resolver can still carry data out. |
| securityJobEgress.dns | list | `[]` | DNS servers the scan Jobs may query on port 53 (TCP and UDP): a list of NetworkPolicy peers (the entries of "to"). Empty means the cluster DNS: pods labelled k8s-app=kube-dns in the kube-system namespace. Set it for NodeLocal DNSCache or another DNS label. Remaining risk: a resolver that recurses can still carry data out in the names it is asked. |
| securityJobEgress.except | list | `[]` | Optional ranges carved out of EVERY cidr above (NetworkPolicy "except"). Kubernetes rejects an entry that lies outside its cidr. |
| securityJobEgress.ports | list | `[443,80]` | TCP ports the cidrs are open on (an empty list means 443 and 80, never all ports). A proxy must be in cidrs, and its port (for example 3128) in this list. |
| service.port | int | `8080` | Service port. |
| service.type | string | `"ClusterIP"` | Service type. |
| serviceAccount.annotations | object | `{}` | Annotations of the ServiceAccount. |
| serviceAccount.create | bool | `true` | Create the ServiceAccount. |
| serviceAccount.name | string | `""` | ServiceAccount name. Defaults to the release's full name. |
| tolerations | list | `[]` | Tolerations of the pod. |

----------------------------------------------
Autogenerated from chart metadata using [helm-docs v1.14.2](https://github.com/norwoodj/helm-docs/releases/v1.14.2)
