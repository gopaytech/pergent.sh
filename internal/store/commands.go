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
}

// What a finished command came to (FinishCommand's outcome). Only a command
// that came to CommandDone counts for RecentCommand; the engine records why
// a refused or dropped one was with any other text.
const CommandDone = "done"

const commandColumns = `id, instance, project_id, mr_iid, project_path, platform, kind, arg, reason,
	comment_id, author, author_id, created_at`

// InsertCommand stores c, which a webhook found in a comment, unless that
// comment is stored already (a redelivery): stored tells which. A zero
// CreatedAt is the database's now.
func (s *Store) InsertCommand(ctx context.Context, c Command) (stored bool, err error) {
	var at *time.Time
	if !c.CreatedAt.IsZero() {
		at = &c.CreatedAt
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO commands (instance, project_id, mr_iid, project_path, platform, kind,
			arg, reason, comment_id, author, author_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, COALESCE($12, now()))
		ON CONFLICT (instance, project_id, mr_iid, comment_id) DO NOTHING`,
		c.MR.Instance, c.MR.ProjectID, c.MR.IID, c.ProjectPath, c.Platform, c.Kind,
		c.Arg, c.Reason, c.CommentID, c.Author, c.AuthorID, at)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ClaimCommands marks up to limit unclaimed commands claimed at now and
// returns them, oldest first. Concurrent callers never claim the same
// command, and a claimed command is not offered again: the claimer finishes
// it with FinishCommand.
func (s *Store) ClaimCommands(ctx context.Context, limit int, now time.Time) ([]Command, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `UPDATE commands SET claimed_at = $2 WHERE id IN (
			SELECT id FROM commands WHERE claimed_at IS NULL ORDER BY id LIMIT $1 FOR UPDATE SKIP LOCKED)
		RETURNING `+commandColumns, limit, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Command
	for rows.Next() {
		var c Command
		if err := rows.Scan(&c.ID, &c.MR.Instance, &c.MR.ProjectID, &c.MR.IID, &c.ProjectPath, &c.Platform, &c.Kind,
			&c.Arg, &c.Reason, &c.CommentID, &c.Author, &c.AuthorID, &c.CreatedAt); err != nil {
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

// RecentCommand reports whether a command of the given kind and argument
// came to CommandDone for mr, having been written at or after since. A
// command still being answered has no outcome yet, so the one being checked
// never finds itself: the caller passes its CreatedAt less the window.
func (s *Store) RecentCommand(ctx context.Context, mr MRKey, kind, arg string, since time.Time) (bool, error) {
	var found bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM commands
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND kind = $4 AND arg = $5
		  AND created_at >= $6 AND outcome = $7)`,
		mr.Instance, mr.ProjectID, mr.IID, kind, arg, since, CommandDone).Scan(&found)
	return found, err
}
