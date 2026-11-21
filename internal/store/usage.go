package store

import (
	"context"
	"errors"
	"slices"
	"sort"
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
// repository, merge request, trigger, provider, model and time, and whose key
// paid (the run's entry: global, or a team and its ID), and marks the run as
// recorded, even with no skills. A row already there is kept.
//
// For a run with model rows (models not nil: a review panel's merge, Job P,
// even when its result has no per-model tokens) it is recordMergeUsage
// instead. Without model rows it is today's statement.
func (s *Store) RecordUsage(ctx context.Context, id string, skills []SkillUsage, models []ModelUsage) error {
	if models != nil {
		return s.recordMergeUsage(ctx, id, skills, models)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	for _, u := range skills {
		if _, err := tx.Exec(ctx, `INSERT INTO run_usage (run_id, skill, instance, project_id, project_path, mr_iid,
				trigger, incremental, provider, model, verdict, reported_at, tokens_in, tokens_out, cache_read, cache_write,
				reasoning, findings, blocking, source, team_id)
			SELECT r.id, $2::text, r.instance, r.project_id, r.project_path, r.mr_iid, r.trigger, r.incremental,
				COALESCE(r.provider, ''), COALESCE(r.model, ''), $3::text, COALESCE(r.reported_at, r.created_at), $4::bigint, $5::bigint,
				$6::bigint, $7::bigint, $8::bigint, $9::integer, $10::integer,
				CASE WHEN r.model_source = 'team' THEN 'team' ELSE 'global' END,
				CASE WHEN r.model_source = 'team' THEN r.model_team_id END
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

// usageInsertSQL inserts what skill $2 of run $1 used into run_usage, with
// the run's repository, merge request, trigger, provider, model and whose key
// paid. The verdict, the time and the counts are $3, $4 and $10, $11; the
// tokens $5 to $9. On a conflict the tokens are added; a count one side
// reported is kept when the other side's is unknown, and stays unknown only
// when both are. What else a conflict does follows it.
//
// A panel's members and its Job P add to the row of the run they count on:
// its source is that run's entry, not each model's own (run_usage_models
// keeps those). So the instance quota, which counts global keys only, counts
// a team-key member of a review on a global entry, and misses a global-key
// member of a review on a team entry. The quota rules are not changed here.
const usageInsertSQL = `INSERT INTO run_usage (run_id, skill, instance, project_id, project_path, mr_iid,
		trigger, incremental, provider, model, verdict, reported_at, tokens_in, tokens_out, cache_read, cache_write,
		reasoning, findings, blocking, source, team_id)
	SELECT r.id, $2::text, r.instance, r.project_id, r.project_path, r.mr_iid, r.trigger, r.incremental,
		COALESCE(r.provider, ''), COALESCE(r.model, ''), $3::text, $4::timestamptz, $5::bigint, $6::bigint,
		$7::bigint, $8::bigint, $9::bigint, $10::integer, $11::integer,
		CASE WHEN r.model_source = 'team' THEN 'team' ELSE 'global' END,
		CASE WHEN r.model_source = 'team' THEN r.model_team_id END
	FROM runs r WHERE r.id = $1::uuid
	ON CONFLICT (run_id, skill) DO UPDATE SET tokens_in = run_usage.tokens_in + EXCLUDED.tokens_in,
		tokens_out = run_usage.tokens_out + EXCLUDED.tokens_out,
		cache_read = COALESCE(run_usage.cache_read + EXCLUDED.cache_read, run_usage.cache_read, EXCLUDED.cache_read),
		cache_write = COALESCE(run_usage.cache_write + EXCLUDED.cache_write, run_usage.cache_write, EXCLUDED.cache_write),
		reasoning = COALESCE(run_usage.reasoning + EXCLUDED.reasoning, run_usage.reasoning, EXCLUDED.reasoning)`

// recordMergeUsage is RecordUsage of Job P's run id: each skill's tokens are
// added to the run's row (the members made it when P runs on the run that
// was split), with P's verdict, counts and time. models are stored as merge rows, each
// with the judge entry of its role (panel_judges). The run's usage_recorded,
// read under the row's lock in the same transaction, stops a second count.
func (s *Store) recordMergeUsage(ctx context.Context, id string, skills []SkillUsage, models []ModelUsage) error {
	if !validUUID(id) {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	var (
		recorded bool
		at       time.Time
	)
	err = tx.QueryRow(ctx, `SELECT usage_recorded, COALESCE(reported_at, created_at) FROM runs
		WHERE id = $1::uuid FOR NO KEY UPDATE`, id).Scan(&recorded, &at)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && recorded) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, u := range byName(skills) {
		if _, err := tx.Exec(ctx, usageInsertSQL+`, verdict = EXCLUDED.verdict, findings = EXCLUDED.findings,
				blocking = EXCLUDED.blocking, reported_at = EXCLUDED.reported_at`,
			id, u.Skill, u.Verdict, at, u.In, u.Out, u.CacheRead, u.CacheWrite, u.Reasoning, u.Findings, u.Blocking); err != nil {
			return err
		}
	}
	for _, m := range models {
		if _, err := tx.Exec(ctx, `INSERT INTO run_usage_models (run_id, part, role, skill, provider, model, entry_name,
				source, team_id, reported_at, tokens_in, tokens_out, cache_read, cache_write, reasoning)
			SELECT r.id, 'merge', $2::text, $3::text, COALESCE(NULLIF($4::text, ''), j.provider, ''),
				COALESCE(NULLIF($5::text, ''), j.model, ''), COALESCE(j.model_entry_name, ''),
				CASE WHEN j.model_source = 'team' THEN 'team' ELSE 'global' END,
				CASE WHEN j.model_source = 'team' THEN j.model_team_id END,
				$6::timestamptz, $7::bigint, $8::bigint, $9::bigint, $10::bigint, $11::bigint
			FROM runs r LEFT JOIN panel_judges j ON j.run_id = r.id AND j.role = $2::text
			WHERE r.id = $1::uuid
			ON CONFLICT (run_id, skill, role) WHERE part = 'merge' DO NOTHING`,
			id, m.Role, m.Skill, cutText(m.Provider), cutText(m.Model), at, m.In, m.Out, m.CacheRead, m.CacheWrite, m.Reasoning); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE runs SET usage_recorded = true WHERE id = $1::uuid`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// byName is skills sorted by name, on a copy: the panel's writes to one
// run's rows take them in one order, so two of them never wait for each other.
func byName(skills []SkillUsage) []SkillUsage {
	out := slices.Clone(skills)
	slices.SortStableFunc(out, func(a, b SkillUsage) int { return strings.Compare(a.Skill, b.Skill) })
	return out
}

// RecordMemberUsage stores what each skill of member id used: one
// run_usage_models row per skill (part member, the member's entry, provider,
// model, source and team), and the tokens added to run_usage of the run that
// was split (memberUsageRunSQL), then marks the member recorded, in one
// transaction; a member already recorded changes nothing. A row the member
// makes has no verdict and no counts: Job P's record sets them. A member
// that failed records the tokens it reported all the same. An unknown or off
// member is no error and writes nothing.
func (s *Store) RecordMemberUsage(ctx context.Context, id string, skills []SkillUsage) error {
	if !validUUID(id) {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	var target string
	err = tx.QueryRow(ctx, memberUsageRunSQL, id).Scan(&target)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// The run's row before the member's, the panel's lock order: the rows
	// added below hold the run's key, and a run is only ever deleted with
	// its row locked first.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM runs WHERE id = $1::uuid FOR KEY SHARE`, target); err != nil {
		return err
	}
	var (
		recorded bool
		at       time.Time
	)
	err = tx.QueryRow(ctx, `SELECT m.usage_recorded, COALESCE(r.reported_at, m.reported_at, r.created_at)
		FROM panel_members m, runs r
		WHERE m.id = $1::uuid AND r.id = $2::uuid FOR UPDATE OF m`, id, target).Scan(&recorded, &at)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && recorded) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, u := range byName(skills) {
		if _, err := tx.Exec(ctx, usageInsertSQL, target, u.Skill, "", at, u.In, u.Out, u.CacheRead, u.CacheWrite, u.Reasoning, 0, 0); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO run_usage_models (run_id, member_id, part, role, skill, provider, model,
				entry_name, source, team_id, reported_at, tokens_in, tokens_out, cache_read, cache_write, reasoning)
			SELECT $9::uuid, m.id, 'member', m.role, $2::text, m.provider, m.model, m.model_entry_name,
				CASE WHEN m.model_source = 'team' THEN 'team' ELSE 'global' END,
				CASE WHEN m.model_source = 'team' THEN m.model_team_id END,
				COALESCE(m.reported_at, $3::timestamptz), $4::bigint, $5::bigint, $6::bigint, $7::bigint, $8::bigint
			FROM panel_members m WHERE m.id = $1::uuid
			ON CONFLICT (member_id, skill) WHERE part = 'member' DO NOTHING`,
			id, u.Skill, at, u.In, u.Out, u.CacheRead, u.CacheWrite, u.Reasoning, target); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE panel_members SET usage_recorded = true WHERE id = $1::uuid`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// memberUsageRunSQL is the run member $1's tokens count on: the run that was
// split. That is the run waiting for the members while they run; after the
// release (the fill at start) it is the newest run of the review created
// before the role's first attempt, as a review can fall back on chain A
// before its split, and the split run is then a later attempt, not the
// review's first run. Job P's later attempts are created after the members,
// so they are never it. An off member has none.
const memberUsageRunSQL = `SELECT x.id::text FROM panel_members m
	JOIN panel_members f ON f.id = COALESCE(m.attempt_of, m.id)
	JOIN runs x ON x.id = m.run_id OR x.attempt_of = m.run_id
	WHERE m.id = $1::uuid AND m.state <> 'off'
	  AND ((x.state = 'waiting' AND x.skip_reason = 'panel_members') OR x.created_at < f.created_at)
	ORDER BY (x.state = 'waiting' AND x.skip_reason = 'panel_members') DESC, x.created_at DESC, x.id DESC
	LIMIT 1`

// MembersWithoutUsage returns up to limit panel members with a stored
// result and no usage recorded, oldest first: the fill at start records
// those whose write was lost.
func (s *Store) MembersWithoutUsage(ctx context.Context, limit int) ([]PanelMember, error) {
	if limit <= 0 {
		return nil, nil
	}
	return s.queryMembers(ctx, `SELECT `+memberColumns+` FROM panel_members
		WHERE NOT usage_recorded AND result IS NOT NULL AND state <> 'off'
		ORDER BY created_at, id LIMIT $1`, limit)
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

// ModelCost is what one model used in a panel review: the sum of its
// run_usage_models rows of one part ("member" or "merge"), role ("a" or
// "b"), entry, and the run (RunID) or member attempt (MemberID, "" for a
// merge row) they count on.
type ModelCost struct {
	Part, Role, RunID, MemberID, Entry, Provider, Model string
	In, Out                                             int64
}

// ReviewModelCosts returns what each model used in the panel review of run
// id (its first run or a later attempt): every member attempt's rows (they
// count on the run that was split) and the merge rows of every run of the
// review (each Job P attempt). Members first, by role and attempt, then the
// merges by run and role. A review with none, or a malformed id, has none.
func (s *Store) ReviewModelCosts(ctx context.Context, id string) ([]ModelCost, error) {
	if !validUUID(id) {
		return nil, nil
	}
	// Each part by its own index: members by their id, merges by run.
	rows, err := s.pool.Query(ctx, `WITH review AS (SELECT COALESCE(attempt_of, id) AS root FROM runs WHERE id = $1::uuid)
		SELECT part, role, run_id::text, COALESCE(member_id::text, ''), entry_name, provider, model,
			sum(tokens_in)::bigint, sum(tokens_out)::bigint
		FROM (SELECT u.part, u.role, u.run_id, u.member_id, u.entry_name, u.provider, u.model, u.tokens_in, u.tokens_out,
				m.created_at AS at
			FROM run_usage_models u JOIN panel_members m ON m.id = u.member_id
			WHERE u.part = 'member' AND m.run_id = (SELECT root FROM review)
			UNION ALL
			SELECT u.part, u.role, u.run_id, u.member_id, u.entry_name, u.provider, u.model, u.tokens_in, u.tokens_out,
				r.created_at AS at
			FROM run_usage_models u JOIN runs r ON r.id = u.run_id
			WHERE u.part = 'merge' AND (r.id = (SELECT root FROM review) OR r.attempt_of = (SELECT root FROM review))) x
		GROUP BY part, role, run_id, member_id, entry_name, provider, model
		ORDER BY part = 'merge', CASE WHEN part = 'member' THEN role END, min(at), run_id, role, entry_name, provider, model`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ModelCost
	for rows.Next() {
		var c ModelCost
		if err := rows.Scan(&c.Part, &c.Role, &c.RunID, &c.MemberID, &c.Entry, &c.Provider, &c.Model, &c.In, &c.Out); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// TokenSum is input and output tokens, plus the counts reported apart and how
// many rows didn't report them.
type TokenSum struct {
	In, Out                          int64
	CacheRead, CacheWrite, Reasoning int64
	Unknown                          int64 // rows without cache and reasoning counts
}

// Add adds b to t.
func (t *TokenSum) Add(b TokenSum) {
	t.In += b.In
	t.Out += b.Out
	t.CacheRead += b.CacheRead
	t.CacheWrite += b.CacheWrite
	t.Reasoning += b.Reasoning
	t.Unknown += b.Unknown
}

// dests are the scan targets for tokenSumColumns, in its order.
func (t *TokenSum) dests() []any {
	return []any{&t.In, &t.Out, &t.CacheRead, &t.CacheWrite, &t.Reasoning, &t.Unknown}
}

// RepoUsage is what one repository used.
// GlobalTokens and TeamTokens are its tokens (in plus out) on global keys
// and on teams' own.
type RepoUsage struct {
	Instance                 string
	ProjectID                int64
	ProjectPath              string
	Tokens                   TokenSum
	Reviews                  int64
	GlobalTokens, TeamTokens int64
}

// DayUsage is what was used on one day, cut at midnight UTC.
type DayUsage struct {
	Day    time.Time // midnight UTC
	Tokens TokenSum
}

// NamedUsage is what one skill or one model used. Provider is the model's
// provider, empty for a skill and for a model recorded before providers were.
type NamedUsage struct {
	Provider string
	Name     string
	Tokens   TokenSum
	Runs     int64
	Failed   int64 // runs where this skill failed (skills only)
}

// ModelUsed is a model a review ran on, with its provider ("" when the review
// was recorded before providers were).
type ModelUsed struct {
	Provider, Model string
}

// MRUsage is what one merge request used, with its latest title and the
// models its reviews ran on.
type MRUsage struct {
	IID     int64
	Title   string
	WebURL  string
	Tokens  TokenSum
	Reviews int64
	Models  []ModelUsed // by provider, then model; a review with no recorded model adds none
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

// sourceTokensColumns are the tokens (in plus out) on global keys and on
// teams' own, of run_usage rows.
const sourceTokensColumns = `COALESCE(sum(COALESCE(tokens_in, 0) + COALESCE(tokens_out, 0)) FILTER (WHERE source = 'global'), 0),
			COALESCE(sum(COALESCE(tokens_in, 0) + COALESCE(tokens_out, 0)) FILTER (WHERE source = 'team'), 0)`

// UsageBySource returns the tokens (in plus out) one repository used since
// the given time on global keys and on teams' own.
func (s *Store) UsageBySource(ctx context.Context, instance string, projectID int64, since time.Time) (global, team int64, err error) {
	err = s.pool.QueryRow(ctx, `SELECT `+sourceTokensColumns+` FROM run_usage
		WHERE instance = $1::text AND project_id = $2::bigint AND reported_at >= $3::timestamptz`,
		instance, projectID, since).Scan(&global, &team)
	return global, team, err
}

// UsageByRepository returns what each repository used since the given time,
// busiest first, with the path it was last reported under. Reviews counts a
// run once, however many skills it ran. An API review has no repository (its
// instance is empty) and is left out.
func (s *Store) UsageByRepository(ctx context.Context, since time.Time) ([]RepoUsage, error) {
	rows, err := s.pool.Query(ctx, `SELECT instance, project_id, (array_agg(project_path ORDER BY reported_at DESC))[1], `+tokenSumColumns+`,
			count(DISTINCT run_id), `+sourceTokensColumns+`
		FROM run_usage WHERE reported_at >= $1::timestamptz AND instance <> ''
		GROUP BY instance, project_id
		ORDER BY COALESCE(sum(tokens_in), 0) + COALESCE(sum(tokens_out), 0) DESC, instance, project_id`, since)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (RepoUsage, error) {
		var u RepoUsage
		err := row.Scan(append([]any{&u.Instance, &u.ProjectID, &u.ProjectPath}, append(u.Tokens.dests(), &u.Reviews, &u.GlobalTokens, &u.TeamTokens)...)...)
		return u, err
	})
}

// TenantUsage is what the reviews of one tenant of an API client used. Tenant
// is "" for reviews asked for with no tenant.
type TenantUsage struct {
	Tenant  string
	Tokens  TokenSum
	Reviews int64
}

// ClientUsage is what one API client's reviews used, with its tenants, busiest
// first.
type ClientUsage struct {
	Name    string
	Tokens  TokenSum
	Reviews int64
	Tenants []TenantUsage
}

// UsageByAPIClient returns what each API client's reviews used since the given
// time, busiest first, with its tenants busiest first. It counts as
// UsageByRepository does, by when the usage was reported, so the page's totals
// are its daily chart's: a client with no usage in the period is not listed,
// and Reviews counts the reviews that left usage.
func (s *Store) UsageByAPIClient(ctx context.Context, since time.Time) ([]ClientUsage, error) {
	rows, err := s.pool.Query(ctx, `SELECT c.name, ar.tenant, `+tokenSumColumnsOfUsage+`, count(DISTINCT u.run_id)
		FROM run_usage u JOIN api_reviews ar ON ar.run_id = u.run_id JOIN api_clients c ON c.id = ar.client_id
		WHERE u.reported_at >= $1::timestamptz
		GROUP BY c.name, ar.tenant
		ORDER BY COALESCE(sum(u.tokens_in), 0) + COALESCE(sum(u.tokens_out), 0) DESC, c.name, ar.tenant`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ClientUsage
	at := map[string]int{}
	for rows.Next() {
		var (
			name string
			t    TenantUsage
		)
		if err := rows.Scan(append([]any{&name, &t.Tenant}, append(t.Tokens.dests(), &t.Reviews)...)...); err != nil {
			return nil, err
		}
		i, ok := at[name]
		if !ok {
			i = len(out)
			at[name] = i
			out = append(out, ClientUsage{Name: name})
		}
		c := &out[i]
		c.Tenants = append(c.Tenants, t)
		c.Tokens.Add(t.Tokens)
		// A run has one tenant, so the counts add up.
		c.Reviews += t.Reviews
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	total := func(c ClientUsage) int64 { return c.Tokens.In + c.Tokens.Out }
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := total(out[i]), total(out[j]); a != b {
			return a > b
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
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
	return s.namedUsage(ctx, `SELECT ''::text, skill, `+tokenSumColumns+`, count(*), count(*) FILTER (WHERE verdict = 'failed')
		FROM run_usage WHERE instance = $1::text AND project_id = $2::bigint AND reported_at >= $3::timestamptz
		GROUP BY skill ORDER BY COALESCE(sum(tokens_in), 0) + COALESCE(sum(tokens_out), 0) DESC, skill`,
		instance, projectID, since)
}

// UsageByModel returns what each model used in a repository since the given
// time, busiest first; one provider's model is apart from another's of the
// same name. Runs counts a run once, however many skills it ran.
func (s *Store) UsageByModel(ctx context.Context, instance string, projectID int64, since time.Time) ([]NamedUsage, error) {
	return s.namedUsage(ctx, `SELECT provider, model, `+tokenSumColumns+`, count(DISTINCT run_id), 0::bigint
		FROM run_usage WHERE instance = $1::text AND project_id = $2::bigint AND reported_at >= $3::timestamptz
		GROUP BY provider, model ORDER BY COALESCE(sum(tokens_in), 0) + COALESCE(sum(tokens_out), 0) DESC, model, provider`,
		instance, projectID, since)
}

func (s *Store) namedUsage(ctx context.Context, query string, args ...any) ([]NamedUsage, error) {
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (NamedUsage, error) {
		var u NamedUsage
		err := row.Scan(append([]any{&u.Provider, &u.Name}, append(u.Tokens.dests(), &u.Runs, &u.Failed)...)...)
		return u, err
	})
}

// UsageByMR returns what the busiest limit merge requests of a repository
// used since the given time, busiest first, with the title and address of the
// latest run and the models its reviews ran on.
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
	mrs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (MRUsage, error) {
		var u MRUsage
		err := row.Scan(append([]any{&u.IID, &u.Title, &u.WebURL}, append(u.Tokens.dests(), &u.Reviews)...)...)
		return u, err
	})
	if err != nil || len(mrs) == 0 {
		return mrs, err
	}

	// The models come from a second query over the same window, so each is
	// listed once however many skills and reviews used it.
	iids := make([]int64, len(mrs))
	at := make(map[int64]int, len(mrs))
	for i, m := range mrs {
		iids[i], at[m.IID] = m.IID, i
	}
	rows, err = s.pool.Query(ctx, `SELECT mr_iid, provider, model FROM run_usage
		WHERE instance = $1::text AND project_id = $2::bigint AND reported_at >= $3::timestamptz
			AND mr_iid = ANY($4::bigint[]) AND model <> ''
		GROUP BY mr_iid, provider, model ORDER BY mr_iid, provider, model`, instance, projectID, since, iids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var iid int64
		var m ModelUsed
		if err := rows.Scan(&iid, &m.Provider, &m.Model); err != nil {
			return nil, err
		}
		mrs[at[iid]].Models = append(mrs[at[iid]].Models, m)
	}
	return mrs, rows.Err()
}

// UsageWasted returns the usage of a repository that bought no finished
// review since the given time: the tokens of skills that failed, the tokens
// of re-runs asked for (counted apart), and the reviews that left
// no result, whose tokens are unknown.
func (s *Store) UsageWasted(ctx context.Context, instance string, projectID int64, since time.Time) (WastedUsage, error) {
	var w WastedUsage
	// A failed skill, or a panel's member tokens when its merge left no
	// result (panelNoResult): both are wasted.
	failed := `(verdict = 'failed' OR ` + panelNoResult + `)`
	err := s.pool.QueryRow(ctx, `SELECT
			COALESCE(sum(tokens_in)  FILTER (WHERE `+failed+`), 0),
			COALESCE(sum(tokens_out) FILTER (WHERE `+failed+`), 0),
			COALESCE(sum(cache_read) FILTER (WHERE `+failed+`), 0),
			COALESCE(sum(cache_write) FILTER (WHERE `+failed+`), 0),
			COALESCE(sum(reasoning)  FILTER (WHERE `+failed+`), 0),
			count(*) FILTER (WHERE `+failed+` AND (cache_read IS NULL OR cache_write IS NULL OR reasoning IS NULL)),
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
	// A review with no result has no usage rows of its own (a panel's has
	// its members', counted as wasted above, never as a review), so these
	// two come from runs. The failure reason tells them apart in any state: a review that failed
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
// nil when the Job didn't report them. Source is whose key paid: SourceGlobal
// or SourceTeam.
type UsageRow struct {
	ReportedAt                       time.Time
	Instance, Project                string
	MR                               int64
	Trigger                          string
	Incremental                      bool
	Provider, Model                  string
	Skill, Verdict                   string
	In, Out                          int64
	CacheRead, CacheWrite, Reasoning *int64
	Findings, Blocking               int
	Source                           string
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
// once. An error from each stops it and is returned. An API review has no
// repository and is left out.
func (s *Store) UsageRows(ctx context.Context, since time.Time, instance string, projectID int64, limit int, each func(UsageRow) error) error {
	rows, err := s.pool.Query(ctx, `SELECT reported_at, instance, project_path, mr_iid, trigger, incremental, model, skill, verdict,
			tokens_in, tokens_out, cache_read, cache_write, reasoning, findings, blocking, provider, source
		FROM run_usage
		WHERE reported_at >= $1::timestamptz AND instance <> '' AND ($2::text = '' OR (instance = $2::text AND project_id = $3::bigint))
		ORDER BY reported_at, run_id, skill
		LIMIT $4::integer`, since, instance, projectID, limit)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var u UsageRow
		if err := rows.Scan(&u.ReportedAt, &u.Instance, &u.Project, &u.MR, &u.Trigger, &u.Incremental, &u.Model, &u.Skill,
			&u.Verdict, &u.In, &u.Out, &u.CacheRead, &u.CacheWrite, &u.Reasoning, &u.Findings, &u.Blocking, &u.Provider, &u.Source); err != nil {
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

// MetricsTotals are the counts the metrics are made of, over every day kept.
// Each row is one repository (as the path it was reported under) and the
// other values it is counted by; mapping them to the labels metrics may carry
// is the caller's work, and the caller adds rows that end up with the same
// labels.
type MetricsTotals struct {
	Tokens     []TokenTotal
	Reviews    []ReviewTotal
	NoResult   []NoResultTotal
	Unfinished []UnfinishedTotal
	SkillRuns  []SkillRunTotal
	Findings   []FindingTotal
	Dismissals []DismissalTotal
	// The API clients' counts are by client, never by tenant: a tenant is the
	// caller's own customer.
	APIReviews   []APIReviewTotal
	APITokens    []APITokenTotal
	APICallbacks []APICallbackTotal
	// Fallbacks are the attempts dropped for the next entry of a chain;
	// Exhausted the entries with a live mark.
	Fallbacks []FallbackTotal
	Exhausted []ExhaustedEntry
}

// FallbackTotal is the attempts dropped from entry From (by name, of Source)
// for the next attempt's entry To ("none" when that attempt has none yet),
// for one class. A dropped attempt is in no other count but the tokens.
type FallbackTotal struct {
	From, To, Class, Source string
	Count                   int64
}

// ExhaustedEntry is an entry with a live mark: its ID, its name (its ID when
// it has no row: the config entries, or one removed), source and class.
type ExhaustedEntry struct {
	ID, Name, Source, Class string
}

// TokenTotal is what one skill used with one model in one repository.
type TokenTotal struct {
	Instance, Project, Skill, Model string
	Tokens                          TokenSum
	Rows                            int64 // run and skill rows counted; Tokens.Unknown of them did not report cache and reasoning
}

// ReviewTotal is the reviews with a result in one repository, by trigger and
// kind. A review is counted once, however many skills it ran.
type ReviewTotal struct {
	Instance, Project, Trigger string
	Incremental                bool
	Reviews                    int64
}

// NoResultTotal is the reviews of one repository that failed before leaving a
// result, by the step they failed at ("" for a failure before any Job).
type NoResultTotal struct {
	Instance, Project, Step string
	Reviews                 int64
}

// UnfinishedTotal is the reviews of one repository that a newer push stopped
// before they left a result.
type UnfinishedTotal struct {
	Instance, Project string
	Superseded        int64
}

// SkillRunTotal is the runs of one skill in one repository that ended in one
// verdict.
type SkillRunTotal struct {
	Instance, Project, Skill, Verdict string
	Runs                              int64
}

// FindingTotal is the findings one skill reported in one repository.
type FindingTotal struct {
	Instance, Project, Skill string
	Blocking, Suggestions    int64
}

// DismissalTotal is the findings dismissed in one repository, by the skill
// that reported them. Project is empty when the projects table has no row for
// the repository.
type DismissalTotal struct {
	Instance, Project, Skill string
	Dismissals               int64
}

// APIReviewTotal is the finished API reviews of one client that ended in one
// result: a stored verdict (Outcome* of runs.outcome) or "superseded". A
// review still queued or running is not counted yet.
type APIReviewTotal struct {
	Client, Result string
	Reviews        int64
}

// APITokenTotal is what the reviews of one API client used. Rows is the run
// and skill rows counted; Tokens.Unknown of them did not report cache and
// reasoning.
type APITokenTotal struct {
	Client string
	Tokens TokenSum
	Rows   int64
}

// The outcomes of an API client's callbacks (APICallbackTotal).
const (
	CallbackOutcomeDelivered     = "delivered"
	CallbackOutcomeFailedAttempt = "failed_attempt"
	CallbackOutcomeGaveUp        = "gave_up"
)

// APICallbackTotal is the callbacks of one API client by outcome. Delivered
// and gave_up count callbacks; failed_attempt counts tries, so a callback
// that failed twice and then went through adds 2 and 1.
type APICallbackTotal struct {
	Client, Outcome string
	Count           int64
}

// MetricsTotals returns the counts for the metrics, one grouped query per
// kind. An API review has no repository and is left out of all but the last
// three, which are the API clients'. A review with no result and one stopped
// by a newer push are told apart as UsageWasted does, so the page and the
// metrics agree. Dismissals come from the dismissals table, which loses a
// repository's rows when the repository is switched off.
func (s *Store) MetricsTotals(ctx context.Context) (MetricsTotals, error) {
	var t MetricsTotals
	rows, err := s.pool.Query(ctx, `SELECT instance, project_path, skill, model, `+tokenSumColumns+`, count(*)
		FROM run_usage WHERE instance <> '' GROUP BY instance, project_path, skill, model`)
	if err != nil {
		return MetricsTotals{}, err
	}
	if t.Tokens, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (TokenTotal, error) {
		var u TokenTotal
		err := row.Scan(append([]any{&u.Instance, &u.Project, &u.Skill, &u.Model}, append(u.Tokens.dests(), &u.Rows)...)...)
		return u, err
	}); err != nil {
		return MetricsTotals{}, err
	}

	// A dropped attempt (fell_back) is not a review: the next attempt is. Its
	// tokens were spent, so Tokens keeps them.
	if rows, err = s.pool.Query(ctx, `SELECT instance, project_path, trigger, incremental, count(DISTINCT run_id)
		FROM run_usage WHERE instance <> '' AND `+notFellBack+` AND NOT `+panelNoResult+`
		GROUP BY instance, project_path, trigger, incremental`); err != nil {
		return MetricsTotals{}, err
	}
	if t.Reviews, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (ReviewTotal, error) {
		var u ReviewTotal
		err := row.Scan(&u.Instance, &u.Project, &u.Trigger, &u.Incremental, &u.Reviews)
		return u, err
	}); err != nil {
		return MetricsTotals{}, err
	}

	// Reviews with no result have no usage rows of their own (a panel's
	// members' rows are left out of the review and skill counts), so these
	// two come from runs, told apart as UsageWasted does.
	if rows, err = s.pool.Query(ctx, `SELECT instance, project_path, failure_step, count(*)
		FROM runs WHERE instance <> '' AND result IS NULL AND failure_reason <> '' AND dispatched_at IS NOT NULL
		  AND outcome <> 'fell_back'
		GROUP BY instance, project_path, failure_step`); err != nil {
		return MetricsTotals{}, err
	}
	if t.NoResult, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (NoResultTotal, error) {
		var u NoResultTotal
		err := row.Scan(&u.Instance, &u.Project, &u.Step, &u.Reviews)
		return u, err
	}); err != nil {
		return MetricsTotals{}, err
	}

	if rows, err = s.pool.Query(ctx, `SELECT instance, project_path, count(*)
		FROM runs WHERE instance <> '' AND state = 'superseded' AND result IS NULL AND failure_reason = '' AND dispatched_at IS NOT NULL
		GROUP BY instance, project_path`); err != nil {
		return MetricsTotals{}, err
	}
	if t.Unfinished, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (UnfinishedTotal, error) {
		var u UnfinishedTotal
		err := row.Scan(&u.Instance, &u.Project, &u.Superseded)
		return u, err
	}); err != nil {
		return MetricsTotals{}, err
	}

	if rows, err = s.pool.Query(ctx, `SELECT instance, project_path, skill, verdict, count(*)
		FROM run_usage WHERE instance <> '' AND `+notFellBack+` AND NOT `+panelNoResult+`
		GROUP BY instance, project_path, skill, verdict`); err != nil {
		return MetricsTotals{}, err
	}
	if t.SkillRuns, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (SkillRunTotal, error) {
		var u SkillRunTotal
		err := row.Scan(&u.Instance, &u.Project, &u.Skill, &u.Verdict, &u.Runs)
		return u, err
	}); err != nil {
		return MetricsTotals{}, err
	}

	// findings counts every finding of the skill, blocking ones included.
	if rows, err = s.pool.Query(ctx, `SELECT instance, project_path, skill,
			COALESCE(sum(blocking), 0), COALESCE(sum(findings - blocking), 0)
		FROM run_usage WHERE instance <> '' GROUP BY instance, project_path, skill`); err != nil {
		return MetricsTotals{}, err
	}
	if t.Findings, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (FindingTotal, error) {
		var u FindingTotal
		err := row.Scan(&u.Instance, &u.Project, &u.Skill, &u.Blocking, &u.Suggestions)
		return u, err
	}); err != nil {
		return MetricsTotals{}, err
	}

	if rows, err = s.pool.Query(ctx, `SELECT d.instance, COALESCE(p.path, ''), d.skill, count(*)
		FROM dismissals d LEFT JOIN projects p ON p.instance = d.instance AND p.project_id = d.project_id
		GROUP BY d.instance, p.path, d.skill`); err != nil {
		return MetricsTotals{}, err
	}
	if t.Dismissals, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (DismissalTotal, error) {
		var u DismissalTotal
		err := row.Scan(&u.Instance, &u.Project, &u.Skill, &u.Dismissals)
		return u, err
	}); err != nil {
		return MetricsTotals{}, err
	}

	// An API review is finished once published, and its recorded outcome says
	// how; a superseded one never was, and one whose publishing failed is a
	// failure as the v1 status has it.
	if rows, err = s.pool.Query(ctx, `SELECT c.name,
			CASE r.state WHEN 'superseded' THEN 'superseded' WHEN 'publish_failed' THEN 'failed' ELSE r.outcome END, count(*)
		FROM api_reviews ar JOIN runs r ON r.id = ar.run_id JOIN api_clients c ON c.id = ar.client_id
		WHERE r.state IN ('published', 'publish_failed', 'superseded')
		GROUP BY 1, 2`); err != nil {
		return MetricsTotals{}, err
	}
	if t.APIReviews, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (APIReviewTotal, error) {
		var u APIReviewTotal
		err := row.Scan(&u.Client, &u.Result, &u.Reviews)
		return u, err
	}); err != nil {
		return MetricsTotals{}, err
	}

	if rows, err = s.pool.Query(ctx, `SELECT c.name, `+tokenSumColumnsOfUsage+`, count(*)
		FROM run_usage u JOIN api_reviews ar ON ar.run_id = u.run_id JOIN api_clients c ON c.id = ar.client_id
		GROUP BY c.name`); err != nil {
		return MetricsTotals{}, err
	}
	if t.APITokens, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (APITokenTotal, error) {
		var u APITokenTotal
		err := row.Scan(append([]any{&u.Client}, append(u.Tokens.dests(), &u.Rows)...)...)
		return u, err
	}); err != nil {
		return MetricsTotals{}, err
	}

	// A callback given up for want of a URL is an admin's choice, not a
	// failure: CountUndeliveredAPICallbacks leaves it out too.
	if rows, err = s.pool.Query(ctx, `SELECT c.name, 'delivered', count(*)
			FROM api_callbacks cb JOIN api_clients c ON c.id = cb.client_id
			WHERE cb.delivered_at IS NOT NULL GROUP BY c.name
		UNION ALL
		SELECT c.name, 'failed_attempt', count(*)
			FROM api_callback_attempts a JOIN api_callbacks cb ON cb.id = a.callback_id JOIN api_clients c ON c.id = cb.client_id
			WHERE NOT (a.error = '' AND a.status_code BETWEEN 200 AND 299) AND a.error <> $1::text GROUP BY c.name
		UNION ALL
		SELECT c.name, 'gave_up', count(*)
			FROM api_callbacks cb JOIN api_clients c ON c.id = cb.client_id
			WHERE cb.gave_up_at IS NOT NULL
			  AND NOT EXISTS (SELECT 1 FROM api_callback_attempts a WHERE a.callback_id = cb.id AND a.error = $1::text)
			GROUP BY c.name`, CallbackNoURL); err != nil {
		return MetricsTotals{}, err
	}
	if t.APICallbacks, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (APICallbackTotal, error) {
		var u APICallbackTotal
		err := row.Scan(&u.Client, &u.Outcome, &u.Count)
		return u, err
	}); err != nil {
		return MetricsTotals{}, err
	}

	// The next attempt of a dropped one is the oldest later attempt of the
	// same review.
	if rows, err = s.pool.Query(ctx, `SELECT COALESCE(f.model_entry_name, ''), COALESCE(NULLIF(n.model_entry_name, ''), 'none'),
			f.outcome_reason, COALESCE(f.model_source, ''), count(*)
		FROM runs f LEFT JOIN LATERAL (SELECT model_entry_name FROM runs n
			WHERE n.attempt_of = COALESCE(f.attempt_of, f.id) AND (n.created_at, n.id) > (f.created_at, f.id)
			ORDER BY n.created_at, n.id LIMIT 1) n ON true
		WHERE f.outcome = 'fell_back'
		GROUP BY 1, 2, 3, 4`); err != nil {
		return MetricsTotals{}, err
	}
	if t.Fallbacks, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (FallbackTotal, error) {
		var u FallbackTotal
		err := row.Scan(&u.From, &u.To, &u.Class, &u.Source, &u.Count)
		return u, err
	}); err != nil {
		return MetricsTotals{}, err
	}

	if rows, err = s.pool.Query(ctx, `SELECT k.model_id, COALESCE(g.name, g.provider || '/' || g.model, tm.name, k.model_id), k.source, k.class
		FROM model_entry_marks k
		LEFT JOIN llm_models g ON k.source = 'global' AND g.id = k.model_id
		LEFT JOIN team_models tm ON k.source = 'team' AND tm.id = k.model_id
		WHERE k.until IS NULL OR k.until > now()
		ORDER BY k.source, 1`); err != nil {
		return MetricsTotals{}, err
	}
	if t.Exhausted, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (ExhaustedEntry, error) {
		var u ExhaustedEntry
		err := row.Scan(&u.ID, &u.Name, &u.Source, &u.Class)
		return u, err
	}); err != nil {
		return MetricsTotals{}, err
	}
	return t, nil
}

// panelNoResult is a run_usage row of a review panel whose merge (Job P)
// left no result: only its members' tokens are there, with no verdict. Such
// a review is a review with no result, as a single review with none is, so
// the review counts leave it out (runs count it as no result) and its tokens
// are wasted.
const panelNoResult = `EXISTS (SELECT 1 FROM runs nr WHERE nr.id = run_usage.run_id AND nr.result IS NULL
	AND nr.failure_reason <> '' AND EXISTS (SELECT 1 FROM panel_members pm
		WHERE pm.run_id = COALESCE(nr.attempt_of, nr.id) AND pm.state <> 'off'))`

// notFellBack keeps the run_usage rows of runs not dropped for the next entry
// of their chain.
const notFellBack = `NOT EXISTS (SELECT 1 FROM runs fb WHERE fb.id = run_usage.run_id AND fb.outcome = 'fell_back')`

// ModelUsage is what one skill used on one model of a panel. CacheRead,
// CacheWrite and Reasoning are nil when the result didn't report them.
type ModelUsage struct {
	Skill, Role, Provider, Model     string
	In, Out                          int64
	CacheRead, CacheWrite, Reasoning *int64
}
