package store

import (
	"context"
	"sort"
	"time"
)

// Command is a command found in a comment of a merge request, kept until the
// engine has answered it. Kind is "review", "review_failed", "dismiss",
// "help" or "unknown"; Arg is the finding ID of a dismiss and Reason its
// reason. CommentID is the platform's ID of the comment, Author and AuthorID
// its author's login and user ID.
type Command struct {
	ID                               int64
	MR                               MRKey
	ProjectPath, Platform, Kind, Arg string
	Reason                           string
	CommentID                        int64
	Author                           string
	AuthorID                         int64
	CreatedAt                        time.Time
	// Attempts is how many times the command was given back to be tried
	// again (ReleaseCommand).
	Attempts int
	// Discussion and ReplyTo say the command was written as a reply in a
	// thread of a finding: the GitLab discussion, or the GitHub review
	// comment it replies to. Empty and 0 for a plain comment.
	Discussion string
	ReplyTo    int64
}

// InThread reports whether the command was written as a reply in a thread.
func (c Command) InThread() bool { return c.Discussion != "" || c.ReplyTo != 0 }

// What a finished command came to (FinishCommand's outcome). The outcomes are:
//   - CommandDone: the command was carried out (a re-run queued, a finding
//     dismissed, a re-run that was queued already);
//   - CommandNoop(why), "done: why": the command was answered and had
//     nothing to do (help, an unknown command, nothing failed, a finding
//     dismissed already);
//   - "refused: why": the command was refused;
//   - "dropped: why": the command was left unanswered but for a 👎;
//   - "gave up: why": the command met errors until it was given up on;
//   - CommandThrottled.
//
// A command that came to CommandDone or CommandNoop counts for
// RecentCommand. Of those, only the latter, and the refused, dropped and
// given-up ones, count for RecentAnswered: a command that was carried out
// does not hold its author's next command off.
const CommandDone = "done"

// commandNoopPrefix starts the outcome of a command that was answered with
// nothing to do.
const commandNoopPrefix = CommandDone + ": "

// CommandNoop is the outcome of a command that was answered and had nothing
// to do, why saying so.
func CommandNoop(why string) string { return commandNoopPrefix + why }

// CommandThrottled is the outcome of a command left unanswered because its
// author had a command on the change answered without doing anything a moment
// before. It does not count as an answer for RecentAnswered, so throttling
// never extends itself.
const CommandThrottled = "throttled"

// commandLease is how long a claim holds a command: one claimed longer ago
// and not finished is taken to be lost (the engine stopped) and is offered
// again.
const commandLease = 5 * time.Minute

const commandColumns = `id, instance, project_id, mr_iid, project_path, platform, kind, arg, reason,
	comment_id, author, author_id, created_at, attempts, discussion_id, reply_to`

// InsertCommand stores c, which a webhook found in a comment, unless that
// comment is stored already (a redelivery): stored tells which. A zero
// CreatedAt is the database's now.
func (s *Store) InsertCommand(ctx context.Context, c Command) (stored bool, err error) {
	var at *time.Time
	if !c.CreatedAt.IsZero() {
		at = &c.CreatedAt
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO commands (instance, project_id, mr_iid, project_path, platform, kind,
			arg, reason, comment_id, author, author_id, created_at, discussion_id, reply_to)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, COALESCE($12, now()), $13, $14)
		ON CONFLICT (instance, project_id, mr_iid, comment_id) DO NOTHING`,
		c.MR.Instance, c.MR.ProjectID, c.MR.IID, c.ProjectPath, c.Platform, c.Kind,
		c.Arg, c.Reason, c.CommentID, c.Author, c.AuthorID, at, c.Discussion, c.ReplyTo)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ClaimCommands marks up to limit commands claimed at now and returns them,
// oldest first. It offers the commands not finished that are due (no
// not_before, or one not after now) and unclaimed, or whose claim is older
// than commandLease; the commands of the instances in skip are left alone.
// Concurrent callers never claim the same command. The claimer finishes it
// with FinishCommand, or gives it back with ReleaseCommand.
func (s *Store) ClaimCommands(ctx context.Context, limit int, now time.Time, skip ...string) ([]Command, error) {
	if limit <= 0 {
		return nil, nil
	}
	if skip == nil {
		skip = []string{} // an empty array, not NULL: `<> ALL(NULL)` is never true
	}
	rows, err := s.pool.Query(ctx, `UPDATE commands SET claimed_at = $2 WHERE id IN (
			SELECT id FROM commands WHERE finished_at IS NULL
			  AND (not_before IS NULL OR not_before <= $2)
			  AND (claimed_at IS NULL OR claimed_at < $4)
			  AND instance <> ALL($3::text[])
			ORDER BY id LIMIT $1 FOR UPDATE SKIP LOCKED)
		RETURNING `+commandColumns, limit, now, skip, now.Add(-commandLease))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Command
	for rows.Next() {
		var c Command
		if err := rows.Scan(&c.ID, &c.MR.Instance, &c.MR.ProjectID, &c.MR.IID, &c.ProjectPath, &c.Platform, &c.Kind,
			&c.Arg, &c.Reason, &c.CommentID, &c.Author, &c.AuthorID, &c.CreatedAt, &c.Attempts, &c.Discussion, &c.ReplyTo); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// RETURNING has no order.
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// FinishCommand records that the engine has answered command id, at now,
// with outcome (CommandDone, or text saying why it was refused or dropped).
func (s *Store) FinishCommand(ctx context.Context, id int64, outcome string, now time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE commands SET finished_at = $3, outcome = $2 WHERE id = $1`, id, outcome, now)
	return err
}

// ReleaseCommand gives a claimed command back, for the engine to try again:
// the claim is cleared, attempts goes up by one and the command is not
// offered before notBefore. A command already finished is left as it is.
func (s *Store) ReleaseCommand(ctx context.Context, id int64, notBefore time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE commands SET claimed_at = NULL, attempts = attempts + 1, not_before = $2
		WHERE id = $1 AND finished_at IS NULL`, id, notBefore)
	return err
}

// DeferCommand gives a claimed command back without counting an attempt: it
// was not tried (or not for long enough to tell), so its turn comes again at
// notBefore with attempts as they were. A command already finished is left
// as it is.
func (s *Store) DeferCommand(ctx context.Context, id int64, notBefore time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE commands SET claimed_at = NULL, not_before = $2
		WHERE id = $1 AND finished_at IS NULL`, id, notBefore)
	return err
}

// PruneCommands deletes the commands finished before the given time and
// returns how many it deleted.
func (s *Store) PruneCommands(ctx context.Context, before time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM commands WHERE finished_at < $1`, before)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// RecentCommand reports whether a command of the given kind and argument
// was answered for mr, having been written at or after since: it came to
// CommandDone or to a CommandNoop. A command still being answered has no
// outcome yet, so the one being checked never finds itself: the caller passes
// its CreatedAt less the window. The rule assumes one processor answers
// commands, one after another (one engine replica): two answered at once
// could both find nothing.
func (s *Store) RecentCommand(ctx context.Context, mr MRKey, kind, arg string, since time.Time) (bool, error) {
	var found bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM commands
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND kind = $4 AND arg = $5
		  AND created_at >= $6 AND (outcome = $7 OR outcome LIKE $8))`,
		mr.Instance, mr.ProjectID, mr.IID, kind, arg, since, CommandDone, commandNoopPrefix+"%").Scan(&found)
	return found, err
}

// RecentAnswered reports whether a command of authorID on mr was finished at
// or after since without being carried out: a reply that did nothing (help,
// nothing to do), a refusal, a drop or a give-up. A command that came to
// CommandDone, and a throttled one, do not count: the former did the work its
// author asked for, the latter would make throttling extend itself. A command
// still being answered has no finish time, so the one being checked never
// finds itself. Like RecentCommand, it assumes one processor answers
// commands.
func (s *Store) RecentAnswered(ctx context.Context, mr MRKey, authorID int64, since time.Time) (bool, error) {
	var found bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM commands
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND author_id = $4
		  AND finished_at >= $5 AND outcome <> $6 AND outcome <> $7)`,
		mr.Instance, mr.ProjectID, mr.IID, authorID, since, CommandThrottled, CommandDone).Scan(&found)
	return found, err
}

// RefusedBefore reports whether an earlier, finished command of the same
// author on the merge request ended with outcome. A command is never finished
// while it is being checked, so the one being checked never finds itself.
func (s *Store) RefusedBefore(ctx context.Context, mr MRKey, authorID int64, outcome string) (bool, error) {
	var found bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM commands
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND author_id = $4
		  AND outcome = $5 AND finished_at IS NOT NULL)`,
		mr.Instance, mr.ProjectID, mr.IID, authorID, outcome).Scan(&found)
	return found, err
}
