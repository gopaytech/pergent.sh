package config

import (
	"fmt"
	"slices"
	"sync"

	"github.com/gopaytech/pergent/rule"
	"gopkg.in/yaml.v3"

	"github.com/gopaytech/pergent.sh/internal/result"
)

// BuiltInRulesListMinVersion is the first pergent release that reads
// PERGENT_BUILT_IN_RULES and loads only the built-in rules it names, as
// "vX.Y.Z". An older pergent ignores the list and loads every built-in rule,
// so "" (no release reads it yet) refuses every image: a repository that
// chose some then gets none. The owner sets it at release time, with
// defaultImage.
var BuiltInRulesListMinVersion = ""

// builtInNames is BuiltInRuleNames, read once from the pinned pergent.
var builtInNames = sync.OnceValue(func() []string {
	var names []string
	for _, e := range rule.Builtin() {
		if e.Name != "" && !slices.Contains(names, e.Name) {
			names = append(names, e.Name)
		}
	}
	return names
})

// BuiltInRuleNames is the names of the built-in rules of the pergent this
// pergent.sh is built with, each once, in builtin.yaml order (a rule with a
// {a,b} glob is several entries of rule.Builtin under one name). The caller
// may change the slice it gets.
func BuiltInRuleNames() []string {
	return slices.Clone(builtInNames())
}

// OrderBuiltInRules splits names into the built-in rules it names, each once
// in builtin.yaml order, and the names that are no built-in rule, each once
// in the order given. Both are nil when empty. It is the one place that
// matches names against rule.Builtin; once a pinned pergent has
// rule.SelectBuiltin, it can use that.
//
// The list it keeps is what a Job gets in PERGENT_BUILT_IN_RULES, so it must
// never be empty there: pergent reads an empty list, or one with no name it
// knows, as every built-in rule. A caller sends PERGENT_NO_BUILT_IN_RULES
// instead, never both.
func OrderBuiltInRules(names []string) (known, unknown []string) {
	chosen := map[string]bool{}
	for _, n := range names {
		if slices.Contains(builtInNames(), n) {
			chosen[n] = true
		} else if !slices.Contains(unknown, n) {
			unknown = append(unknown, n)
		}
	}
	for _, n := range builtInNames() {
		if chosen[n] {
			known = append(known, n)
		}
	}
	return known, unknown
}

// BuiltInRulesListImage says whether image runs a pergent that loads only
// the built-in rules PERGENT_BUILT_IN_RULES names: its tag is a version
// (result.VersionAtLeast rules) at or above BuiltInRulesListMinVersion.
// reason is the refusal's text, "" when it passes. A run's page shows it, so
// it names the tag only, never the registry.
func BuiltInRulesListImage(image string) (ok bool, reason string) {
	min := BuiltInRulesListMinVersion
	if min == "" {
		return false, "no review image can load only the chosen built-in rules yet"
	}
	tag := ImageTag(image)
	if tag == "" {
		return false, fmt.Sprintf("the review image has no tag, so its pergent version is not known; choosing built-in rules needs %s or later", min)
	}
	atLeast, known := result.VersionAtLeast(tag, min)
	if !known {
		return false, fmt.Sprintf("image tag %q is not a version (vX.Y.Z); choosing built-in rules needs %s or later", tag, min)
	}
	if !atLeast {
		return false, fmt.Sprintf("image tag %q is older than %s, the first pergent that loads only the chosen built-in rules", tag, min)
	}
	return true, ""
}

// builtInRuleList is repositories[].built_in_rules: a list of built-in rule
// names. Up to v0.18.0 it was a switch, read as a *bool, so an old config
// still starts: a value yaml.v3 decodes into a bool is the switch, true every
// built-in rule (BuiltInRuleNames) and false none.
type builtInRuleList []string

func (l *builtInRuleList) UnmarshalYAML(value *yaml.Node) error {
	var on bool
	if value.Kind == yaml.ScalarNode && value.Decode(&on) == nil {
		if on {
			*l = BuiltInRuleNames()
		} else {
			*l = builtInRuleList{}
		}
		return nil
	}
	if value.Kind != yaml.SequenceNode {
		return fmt.Errorf("line %d: built_in_rules must be a list of built-in rule names, such as [go, yaml]", value.Line)
	}
	out := make(builtInRuleList, 0, len(value.Content))
	for _, n := range value.Content {
		if n.Kind != yaml.ScalarNode {
			return fmt.Errorf("line %d: built_in_rules must be a list of built-in rule names, such as [go, yaml]", n.Line)
		}
		out = append(out, n.Value)
	}
	*l = out
	return nil
}

// orderedBuiltIns is l in builtin.yaml order, each once, with the names that
// are no built-in rule after them, for validate to refuse.
func (l builtInRuleList) orderedBuiltIns() []string {
	known, unknown := OrderBuiltInRules(l)
	return append(known, unknown...)
}
