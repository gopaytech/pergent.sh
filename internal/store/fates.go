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
// webhook said it was merged (false: closed). RequestedAt names the request:
// whoever works on the job hands it back to ReplaceFates, FateJobFailed or
// DropFateJob, which leave the job alone if a newer request replaced it
// meanwhile. Attempts is how many times working them out has failed.
type FateJob struct {
	MR          MRKey
	ProjectPath string
	Merged      bool
	RequestedAt time.Time
	Attempts    int
}

// SkillResult is the findings of one skill at one severity that were decided
// in a window. Raised, Fixed, Open and WentAway count the findings of merged
// changes; Dismissed also counts those of closed ones, so Raised can be less
// than the columns' sum.
type SkillResult struct {
	Skill, Severity                          string
	Raised, Fixed, Dismissed, Open, WentAway int
	// EditedAt is set when the window holds more than one hash of the skill
	// (on each of its severities): when the newest hash first appears.
	EditedAt *time.Time
}

// RequestFates records that the fates of a change are to be worked out, as of
// at: merged is whether it was merged, false if it was closed. A job there
// already starts over (a close, a reopen and a merge end with the merge), with
// a new RequestedAt. A merged change can't become a closed one, and its
// fates are final once worked out: a redelivered or late webhook leaves a
// merged job alone when it finished, or when it is a close.
func (s *Store) RequestFates(ctx context.Context, mr MRKey, projectPath string, merged bool, at time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO fate_jobs (instance, project_id, mr_iid, project_path, merged, requested_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (instance, project_id, mr_iid) DO UPDATE SET
			project_path = EXCLUDED.project_path, merged = EXCLUDED.merged, requested_at = EXCLUDED.requested_at,
			attempts = 0, not_before = NULL, finished_at = NULL
		WHERE NOT (fate_jobs.merged AND (fate_jobs.finished_at IS NOT NULL OR NOT EXCLUDED.merged))`,
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

// FateJobFailed records a failed attempt on the job requested at requestedAt:
// it has now failed attempts times and is not offered before notBefore. At
// maxFateAttempts it is not offered again. A job finished, gone or requested
// again meanwhile is left as it is.
func (s *Store) FateJobFailed(ctx context.Context, mr MRKey, requestedAt, notBefore time.Time, attempts int) error {
	_, err := s.pool.Exec(ctx, `UPDATE fate_jobs SET attempts = $5, not_before = $6
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND requested_at = $4 AND finished_at IS NULL`,
		mr.Instance, mr.ProjectID, mr.IID, requestedAt, attempts, notBefore)
	return err
}

// DropFateJob forgets the job requested at requestedAt, for a change found
// open again. A job finished or requested again meanwhile is kept, and so
// are the change's fates.
func (s *Store) DropFateJob(ctx context.Context, mr MRKey, requestedAt time.Time) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM fate_jobs
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND requested_at = $4 AND finished_at IS NULL`,
		mr.Instance, mr.ProjectID, mr.IID, requestedAt)
	return err
}

// ReplaceFates stores rows as the fates of a change, decided at at, in place
// of any it had, and marks its job finished with the path and merged as they
// are now. It acts only on the job requested at requestedAt that is not
// finished: if the job was requested again meanwhile (a webhook during the
// pass), was dropped or already finished, it stores nothing and stored is
// false. merged says whether the change was merged and byBot whether a bot
// opened it. Either all of it is stored or none.
func (s *Store) ReplaceFates(ctx context.Context, mr MRKey, requestedAt time.Time, projectPath string, rows []FindingFate, merged, byBot bool, at time.Time) (stored bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	tag, err := tx.Exec(ctx, `UPDATE fate_jobs SET project_path = $5, merged = $6, finished_at = $7
		WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND requested_at = $4 AND finished_at IS NULL`,
		mr.Instance, mr.ProjectID, mr.IID, requestedAt, projectPath, merged, at)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() != 1 {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `DELETE FROM finding_fates WHERE instance = $1 AND project_id = $2 AND mr_iid = $3`,
		mr.Instance, mr.ProjectID, mr.IID); err != nil {
		return false, err
	}
	for _, r := range rows {
		if _, err := tx.Exec(ctx, `INSERT INTO finding_fates (instance, project_id, mr_iid, fingerprint, skill, skill_hash,
				severity, fate, merged, by_bot, decided_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			mr.Instance, mr.ProjectID, mr.IID, r.Fingerprint, r.Skill, r.SkillHash, r.Severity, r.Fate, merged, byBot, at); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
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

// FateChecked records that the sweep looked at a change that has no job at
// at (it was still open), so ChangesWithoutFates offers the others first.
func (s *Store) FateChecked(ctx context.Context, mr MRKey, at time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO fate_checks (instance, project_id, mr_iid, checked_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (instance, project_id, mr_iid) DO UPDATE SET checked_at = EXCLUDED.checked_at`,
		mr.Instance, mr.ProjectID, mr.IID, at)
	return err
}

// ChangesWithoutFates returns the changes the sweep is to look at: at most
// one per repository, up to limit in all, the one least recently checked
// (FateChecked; never checked first, the one with the oldest review first, so
// the new open changes of a repository can't keep an old missed merge from
// being found). A change qualifies
// with a decided run (of a merge request: an API review has no change on a
// forge) created at or after since and either no fate job, or a
// job finished as a close and a decided run created after it (it was reopened
// and its merge or close not recorded). Each is one job with the project path
// of its newest run; Merged, RequestedAt and Attempts are not set, as nothing
// says yet how the change ended.
func (s *Store) ChangesWithoutFates(ctx context.Context, since time.Time, limit int) ([]FateJob, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT instance, project_id, mr_iid, project_path FROM (
			SELECT c.instance, c.project_id, c.mr_iid, c.project_path, c.created_at, k.checked_at,
			       row_number() OVER (PARTITION BY c.instance, c.project_id
			                          ORDER BY k.checked_at NULLS FIRST, c.created_at, c.mr_iid) AS rn
			FROM (
				SELECT DISTINCT ON (r.instance, r.project_id, r.mr_iid)
				       r.instance, r.project_id, r.mr_iid, r.project_path, r.created_at
				FROM runs r
				LEFT JOIN fate_jobs j ON j.instance = r.instance AND j.project_id = r.project_id AND j.mr_iid = r.mr_iid
				WHERE r.kind = 'mr' AND r.outcome <> '' AND r.created_at >= $1
				  AND (j.instance IS NULL OR (j.finished_at IS NOT NULL AND NOT j.merged AND r.created_at > j.finished_at))
				ORDER BY r.instance, r.project_id, r.mr_iid, r.created_at DESC, r.id DESC
			) c
			LEFT JOIN fate_checks k ON k.instance = c.instance AND k.project_id = c.project_id AND k.mr_iid = c.mr_iid
		) ranked
		WHERE rn = 1
		ORDER BY checked_at NULLS FIRST, created_at, instance, project_id, mr_iid LIMIT $2`, since, limit)
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
// changes they closed, which count only as Dismissed.
//
// EditedAt is set on both severities of a skill whose counted findings carry
// more than one skill hash: the first time the newest hash appears, the
// newest being the one first seen last. A finding with no hash isn't a
// version.
func (s *Store) SkillResults(ctx context.Context, instance string, projectID int64, since time.Time) ([]SkillResult, error) {
	rows, err := s.pool.Query(ctx, `SELECT skill, severity, count(*) FILTER (WHERE merged),
			count(*) FILTER (WHERE merged AND fate = 'fixed'), count(*) FILTER (WHERE fate = 'dismissed'),
			count(*) FILTER (WHERE merged AND fate = 'open'), count(*) FILTER (WHERE merged AND fate = 'went_away')
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

	// When each hash was first seen, per skill.
	hashRows, err := s.pool.Query(ctx, `SELECT skill, skill_hash, min(decided_at)
		FROM finding_fates WHERE `+countedFates+` AND skill_hash <> ''
		GROUP BY skill, skill_hash`, instance, projectID, since)
	if err != nil {
		return nil, err
	}
	defer hashRows.Close()
	type seen struct {
		hash  string
		first time.Time
	}
	newest := map[string]seen{}
	versions := map[string]int{}
	for hashRows.Next() {
		var skill string
		var h seen
		if err := hashRows.Scan(&skill, &h.hash, &h.first); err != nil {
			return nil, err
		}
		versions[skill]++
		if cur, ok := newest[skill]; !ok || h.first.After(cur.first) || (h.first.Equal(cur.first) && h.hash > cur.hash) {
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
// jobs requested and the sweep's checks made before it, and returns how many
// rows it deleted.
func (s *Store) PruneFates(ctx context.Context, before time.Time) (int, error) {
	total := 0
	for _, q := range []string{
		`DELETE FROM finding_fates WHERE decided_at < $1`,
		`DELETE FROM fate_jobs WHERE requested_at < $1`,
		`DELETE FROM fate_checks WHERE checked_at < $1`,
	} {
		tag, err := s.pool.Exec(ctx, q, before)
		if err != nil {
			return total, err
		}
		total += int(tag.RowsAffected())
	}
	return total, nil
}
