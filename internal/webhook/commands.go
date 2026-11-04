package webhook

import (
	"context"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// gitlabBotName matches the usernames GitLab gives its project and group
// access tokens (project_<id>_bot, project_<id>_bot_<hex>, group_<id>_bot…)
// and service accounts: automation, never a person. The engine also asks
// GitLab whether the author is a bot (a user's bot flag) before it acts.
var gitlabBotName = regexp.MustCompile(`^((project|group)_\d+_bot(_[0-9a-f]+)?|service_account_.*)$`)

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

// reviewMarker starts the hidden marker of every review comment pergent and
// pergent.sh post (<!-- pergent -->, <!-- pergent:findings ... -->).
const reviewMarker = "<!-- pergent"

// ParseCommand finds the command in a comment. The first line that starts
// with @mention (any case, as a whole word, after at most 3 spaces) decides,
// and what follows the mention is the command: "review", "review failed",
// "dismiss <id> [reason]" (id: 6 to 12 hex digits; the reason follows the id
// on the same line) or "help". Anything else after the mention is kind
// "unknown", so the person is told what the bot understands. A line that is
// only the mention is passed over, so "@bot" then "@bot review" on the next
// line is a review.
//
// Text that is not the author's own is never read: lines in a fenced code
// block (``` or ~~~), in an HTML comment, indented 4 spaces or a tab (code),
// or quoted (">"). A mention elsewhere in a sentence is no command. ok is
// false when no line commands the bot; an empty mention matches nothing.
func ParseCommand(body, mention string) (kind, arg, reason string, ok bool) {
	return findCommand(body, mention, parseWords)
}

// ParseThreadCommand is ParseCommand for a comment written as a reply in a
// thread, where the finding is already known: "dismiss" takes an optional
// finding ID. When the first word after it is a valid finding ID it is arg;
// otherwise arg is "" and the whole rest of the line is the reason (the engine
// then decides whether that first word was meant as an ID). Other commands
// are read as ParseCommand does.
func ParseThreadCommand(body, mention string) (kind, arg, reason string, ok bool) {
	return findCommand(body, mention, parseThreadWords)
}

// findCommand finds the command line (see ParseCommand) and reads what follows
// the mention with parse.
func findCommand(body, mention string, parse func(rest string) (kind, arg, reason string)) (kind, arg, reason string, ok bool) {
	mention = strings.TrimSpace(mention)
	if mention == "" {
		return "", "", "", false
	}
	var (
		fence     string // the opening fence's character repeated, "" outside a fence
		inComment bool
	)
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		if fence != "" {
			if closesFence(line, fence) {
				fence = ""
			}
			continue
		}
		if inComment {
			end := strings.Index(line, "-->")
			if end < 0 {
				continue
			}
			inComment = false
			line = line[end+len("-->"):]
		}
		if indented(line) {
			continue
		}
		if f := opensFence(line); f != "" {
			fence = f
			continue
		}
		line, inComment = withoutComments(line)
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ">") {
			continue
		}
		rest, found := afterMention(line, mention)
		if !found || strings.Trim(rest, " \t.,:;!?") == "" {
			continue
		}
		kind, arg, reason = parse(rest)
		return kind, arg, reason, true
	}
	return "", "", "", false
}

// indented reports whether line starts with 4 or more spaces, or a tab: code.
func indented(line string) bool {
	n := 0
	for _, c := range line {
		switch c {
		case ' ':
			n++
			if n >= 4 {
				return true
			}
		case '\t':
			return true
		default:
			return false
		}
	}
	return false
}

// opensFence returns the fence (at least 3 backticks or tildes) that line
// opens, "" when it opens none.
func opensFence(line string) string {
	t := strings.TrimLeft(line, " ")
	for _, c := range []byte{'`', '~'} {
		n := len(t) - len(strings.TrimLeft(t, string(c)))
		if n < 3 {
			continue
		}
		// A backtick fence's info string has no backtick: "```code``` text"
		// is inline code, not a fence.
		if c == '`' && strings.Contains(t[n:], "`") {
			continue
		}
		return strings.Repeat(string(c), n)
	}
	return ""
}

// closesFence reports whether line closes fence: only the same character, at
// least as many times, and spaces.
func closesFence(line, fence string) bool {
	if indented(line) {
		return false
	}
	t := strings.TrimSpace(line)
	return len(t) >= len(fence) && strings.Trim(t, fence[:1]) == ""
}

// withoutComments removes the HTML comments from line. open is set when a
// comment starts in line and goes on in the next lines.
func withoutComments(line string) (rest string, open bool) {
	for {
		start := strings.Index(line, "<!--")
		if start < 0 {
			return line, false
		}
		end := strings.Index(line[start+len("<!--"):], "-->")
		if end < 0 {
			return line[:start], true
		}
		line = line[:start] + line[start+len("<!--")+end+len("-->"):]
	}
}

// afterMention returns the text after @mention at the start of line, when it
// is a whole word: not followed by more of a name (@name-2, @name.bot).
func afterMention(line, mention string) (rest string, found bool) {
	end := 1 + len(mention)
	if line == "" || line[0] != '@' || end > len(line) || !strings.EqualFold(line[1:end], mention) {
		return "", false
	}
	if end < len(line) {
		next, size := utf8.DecodeRuneInString(line[end:])
		if isNameRune(next) || next == '-' {
			return "", false
		}
		if next == '.' {
			if after, _ := utf8.DecodeRuneInString(line[end+size:]); isNameRune(after) || after == '-' {
				return "", false
			}
		}
	}
	return line[end:], true
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

// parseThreadWords reads the command in the text after the mention in a
// thread: parseWords, except that "dismiss" needs no ID.
func parseThreadWords(rest string) (kind, arg, reason string) {
	text := strings.TrimSpace(strings.TrimLeft(rest, " \t:,"))
	fields := strings.Fields(text)
	if len(fields) == 0 || !strings.EqualFold(strings.TrimRight(fields[0], ".,:;!?"), "dismiss") {
		return parseWords(rest)
	}
	reason = strings.TrimSpace(text[len(fields[0]):])
	if len(fields) > 1 && validFindingID(fields[1]) {
		// As written: when it isn't the finding's ID it is the start of the
		// reason, and a reason keeps its case. Whatever compares it as an ID
		// ignores the case.
		arg = fields[1]
		reason = strings.TrimSpace(reason[len(fields[1]):])
	}
	if r := []rune(reason); len(r) > maxReasonRunes {
		reason = string(r[:maxReasonRunes])
	}
	return kindDismiss, arg, reason
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
	// Discussion (GitLab: the discussion's ID) and ReplyTo (GitHub: the ID of
	// the thread's first comment) are set when the comment is a reply in a
	// thread; its commands are read with ParseThreadCommand.
	Discussion string
	ReplyTo    int64
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
	// Review comments carry the hidden marker and their text lists commands
	// as examples; whoever posted one (pergent CI posts as a person on
	// GitLab), it is not a command.
	if strings.Contains(c.Body, reviewMarker) {
		return outcomeIgnore, "review_comment", nil
	}
	mention, ignore, ok := d.Bots(c.Instance)
	if !ok || mention == "" {
		return outcomeIgnore, "no_bot", nil
	}
	if c.Author == "" || c.AuthorIsBot || strings.HasSuffix(strings.ToLower(c.Author), "[bot]") {
		return outcomeIgnore, "bot_author", nil
	}
	if c.Platform == config.PlatformGitLab && gitlabBotName.MatchString(c.Author) {
		return outcomeIgnore, "bot_author", nil
	}
	for _, name := range ignore {
		if name != "" && strings.EqualFold(name, c.Author) {
			return outcomeIgnore, "bot_author", nil
		}
	}
	parse := ParseCommand
	if c.Discussion != "" || c.ReplyTo != 0 {
		parse = ParseThreadCommand
	}
	kind, arg, why, ok := parse(c.Body, mention)
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
		Discussion: c.Discussion, ReplyTo: c.ReplyTo,
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
