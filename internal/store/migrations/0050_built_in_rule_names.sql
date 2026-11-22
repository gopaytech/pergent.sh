-- Built-in rules become a list a repository chooses, in place of a switch.
-- It only adds, so the previous release runs on the migrated database: it
-- still reads and writes the old built_in_rules switches, which this release
-- leaves as they are and no longer reads.

-- The built-in rules a repository gets, by name (NULL or empty: none).
ALTER TABLE dashboard_repos ADD COLUMN built_in_rule_names text[];

-- A repository with the switch on gets every built-in rule of pergent
-- v0.11.0, in its builtin.yaml order, so its reviews keep the rules they had.
-- Off or NULL gets none. The admin default (repo_defaults.built_in_rules) is
-- gone and is not copied.
UPDATE dashboard_repos SET built_in_rule_names = ARRAY['properties', 'mapper_dao_xml', 'pom_xml', 'build_gradle',
    'package_json', 'cargo_toml', 'move_toml', 'move', 'composer_json', 'json', 'github_workflows', 'github_config',
    'yaml', 'java', 'go', 'freemarker', 'handlebars_mustache', 'jinja', 'pug', 'ejs_liquid_njk', 'haml_slim', 'erb',
    'arkts', 'astro', 'ts_js', 'kotlin', 'fsharp', 'csharp', 'rust', 'cpp', 'c', 'cuda_hip', 'python', 'ruby', 'php',
    'protobuf', 'po', 'pot', 'graphql', 'prisma', 'julia', 'r', 'terraform', 'bicep', 'dhall', 'nix', 'haskell', 'nim',
    'swift', 'nushell', 'elm', 'jsonnet', 'zig', 'thrift', 'starlark', 'capnp', 'ocaml', 'verilog', 'vhdl', 'matlab',
    'solidity', 'vyper', 'rego', 'shader', 'powershell', 'clojure', 'sql']
  WHERE built_in_rules;

-- Why a run's chosen built-in rules were left out (its image can't load only
-- those), shown on its page; NULL when they were not.
ALTER TABLE runs ADD COLUMN built_in_rules_note text;
