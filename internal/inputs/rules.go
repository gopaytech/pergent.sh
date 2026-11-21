package inputs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/gopaytech/pergent/rule"
)

// LoadRules reads the repository's .pergent/rules.yaml at ref, the same ref
// as the repository skills, so a change cannot edit its own rules. No file is
// no entries. A file pergent's rule.Load refuses is a *ProblemError naming
// the file and the entry; a failed read is the source's error.
func LoadRules(ctx context.Context, src Source, projectID int64, ref string) ([]rule.Entry, error) {
	content, found, err := src.GetFile(ctx, projectID, rule.RepoFile, ref)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", rule.RepoFile, err)
	}
	if !found {
		return nil, nil
	}
	entries, err := rule.Load(content, rule.SourceRepo)
	if err != nil {
		return nil, &ProblemError{Name: "rules", Path: rule.RepoFile, Problem: strings.TrimPrefix(err.Error(), rule.RepoFile+": ")}
	}
	return entries, nil
}

// NamedRuleSet is a library rule set as RuleList receives it.
type NamedRuleSet struct{ Name, Content string }

// RuleList is the rule list of a review: the repository's entries, then each
// set's, in the order given, each set in its own order. A set that no longer
// loads (a row saved by an older release, or edited in the database) is a
// *ProblemError naming the set.
func RuleList(repo []rule.Entry, sets []NamedRuleSet) ([]rule.Entry, error) {
	list := append([]rule.Entry(nil), repo...)
	for _, s := range sets {
		entries, err := rule.Load([]byte(s.Content), rule.DashboardSource(s.Name))
		if err != nil {
			prefix := fmt.Sprintf("rule set %q", s.Name)
			return nil, &ProblemError{Name: s.Name, Path: prefix, Problem: strings.TrimPrefix(err.Error(), prefix+": ")}
		}
		list = append(list, entries...)
	}
	return list, nil
}

// RulesHash is the hash a run records for its rule list and built-in switch:
// a new hash makes the next push review every skill with rules in full.
// (The Jobs image joins it once built-in texts ship inside the image.)
func RulesHash(list []rule.Entry, builtIn bool) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("rules %s built-in %t", rule.Hash(list), builtIn)))
	return hex.EncodeToString(sum[:])
}

// EncodeRules is the Job's rules.json for list: nil for an empty list, so a
// Job with no rules is the Job of before.
func EncodeRules(list []rule.Entry) ([]byte, error) {
	if len(list) == 0 {
		return nil, nil
	}
	data, err := rule.EncodeList(list)
	if err != nil {
		return nil, errors.New("encode the rule list: " + err.Error())
	}
	return data, nil
}

// RuleSetLookup finds the rule sets a list names: Names is the list read
// (the default list for nil names), Stored the text of each stored set found.
type RuleSetLookup func(ctx context.Context, names []string) (resolved []string, stored map[string]string, err error)

// BuildRules is the rule list a review gets, the one place it is built: the
// repository's .pergent/rules.yaml at ref (only withRepo), then each rule set
// names lists (nil: the default list), in order, a config-file set
// (configSet) winning over a stored one of the same name. problem is why a
// review must fail (the file or a set can't be used, a set is in neither
// place); err is a source or store error, which is retried.
func BuildRules(ctx context.Context, src Source, projectID int64, ref string, names []string, withRepo bool,
	configSet func(name string) (content string, ok bool), lookup RuleSetLookup) (list []rule.Entry, problem string, err error) {
	var repo []rule.Entry
	if withRepo {
		repo, err = LoadRules(ctx, src, projectID, ref)
		var pe *ProblemError
		switch {
		case errors.As(err, &pe):
			return nil, pe.Error(), nil
		case err != nil:
			return nil, "", err
		}
	}
	resolved, stored, err := lookup(ctx, names)
	if err != nil {
		return nil, "", fmt.Errorf("read the rule library: %w", err)
	}
	sets := make([]NamedRuleSet, 0, len(resolved))
	for _, n := range resolved {
		if content, ok := configSet(n); ok {
			sets = append(sets, NamedRuleSet{Name: n, Content: content})
			continue
		}
		content, ok := stored[n]
		if !ok {
			return nil, fmt.Sprintf("rule set %q is not in the rule library", n), nil
		}
		sets = append(sets, NamedRuleSet{Name: n, Content: content})
	}
	list, err = RuleList(repo, sets)
	var pe *ProblemError
	if errors.As(err, &pe) {
		return nil, pe.Error(), nil
	}
	return list, "", err
}
