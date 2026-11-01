package store

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

// What a skill fetch is for (SkillFetch.Purpose), and where it stands
// (SkillFetch.Status).
const (
	FetchInstall = "install"
	FetchUpdate  = "update"

	FetchRunning = "running"
	FetchDone    = "done"
	FetchFailed  = "failed"
)

// ErrSkillFetchNotFound is returned for a skill fetch that does not exist
// (or was pruned).
var ErrSkillFetchNotFound = errors.New("store: skill fetch not found")

// msgFetchStale is the error of a fetch the sweep gave up on.
const msgFetchStale = "reading the source took too long"

// FetchedFile is one skill entry a fetch found: Name is the skill's name, or
// the file or folder name when Problem says why it can't be installed.
// Content is empty when it was not kept (too large, or holding a secret).
type FetchedFile struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Content string `json:"content"`
	Problem string `json:"problem"`
}

// SkillFetch is one read of a skill source. Kind is SourceKindInstance
// (Instance and Repo) or SourceKindURL (URL); Ref is the branch or tag read,
// Path the folder. Purpose is FetchInstall or FetchUpdate; for an update,
// FromRef is the ref the installed skills came from. Status is FetchRunning,
// then FetchDone (Commit and Files set) or FetchFailed (Error set).
// RequestedByID is the requester's account (0: none).
type SkillFetch struct {
	ID, Kind, Instance, Repo, URL, Ref, FromRef, Path, Purpose string // Purpose "install" | "update"
	RequestedBy                                                string
	RequestedByID                                              int64
	Status, Commit, Error                                      string // Status "running" | "done" | "failed"
	Files                                                      []FetchedFile
	CreatedAt, ExpiresAt                                       time.Time
}

// fetchID is the shape of a fetch's ID; anything else is not found, without
// asking Postgres to cast it.
var fetchID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// CreateSkillFetch records a fetch, running, and returns its ID. tokenHash is
// the hash of the Job's one-time result token; nil for a read by the API.
func (s *Store) CreateSkillFetch(ctx context.Context, f SkillFetch, tokenHash []byte) (string, error) {
	var requester *int64
	if f.RequestedByID != 0 {
		requester = &f.RequestedByID
	}
	var id string
	err := s.pool.QueryRow(ctx, `INSERT INTO skill_fetches (id, kind, instance, repo, url, ref, from_ref, path, purpose,
			requested_by, requested_by_id, status, token_hash, created_at, expires_at)
		VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'running', $11, $12, $13) RETURNING id::text`,
		f.Kind, f.Instance, f.Repo, f.URL, f.Ref, f.FromRef, f.Path, f.Purpose,
		f.RequestedBy, requester, tokenHash, f.CreatedAt, f.ExpiresAt).Scan(&id)
	return id, err
}

// SkillFetch returns a fetch, with its files. A missing one is
// ErrSkillFetchNotFound.
func (s *Store) SkillFetch(ctx context.Context, id string) (SkillFetch, error) {
	if !fetchID.MatchString(id) {
		return SkillFetch{}, ErrSkillFetchNotFound
	}
	var (
		f         SkillFetch
		requester *int64
		files     []byte
	)
	err := s.pool.QueryRow(ctx, `SELECT id::text, kind, instance, repo, url, ref, from_ref, path, purpose,
			requested_by, requested_by_id, status, commit, error, files, created_at, expires_at
		FROM skill_fetches WHERE id = $1::uuid`, id).
		Scan(&f.ID, &f.Kind, &f.Instance, &f.Repo, &f.URL, &f.Ref, &f.FromRef, &f.Path, &f.Purpose,
			&f.RequestedBy, &requester, &f.Status, &f.Commit, &f.Error, &files, &f.CreatedAt, &f.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return SkillFetch{}, ErrSkillFetchNotFound
	}
	if err != nil {
		return SkillFetch{}, err
	}
	if requester != nil {
		f.RequestedByID = *requester
	}
	if files != nil {
		if err := json.Unmarshal(files, &f.Files); err != nil {
			return SkillFetch{}, err
		}
	}
	return f, nil
}

// SkillFetchTokenIs reports whether tokenHash is the token of the fetch,
// which is still running and not expired at now. It spends nothing: it is the
// cheap check before the work CompleteSkillFetch's atomic spend then guards.
func (s *Store) SkillFetchTokenIs(ctx context.Context, id string, tokenHash []byte, now time.Time) (bool, error) {
	if !fetchID.MatchString(id) || tokenHash == nil {
		return false, nil
	}
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM skill_fetches
		WHERE id = $1::uuid AND status = 'running' AND token_hash = $2 AND expires_at > $3)`, id, tokenHash, now).Scan(&ok)
	return ok, err
}

// CompleteSkillFetch records what a running fetch read: done with commit and
// files, or failed with errText. It reports whether it did; a fetch that is
// not running, has expired at now, or does not exist is false. tokenHash must
// match the fetch's: nil for a read by the API, the hash of the Job's token
// otherwise, which is spent.
func (s *Store) CompleteSkillFetch(ctx context.Context, id string, tokenHash []byte, commit, errText string, files []FetchedFile, now time.Time) (bool, error) {
	if !fetchID.MatchString(id) {
		return false, nil
	}
	status := FetchDone
	var data []byte
	if errText != "" {
		status, commit, files = FetchFailed, "", nil
	}
	if files != nil {
		var err error
		if data, err = json.Marshal(files); err != nil {
			return false, err
		}
	}
	tag, err := s.pool.Exec(ctx, `UPDATE skill_fetches SET status = $3, commit = $4, error = $5, files = $6, token_hash = NULL
		WHERE id = $1::uuid AND status = 'running' AND token_hash IS NOT DISTINCT FROM $2 AND expires_at > $7`,
		id, tokenHash, status, commit, errText, data, now)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// FailStaleSkillFetches fails the fetches still running that started before
// before (their reader or Job is gone) and spends their tokens. It returns
// how many.
func (s *Store) FailStaleSkillFetches(ctx context.Context, before time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE skill_fetches SET status = 'failed', error = $2, token_hash = NULL
		WHERE status = 'running' AND created_at < $1`, before, msgFetchStale)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// PruneSkillFetches deletes the fetches expired at now and returns how many.
func (s *Store) PruneSkillFetches(ctx context.Context, now time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM skill_fetches WHERE expires_at <= $1`, now)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// InstalledFrom returns the library skills installed from src's source
// (Kind, and Instance and Repo, the repository's case ignored, or URL) at
// src.Ref, with their text, by name. Path and Commit of src are ignored.
func (s *Store) InstalledFrom(ctx context.Context, src SkillSource) ([]LibrarySkill, error) {
	rows, err := s.pool.Query(ctx, `SELECT name, version, content, octet_length(content), updated_at, updated_by, `+sourceColumns+`
		FROM library_skills
		WHERE source_kind = $1 AND source_ref = $2
		  AND (($1 = 'instance' AND source_instance = $3 AND lower(source_repo) = lower($4))
		    OR ($1 = 'url' AND source_url = $5))
		ORDER BY name`, src.Kind, src.Ref, src.Instance, src.Repo, src.URL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LibrarySkill
	for rows.Next() {
		var (
			k                              LibrarySkill
			kind                           *string
			inst, repo, url, ref, pth, cmt string
		)
		if err := rows.Scan(&k.Name, &k.Version, &k.Content, &k.Size, &k.UpdatedAt, &k.UpdatedBy,
			&kind, &inst, &repo, &url, &ref, &pth, &cmt); err != nil {
			return nil, err
		}
		k.Source = scanSource(kind, inst, repo, url, ref, pth, cmt)
		out = append(out, k)
	}
	return out, rows.Err()
}
