# Metrics

pergent.sh can serve Prometheus metrics for the tokens, reviews and findings on the [Usage](usage.md) page. They're off by default.

## Turn them on

With the chart, set `metrics.enabled: true`. Without it, set `metrics_listen` in the config.

This starts a second server that answers `GET /metrics` and nothing else. The main port never serves it.

**Keep it inside the cluster.** The port has no sign-in, and it names repositories, skills and models. Never route it through `public_url` or an Ingress.

The chart adds its own `ClusterIP` Service on port 9090, named like the main one plus `-metrics`. A `service.type: LoadBalancer` on the main Service never carries it.

Point Prometheus at `http://<that Service>.<namespace>.svc:9090/metrics`, or set `metrics.serviceMonitor.enabled: true` (needs the Prometheus Operator's CRDs). `metrics.serviceMonitor.interval` is 60s by default, and `metrics.serviceMonitor.labels` adds the labels your Prometheus selects by.

## Metrics

| Metric | Type | Labels | Counts |
| --- | --- | --- | --- |
| `pergent_tokens_total` | counter | `platform_instance`, `repository`, `skill`, `model`, `kind` | Tokens. `kind` is `input`, `output`, `reasoning`, `cache_read` or `cache_write`. |
| `pergent_reviews_total` | counter | `platform_instance`, `repository`, `trigger`, `incremental` | Reviews that left a result. `trigger` is `push`, `start`, `rerun` or `rerun_failed`. |
| `pergent_reviews_no_result_total` | counter | `platform_instance`, `repository`, `step` | Reviews that failed before leaving a result. `step` is where: `starting`, `clone`, `review`, `delivery`, `unknown`, or `other` for a failure before a Job existed. `sum without (step)` is the count before the label. |
| `pergent_reviews_superseded_total` | counter | `platform_instance`, `repository` | Reviews stopped by a newer push. |
| `pergent_skill_runs_total` | counter | `platform_instance`, `repository`, `skill`, `verdict` | Skill runs. `verdict` is `no_blocking_issues`, `has_blocking_issues`, `failed` or `(other)`. |
| `pergent_findings_total` | counter | `platform_instance`, `repository`, `skill`, `severity` | Findings. `severity` is `blocking` or `suggestion`. |
| `pergent_findings_dismissed` | gauge | `platform_instance`, `repository`, `skill` | Findings dismissed right now. Graph it as it is, without `rate()`. |

Values are read from the database at each scrape, so they survive a restart and include earlier reviews.

## Reading the labels

**`platform_instance`** is the GitLab or GitHub instance's name in pergent.sh. It isn't called `instance` because Prometheus sets that label to the scrape address.

**Tokens** add up like the Usage page: a total is `input` plus `output`. The `reasoning`, `cache_read` and `cache_write` series are absent until a Job runs a pergent that reports them. Absent means unknown; there's no zero standing in for it.

**`model`** is the model name alone, with no provider. The same name from two providers is one series. The [Usage](usage.md) page and its CSV show the provider.

**`repository`** is named only while the repository is switched on. Otherwise it's `(switched off)`.

**`skill`** is named only if the library or a switched-on repository's skill list names it, or it's the fixed `impact` check. Anything else, including skills from a repository's `.pergent/skills` folder, is `(other)`.

No merge request number, title, finding text or other text a person wrote is ever a label.

## Watch out for `(switched off)`

Switching a repository off moves its whole lifetime count to `(switched off)`. Prometheus reads that as one huge burst of new usage, and as a false reset when you switch it back on. Renaming a repository on the platform does the same.

Leave it out of `rate()` and `increase()` panels and alerts:

```
rate(pergent_tokens_total{repository!="(switched off)"}[5m])
```

## Grafana dashboard

[`docs/grafana/pergent-sh.json`](../grafana/pergent-sh.json) is an example dashboard: reviews, tokens by model, repository and skill, and a table of each skill's runs and findings. Import it under **Dashboards → New → Import** and pick your Prometheus data source at the top. The Instance, Repository, Skill and Model filters fill in from the metrics.

Its panels leave out `(switched off)`. Reviews are rare next to a scrape, so its charts show bars of at least an hour.

A series only exists from its first count, so `increase()` can't see the count that created it: the first review of a new repository, skill or model is missing from the charts. Totals on the [Usage](usage.md) page include it.

Metrics hold tokens, not prices, so the dashboard shows no cost.

## Scrape errors

- `500` with an empty body: the database couldn't be read. The error goes to the log.
- `503` with an empty body: another scrape is still running. One runs at a time.
