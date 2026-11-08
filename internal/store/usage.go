package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

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

// RunUsage returns what run id used, summed over its recorded skills; found
// is false when its usage isn't recorded yet. A re-run of failed skills keeps
// the other skills' results in its stored result, so its own tokens are only
// in the table.
func (s *Store) RunUsage(ctx context.Context, id string) (in, out int64, found bool, err error) {
	// bool_or over no rows is NULL.
	var recorded *bool
	err = s.pool.QueryRow(ctx, `SELECT COALESCE(sum(tokens_in), 0), COALESCE(sum(tokens_out), 0), bool_or(true)
		FROM run_usage WHERE run_id = $1::uuid`, id).Scan(&in, &out, &recorded)
	return in, out, recorded != nil && *recorded, err
}

// TokenSum is input and output tokens, plus the counts reported apart and how
// many rows didn't report them.
type TokenSum struct {
	In, Out                          int64
	CacheRead, CacheWrite, Reasoning int64
	Unknown                          int64 // rows without cache and reasoning counts
}

// dests are the scan targets for tokenSumColumns, in its order.
func (t *TokenSum) dests() []any {
	return []any{&t.In, &t.Out, &t.CacheRead, &t.CacheWrite, &t.Reasoning, &t.Unknown}
}

// RepoUsage is what one repository used.
type RepoUsage struct {
	Instance    string
	ProjectID   int64
	ProjectPath string
	Tokens      TokenSum
	Reviews     int64
}

// DayUsage is what was used on one day, cut at midnight UTC.
type DayUsage struct {
	Day    time.Time // midnight UTC
	Tokens TokenSum
}

// NamedUsage is what one skill or one model used.
type NamedUsage struct {
	Name   string
	Tokens TokenSum
	Runs   int64
	Failed int64 // runs where this skill failed (skills only)
}

// MRUsage is what one merge request used, with its latest title.
type MRUsage struct {
	IID     int64
	Title   string
	WebURL  string
	Tokens  TokenSum
	Reviews int64
}

// WastedUsage is the usage that bought no finished review.
type WastedUsage struct {
	Failed     TokenSum // tokens of skills that failed
	Reruns     TokenSum // tokens of re-runs, not counted as waste
	RerunCount int64
	NoResult   int64 // reviews that left no result
	Superseded int64 // reviews stopped by a newer push before their result
}

// ReviewCost is the tokens one review used.
type ReviewCost struct {
	RunID       string
	IID         int64
	Incremental bool
	Total       int64
	At          time.Time
}

// CostPerReview is the median review of each kind and the reviews far above
// it. A median is nil without reviews of that kind.
type CostPerReview struct {
	FullMedian, IncrementalMedian *float64
	Outliers                      []ReviewCost
}

// tokenSumColumns are the sums every usage query returns, in the order of
// TokenSum.dests. A sum skips NULL, so a row that didn't report a count adds
// to Unknown and nothing to that sum.
const tokenSumColumns = `COALESCE(sum(tokens_in), 0), COALESCE(sum(tokens_out), 0),
	COALESCE(sum(cache_read), 0), COALESCE(sum(cache_write), 0), COALESCE(sum(reasoning), 0),
	count(*) FILTER (WHERE cache_read IS NULL OR cache_write IS NULL OR reasoning IS NULL)`

// tokenSumColumnsOfUsage is tokenSumColumns for a query that joins runs, so
// the columns say they are run_usage's.
var tokenSumColumnsOfUsage = strings.NewReplacer(
	"tokens_in", "u.tokens_in", "tokens_out", "u.tokens_out", "cache_read", "u.cache_read",
	"cache_write", "u.cache_write", "reasoning", "u.reasoning").Replace(tokenSumColumns)

// outlierFactor and maxOutliers bound the reviews UsageCostPerReview names.
const (
	outlierFactor = 3
	maxOutliers   = 20
)

// UsageByRepository returns what each repository used since the given time,
// busiest first, with the path it was last reported under. Reviews counts a
// run once, however many skills it ran.
func (s *Store) UsageByRepository(ctx context.Context, since time.Time) ([]RepoUsage, error) {
	rows, err := s.pool.Query(ctx, `SELECT instance, project_id, (array_agg(project_path ORDER BY reported_at DESC))[1], `+tokenSumColumns+`,
			count(DISTINCT run_id)
		FROM run_usage WHERE reported_at >= $1::timestamptz
		GROUP BY instance, project_id
		ORDER BY COALESCE(sum(tokens_in), 0) + COALESCE(sum(tokens_out), 0) DESC, instance, project_id`, since)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (RepoUsage, error) {
		var u RepoUsage
		err := row.Scan(append([]any{&u.Instance, &u.ProjectID, &u.ProjectPath}, append(u.Tokens.dests(), &u.Reviews)...)...)
		return u, err
	})
}

// UsageDaily returns what was used on each day since the given time, oldest
// first, for one repository or, with an empty instance, for every one. A day
// without usage has no entry.
func (s *Store) UsageDaily(ctx context.Context, since time.Time, instance string, projectID int64) ([]DayUsage, error) {
	rows, err := s.pool.Query(ctx, `SELECT ((reported_at AT TIME ZONE 'UTC')::date)::timestamp AT TIME ZONE 'UTC' AS day, `+tokenSumColumns+`
		FROM run_usage
		WHERE reported_at >= $1::timestamptz AND ($2::text = '' OR (instance = $2::text AND project_id = $3::bigint))
		GROUP BY day ORDER BY day`, since, instance, projectID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (DayUsage, error) {
		var u DayUsage
		err := row.Scan(append([]any{&u.Day}, u.Tokens.dests()...)...)
		u.Day = u.Day.UTC()
		return u, err
	})
}

// UsageBySkill returns what each skill used in a repository since the given
// time, busiest first, with the runs in which it failed.
func (s *Store) UsageBySkill(ctx context.Context, instance string, projectID int64, since time.Time) ([]NamedUsage, error) {
	return s.namedUsage(ctx, `SELECT skill, `+tokenSumColumns+`, count(*), count(*) FILTER (WHERE verdict = 'failed')
		FROM run_usage WHERE instance = $1::text AND project_id = $2::bigint AND reported_at >= $3::timestamptz
		GROUP BY skill ORDER BY COALESCE(sum(tokens_in), 0) + COALESCE(sum(tokens_out), 0) DESC, skill`,
		instance, projectID, since)
}

// UsageByModel returns what each model used in a repository since the given
// time, busiest first. Runs counts a run once, however many skills it ran.
func (s *Store) UsageByModel(ctx context.Context, instance string, projectID int64, since time.Time) ([]NamedUsage, error) {
	return s.namedUsage(ctx, `SELECT model, `+tokenSumColumns+`, count(DISTINCT run_id), 0::bigint
		FROM run_usage WHERE instance = $1::text AND project_id = $2::bigint AND reported_at >= $3::timestamptz
		GROUP BY model ORDER BY COALESCE(sum(tokens_in), 0) + COALESCE(sum(tokens_out), 0) DESC, model`,
		instance, projectID, since)
}

func (s *Store) namedUsage(ctx context.Context, query string, args ...any) ([]NamedUsage, error) {
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (NamedUsage, error) {
		var u NamedUsage
		err := row.Scan(append([]any{&u.Name}, append(u.Tokens.dests(), &u.Runs, &u.Failed)...)...)
		return u, err
	})
}

// UsageByMR returns what the busiest limit merge requests of a repository
// used since the given time, busiest first, with the title and address of the
// latest run.
func (s *Store) UsageByMR(ctx context.Context, instance string, projectID int64, since time.Time, limit int) ([]MRUsage, error) {
	rows, err := s.pool.Query(ctx, `SELECT u.mr_iid, (array_agg(r.title ORDER BY r.created_at DESC))[1],
			(array_agg(r.web_url ORDER BY r.created_at DESC))[1], `+tokenSumColumnsOfUsage+`, count(DISTINCT u.run_id)
		FROM run_usage u JOIN runs r ON r.id = u.run_id
		WHERE u.instance = $1::text AND u.project_id = $2::bigint AND u.reported_at >= $3::timestamptz
		GROUP BY u.mr_iid
		ORDER BY COALESCE(sum(u.tokens_in), 0) + COALESCE(sum(u.tokens_out), 0) DESC, u.mr_iid
		LIMIT $4::integer`, instance, projectID, since, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (MRUsage, error) {
		var u MRUsage
		err := row.Scan(append([]any{&u.IID, &u.Title, &u.WebURL}, append(u.Tokens.dests(), &u.Reviews)...)...)
		return u, err
	})
}

// UsageWasted returns the usage of a repository that bought no finished
// review since the given time: the tokens of skills that failed, the tokens
// of re-runs asked for (counted apart), and the reviews that left
// no result, whose tokens are unknown.
func (s *Store) UsageWasted(ctx context.Context, instance string, projectID int64, since time.Time) (WastedUsage, error) {
	var w WastedUsage
	err := s.pool.QueryRow(ctx, `SELECT
			COALESCE(sum(tokens_in)  FILTER (WHERE verdict = 'failed'), 0),
			COALESCE(sum(tokens_out) FILTER (WHERE verdict = 'failed'), 0),
			COALESCE(sum(cache_read) FILTER (WHERE verdict = 'failed'), 0),
			COALESCE(sum(cache_write) FILTER (WHERE verdict = 'failed'), 0),
			COALESCE(sum(reasoning)  FILTER (WHERE verdict = 'failed'), 0),
			count(*) FILTER (WHERE verdict = 'failed' AND (cache_read IS NULL OR cache_write IS NULL OR reasoning IS NULL)),
			COALESCE(sum(tokens_in)  FILTER (WHERE trigger IN ('rerun', 'rerun_failed')), 0),
			COALESCE(sum(tokens_out) FILTER (WHERE trigger IN ('rerun', 'rerun_failed')), 0),
			COALESCE(sum(cache_read) FILTER (WHERE trigger IN ('rerun', 'rerun_failed')), 0),
			COALESCE(sum(cache_write) FILTER (WHERE trigger IN ('rerun', 'rerun_failed')), 0),
			COALESCE(sum(reasoning)  FILTER (WHERE trigger IN ('rerun', 'rerun_failed')), 0),
			count(*) FILTER (WHERE trigger IN ('rerun', 'rerun_failed') AND (cache_read IS NULL OR cache_write IS NULL OR reasoning IS NULL)),
			count(DISTINCT run_id) FILTER (WHERE trigger IN ('rerun', 'rerun_failed'))
		FROM run_usage WHERE instance = $1::text AND project_id = $2::bigint AND reported_at >= $3::timestamptz`,
		instance, projectID, since).Scan(append(append(w.Failed.dests(), w.Reruns.dests()...), &w.RerunCount)...)
	if err != nil {
		return WastedUsage{}, err
	}
	// A review with no result has no usage rows, so these two come from runs.
	// The failure reason tells them apart in any state: a review that failed
	// keeps its reason when a later re-run supersedes it, and a run stopped
	// by a newer push has none. A superseded run that kept its result is
	// counted in neither: its skills are in run_usage like any other.
	err = s.pool.QueryRow(ctx, `SELECT
			count(*) FILTER (WHERE result IS NULL AND failure_reason <> '' AND dispatched_at IS NOT NULL),
			count(*) FILTER (WHERE state = 'superseded' AND result IS NULL AND failure_reason = '' AND dispatched_at IS NOT NULL)
		FROM runs WHERE instance = $1::text AND project_id = $2::bigint AND created_at >= $3::timestamptz`,
		instance, projectID, since).Scan(&w.NoResult, &w.Superseded)
	if err != nil {
		return WastedUsage{}, err
	}
	return w, nil
}

// UsageCostPerReview returns the median tokens of a full review and of an
// incremental one in a repository since the given time, and the full or
// incremental reviews that used more than three times their kind's median,
// costliest first. A re-run of only the failed skills covers a subset, so it
// is in neither.
func (s *Store) UsageCostPerReview(ctx context.Context, instance string, projectID int64, since time.Time) (CostPerReview, error) {
	rows, err := s.pool.Query(ctx, `WITH per_run AS (
			SELECT run_id, bool_or(incremental) AS incremental, max(mr_iid) AS iid, max(reported_at) AS at,
				sum(tokens_in + tokens_out)::bigint AS total
			FROM run_usage
			WHERE instance = $1::text AND project_id = $2::bigint AND reported_at >= $3::timestamptz AND trigger <> 'rerun_failed'
			GROUP BY run_id
		), medians AS (
			SELECT incremental, percentile_cont(0.5) WITHIN GROUP (ORDER BY total) AS median FROM per_run GROUP BY incremental
		)
		SELECT p.run_id::text, p.iid, p.incremental, p.total, p.at, m.median
		FROM per_run p JOIN medians m USING (incremental)
		ORDER BY p.total DESC, p.run_id::text`, instance, projectID, since)
	if err != nil {
		return CostPerReview{}, err
	}
	defer rows.Close()
	var c CostPerReview
	for rows.Next() {
		var r ReviewCost
		var median float64
		if err := rows.Scan(&r.RunID, &r.IID, &r.Incremental, &r.Total, &r.At, &median); err != nil {
			return CostPerReview{}, err
		}
		m := median
		if r.Incremental {
			c.IncrementalMedian = &m
		} else {
			c.FullMedian = &m
		}
		if float64(r.Total) > outlierFactor*median && len(c.Outliers) < maxOutliers {
			c.Outliers = append(c.Outliers, r)
		}
	}
	if err := rows.Err(); err != nil {
		return CostPerReview{}, err
	}
	return c, nil
}

// UsageRow is one run and skill, as the CSV export lists it. It holds no
// title and no text a person wrote. CacheRead, CacheWrite and Reasoning are
// nil when the Job didn't report them.
type UsageRow struct {
	ReportedAt                       time.Time
	Instance, Project                string
	MR                               int64
	Trigger                          string
	Incremental                      bool
	Model, Skill, Verdict            string
	In, Out                          int64
	CacheRead, CacheWrite, Reasoning *int64
	Findings, Blocking               int
}

// UsageProjectID returns the platform ID of the repository at project (matched
// ignoring case) on instance, as its latest usage row has it; ok is false when
// the repository has none. The projects table forgets a repository that is
// switched off, its usage stays.
func (s *Store) UsageProjectID(ctx context.Context, instance, project string) (int64, bool, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `SELECT project_id FROM run_usage
		WHERE instance = $1::text AND lower(project_path) = lower($2::text)
		ORDER BY reported_at DESC LIMIT 1`, instance, project).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// UsageRows calls each with the usage rows since the given time, oldest
// first, at most limit of them, for one repository or, with an empty
// instance, for every one. It hands them over as the database sends them
// instead of collecting them, so an export of 90 days is never in memory at
// once. An error from each stops it and is returned.
func (s *Store) UsageRows(ctx context.Context, since time.Time, instance string, projectID int64, limit int, each func(UsageRow) error) error {
	rows, err := s.pool.Query(ctx, `SELECT reported_at, instance, project_path, mr_iid, trigger, incremental, model, skill, verdict,
			tokens_in, tokens_out, cache_read, cache_write, reasoning, findings, blocking
		FROM run_usage
		WHERE reported_at >= $1::timestamptz AND ($2::text = '' OR (instance = $2::text AND project_id = $3::bigint))
		ORDER BY reported_at, run_id, skill
		LIMIT $4::integer`, since, instance, projectID, limit)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var u UsageRow
		if err := rows.Scan(&u.ReportedAt, &u.Instance, &u.Project, &u.MR, &u.Trigger, &u.Incremental, &u.Model, &u.Skill,
			&u.Verdict, &u.In, &u.Out, &u.CacheRead, &u.CacheWrite, &u.Reasoning, &u.Findings, &u.Blocking); err != nil {
			return err
		}
		u.ReportedAt = u.ReportedAt.UTC()
		if err := each(u); err != nil {
			return err
		}
	}
	return rows.Err()
}

// SkillTokens is what one skill used on the changes the Skill results tab
// counts.
type SkillTokens struct {
	Skill   string
	In, Out int64
}

// UsageOnDecidedChanges returns, per skill, the tokens used on the changes
// the Skill results tab counted in the window from since: every change merged
// in it that a person opened (a bot-opened one is known only from its
// findings, so one with none is counted whoever opened it), and the changes
// closed in it that hold a dismissal. A change with no findings is counted
// when it was merged. The tokens are all those the change's reviews used, not
// only those used in the window.
func (s *Store) UsageOnDecidedChanges(ctx context.Context, instance string, projectID int64, since time.Time) ([]SkillTokens, error) {
	rows, err := s.pool.Query(ctx, `WITH decided AS (
			SELECT j.mr_iid FROM fate_jobs j
			WHERE j.instance = $1::text AND j.project_id = $2::bigint AND j.finished_at >= $3::timestamptz AND j.merged
			  AND NOT EXISTS (SELECT 1 FROM finding_fates f WHERE f.instance = j.instance AND f.project_id = j.project_id
			                  AND f.mr_iid = j.mr_iid AND f.by_bot)
			UNION
			SELECT mr_iid FROM finding_fates WHERE `+countedFates+`
		)
		SELECT u.skill, COALESCE(sum(u.tokens_in), 0), COALESCE(sum(u.tokens_out), 0)
		FROM run_usage u JOIN decided d ON d.mr_iid = u.mr_iid
		WHERE u.instance = $1::text AND u.project_id = $2::bigint
		GROUP BY u.skill ORDER BY u.skill`, instance, projectID, since)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (SkillTokens, error) {
		var t SkillTokens
		err := row.Scan(&t.Skill, &t.In, &t.Out)
		return t, err
	})
}
