# Tracing a review to its Job, and a failed Job to its cause

Status: agreed with the owner on 2026-11-10.

## Why

When a review fails, the operator has to work out which pod in the cluster ran it, and the pod is often gone or says little. Today:

- A review Job is `pergent-review-<12 hex of the run ID>` with the labels `pergent.sh/run=<run ID>` and `app.kubernetes.io/managed-by=pergent-sh`. Nothing on it names the project or the merge request.
- The review page shows neither the Job nor the pod.
- The stored log (`run_logs`, last 200 KB, 30 days) is the `pergent` container's only. The `checkout` init container, which clones, is never kept, so a clone failure leaves nothing on the dashboard.
- A pod stuck on an image error fails the review after 5 minutes (`kube/inspect.go`, `stuckReasons`). A pod that stays `Pending` (no node), or a Job that never gets a pod (quota, LimitRange, PodSecurity), fails only at the Job deadline, and nothing records why.
- Jobs are deleted an hour after they finish (`TTLSecondsAfterFinished: 3600`), with their pods.

The owner hits all four kinds of failure: the clone, a pod that never starts, pergent or the model, and the deadline.

## Goals

- From a review, an admin finds its Job and pod without searching.
- From the cluster, `kubectl` finds the Jobs and pods of one merge request by label.
- A failed review keeps enough to diagnose it after the Job is gone: the clone log, and, for a pod that didn't start, why.
- The failure line of a Job failure says which step failed.
- A Job whose pod can't be created or scheduled fails in 10 minutes, not at the deadline.

## Non-goals

- Keeping Jobs longer. The hour stays.
- Backfilling older reviews.
- Streaming logs live, or showing anything from the cluster that isn't captured.
- Readiness-scan and skill-fetch Jobs. Only review Jobs change.
- Failures before a Job exists (no skills, change too large, model unusable, app not installed, no diff, a Job that couldn't be created). Their text stays as it is.
- Superseded runs. `cleanUp` keeps taking only the pergent log (my call).

## Anti-goals

- **A secret in a log, an event or a label.** The clone log is redacted exactly as the pergent log is (the Job's own Secret values and the engine's LLM keys; nothing stored when the Secret can't be read). Events are redacted the same way. Labels and annotations carry no token, URL credential or anything from the Job's Secret.
- **Cluster details reaching non-admins or the PR.** Job, pod and namespace names, the kubectl commands, the scheduler's message and events are for admins only.
- **Another repository's name reaching someone who may not see it.** The dashboard hides linked repositories from non-admins (`api_read.go`); the clone log does too.
- **A false pass.** None of this changes what counts as a pass. A capture that fails is logged and leaves the review's outcome as it was.
- **Failing a review that would have run.** The 10-minute rules apply only to a Job with no pod or an unscheduled pod, not to one whose containers are starting.
- **Losing the out-of-memory signal.** `OOMKilled` stays in the failure text: the "Needs attention" count and `refreshOutOfMemory` (`watch.go`) find it by substring.
- **Breaking the previous release.** Migrations only add.

## Decisions

These are the owner's.

| Question | Decision |
|---|---|
| Where to start looking | Both: the review page and kubectl labels. |
| What the Job and pod carry | Labels: instance, project ID, MR number, full head SHA. Annotations: full project path and MR URL. |
| The clone log | Kept for any failed review, beside the pergent log. |
| Who reads the clone log | Anyone who can see the review, as with the pergent log. Lines naming a linked repository are masked for non-admins. |
| A pod that never starts | On failure, keep the pod's phase, each container's waiting reason, and the last few events, redacted, for admins. |
| Naming the failed step | Yes, for Job failures. Failures before a Job keep their text. |
| What non-admins and the PR/MR comment see | The step plus the detail they see today: exit codes, `OOMKilled`, the image error reason. Not the scheduler's message, events, or pod, Job and namespace names. |
| Who sees Job/pod names, the kubectl commands, the scheduler's message and events | Admins only. |
| A pod Pending, or a Job with no pod | Fails the review after 10 minutes with the reason. |
| How long a Job stays | 1 hour, unchanged. |
| A linked repository's clone failed but the Job completed | The clone log is kept too. |
| Masking a linked repository for non-admins | Its URL becomes `linked-N`; the rest of the line stays. |
| The failure metric | `pergent_reviews_no_result_total` gains a `step` label. |
| Where it's documented | A new `docs/setup/troubleshooting.md`. |
| The admin block | Shown while the review runs, and after. |
| The Job's Secret and ConfigMap | Carry the same labels. |
| Delivery | One pull request. No release. |
| A Starting failure | Not retried. Someone presses re-run. |
| Repeated clone failures | A "Needs attention" item at 2 or more in 7 days, like out-of-memory. |
| Which events | Every type, the last 10. |
| Watcher failures | The Job disappeared is Unknown, finished without a result is Delivery, pergent's own error result is Review; each keeps its text. |
| How long the clone log and pod state stay | 30 days, as the pergent log. |

## How it works

### Labels and annotations

Each review Job, its pod template, its Secret and its ConfigMap get, beside today's labels:

| Key | Value |
|---|---|
| `pergent.sh/instance` | the instance's name |
| `pergent.sh/project-id` | the platform's project ID |
| `pergent.sh/mr` | the MR number (GitHub PR number) |
| `pergent.sh/head` | the full 40-character head SHA |

and annotations `pergent.sh/project` (the full path) and `pergent.sh/mr-url`.

`JobParams` has none of these today; dispatch passes them from `run.MR` and the MR's web URL. Instance names already match `^[a-z0-9-]+$`. Any value that isn't a valid label value is left out, not altered, so a label never claims a value it doesn't have (my call). Annotations hold any text.

So `kubectl -n <ns> get pods -l pergent.sh/project-id=123,pergent.sh/mr=42` lists every review pod of that MR still in the cluster.

### The review page, for admins

On a run that has a Job, while it runs and after, admins see the namespace, Job name and pod name (the newest pod of the Job, as `Launcher.Logs` picks it, recorded when known), and copyable commands:

```
kubectl -n <ns> get pods -l job-name=<job>
kubectl -n <ns> logs <pod> -c checkout
kubectl -n <ns> logs <pod> -c pergent
kubectl -n <ns> describe pod <pod>
```

The page says the Job is deleted an hour after it finishes; after that only what was captured is left. The API leaves these fields out for non-admins rather than sending them hidden.

### What a failed review keeps

The capture runs once per run, best effort, when the Job is seen to have failed: its `Failed` condition is set, or it is stuck (an image error, or the 10-minute rules below) and about to be deleted. The stuck path takes it before `Launcher.Delete`, which it doesn't today. A Job that completed keeps the clone log too when the checkout log shows a linked repository's clone failed (the owner's); otherwise it keeps only the pergent log, as today, even with failed skills (my call: the capture happens before the outcome is known).

1. **The pergent log**, as today. For a deadline failure the pod is often already gone, so this may be empty, as it is today.
2. **The clone log**: the `checkout` init container's log, redacted the same way and cut to its own tail (64 KB, my call: the script is quiet, `git -q`, so failures are short). It includes the linked repositories' clone lines. In the non-admin view, each linked repository's URL is replaced by its `linked-N` name and the rest of the line is kept, so an error like "Authentication failed" still shows.
3. **The pod's state** (admins only): the pod phase, its `PodScheduled` condition's reason and message, each container's waiting or terminated reason and exit code, and the last 10 events of the pod and of the Job, of every type (Normal ones give the timeline), each with type, reason, message, count and last time. For a Job with no pod, only the Job's events. Redacted, stored as text.

The clone log and pod state are pruned after 30 days with the pergent log.

Events expire on their own (about an hour by default), so they are read at failure, not later. Reading them needs `list` on `events` in the chart's Role.

### Naming the step

A Job failure's reason names its step, then the detail. Today's texts already cover most of them. The detail split is the one decided above.

| Step | Example on the review page, for admins | For non-admins and in the PR/MR comment |
|---|---|---|
| Starting | Pod didn't start: 0/3 nodes are available: 3 Insufficient memory. | The review couldn't start: no node could run it. |
| Starting | No pod was created: exceeded quota: compute-resources. | The review couldn't start: no pod could be created. |
| Starting | Pod didn't start (ImagePullBackOff). | The review couldn't start (ImagePullBackOff). |
| Clone | The clone failed (checkout exited with code 128). | Same. |
| Clone | The deadline passed during the clone. | Same. |
| Review | The review job ran out of memory (OOMKilled). | Same. |
| Review | The deadline passed during the review. | Same. |
| Unknown | The deadline passed (DeadlineExceeded). | Same. |
| Delivery | The review job couldn't deliver its result (exit code 4). | Same. |
| Delivery | The Job finished without sending a result (today's `reasonFinishedNoSend`). | Same. |
| Review | pergent's own error result, e.g. untrusted instructions (today's text). | Same. |
| Unknown | The Job disappeared (today's `reasonDisappeared`). | Same. |

The exact wording is the builder's; the split is not. The admin-only detail is stored apart from `failure_reason`, which is what non-admins, lists and the comment read.

**Which step a deadline hit (my call).** On `DeadlineExceeded` the Job controller kills the pod first, and the watcher looks at least 60 seconds later, so the pod is usually gone. The step comes from what earlier watcher ticks saw: a checkout that had terminated with exit 0 means the review; a pod seen with checkout still running means the clone; nothing seen means Unknown. A checkout that exited 137 or 143 on a Job failed by `DeadlineExceeded` is the deadline during the clone, not a clone failure, so that check comes before the exit-code check in `inspect()`.

### The 10-minute rules

- A pod with `PodScheduled=False` for more than 10 minutes counts as stuck, like the image errors after 5. The reason is the scheduler's message.
- A Job with no pod 10 minutes after it was created counts as stuck. The reason comes from the Job's `FailedCreate` events; without events access it is "no pod was created" with no detail.

Either way the capture runs, the Job is deleted and the review fails with the Starting step. A pod that is scheduled but whose containers are still creating is not covered. A Starting failure is not retried; the re-run button starts it again.

### Repeated clone failures

A new "Needs attention" kind beside `out_of_memory` (`engine/attention.go`): when 2 or more of a repository's runs failed at the Clone step in the last 7 days, the item says the clones of this repository keep failing and to check the token's access and the network. It clears as the out-of-memory item does. The count reads the step, not a substring of the text.

### The metric

`pergent_reviews_no_result_total` gains a `step` label: `starting`, `clone`, `review`, `delivery`, `unknown`, or `other` for failures before a Job exists. Existing series split by step; a query that sums them is unchanged. The example Grafana dashboard (`docs/grafana/pergent-sh.json`) gets a panel by step.

### Docs

A new `docs/setup/troubleshooting.md`, linked from `docs/README.md`: how to find a review's Job and pod (the labels and the admin block), what each step means, and the common causes per step (no node or quota, image pull, clone token or network, OOM, deadline). Short.

## Constraints

- Existing patterns: `kube.Inspect` reads the Job and pods, `captureLogWithin` takes the log before `Launcher.Delete`, run fields go through `store/runs.go`, admin-only fields follow how the dashboard already hides admin data (`role < RoleAdmin`), and `openapi_test.go` checks every view.
- The chart's Role gains `list` on `events`. A cluster where it is missing still works: the events are left out and a warning is logged.
- `docs/upgrading.md` says what changes (the new labels, the `step` label on the metric, the events permission), and the comment layout doc says a Job failure's text now names the step.
- One pull request. No release is part of this work.
- The review comment's other parts don't change.

## Left to the builder

- Table and column names (a second log row per run, or a kind column on `run_logs`; where the admin-only detail lives).
- The exact failure texts, and how a line is recognized as naming a linked repository.
- The layout of the admin block on the review page.
- Whether readiness-scan and skill-fetch Jobs later get the same labels.
