package store

import "context"

// SkillUsage is what one skill used in one run, as the Job reported it.
// CacheRead, CacheWrite and Reasoning are nil when the Job didn't report
// them.
type SkillUsage struct {
	Skill, Verdict                   string
	In, Out                          int64
	CacheRead, CacheWrite, Reasoning *int64
	Findings, Blocking               int
}

// RecordUsage stores what each skill of run id used, with the run's
// repository, merge request, trigger, model and time, and marks the run as
// recorded, even with no skills. A row already there is kept.
func (s *Store) RecordUsage(ctx context.Context, id string, skills []SkillUsage) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	for _, u := range skills {
		if _, err := tx.Exec(ctx, `INSERT INTO run_usage (run_id, skill, instance, project_id, project_path, mr_iid,
				trigger, incremental, model, verdict, reported_at, tokens_in, tokens_out, cache_read, cache_write,
				reasoning, findings, blocking)
			SELECT r.id, $2::text, r.instance, r.project_id, r.project_path, r.mr_iid, r.trigger, r.incremental,
				COALESCE(r.model, ''), $3::text, COALESCE(r.reported_at, r.created_at), $4::bigint, $5::bigint,
				$6::bigint, $7::bigint, $8::bigint, $9::integer, $10::integer
			FROM runs r WHERE r.id = $1::uuid
			ON CONFLICT (run_id, skill) DO NOTHING`,
			id, u.Skill, u.Verdict, u.In, u.Out, u.CacheRead, u.CacheWrite, u.Reasoning, u.Findings, u.Blocking); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE runs SET usage_recorded = true WHERE id = $1::uuid`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RunsWithoutUsage returns up to limit runs with a stored result and no
// usage recorded, oldest first.
func (s *Store) RunsWithoutUsage(ctx context.Context, limit int) ([]Run, error) {
	return s.queryRuns(ctx, `SELECT `+runColumns+` FROM runs
		WHERE NOT usage_recorded AND result IS NOT NULL ORDER BY created_at LIMIT $1`, limit)
}
