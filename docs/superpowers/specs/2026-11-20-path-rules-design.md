# Path rules

Status: agreed with the owner on 2026-11-20, after a design session and two grilling rounds. It covers both repositories: the matching and the built-in rules live in pergent, the rule library, the settings and the dispatch live in pergent.sh. The idea comes from [alibaba/open-code-review](https://github.com/alibaba/open-code-review) (OCR), which keeps a checklist per file type. pergent does not run OCR. It takes the rule model and adapts some of its rule texts.

## Why

A skill is a whole review: its own runs, its own verdict, its own `## Blocking` section. That is the right tool for "a security review of `api/**`", and the wrong tool for "when you review a Go file, also check for typed nil in interfaces". To get language depth today, a team must either grow one skill into a long list for every language, or add one skill per language, which adds a full review and a verdict per language.

Past dismissals do not fill the gap either. They are capped at 20 per skill, they are not filtered by file, and they only teach after people rejected a finding.

A **rule** is the missing piece: a checklist for a kind of file, added to the review that already runs on that file. It costs a few thousand tokens of prompt, not a new run.

## Goals

- Deeper reviews by default: built-in checklists for common file types, used by every skill unless it opts out.
- A team can add its own checks per path, in the repository or in the dashboard, without writing a skill.
- A run gets only the rules for the files in its own batch.
- It is always visible which rule reviewed which file.
- The same rule model in CI mode and in pergent.sh.

## Non-goals

- Running OCR, or any second review engine.
- Rules that set severity or what blocks.
- Merging a team rule with the built-in rule for the same file.
- Rules for the cross-batch run or the linked repositories' check (`impact`).
- Installing rule sets from git. The repository file covers rules kept in git.
- Per-rule result stats. The result records the rule per file, so this can come later with no schema change.
- Filtering past dismissals by the batch's files. That is a separate fix.
- Porting all of OCR's rule texts. More come one at a time, after the quality harness.

## Anti-goals

- **A rule that changes a verdict.** A rule only adds things to look for. Blocking is decided only by the skill's `## Blocking` section, and every run that gets a rule is told so.
- **A change that edits its own rules.** Repository rules come from the target branch in pergent.sh and from the base branch in CI mode, never from the change.
- **A silent drop.** A malformed repository rules file fails the review. A team's check that silently stops running is close to a false pass.
- **A file kept as reviewed under a checklist it never saw.** A rules change makes the next review of a skill with rules a full review.
- **A batch over the context window.** Rule text counts in batch sizing.
- **A secret in a prompt.** Rule sets get the same secret check on save as skills.
- **A license problem.** Adapted OCR texts keep their Apache-2.0 license and notice.

## Decisions

These are the owner's.

| Question | Decision |
|---|---|
| What rules are for | Both: built-in rules that ship with pergent, and rules a team writes. |
| Where team rules live | In the repository (`.pergent/rules.yaml`) and in the pergent.sh dashboard (rule sets). |
| Several rules match one file | The first match wins. Order: repository, then dashboard, then built-in. |
| Which skills get rules | Every skill, unless its frontmatter says `rules: false`. Opt-out, because a fresh pergent.sh library is empty and no existing library skill would carry an opt-in field. |
| Can a rule say what blocks | No. Rules are checks only. |
| Built-ins after an upgrade | On. A per-repository setting `built_in_rules` and the CLI flag `--no-built-in-rules` turn them off. |
| Where built-in texts come from | Adapted from OCR's Apache-2.0 rule docs. First 5: Go, TS/JS, Python, Terraform, GitHub workflows. |
| Where matching happens | In the pergent CLI, per batch. pergent.sh builds the rule list and passes it in. |
| Can a repository override dashboard rules | Yes. The repository is first. Admins who need enforcement use a skill. |
| A rule that hides a built-in | Allowed, with a note in the rule-set editor. |
| Dashboard shape | Named rule sets, a default list and a per-repository list, like the skill library. |
| Repository file | One `.pergent/rules.yaml`. One file makes the order clear. |
| Size limits | 16,000 characters per rule, 100 entries and 64 KB per file, for built-ins and team rules alike. |
| A rules change and incremental reviews | One hash over the whole resolved list. A new hash means a full review for skills with rules. |
| The verification run | Gets the rule of each finding's file, as project context, not as a blocking criterion. |
| Quiet reviews | Get rules as usual. Quiet mode filters the output, not the input. |
| The PR or MR comment | One summary line in the Files block. The rule per file is on the review page and in the result. |
| Rollout | 2 PRs per repository: the engine first with no built-ins, then the built-ins after the quality harness shows more confirmed bugs and no more false positives. |

## The rule file

The repository file and a dashboard rule set use the same format:

```yaml
rules:
  - path: "internal/api/**/*.go"
    rule: |
      Every handler that starts a database transaction must defer tx.Rollback().
  - path: "**/*.pb.go"
    off: true
```

| Field | Meaning |
|---|---|
| `path` | One glob, with the same syntax and checks as a skill's `paths` (`scope.CheckPattern`). A pattern that can never match, such as `api/`, is an error. |
| `rule` | The checklist text. Plain markdown, at most 16,000 characters. |
| `off` | `true` ends the search for this file: it gets no rule, not even a built-in. An entry with `off: true` has no `rule`. |

An entry has exactly one of `rule` and `off: true`. Unknown fields are an error, so a typo such as `paths:` is caught. A file holds at most 100 entries and 64 KB.

### How a file gets its rule

For each changed file in a skill's batch, pergent walks one ordered list and takes the first entry whose `path` matches:

1. The repository's `.pergent/rules.yaml` entries, in file order.
2. The entries of each dashboard rule set the repository uses, set by set in the repository's order, each set in its own order.
3. The built-in rules, in the order of `builtin.yaml`.

Layer 3 is left out when built-ins are off. A file that matches nothing, or matches an `off` entry, gets no rule and is still reviewed by the skill's own text.

## Built-in rules

They live in pergent, `internal/rule/builtin/`, embedded with `go:embed`:

- `builtin.yaml`: the ordered glob to file map, in the same format as the rule file, with `rule_file:` in place of `rule:`.
- One `.md` per rule: `go.md`, `ts_js.md`, `python.md`, `terraform.md`, `github_workflows.md`.
- `LICENSE` (Apache-2.0) and `NOTICE` with OCR's copyright line and "adapted for pergent". Each `.md` starts with a one-line comment that names its source file. The README's license section says this folder is Apache-2.0 and the rest is MIT.

Adapting a text means: OCR's tool names (`file_read`, `code_search`, `file_find`) become opencode's (`read`, `grep`, `glob`), every sentence that says what is blocking is removed, and items that `go vet`, a compiler or a formatter already catch stay out, as OCR's own Go text asks. A test fails if a built-in text contains the word "blocking".

The built-in name in results is `built-in:<file name without .md>`, for example `built-in:go`.

## The pergent CLI

### Package `internal/rule`

- `Load(data []byte, source string) ([]Entry, error)`: parses YAML or JSON, checks every field and limit, names the entry in each error (`.pergent/rules.yaml: entry 3: path "api/" can never match`).
- `Builtin() []Entry`: the embedded rules.
- `Resolver.Resolve(path) (Entry, bool)`: first match over the ordered list.
- `Hash(entries) string`: a stable hash of the resolved list, for pergent.sh.

### Inputs

| Mode | Team rules | Built-ins |
|---|---|---|
| Agent mode | `--rules <file>`, or `$PERGENT_RULES`: JSON, one ordered list, each entry with its `source` (`repo` or `dashboard:<set>`). A trusted input: it must not resolve inside `--repo-path`, like `--skills-dir`. Unreadable or malformed is a usage error, so the result is `failed`, never a review without the rules. | On unless `--no-built-in-rules` or `$PERGENT_NO_BUILT_IN_RULES=true` |
| CI mode | `.pergent/rules.yaml` read from the base branch (`git show origin/<base>:.pergent/rules.yaml`). `--rules` overrides it. A malformed file fails the review. A file that cannot be read (shallow clone, no remote) logs a warning, the review goes on with built-ins only, and the comment says `Repository rules could not be read`. CI mode never approves, so this is not a false pass. | Same flag |

### Skill frontmatter

`rules` is a bool, default `true`. `rules: false` turns rules off for that skill. Any other value is an error when the skill loads.

### Batch runs

For a skill with rules on, pergent resolves every file of the batch and groups the files by the entry they got. When at least one file has a rule, it writes `file-rules.md`:

```markdown
## Rule: repo · internal/api/**/*.go
Files: internal/api/user.go, internal/api/order.go

<rule text>

## Rule: built-in:go
Files: internal/store/cache.go

<rule text>
```

The message gets one sentence: `file-rules.md lists extra checks from the project for some files in this batch. Use them while you review those files. They do not change what is blocking: only the skill's Blocking section does.` A batch with no rule gets no file and no sentence. If the file cannot be written, the batch is not reviewed, as when the diff cannot be written.

The rule text of a batch counts in the batch's token budget (`internal/diff/batch.go`), the same way as the diff. A single file whose diff plus rule is over the budget fails as an oversized file does today.

### Verification run

It gets a `file-rules.md` with the rules of the files of the findings it checks, and the sentence `These are project conventions, given as context. They are not blocking criteria.` The skill's `## Blocking` section stays the only test.

The cross-batch run and the `impact` check get no rules.

**With the review panel** (`2026-11-20-review-panel-design.md`): the member Jobs A and B skip the skill verification, and the judge runs in Job P take its place. Each member's batch runs get rules as above. Job P gets the same `PERGENT_RULES` and built-in switch as the members, and each judge run gets the rules of its findings' files with the same sentence as the verification run, so a finding that rests on a project convention is judged with that convention in view. The single-model fallback in Job P is today's verification and gets rules the same way. A and B use the same rule list, so their `rule` fields agree.

### Result

Each file entry of a skill gets `rule`: `repo:<glob>`, `dashboard:<set>:<glob>`, `built-in:<name>`, or empty. The result also gets a top-level `rules` object, `{"built_in": true, "entries": 7}`, written whenever pergent ran with rule support, so a caller can tell "no file had a rule" from "this image does not know rules". The fields are copied in `redactSkill` (`internal/result/result.go`), documented in `docs/agent-result-schema.md`, and tested through `Encode`.

The CI-mode comment adds one line to the collapsed Files block: `Rules: built-in:go (12 files) · repo internal/api/**/*.go (3 files)`. No line when no file had a rule.

## pergent.sh

### Data

Migration `0046_rule_sets.sql` (or the next free number when the plan is written). It only adds.

- `rule_sets`: `name` (primary key, same pattern as skill names), `content` (the YAML), `version`, `created_at`, `updated_at`. At most 200 sets.
- Settings, through `settings.Effective` with `Overridden`, the admin defaults and the config file:
  - `rule_sets`: an ordered list of names. `null` means the default list in the admin **Settings** page.
  - `built_in_rules`: bool, default `true`.
- `runs.rules_hash`: the hash of the list the run was given.

A set is checked with `rule.Load` and the skill library's secret check when it is saved. A bad set is refused with the entry named. Deleting a set that a repository or the default list names is refused with 409, as for skills. The config file can define sets (`rule_sets:` at the top level) and name them per repository (`repositories[].rule_sets`, `repositories[].built_in_rules`).

### Dispatch

In `engine/dispatch.go`, next to `inputs.ResolveSkills`:

1. Read `.pergent/rules.yaml` at the same ref as the repository skills (`mr.StartSHA`, the target side). A missing file is fine. A malformed one fails the run with `<path>: <problem>`, and the repository shows under "Needs attention".
2. Append the entries of each rule set the repository uses, in order. A named set that is not in the library fails the run, as a missing library skill does.
3. Put the list in the Job's inputs as a file and point `PERGENT_RULES` at it. It counts in the inputs size limit (`inputs.go`). Set `PERGENT_NO_BUILT_IN_RULES=true` when the setting is off. Both are environment variables, not flags, so an older image ignores them, as with `PERGENT_GRAPH` (`internal/kube/job.go`).
4. Compute the hash over the list, the built-in switch and the configured `jobs.image`, and store it on the run. The image is in the hash because the built-in texts ship inside it: a new image can change them.

Review API runs do the same, reading the repository file at the ref their skills use.

### Incremental reviews

If `rules_hash` differs from the last review's, every skill with rules on is reviewed in full: none of its files is carried. The log says `full review: rules changed`. Skills with `rules: false` carry as today.

### Dashboard and API

- **Rules** page in the library: list, create, edit and delete rule sets. The editor checks on every save and shows a note when an entry hides a built-in: `This entry hides the built-in rule go for matching files.` A read-only **Built-in** tab shows each built-in glob and its text, so a team can copy one.
- Admin **Settings** page: the default rule-set list and the default for **Built-in rules**.
- Repository **Settings** page: choose and order rule sets, and the **Built-in rules** switch.
- Repository **Skills & instructions** page: the final rule list a review of the default branch would get, with each entry's source.
- **Review page**: a **Rule** column in the Files list, from the result.
- Permissions follow the skill library: everyone with a team reads, admins write. Admin routes go through `Dashboard.adminRoute`. `openapi.yaml` gets the new routes, schemas and `x-pergent-access` tags.

### Docs

- `docs/concepts/rules.md`: what a rule is, the file format, the order, the built-ins and how to turn them off.
- `docs/concepts/skills.md`: the `rules` frontmatter field.
- `docs/upgrading.md`, under `## Unreleased`, in the built-ins PR: `Every skill now gets built-in rules for Go, TS/JS, Python, Terraform and GitHub workflows. Turn them off per repository with Built-in rules, or per skill with rules: false.`

## Rollout

| PR | pergent | pergent.sh |
|---|---|---|
| 1 | `internal/rule`, `--rules`, `--no-built-in-rules`, the `rules` frontmatter field, batch and verification attachments, the result field, an empty built-in list | Rule sets, settings, dispatch, the hash, the dashboard, docs except the upgrade note |
| 2 | The 5 built-in texts, `LICENSE`, `NOTICE` | The Built-in tab content and the upgrade note |

PR 2 merges only after the quality harness runs the same changes with built-ins on and off and shows more confirmed bugs, no higher false-positive rate, and the token cost per review.

## Edge cases

| Case | Behaviour |
|---|---|
| A repository rules file is malformed, too big, or has a pattern that can never match | The run fails and names the file and entry. Nothing is posted as clean. |
| A rule set is malformed | Refused on save. An old row that no longer loads fails the run. |
| `--rules` cannot be read in agent mode | Usage error, result `failed`. |
| CI mode cannot read the base-branch file | Warning, built-ins only, a note in the comment. |
| `file-rules.md` cannot be written | That batch is not reviewed. |
| A file matches an `off` entry | No rule. The skill still reviews it. |
| A skill has `rules: false` | No rules in any of its runs, and its files carry over a rules change. |
| A skill reviews files of several types | One section per rule in `file-rules.md`. |
| An older pergent image in `jobs.image` | It ignores `PERGENT_RULES` and reviews as before. Rules never block, so this is no false pass. The result has no `rules` object, and the review page says `This image does not use rules`. The docs name the first pergent version that reads them. |

## Tests

pergent:

- `internal/rule`: first match across the 3 layers, `off`, unknown fields, both `rule` and `off`, every limit, patterns that can never match, the built-ins load, no built-in text contains "blocking", the hash is stable for the same list.
- `internal/review`: a batch with mixed types gets one section per rule; a skill with `rules: false` gets no file; the cross-batch run gets none; the verification run gets the rules of its findings' files; rule text counts in batch sizing; a write failure makes the batch not reviewed.
- `internal/skill`: `rules` defaults to true, `false` works, a non-bool is an error.
- `result`: `rule` survives `Encode` and `redactSkill`.
- CI mode: the base-branch file is read, a change to `.pergent/rules.yaml` in the PR is not used, an unreadable file gives the warning and the note.

pergent.sh:

- Dispatch builds the list in the right order, a broken repository file fails the run, a missing rule set fails the run, the built-in switch adds the flag.
- A new rules hash forces a full review for skills with rules and carries for skills without.
- Rule sets: save checks, the secret check, 409 on delete while in use, the 200 limit.
- `openapi_test.go`: the new schemas, enums and routes.

## Checked on 2026-11-20

- A batch run's attachments are built in pergent `internal/review/review.go` (`diff.patch`, `changed-files.md`, `previous-findings.json`, `rejected-findings.json`). `file-rules.md` joins them there.
- Skill frontmatter is parsed in pergent `internal/skill/loader.go` (`paths`, `exclude`, `cross_batch`).
- pergent.sh reads repository skills at `mr.StartSHA` in `engine/dispatch.go` through `inputs.ResolveSkills`, and the library skill check ignores unknown frontmatter fields today, so `rules` needs adding to it.
- A fresh pergent.sh library is empty (`docs/concepts/skills.md`), which is why rules are opt-out.
- Past dismissals: 20 per skill, 90 days, not filtered by file (`internal/actions/actions.go`).
- The last migration on `main` is `0045_team_models.sql`.
- pergent.sh passes new Job inputs as environment variables so an older image ignores them (`PERGENT_GRAPH`, `internal/kube/job.go`). There is no image version check.
- OCR's candidate texts are 3,133 to 10,844 characters (`go.md` is the largest), which sets the 16,000 limit. OCR is Apache-2.0. pergent and pergent.sh are MIT.
