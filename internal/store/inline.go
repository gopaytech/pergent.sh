package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// InlineThread is the platform thread pergent.sh posted for a finding of a
// merge request: the GitHub review thread (node ID) and its first comment,
// or the GitLab discussion and its first note. State is "open", or how
// pergent.sh resolved it: "fixed" (the finding is gone), "gone" (its file
// left the change) or "dismissed". A thread a person resolved by hand stays
// "open" here; the platform says it is resolved. Fingerprint is the
// finding's stable fingerprint; File, Line, EndLine and Commit are where
// and on which commit the thread is anchored.
type InlineThread struct {
	MR                 MRKey
	Fingerprint, Skill string
	File               string
	Line, EndLine      int
	Commit             string
	ThreadID           string
	CommentID          int64
	State              string
	// LeftOutNoted: the thread was told its finding was left out of a review
	// because of a past dismissal. PutInlineThread leaves it as it is.
	LeftOutNoted bool
}

const inlineThreadColumns = `instance, project_id, mr_iid, fingerprint, skill, file, line, end_line, commit_sha,
	thread_id, comment_id, state`

// inlineThreadSelect is the columns a read returns: those a write sets, and
// the one only NoteInlineThreadLeftOut sets.
const inlineThreadSelect = inlineThreadColumns + `, left_out_noted`

func scanInlineThread(row pgx.Row) (InlineThread, error) {
	var t InlineThread
	err := row.Scan(&t.MR.Instance, &t.MR.ProjectID, &t.MR.IID, &t.Fingerprint, &t.Skill, &t.File, &t.Line, &t.EndLine,
		&t.Commit, &t.ThreadID, &t.CommentID, &t.State, &t.LeftOutNoted)
	return t, err
}

// InlineThreads returns the threads of mr, ordered by file, line and
// fingerprint.
func (s *Store) InlineThreads(ctx context.Context, mr MRKey) ([]InlineThread, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+inlineThreadSelect+` FROM inline_threads
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3
		ORDER BY file, line, fingerprint`, mr.Instance, mr.ProjectID, mr.IID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InlineThread
	for rows.Next() {
		t, err := scanInlineThread(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// PutInlineThread stores t, replacing the thread of t's merge request and
// fingerprint if there is one (the row's creation time stays). A thread ID
// another fingerprint of the merge request holds is refused, and so is a
// state other than the four.
func (s *Store) PutInlineThread(ctx context.Context, t InlineThread) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO inline_threads (`+inlineThreadColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (instance, project_id, mr_iid, fingerprint) DO UPDATE SET
			skill = EXCLUDED.skill, file = EXCLUDED.file, line = EXCLUDED.line, end_line = EXCLUDED.end_line,
			commit_sha = EXCLUDED.commit_sha, thread_id = EXCLUDED.thread_id, comment_id = EXCLUDED.comment_id,
			state = EXCLUDED.state, updated_at = now()`,
		t.MR.Instance, t.MR.ProjectID, t.MR.IID, t.Fingerprint, t.Skill, t.File, t.Line, t.EndLine, t.Commit,
		t.ThreadID, t.CommentID, t.State)
	return err
}

// RebindInlineThread moves the thread of fingerprint from to fingerprint to
// (the finding was reworded), and to line, endLine and commit. A merge
// request with no thread for from is left as it is; one that has a thread
// for to is an error and changes nothing.
func (s *Store) RebindInlineThread(ctx context.Context, mr MRKey, from, to string, line, endLine int, commit string) error {
	_, err := s.pool.Exec(ctx, `UPDATE inline_threads SET fingerprint = $5, line = $6, end_line = $7, commit_sha = $8,
			updated_at = now()
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND fingerprint = $4`,
		mr.Instance, mr.ProjectID, mr.IID, from, to, line, endLine, commit)
	return err
}

// SetInlineThreadState sets the state of the thread of fingerprint ("open",
// "fixed", "gone" or "dismissed"). A fingerprint with no thread is left as it
// is, not an error.
func (s *Store) SetInlineThreadState(ctx context.Context, mr MRKey, fingerprint, state string) error {
	_, err := s.pool.Exec(ctx, `UPDATE inline_threads SET state = $5, updated_at = now()
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND fingerprint = $4`,
		mr.Instance, mr.ProjectID, mr.IID, fingerprint, state)
	return err
}

// NoteInlineThreadLeftOut records that the thread of fingerprint was told its
// finding was left out of a review because of a past dismissal. A fingerprint
// with no thread is left as it is, not an error.
func (s *Store) NoteInlineThreadLeftOut(ctx context.Context, mr MRKey, fingerprint string) error {
	_, err := s.pool.Exec(ctx, `UPDATE inline_threads SET left_out_noted = true, updated_at = now()
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND fingerprint = $4`,
		mr.Instance, mr.ProjectID, mr.IID, fingerprint)
	return err
}

// DeleteInlineThread forgets the thread of fingerprint; deleting one that is
// not there is not an error.
func (s *Store) DeleteInlineThread(ctx context.Context, mr MRKey, fingerprint string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM inline_threads
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND fingerprint = $4`,
		mr.Instance, mr.ProjectID, mr.IID, fingerprint)
	return err
}

// InlineThreadFor finds the thread a reply belongs to: by GitLab discussion
// ID when discussion is set, else by the GitHub first comment ID replyTo. A
// zero replyTo with no discussion is not a reply, so nothing is found.
func (s *Store) InlineThreadFor(ctx context.Context, mr MRKey, discussion string, replyTo int64) (InlineThread, bool, error) {
	var row pgx.Row
	switch {
	case discussion != "":
		row = s.pool.QueryRow(ctx, `SELECT `+inlineThreadSelect+` FROM inline_threads
			WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND thread_id = $4`,
			mr.Instance, mr.ProjectID, mr.IID, discussion)
	case replyTo != 0:
		row = s.pool.QueryRow(ctx, `SELECT `+inlineThreadSelect+` FROM inline_threads
			WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND comment_id = $4`,
			mr.Instance, mr.ProjectID, mr.IID, replyTo)
	default:
		return InlineThread{}, false, nil
	}
	t, err := scanInlineThread(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return InlineThread{}, false, nil
	}
	if err != nil {
		return InlineThread{}, false, err
	}
	return t, true, nil
}
