// Package links holds the rules for linked repositories: which repository may
// link which (by visibility), whether a set of links is valid, and which
// links a change touches. It is pure functions over plain data and reads no
// database and no platform.
package links

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/gopaytech/pergent/scope"

	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// Max is the most links a repository has.
const Max = 3

// targetRE is a GitHub owner/name. It is strict because a target is later
// written into a clone URL: no scheme, no extra path, no space.
var targetRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// validTarget reports whether t is a plain owner/name: the pattern, no "."
// or ".." segment (they would walk out of a clone URL's path) and no ".git"
// suffix (the clone URL adds it, and the name would get round the checks for
// "itself" and "twice").
func validTarget(t string) bool {
	if !targetRE.MatchString(t) {
		return false
	}
	for _, seg := range strings.Split(t, "/") {
		if seg == "." || seg == ".." || strings.HasSuffix(strings.ToLower(seg), ".git") {
			return false
		}
	}
	return true
}

// openness ranks a visibility: the higher, the more closed.
func openness(v forge.Visibility) (int, bool) {
	switch v {
	case forge.VisPublic:
		return 0, true
	case forge.VisInternal:
		return 1, true
	case forge.VisPrivate:
		return 2, true
	}
	return 0, false
}

// Check reports whether a repository of visibility source may link one of
// visibility target: the target must be as open or more open. A public
// repository links public ones; an internal one, internal or public; a
// private one, anything. A visibility that is not one of the three is an
// error, on either side.
func Check(source, target forge.Visibility) error {
	s, ok := openness(source)
	if !ok {
		return fmt.Errorf("the repository's visibility %q is not public, internal or private", source)
	}
	t, ok := openness(target)
	if !ok {
		return fmt.Errorf("the linked repository's visibility %q is not public, internal or private", target)
	}
	if t > s {
		return fmt.Errorf("a %s repository can't link a %s one: the linked repository must be as open or more open", source, target)
	}
	return nil
}

// Validate checks the links of the repository source ("owner/name"): at most
// Max, each target an owner/name other than source itself (GitHub ignores
// case), no target twice, and every watched path a glob scope.CheckPattern
// accepts. No watched paths is allowed: the link then matches every change.
// No links is valid.
func Validate(source string, ls []store.Link) error {
	if len(ls) > Max {
		return fmt.Errorf("a repository has at most %d linked repositories, got %d", Max, len(ls))
	}
	seen := make(map[string]bool, len(ls))
	for i, l := range ls {
		if !validTarget(l.Target) {
			return fmt.Errorf("link %d: target %q must be owner/name", i+1, l.Target)
		}
		key := strings.ToLower(l.Target)
		if key == strings.ToLower(source) {
			return fmt.Errorf("link %d (%s): a repository can't link itself", i+1, l.Target)
		}
		if seen[key] {
			return fmt.Errorf("link %d (%s): the same repository is linked twice", i+1, l.Target)
		}
		seen[key] = true
		for _, p := range l.Watched {
			if err := scope.CheckPattern(p); err != nil {
				return fmt.Errorf("link %d (%s): watched: %w", i+1, l.Target, err)
			}
		}
	}
	return nil
}

// Match returns the links a change touches, in order: a link with watched
// paths when one of files matches one of them, a link with none when there is
// at least one file. No files match nothing. It does not change ls.
func Match(ls []store.Link, files []string) []store.Link {
	if len(files) == 0 {
		return nil
	}
	var out []store.Link
	for _, l := range ls {
		if len(l.Watched) == 0 || matchesAny(l.Watched, files) {
			out = append(out, l)
		}
	}
	return out
}

func matchesAny(patterns, files []string) bool {
	for _, f := range files {
		if scope.MatchAny(patterns, f) {
			return true
		}
	}
	return false
}
