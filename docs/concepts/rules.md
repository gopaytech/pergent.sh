# 📏 Path rules

A **rule** is a checklist for a kind of file. pergent adds it to the review that already runs on that file. A skill is a whole review, with its own runs and its own verdict. A rule is not: it adds a few things to look for, at the cost of some prompt text, and no new run.

**A rule never changes what is blocking.** Only the skill's `## Blocking` section decides that, and every run that gets a rule is told so.

## The rule file

A repository keeps its rules in `.pergent/rules.yaml`. A rule set in the dashboard uses the same format.

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
| `path` | One glob, with the same syntax and checks as a skill's `paths`. A pattern that can never match, such as `api/`, is an error. |
| `rule` | The checklist text. Plain Markdown, at most 16,000 characters. |
| `off` | `true` ends the search for matching files: they get no rule, not even a built-in one. An entry with `off: true` has no `rule`. |

An entry has exactly one of `rule` and `off: true`. Unknown fields are an error, so a typo such as `paths:` is caught. `off` is a YAML bool (`true`, `false`); `yes` is an error. A file holds at most 100 entries and 64 KB.

## Which rule a file gets

For each changed file in a skill's batch, pergent walks one ordered list and takes the first entry whose `path` matches:

1. The repository's `.pergent/rules.yaml`, in file order. It is read from the **target branch**, the same commit as the repository's skills, so a change cannot edit its own rules.
2. The entries of each rule set the repository uses, set by set in the repository's order.
3. pergent's built-in rules, unless **Built-in rules** is off.

A file that matches nothing, or matches an `off` entry, gets no rule. The skill still reviews it with its own text.

**The built-in list is empty for now.** The first built-in texts come in a later change, once the quality harness shows they help. The **Built-in** tab of the Rules page lists them.

## Rule sets

Rule sets are on the **Rules** page. Everyone signed in reads them; admins create, edit and delete them.

- A set is checked when it is saved, with pergent's own loader: a bad set is refused with the entry named, for example `rule set "go-api": entry 3: path: pattern "api/" can never match a file`. It must not contain a secret pergent.sh holds. The library holds at most 200 sets.
- The editor adds a note for an entry that comes before a built-in rule its files may also match: `This entry hides the built-in rule go for matching files.` That is allowed.
- A set that a repository or the default list uses can't be deleted (409, with the repositories named).
- The config file can define sets under `rule_sets:` (a `name` and the rule file as `content`). They show on the Rules page as **config file** and are read-only there. A config-file repository names them in `repositories[].rule_sets`. A repository the dashboard manages, and the default list, choose sets of the library.

## Settings

| Setting | Where | Default |
|---|---|---|
| **Rule sets used for this repository** | A repository's **Settings**, or `repositories[].rule_sets` | The default list: **Default rule sets** under **Admin → Settings** |
| **Built-in rules** | A repository's **Settings**, the default under **Admin → Settings**, or `repositories[].built_in_rules` | On |

A skill with `rules: false` in its frontmatter gets no rules in any run ([Skills and instructions](skills.md)).

A repository's **Skills & instructions** tab shows the rule list a review of the default branch would get, with each entry's source.

## When the rules can't be used

The review fails, nothing is posted as clean, and the repository shows under **Needs attention** (`rules_invalid`):

- `.pergent/rules.yaml` is malformed, too big, or has a pattern that can never match: `.pergent/rules.yaml: entry 1: needs a rule or off: true`.
- A rule set the repository names is not in the library: `rule set "x" is not in the rule library`.
- A stored set no longer loads.

A check that silently stops running is close to a false pass, so none of these reviews without the rules.

## Reviews and the rule list

- A review records the hash of its rule list and the built-in switch. When the next push has another hash, every skill with rules on is reviewed in full: none of its files is carried over, and the log says `rules changed`. A skill with `rules: false` carries as before. A review from before rules existed counts as a change, so the first push after the upgrade is a full review for skills with rules on.
- Quiet reviews get rules as usual.
- [API reviews](../setup/review-api.md) read no repository file: they get the default rule sets and the default **Built-in rules** switch.
- The rule list reaches the review Job as a file behind the environment variable `PERGENT_RULES`, and `PERGENT_NO_BUILT_IN_RULES=true` when the built-ins are off. A pergent image built before rules ignores both and reviews as before. Rules never block, so this is not a false pass. Its result has no `rules` object, and the review page says `This image does not use rules.`
- The review page shows the rule each reviewed file was checked with, and the comment's **Files** block ends with one line that counts the files per rule.
