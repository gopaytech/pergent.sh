# Choose built-in rules per repository

**Goal:** replace the Built-in rules switch with a per-repository list of built-in rule names, so a repository pays only for the rules it wants.

**Source of the design:** the owner's decisions for this piece (2026-11-22), below. There is no separate design doc.

## Decisions (binding)

1. Per repository only. A repository has a list of built-in rule names; empty (the default) is none. The admin default for built-ins goes: the switch on the admin **Settings** page, the `builtInRules` field of `/api/admin/defaults` and its effect.
2. pergent gets `PERGENT_BUILT_IN_RULES=<comma-separated names>` (built in parallel, not released). It loads only those built-ins and skips unknown names. With an empty list pergent.sh keeps sending `PERGENT_NO_BUILT_IN_RULES=true`.
3. Image gate: `config.BuiltInRulesListMinVersion` ("" for now: no image qualifies), the same pattern as `PanelMinVersion`. A list and an image that does not qualify: `PERGENT_NO_BUILT_IN_RULES=true`, no list, and the run page says why. A list and an image that qualifies: `PERGENT_BUILT_IN_RULES=<list>` and no NO flag. Never both.
4. Migration: `built_in_rules` true gets every current built-in name, in `builtin.yaml` order; false or NULL gets an empty list. Additive only.
5. Config: `repositories[].built_in_rules` is a list of names. A bool is refused with a message that says to list names. An unknown name is refused in the config and the API (400), named.
6. The rules hash takes in the sorted list and the image. An empty list keeps the hash of a switch that was off, so most repositories get no extra full review.
7. UI: a picker on the repository **Settings** tab (searchable, globs shown, checkboxes, **Select none**, `3 of 67 chosen`); the admin switch goes; the Rules page **Built-in** tab says how to choose.
8. API reviews use the same resolution. They have no repository, and the default is gone, so they get no built-ins.

## Tasks

1. `internal/config`: `BuiltInRuleNames`, `OrderBuiltInRules`, `BuiltInRulesListMinVersion`, `BuiltInRulesListImage` (shares the version check with `PanelImage`); `built_in_rules` reads a list, refuses a bool, refuses unknown names. Tests first.
2. `internal/store`: migration `0050_built_in_rule_names.sql` (`dashboard_repos.built_in_rule_names text[]`, the backfill, `runs.built_in_rules_note text`); `DashboardRepo.BuiltInRules []string` on the new column; `RepoDefaults` stops reading and writing `built_in_rules`; `SetRunRulesHash` takes the note; `InheritPanelSplit` copies it. Tests: the backfill names equal `rule.Builtin()` names, the mapping on a 0049 database, a 0049-era insert still works, round trips.
3. `internal/settings`: `Effective.BuiltInRules []string`, known names only in `builtin.yaml` order, no default level.
4. `internal/inputs`: `RulesHash(list, builtIns []string, image)`.
5. `internal/kube`: `JobParams.BuiltInRules` sends `PERGENT_BUILT_IN_RULES`; it wins over `NoBuiltInRules`.
6. `internal/engine`: `builtInRulesFor(chosen, image)` decides the list and the note; dispatch and API reviews use it; the note is stored with the rules hash. Golden Job file must not change.
7. `internal/dashboard` and `openapi.yaml`: the settings PUT takes a list, the defaults API loses `builtInRules`, the run page gets `builtInRulesNote`.
8. `web/`: `BuiltInRulePicker`, the Settings tab, the admin Settings page, the Rules page text, the run page note. Tests first.
9. Docs: `docs/concepts/rules.md`, `docs/reference/configuration.md`, `docs/reference/http-routes.md`, `docs/setup/review-api.md`, `docs/upgrading.md` `## Unreleased`.

## Left for the pergent release

Set `BuiltInRulesListMinVersion` to the pergent release that reads `PERGENT_BUILT_IN_RULES`, with `defaultImage` and the `go.mod` pin, and replace the docs' "the next pergent release" placeholders.
