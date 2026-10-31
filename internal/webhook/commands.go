package webhook

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gopaytech/pergent.sh/internal/store"
)

// maxReasonRunes is how much of a dismissal's reason is kept.
const maxReasonRunes = 500

// The kinds of command a comment can carry (store.Command.Kind).
const (
	kindReview       = "review"
	kindReviewFailed = "review_failed"
	kindDismiss      = "dismiss"
	kindHelp         = "help"
	kindUnknown      = "unknown"
)

// CommandStore is the part of the store that keeps the commands found in
// comments for the engine to answer.
type CommandStore interface {
	InsertCommand(ctx context.Context, c store.Command) (stored bool, err error)
}

// Bots tells, for a connected instance, the handle people type after @ to
// address the review bot and the usernames whose comments are never
// commands (the review bot and the approve bot). ok is false for an instance
// that isn't connected, whose comments are ignored.
type Bots func(instance string) (mention string, ignore []string, ok bool)

// ParseCommand finds the command in a comment: the first line that has
// @mention (any case, as a whole word) decides, and what follows the mention
// is the command: "review", "review failed", "dismiss <id> [reason]" (id: 6
// to 12 hex digits) or "help". Anything else after the mention is kind
// "unknown", so the person is told what the bot understands. A line that
// quotes another comment (starts with ">") is skipped, so replying with a
// quote doesn't run the quoted command again. ok is false when no line
// mentions the bot; an empty mention matches nothing.
func ParseCommand(body, mention string) (kind, arg, reason string, ok bool) {
	mention = strings.TrimSpace(mention)
	if mention == "" {
		return "", "", "", false
	}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, ">") {
			continue
		}
		rest, found := afterMention(line, mention)
		if !found {
			continue
		}
		kind, arg, reason = parseWords(rest)
		return kind, arg, reason, true
	}
	return "", "", "", false
}

// afterMention returns the text after the first @mention in line that is a
// whole word: not part of a longer name or an address (me@host), and not
// followed by more of a name (@name-2, @name.bot).
func afterMention(line, mention string) (rest string, found bool) {
	for i := 0; i < len(line); i++ {
		if line[i] != '@' {
			continue
		}
		if prev, _ := utf8.DecodeLastRuneInString(line[:i]); i > 0 && isNameRune(prev) {
			continue
		}
		end := i + 1 + len(mention)
		if end > len(line) || !strings.EqualFold(line[i+1:end], mention) {
			continue
		}
		next, size := utf8.DecodeRuneInString(line[end:])
		if end < len(line) {
			if isNameRune(next) || next == '-' {
				continue
			}
			if next == '.' {
				if after, _ := utf8.DecodeRuneInString(line[end+size:]); isNameRune(after) || after == '-' {
					continue
				}
			}
		}
		return line[end:], true
	}
	return "", false
}

func isNameRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' }

// parseWords reads the command in the text after the mention.
func parseWords(rest string) (kind, arg, reason string) {
	rest = strings.TrimSpace(strings.TrimLeft(rest, " \t:,"))
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return kindUnknown, "", ""
	}
	if strings.EqualFold(fields[0], "dismiss") {
		if len(fields) < 2 || !validFindingID(fields[1]) {
			return kindUnknown, "", ""
		}
		reason = strings.TrimSpace(rest[len(fields[0]):])
		reason = strings.TrimSpace(reason[len(fields[1]):])
		if r := []rune(reason); len(r) > maxReasonRunes {
			reason = string(r[:maxReasonRunes])
		}
		return kindDismiss, strings.ToLower(fields[1]), reason
	}
	words := make([]string, len(fields))
	for i, f := range fields {
		words[i] = strings.ToLower(f)
	}
	words[len(words)-1] = strings.TrimRight(words[len(words)-1], ".!")
	switch strings.Join(words, " ") {
	case "review":
		return kindReview, "", ""
	case "review failed":
		return kindReviewFailed, "", ""
	case "help":
		return kindHelp, "", ""
	}
	return kindUnknown, "", ""
}

// validFindingID reports whether s is a finding ID: 6 to 12 hex digits.
func validFindingID(s string) bool {
	if len(s) < 6 || len(s) > 12 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// comment is a new comment on a merge request or pull request, as a webhook
// reports it.
type comment struct {
	Platform, Instance string
	// Project is the project path or repository name as the payload spells it.
	Project   string
	ProjectID int64
	IID       int64
	CommentID int64
	Author    string
	AuthorID  int64
	// AuthorIsBot is set when the platform itself says the author is a bot.
	AuthorIsBot bool
	Body        string
}

// commands records the commands people write in comments.
type commands struct {
	Bots  Bots
	Store CommandStore
	Repos Repositories
	Wake  func()
}

// receive stores the command in c when there is one. It does no platform
// call. outcome and reason are for the log; err is a storage failure, which
// the webhook answers with 500 so the platform sends the event again.
func (d commands) receive(ctx context.Context, c comment) (outcome, reason string, err error) {
	if d.Bots == nil || d.Store == nil {
		return outcomeIgnore, "commands_off", nil
	}
	mention, ignore, ok := d.Bots(c.Instance)
	if !ok || mention == "" {
		return outcomeIgnore, "no_bot", nil
	}
	if c.Author == "" || c.AuthorIsBot || strings.HasSuffix(strings.ToLower(c.Author), "[bot]") {
		return outcomeIgnore, "bot_author", nil
	}
	for _, name := range ignore {
		if name != "" && strings.EqualFold(name, c.Author) {
			return outcomeIgnore, "bot_author", nil
		}
	}
	kind, arg, why, ok := ParseCommand(c.Body, mention)
	if !ok {
		return outcomeIgnore, "no_command", nil
	}
	if c.ProjectID <= 0 || c.IID <= 0 || c.CommentID <= 0 {
		return outcomeIgnore, "missing_ids", nil
	}
	repo, ok, err := lookup(ctx, d.Repos, c.Platform, c.Instance, c.Project)
	if err != nil {
		return "lookup_error", "", err
	}
	if !ok {
		return outcomeIgnore, "unknown_project", nil
	}
	stored, err := d.Store.InsertCommand(ctx, store.Command{
		MR:          store.MRKey{Instance: c.Instance, ProjectID: c.ProjectID, IID: c.IID},
		ProjectPath: repo.Project, Platform: c.Platform, Kind: kind, Arg: arg, Reason: why,
		CommentID: c.CommentID, Author: c.Author, AuthorID: c.AuthorID,
	})
	if err != nil {
		return "command_error", "", err
	}
	if !stored {
		return "command_duplicate", kind, nil
	}
	if d.Wake != nil {
		d.Wake()
	}
	return "command_stored", kind, nil
}
