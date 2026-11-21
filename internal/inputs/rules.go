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
