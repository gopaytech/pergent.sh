package store

import (
	"context"
	"time"
)

// maxFateAttempts is how many failed attempts stop a fate job.
const maxFateAttempts = 5

// FindingFate is what became of one finding of a change: Fate is "fixed",
// "dismissed", "open" or "went_away". Skill is the skill's name and SkillHash
// the hash of its content in the last review that reported the finding ("" if
// none was recorded).
type FindingFate struct {
	Fingerprint, Skill, SkillHash, Severity, Fate string
}

// FateJob is a change whose fates are to be worked out. Merged is whether the
// webhook said it was merged (false: closed). Attempts is how many times
// working them out has failed.
type FateJob struct {
	MR          MRKey
	ProjectPath string
	Merged      bool
	RequestedAt time.Time
	Attempts    int
}

// SkillResult is the findings of one skill at one severity that were decided
// in a window: how many were raised and what became of them.
type SkillResult struct {
	Skill, Severity                          string
	Raised, Fixed, Dismissed, Open, WentAway int
	// EditedAt: when the row's newest skill hash first appears, if the window holds an older one.
	EditedAt *time.Time
}

// RequestFates records that the fates of a change are to be worked out, as of
// at: merged is whether it was merged, false if it was closed. A job there
// already starts over (a close, a reopen and a merge end with the merge), unless
// it finished for a merged change, whose fates are final: a redelivered
// webhook changes nothing.
func (s *Store) RequestFates(ctx context.Context, mr MRKey, projectPath string, merged bool, at time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO fate_jobs (instance, project_id, mr_iid, project_path, merged, requested_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (instance, project_id, mr_iid) DO UPDATE SET
			project_path = EXCLUDED.project_path, merged = EXCLUDED.merged, requested_at = EXCLUDED.requested_at,
			attempts = 0, not_before = NULL, finished_at = NULL
		WHERE NOT (fate_jobs.merged AND fate_jobs.finished_at IS NOT NULL)`,
		mr.Instance, mr.ProjectID, mr.IID, projectPath, merged, at)
	return err
}

// ClaimFateJobs returns up to limit fate jobs to work on at now, oldest
// request first: not finished, not failed maxFateAttempts times, and not held
// back by a not_before still to come. Nothing is marked: the one loop that
// works on them ends each with ReplaceFates, FateJobFailed or DropFateJob.
func (s *Store) ClaimFateJobs(ctx context.Context, now time.Time, limit int) ([]FateJob, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT instance, project_id, mr_iid, project_path, merged, requested_at, attempts
		FROM fate_jobs
		WHERE finished_at IS NULL AND attempts < $3 AND (not_before IS NULL OR not_before <= $1)
		ORDER BY requested_at, instance, project_id, mr_iid LIMIT $2`, now, limit, maxFateAttempts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FateJob
	for rows.Next() {
		var j FateJob
		if err := rows.Scan(&j.MR.Instance, &j.MR.ProjectID, &j.MR.IID, &j.ProjectPath, &j.Merged, &j.RequestedAt, &j.Attempts); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// FateJobFailed records a failed attempt: the job has now failed attempts
// times and is not offered before notBefore. At maxFateAttempts it is not
// offered again. A job finished or gone meanwhile is left as it is.
func (s *Store) FateJobFailed(ctx context.Context, mr MRKey, notBefore time.Time, attempts int) error {
	_, err := s.pool.Exec(ctx, `UPDATE fate_jobs SET attempts = $4, not_before = $5
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND finished_at IS NULL`,
		mr.Instance, mr.ProjectID, mr.IID, attempts, notBefore)
	return err
}

// DropFateJob forgets a job not finished, for a change found open again. A
// finished job is kept, and so are the change's fates.
func (s *Store) DropFateJob(ctx context.Context, mr MRKey) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM fate_jobs
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND finished_at IS NULL`,
		mr.Instance, mr.ProjectID, mr.IID)
	return err
}

// ReplaceFates stores rows as the fates of a change, decided at at, in place
// of any it had, and marks its job finished (a change without a job gets a
// finished one). merged says whether it was merged and byBot whether a bot
// opened it. Either all of it is stored or none.
func (s *Store) ReplaceFates(ctx context.Context, mr MRKey, projectPath string, rows []FindingFate, merged, byBot bool, at time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if _, err := tx.Exec(ctx, `DELETE FROM finding_fates WHERE instance = $1 AND project_id = $2 AND mr_iid = $3`,
		mr.Instance, mr.ProjectID, mr.IID); err != nil {
		return err
	}
	for _, r := range rows {
		if _, err := tx.Exec(ctx, `INSERT INTO finding_fates (instance, project_id, mr_iid, fingerprint, skill, skill_hash,
				severity, fate, merged, by_bot, decided_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			mr.Instance, mr.ProjectID, mr.IID, r.Fingerprint, r.Skill, r.SkillHash, r.Severity, r.Fate, merged, byBot, at); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO fate_jobs (instance, project_id, mr_iid, project_path, merged, requested_at, finished_at)
		VALUES ($1, $2, $3, $4, $5, $6, $6)
		ON CONFLICT (instance, project_id, mr_iid) DO UPDATE SET
			project_path = EXCLUDED.project_path, merged = EXCLUDED.merged, finished_at = EXCLUDED.finished_at`,
		mr.Instance, mr.ProjectID, mr.IID, projectPath, merged, at); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// DecidedRuns returns the runs of a merge request whose review was decided
// (they have an outcome), oldest first. A run since superseded still counts:
// superseding leaves its outcome as it was, and the findings it reported are
// what the later runs were measured against.
func (s *Store) DecidedRuns(ctx context.Context, mr MRKey) ([]Run, error) {
	return s.queryRuns(ctx, `SELECT `+runColumns+` FROM runs
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND outcome <> ''
		ORDER BY created_at, id`, mr.Instance, mr.ProjectID, mr.IID)
}

// ChangesWithoutFates returns up to limit changes, newest review first, that
// have a decided run created at or after since and no fate job, for the sweep
// that catches a merge or close whose webhook was missed. Each is one job with
// the project path of its newest run; Merged, RequestedAt and Attempts are
// not set, as nothing says yet how the change ended.
func (s *Store) ChangesWithoutFates(ctx context.Context, since time.Time, limit int) ([]FateJob, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT instance, project_id, mr_iid, project_path FROM (
			SELECT DISTINCT ON (r.instance, r.project_id, r.mr_iid)
			       r.instance, r.project_id, r.mr_iid, r.project_path, r.created_at
			FROM runs r
			WHERE r.outcome <> '' AND r.created_at >= $1
			  AND NOT EXISTS (SELECT 1 FROM fate_jobs j
			                  WHERE j.instance = r.instance AND j.project_id = r.project_id AND j.mr_iid = r.mr_iid)
			ORDER BY r.instance, r.project_id, r.mr_iid, r.created_at DESC, r.id DESC
		) c
		ORDER BY created_at DESC, instance, project_id, mr_iid LIMIT $2`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FateJob
	for rows.Next() {
		var j FateJob
		if err := rows.Scan(&j.MR.Instance, &j.MR.ProjectID, &j.MR.IID, &j.ProjectPath); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// countedFates selects the fates SkillResults counts, from $1 (instance), $2
// (project) and $3 (since): the changes merged that a person opened, and what
// people dismissed on changes they closed.
const countedFates = `instance = $1 AND project_id = $2 AND decided_at >= $3
	AND NOT by_bot AND (merged OR fate = 'dismissed')`

// SkillResults returns, per skill and severity (blocking first), what became
// of the findings of a repository decided at or after since. It counts the
// findings of merged changes opened by people, and the dismissed ones of
// changes they closed.
//
// EditedAt is set on both severities of a skill whose counted findings carry
// more than one skill hash: the first time the newest hash appears. A finding
// with no hash isn't a version.
func (s *Store) SkillResults(ctx context.Context, instance string, projectID int64, since time.Time) ([]SkillResult, error) {
	rows, err := s.pool.Query(ctx, `SELECT skill, severity, count(*),
			count(*) FILTER (WHERE fate = 'fixed'), count(*) FILTER (WHERE fate = 'dismissed'),
			count(*) FILTER (WHERE fate = 'open'), count(*) FILTER (WHERE fate = 'went_away')
		FROM finding_fates WHERE `+countedFates+`
		GROUP BY skill, severity ORDER BY skill, severity`, instance, projectID, since)
	if err != nil {
		return nil, err
	}
	var out []SkillResult
	for rows.Next() {
		var r SkillResult
		if err := rows.Scan(&r.Skill, &r.Severity, &r.Raised, &r.Fixed, &r.Dismissed, &r.Open, &r.WentAway); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// When each hash was first and last seen, per skill.
	hashRows, err := s.pool.Query(ctx, `SELECT skill, skill_hash, min(decided_at), max(decided_at)
		FROM finding_fates WHERE `+countedFates+` AND skill_hash <> ''
		GROUP BY skill, skill_hash`, instance, projectID, since)
	if err != nil {
		return nil, err
	}
	defer hashRows.Close()
	type seen struct {
		hash        string
		first, last time.Time
	}
	newest := map[string]seen{}
	versions := map[string]int{}
	for hashRows.Next() {
		var skill string
		var h seen
		if err := hashRows.Scan(&skill, &h.hash, &h.first, &h.last); err != nil {
			return nil, err
		}
		versions[skill]++
		if cur, ok := newest[skill]; !ok || h.last.After(cur.last) || (h.last.Equal(cur.last) && h.hash > cur.hash) {
			newest[skill] = h
		}
	}
	if err := hashRows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if versions[out[i].Skill] > 1 {
			at := newest[out[i].Skill].first
			out[i].EditedAt = &at
		}
	}
	return out, nil
}

// PruneFates deletes the fates decided before the given time, and the fate
// jobs requested before it, and returns how many rows it deleted.
func (s *Store) PruneFates(ctx context.Context, before time.Time) (int, error) {
	fates, err := s.pool.Exec(ctx, `DELETE FROM finding_fates WHERE decided_at < $1`, before)
	if err != nil {
		return 0, err
	}
	jobs, err := s.pool.Exec(ctx, `DELETE FROM fate_jobs WHERE requested_at < $1`, before)
	if err != nil {
		return 0, err
	}
	return int(fates.RowsAffected() + jobs.RowsAffected()), nil
}
