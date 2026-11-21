package engine

import (
	"context"

	"github.com/gopaytech/pergent/rule"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// attRulesInvalid lists a repository under "Needs attention" whose review
// failed on its rules: a malformed .pergent/rules.yaml, a rule set that is
// not in the library, or a stored set that no longer loads.
const attRulesInvalid = "rules_invalid"

// ruleSetReader reads stored rule sets by name; nil names are the default
// list (Store.ResolveRuleSets).
type ruleSetReader func(ctx context.Context, names []string) (store.ResolvedRuleSets, error)

// ruleList builds the rule list a review gets: the repository's
// .pergent/rules.yaml at ref (only withRepo), then each rule set names lists
// (nil: the default list), in order. A config-file set wins over a stored one
// of the same name. problem is why the review must fail (the file or a set
// can't be used, a set is in neither place); err is a source or store error,
// which is retried.
func ruleList(ctx context.Context, src inputs.Source, projectID int64, ref string, names []string,
	cfg *config.Config, read ruleSetReader, withRepo bool) (list []rule.Entry, problem string, err error) {
	return inputs.BuildRules(ctx, src, projectID, ref, names, withRepo, configRuleSet(cfg), storedRuleSets(read))
}

// configRuleSet finds a rule set of the config file.
func configRuleSet(cfg *config.Config) func(string) (string, bool) {
	return func(name string) (string, bool) {
		cs, ok := cfg.RuleSet(name)
		return cs.Content, ok
	}
}

// storedRuleSets adapts a ruleSetReader to inputs.RuleSetLookup.
func storedRuleSets(read ruleSetReader) inputs.RuleSetLookup {
	return func(ctx context.Context, names []string) ([]string, map[string]string, error) {
		resolved, err := read(ctx, names)
		if err != nil {
			return nil, nil, err
		}
		stored := make(map[string]string, len(resolved.Sets))
		for n, rs := range resolved.Sets {
			stored[n] = rs.Content
		}
		return resolved.Names, stored, nil
	}
}

// failRules ends a run whose rules can't be used and lists the repository
// under "Needs attention": a check that silently stops running is close to a
// false pass.
func (d *dispatcher) failRules(ctx context.Context, instance, project, reason string) error {
	d.e.setAttention(ctx, instance, project, attRulesInvalid, d.redact(reason))
	return d.fail(ctx, reason)
}

// rulesProblem is addRules' error for rules that can't be used: the run
// fails with reason. Any other error is retried.
type rulesProblem struct{ reason string }

func (p *rulesProblem) Error() string { return p.reason }

// addRules builds the run's rule list (the repository file at ref only when
// withRepo, then the repository's rule sets), puts it in the inputs and sets
// the run's rules hash.
func (d *dispatcher) addRules(ctx context.Context, src inputs.Source, repo settings.Effective, ref string, withRepo bool, in *inputs.Inputs) error {
	list, problem, err := ruleList(ctx, src, d.run.MR.ProjectID, ref, repo.RuleSets, d.e.Config, d.e.Store.ResolveRuleSets, withRepo)
	if err != nil {
		return err
	}
	if problem != "" {
		return &rulesProblem{reason: problem}
	}
	if in.Rules, err = inputs.EncodeRules(list); err != nil {
		return err
	}
	d.rulesHash = inputs.RulesHash(list, repo.BuiltInRules)
	return nil
}
