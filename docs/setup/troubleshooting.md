# Troubleshooting a failed review

Each review runs as a Kubernetes Job. When one fails, the failure line says which step failed. This page shows how to find the review's Job and pod, and what to check for each step.

## Find the Job and pod

**From the review page.** Admins see a Kubernetes group on the review, while it runs and after: the namespace, the Job, the newest pod, and four commands to copy:

```
kubectl -n <namespace> get pods -l job-name=<job>
kubectl -n <namespace> logs <pod> -c checkout
kubectl -n <namespace> logs <pod> -c pergent
kubectl -n <namespace> describe pod <pod>
```

Nobody else sees the group, and the API leaves it out. Until the Job has a pod, the commands that name a pod are left out too.

**From the cluster.** Each review Job, its pod, its Secret and its ConfigMap carry these labels:

| Label | Value |
| --- | --- |
| `pergent.sh/instance` | The instance's name in pergent.sh. |
| `pergent.sh/project-id` | The platform's project ID. |
| `pergent.sh/mr` | The merge request or pull request number. |
| `pergent.sh/head` | The full 40-character head commit. |

To list every review pod of one change that is still in the cluster:

```
kubectl -n <namespace> get pods -l pergent.sh/project-id=<id>,pergent.sh/mr=<number>
```

The Job and pod also carry the annotations `pergent.sh/project` (the full path) and `pergent.sh/mr-url`. A label value that Kubernetes would refuse is left out, never changed.

## When the Job is gone

A Job and its pods are deleted an hour after they finish. A failed review keeps what it needs for 30 days, under the **Logs** tab of the review page:

- **The pergent log**: the last 200 KB of the `pergent` container.
- **The clone log**: the last 64 KB of the `checkout` container, which clones. Anyone who can see the review can read it. Lines that name a linked repository show it as `linked-1`, `linked-2` and so on to people who aren't admins, and show other repositories' addresses as a placeholder. Credentials are replaced by `[REDACTED]`, as in the pergent log.
- **The pod state** (admins only): the pod's phase, why it wasn't scheduled, each container's waiting or terminated reason and exit code, and the last 10 events of the pod and the Job.

A Job that completed keeps its clone log only when a linked repository's clone failed. A review whose pod had already been killed by the deadline may have an empty pergent log.

Events are read when the failure is seen, because the cluster drops them after about an hour. Reading them needs `list` on `events` in the chart's Role. Without it the pod state has no events and pergent.sh logs a warning, once per run. See [Upgrading](../upgrading.md#upgrading-to-0160).

## The steps

The failure line starts with what failed. The **Logs** tab and the comment show it; the cluster details only show to admins.

| Step | The failure line | Usual causes | Look at |
| --- | --- | --- | --- |
| `starting` | `the review couldn't start: no node could run it` | The pod stayed unschedulable for 10 minutes: not enough CPU or memory on any node, a taint, a node selector. | Pod state: the scheduler's message. |
| `starting` | `the review couldn't start: no pod could be created` | The Job made no pod in 10 minutes: a quota, a LimitRange or a PodSecurity rule refused it. | Pod state: the Job's `FailedCreate` event. |
| `starting` | `the review couldn't start (ImagePullBackOff)`, and `ErrImagePull`, `InvalidImageName`, `CreateContainerConfigError` | The image name or tag is wrong, or the cluster can't pull it. The pod waited more than 5 minutes. | `kubectl describe pod`. Check `jobs.image`. |
| `clone` | `the clone failed (checkout exited with code N)` | The clone token can't read the repository or a linked one, or the Job can't reach the platform (`jobEgress.cidrs`). | The clone log. |
| `clone` | `the clone ran out of memory (OOMKilled)` | A very large repository. | Raise the memory limit: `jobs.resources`, or `job_memory` for one repository. |
| `clone` | `the clone didn't finish before the deadline (DeadlineExceeded)` | A slow network or a very large repository. | The clone log. |
| `review` | `the review job ran out of memory (OOMKilled)` | The memory limit is too low for this change. | Raise the memory limit. |
| `review` | `the review didn't finish before the deadline (DeadlineExceeded)` | The model is slow, or the change is big. | The pergent log. Check `jobs.timeout`. |
| `review` | `the review failed: <reason>` | pergent refused or couldn't finish the review, for example untrusted instructions. The reason is its own. | The pergent log. |
| `delivery` | `the review job couldn't deliver its result (exit code 4)` | The Job couldn't post to pergent.sh. | The pergent log. Check `callback_url` and the NetworkPolicy. |
| `delivery` | `the review job finished without sending a result` | The Job ended and sent nothing. | The pergent log. |
| `unknown` | `the review job hit its deadline (DeadlineExceeded)` | The deadline passed and the pod was already gone, so the step isn't known. | The pergent log, if any. |
| `unknown` | `the review job disappeared before sending a result`, `the review job failed (<reason>)` | Someone deleted the Job, or the cluster lost it. | `kubectl get events`. |

A `starting` failure is not retried. Press **Re-run** on the review when the cause is fixed.

A failure before a Job exists (no skills, a change too large, a model that can't be used, an App that isn't installed) isn't a step. Its text is as it was, and the metric counts it as `other`.

## Repeated clone failures

When 2 or more reviews of a repository failed at the clone step in 7 days, "Needs attention" says its clones keep failing. Check the clone token's access and the network to the platform. The item goes when fewer fail ([Logs and "Needs attention"](../concepts/dashboard.md#logs-and-needs-attention)).

## Counting failures

`pergent_reviews_no_result_total` has a `step` label with the steps above ([Metrics](../concepts/metrics.md)). The example Grafana dashboard has a panel for it.
