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

// ScanModelUsage is what one model used in a security scan: one run_usage row
// on the scan's carrier run. Usage.Skill is the row's skill.
type ScanModelUsage struct {
	Provider, Model string
	Usage           SkillUsage
}

// RecordUsage stores what each skill of run id used, with the run's
// repository, merge request, trigger, provider, model and time, and whose key
// paid (the run's entry: global, or a team and its ID), and the owner team of
// its repository now (ownerTeamSQL), which a team limit counts by, and marks the run as
// recorded, even with no skills. A row already there is kept.
func (s *Store) RecordUsage(ctx context.Context, id string, skills []SkillUsage) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	for _, u := range skills {
		if _, err := tx.Exec(ctx, `INSERT INTO run_usage (run_id, skill, instance, project_id, project_path, mr_iid,
				trigger, incremental, provider, model, verdict, reported_at, tokens_in, tokens_out, cache_read, cache_write,
				reasoning, findings, blocking, source, team_id, owner_team_id)
			SELECT r.id, $2::text, r.instance, r.project_id, r.project_path, r.mr_iid, r.trigger, r.incremental,
				COALESCE(r.provider, ''), COALESCE(r.model, ''), $3::text, COALESCE(r.reported_at, r.created_at), $4::bigint, $5::bigint,
				$6::bigint, $7::bigint, $8::bigint, $9::integer, $10::integer,
				CASE WHEN r.model_source = 'team' THEN 'team' ELSE 'global' END,
				CASE WHEN r.model_source = 'team' THEN r.model_team_id END, `+ownerTeamSQL("r.instance", "r.project_path")+`
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
// the run's repository, merge request, trigger, provider, model, whose key
// paid and the owner team of its repository now (ownerTeamSQL). The verdict, the time and the counts
// are $3, $4 and $10, $11; the tokens $5 to $9. On a conflict the tokens are added; a count one side
// reported is kept when the other side's is unknown, and stays unknown only
// when both are. What else a conflict does follows it.
//
// A multi review's members and its judge add to the row of the run they
// count on: its source and model are that run's (reviewer 1's), not each
// model's own. run_usage_by_model keeps those, so every count that splits by
// source or model reads splitUsageSQL instead (Ruling P62).
var usageInsertSQL = `INSERT INTO run_usage (run_id, skill, instance, project_id, project_path, mr_iid,
		trigger, incremental, provider, model, verdict, reported_at, tokens_in, tokens_out, cache_read, cache_write,
		reasoning, findings, blocking, source, team_id, owner_team_id)
	SELECT r.id, $2::text, r.instance, r.project_id, r.project_path, r.mr_iid, r.trigger, r.incremental,
		COALESCE(r.provider, ''), COALESCE(r.model, ''), $3::text, $4::timestamptz, $5::bigint, $6::bigint,
		$7::bigint, $8::bigint, $9::bigint, $10::integer, $11::integer,
		CASE WHEN r.model_source = 'team' THEN 'team' ELSE 'global' END,
		CASE WHEN r.model_source = 'team' THEN r.model_team_id END, ` + ownerTeamSQL("r.instance", "r.project_path") + `
	FROM runs r WHERE r.id = $1::uuid
	ON CONFLICT (run_id, skill) DO UPDATE SET tokens_in = run_usage.tokens_in + EXCLUDED.tokens_in,
		tokens_out = run_usage.tokens_out + EXCLUDED.tokens_out,
		cache_read = COALESCE(run_usage.cache_read + EXCLUDED.cache_read, run_usage.cache_read, EXCLUDED.cache_read),
		cache_write = COALESCE(run_usage.cache_write + EXCLUDED.cache_write, run_usage.cache_write, EXCLUDED.cache_write),
		reasoning = COALESCE(run_usage.reasoning + EXCLUDED.reasoning, run_usage.reasoning, EXCLUDED.reasoning)`

// splitUsageSQL is run_usage with a multi review's tokens split by Job, as
// entryTokensSQL counts them (Ruling L8, P62): a run that has
// run_usage_by_model rows counts those, each reviewer attempt and each judge
// on its own source, provider and model, with its run's repository. That
// run's run_usage rows, the total of all its Jobs on reviewer 1's source and
// model, are left out. Any other run, a --no-judge run among them, counts
// its run_usage rows as they are. The columns are the ones the counts by
// source, by model and by owner team read. A run_usage_by_model row has no
// owner team of its own: it takes the one of the run_usage row it was added
// to (the same run and skill, at most one: the key), as entryTokensSQL does.
const splitUsageSQL = `(SELECT u.run_id, u.instance, u.project_id, u.project_path, u.skill, u.provider, u.model,
		u.source, u.reported_at, u.tokens_in, u.tokens_out, u.cache_read, u.cache_write, u.reasoning, u.owner_team_id
	FROM run_usage u WHERE NOT EXISTS (SELECT 1 FROM run_usage_by_model b WHERE b.run_id = u.run_id)
	UNION ALL
	SELECT b.run_id, r.instance, r.project_id, r.project_path, b.skill, b.provider, b.model,
		b.source, b.reported_at, b.tokens_in, b.tokens_out, b.cache_read, b.cache_write, b.reasoning, o.owner_team_id
	FROM run_usage_by_model b JOIN runs r ON r.id = b.run_id
		LEFT JOIN run_usage o ON o.run_id = b.run_id AND o.skill = b.skill) split_usage`

// RecordJudgeUsage is RecordUsage of a multi review's judge run id
// (recordJudgedUsage): each skill's tokens are added to the run's run_usage
// row with the judge's verdict and counts, and one part judge row per skill
// is stored with the run's review_judges entry, provider and model. A run
// with no_judge, or with no review_judges row, stores none: no model judged.
// usage_recorded, read under the run's lock, stops a second count. An
// unknown run, or a malformed id, is no error and writes nothing.
func (s *Store) RecordJudgeUsage(ctx context.Context, id string, skills []SkillUsage) error {
	return s.recordJudgedUsage(ctx, id, skills, func(tx pgx.Tx, at time.Time) error {
		for _, u := range byName(skills) {
			if _, err := tx.Exec(ctx, `INSERT INTO run_usage_by_model (run_id, part, skill, provider, model, entry_name,
					source, team_id, reported_at, tokens_in, tokens_out, cache_read, cache_write, reasoning)
				SELECT j.run_id, 'judge', $2::text, j.provider, j.model, j.model_entry_name,
					CASE WHEN j.model_source = 'team' THEN 'team' ELSE 'global' END,
					CASE WHEN j.model_source = 'team' THEN j.model_team_id END,
					$3::timestamptz, $4::bigint, $5::bigint, $6::bigint, $7::bigint, $8::bigint
				FROM review_judges j WHERE j.run_id = $1::uuid AND NOT j.no_judge
				ON CONFLICT (run_id, skill) WHERE part = 'judge' DO NOTHING`,
				id, u.Skill, at, u.In, u.Out, u.CacheRead, u.CacheWrite, u.Reasoning); err != nil {
				return err
			}
		}
		return nil
	})
}

// recordJudgedUsage records the usage of run id, the run that judged a
// multi review (the judge Job), in one transaction: each
// skill's tokens are added to the run's row (the members made it when it
// runs on the run that was split), with its verdict, counts and time, then
// models adds the model rows at that time. The run's usage_recorded, read
// under the row's lock in the same transaction, stops a second count. An
// unknown run, or a malformed id, writes nothing.
func (s *Store) recordJudgedUsage(ctx context.Context, id string, skills []SkillUsage, models func(tx pgx.Tx, at time.Time) error) error {
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
	if err := models(tx, at); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE runs SET usage_recorded = true WHERE id = $1::uuid`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// byName is skills sorted by name, on a copy: a multi review's writes to one
// run's rows take them in one order, so two of them never wait for each other.
func byName(skills []SkillUsage) []SkillUsage {
	out := slices.Clone(skills)
	slices.SortStableFunc(out, func(a, b SkillUsage) int { return strings.Compare(a.Skill, b.Skill) })
	return out
}

// RecordReviewMemberUsage stores what each skill of review member id (one
// attempt of reviewer 1 to 5) used (recordMemberUsage): one
// run_usage_by_model row per skill (part reviewer, the member's number,
// entry, provider, model, source and team).
func (s *Store) RecordReviewMemberUsage(ctx context.Context, id string, skills []SkillUsage) error {
	return s.recordMemberUsage(ctx, reviewMemberUsage, id, skills)
}

// memberUsage is what recordMemberUsage writes for a member kind: the
// member table, the run its tokens count on, and the model row.
type memberUsage struct {
	table  string // review_members
	runSQL string // the run member $1's tokens count on
	// rowSQL adds the model row of member $1 and skill $2 on run $9: the
	// time is $3 (when the member has none), the tokens $4 to $8.
	rowSQL string
}

var (
	reviewMemberUsage = memberUsage{table: "review_members", runSQL: reviewMemberUsageRunSQL,
		rowSQL: `INSERT INTO run_usage_by_model (run_id, member_id, part, reviewer, skill, provider, model,
			entry_name, source, team_id, reported_at, tokens_in, tokens_out, cache_read, cache_write, reasoning)
		SELECT $9::uuid, m.id, 'reviewer', m.reviewer, $2::text, m.provider, m.model, m.model_entry_name,
			CASE WHEN m.model_source = 'team' THEN 'team' ELSE 'global' END,
			CASE WHEN m.model_source = 'team' THEN m.model_team_id END,
			COALESCE(m.reported_at, $3::timestamptz), $4::bigint, $5::bigint, $6::bigint, $7::bigint, $8::bigint
		FROM review_members m WHERE m.id = $1::uuid
		ON CONFLICT (member_id, skill) WHERE part = 'reviewer' DO NOTHING`}
)

// recordMemberUsage stores what each skill of member id used: one model row
// per skill (k.rowSQL), and the tokens added to run_usage of the run that
// was split (k.runSQL), then marks the member recorded, in one transaction;
// a member already recorded changes nothing. A row the member makes has no
// verdict and no counts: the judge's record sets them. A member that failed
// records the tokens it reported all the same. An unknown or off member is
// no error and writes nothing.
func (s *Store) recordMemberUsage(ctx context.Context, k memberUsage, id string, skills []SkillUsage) error {
	if !validUUID(id) {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	var target string
	err = tx.QueryRow(ctx, k.runSQL, id).Scan(&target)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// The run's row before the member's, the lock order of multi reviews: the rows added below hold the run's key, and a run is only
	// ever deleted with its row locked first.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM runs WHERE id = $1::uuid FOR KEY SHARE`, target); err != nil {
		return err
	}
	var (
		recorded bool
		at       time.Time
	)
	err = tx.QueryRow(ctx, `SELECT m.usage_recorded, COALESCE(r.reported_at, m.reported_at, r.created_at)
		FROM `+k.table+` m, runs r
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
		if _, err := tx.Exec(ctx, k.rowSQL, id, u.Skill, at, u.In, u.Out, u.CacheRead, u.CacheWrite, u.Reasoning, target); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE `+k.table+` SET usage_recorded = true WHERE id = $1::uuid`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// reviewMemberUsageRunSQL is the run review member $1's tokens count on:
// the run that was split. That is the run waiting for the members while
// they run; after the release (the fill at start) it is the newest run of
// the review created before the reviewer's first attempt, as a review can
// fall back on chain A before its split, and the split run is then a later
// attempt, not the review's first run. The judge's later attempts are
// created after the members, so they are never it. An off member has none.
const reviewMemberUsageRunSQL = `SELECT x.id::text FROM review_members m
	JOIN review_members f ON f.id = COALESCE(m.attempt_of, m.id)
	JOIN runs x ON x.id = m.run_id OR x.attempt_of = m.run_id
	WHERE m.id = $1::uuid AND m.state <> 'off'
	  AND ((x.state = 'waiting' AND x.skip_reason = 'review_members') OR x.created_at < f.created_at)
	ORDER BY (x.state = 'waiting' AND x.skip_reason = 'review_members') DESC, x.created_at DESC, x.id DESC
	LIMIT 1`

// membersWithoutUsageSQL picks up to $1 members with a stored result and no
// usage recorded, oldest first.
const membersWithoutUsageSQL = ` WHERE NOT usage_recorded AND result IS NOT NULL AND state <> 'off'
	ORDER BY created_at, id LIMIT $1`

// ReviewMembersWithoutUsage returns up to limit review members with a
// stored result and no usage recorded, oldest first: the fill at start
// records those whose write was lost.
func (s *Store) ReviewMembersWithoutUsage(ctx context.Context, limit int) ([]ReviewMember, error) {
	if limit <= 0 {
		return nil, nil
	}
	return s.queryReviewMembers(ctx, `SELECT `+reviewMemberColumns+` FROM review_members`+membersWithoutUsageSQL, limit)
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
func (s *Store) RunUsage(ctx context.Context, id string) (t TokenSum, found bool, err error) {
	// bool_or over no rows is NULL.
	var recorded *bool
	err = s.pool.QueryRow(ctx, `SELECT `+tokenSumColumns+`, bool_or(true)
		FROM run_usage WHERE run_id = $1::uuid`, id).Scan(append(t.dests(), &recorded)...)
	return t, recorded != nil && *recorded, err
}

// ReviewCost is what one model used in a multi review: the sum of its
// run_usage_by_model rows of one part ("reviewer" or "judge"), reviewer
// number (0 for the judge), entry, and the run (RunID) or reviewer attempt
// (MemberID, "" for a judge row) they count on.
type ReviewCost struct {
	Part                                    string // "reviewer" or "judge"
	Reviewer                                int    // 0 for the judge
	RunID, MemberID, Entry, Provider, Model string
	Tokens                                  TokenSum
}

// ReviewCosts returns what each model used in the multi review of run id
// (its first run or a later attempt): every reviewer attempt's rows (they
// count on the run that was split) and the judge rows of every run of the
// review (each judge attempt). Reviewers first, by number and attempt, then
// the judge runs oldest first. A review with none, or a malformed id, has
// none.
func (s *Store) ReviewCosts(ctx context.Context, id string) ([]ReviewCost, error) {
	if !validUUID(id) {
		return nil, nil
	}
	// Each part by its own index: reviewers by their id, judges by run.
	rows, err := s.pool.Query(ctx, `WITH review AS (SELECT COALESCE(attempt_of, id) AS root FROM runs WHERE id = $1::uuid)
		SELECT part, COALESCE(reviewer, 0), run_id::text, COALESCE(member_id::text, ''), entry_name, provider, model,
			`+tokenSumColumns+`
		FROM (SELECT u.part, u.reviewer, u.run_id, u.member_id, u.entry_name, u.provider, u.model,
				u.tokens_in, u.tokens_out, u.cache_read, u.cache_write, u.reasoning, m.created_at AS at
			FROM run_usage_by_model u JOIN review_members m ON m.id = u.member_id
			WHERE u.part = 'reviewer' AND m.run_id = (SELECT root FROM review)
			UNION ALL
			SELECT u.part, u.reviewer, u.run_id, u.member_id, u.entry_name, u.provider, u.model,
				u.tokens_in, u.tokens_out, u.cache_read, u.cache_write, u.reasoning, r.created_at AS at
			FROM run_usage_by_model u JOIN runs r ON r.id = u.run_id
			WHERE u.part = 'judge' AND (r.id = (SELECT root FROM review) OR r.attempt_of = (SELECT root FROM review))) x
		GROUP BY part, reviewer, run_id, member_id, entry_name, provider, model
		ORDER BY part = 'judge', reviewer, min(at), run_id, member_id, entry_name, provider, model`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReviewCost
	for rows.Next() {
		var c ReviewCost
		if err := rows.Scan(append([]any{&c.Part, &c.Reviewer, &c.RunID, &c.MemberID, &c.Entry, &c.Provider, &c.Model}, c.Tokens.dests()...)...); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// allTokensSQL is every token a row used: input, cache read, cache write,
// output and reasoning, each NULL as 0, all counted the same. Limits, quotas
// and budgets count this.
const allTokensSQL = "COALESCE(tokens_in, 0) + COALESCE(cache_read, 0) + COALESCE(cache_write, 0) + COALESCE(tokens_out, 0) + COALESCE(reasoning, 0)"

// allTokensOf is allTokensSQL with each column of table alias (alias.tokens_in).
func allTokensOf(alias string) string {
	return "COALESCE(" + alias + ".tokens_in, 0) + COALESCE(" + alias + ".cache_read, 0) + COALESCE(" + alias + ".cache_write, 0) + COALESCE(" +
		alias + ".tokens_out, 0) + COALESCE(" + alias + ".reasoning, 0)"
}

// TokenSum is the tokens of some rows: input, output, the counts reported
// apart, and how many rows did not report them. Total adds all five parts.
type TokenSum struct {
	In, Out                          int64
	CacheRead, CacheWrite, Reasoning int64
	Unknown                          int64 // rows without cache and reasoning counts
}

// Prompt is the tokens sent: input, cache read and cache write.
func (t TokenSum) Prompt() int64 { return t.In + t.CacheRead + t.CacheWrite }

// Output is the tokens the model wrote: output and reasoning.
func (t TokenSum) Output() int64 { return t.Out + t.Reasoning }

// Total is all tokens: Prompt plus Output.
func (t TokenSum) Total() int64 { return t.Prompt() + t.Output() }

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
// GlobalTokens and TeamTokens are its tokens (all parts) on global keys
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

// ReviewTokens is the tokens one review used.
type ReviewTokens struct {
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
	Outliers                      []ReviewTokens
}

// tokenSumColumns are the sums every usage query returns, in the order of
// TokenSum.dests. A sum skips NULL, so a row that didn't report a count adds
// to Unknown and nothing to that sum.
const tokenSumColumns = `COALESCE(sum(tokens_in), 0), COALESCE(sum(tokens_out), 0),
	COALESCE(sum(cache_read), 0), COALESCE(sum(cache_write), 0), COALESCE(sum(reasoning), 0),
	count(*) FILTER (WHERE cache_read IS NULL OR cache_write IS NULL OR reasoning IS NULL)`

// tokenSumColumnsNamed is tokenSumColumns with a name per column, for a
// subquery: t_in, t_out, cr, cw, rs and unknown.
const tokenSumColumnsNamed = `COALESCE(sum(tokens_in), 0) AS t_in, COALESCE(sum(tokens_out), 0) AS t_out,
	COALESCE(sum(cache_read), 0) AS cr, COALESCE(sum(cache_write), 0) AS cw, COALESCE(sum(reasoning), 0) AS rs,
	count(*) FILTER (WHERE cache_read IS NULL OR cache_write IS NULL OR reasoning IS NULL) AS unknown`

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

// sourceTokensColumns are all the tokens on global keys and on
// teams' own, of splitUsageSQL rows.
const sourceTokensColumns = `COALESCE(sum(` + allTokensSQL + `) FILTER (WHERE source = 'global'), 0),
			COALESCE(sum(` + allTokensSQL + `) FILTER (WHERE source = 'team'), 0)`

// sourceTokensColumnsNamed is sourceTokensColumns named global and team,
// for a subquery.
const sourceTokensColumnsNamed = `COALESCE(sum(` + allTokensSQL + `) FILTER (WHERE source = 'global'), 0) AS global,
			COALESCE(sum(` + allTokensSQL + `) FILTER (WHERE source = 'team'), 0) AS team`

// UsageBySource returns the tokens (all parts) one repository used since
// the given time on global keys and on teams' own. A multi review counts
// each Job on its own source (splitUsageSQL).
func (s *Store) UsageBySource(ctx context.Context, instance string, projectID int64, since time.Time) (global, team int64, err error) {
	err = s.pool.QueryRow(ctx, `SELECT `+sourceTokensColumns+` FROM `+splitUsageSQL+`
		WHERE instance = $1::text AND project_id = $2::bigint AND reported_at >= $3::timestamptz`,
		instance, projectID, since).Scan(&global, &team)
	return global, team, err
}

// UsageByRepository returns what each repository used since the given time,
// busiest first, with the path it was last reported under. Reviews counts a
// run once, however many skills it ran. An API review has no repository (its
// instance is empty) and is left out.
func (s *Store) UsageByRepository(ctx context.Context, since time.Time) ([]RepoUsage, error) {
	// The split by source reads splitUsageSQL, so a multi review counts each
	// Job on its own source; the totals stay run_usage's.
	rows, err := s.pool.Query(ctx, `SELECT a.instance, a.project_id, a.path, a.t_in, a.t_out, a.cr, a.cw, a.rs, a.unknown,
			a.reviews, COALESCE(src.global, 0), COALESCE(src.team, 0)
		FROM (SELECT instance, project_id, (array_agg(project_path ORDER BY reported_at DESC))[1] AS path,
				`+tokenSumColumnsNamed+`, count(DISTINCT run_id) AS reviews
			FROM run_usage WHERE reported_at >= $1::timestamptz AND instance <> ''
			GROUP BY instance, project_id) a
		LEFT JOIN (SELECT instance, project_id, `+sourceTokensColumnsNamed+`
			FROM `+splitUsageSQL+` WHERE reported_at >= $1::timestamptz AND instance <> ''
			GROUP BY instance, project_id) src USING (instance, project_id)
		ORDER BY a.t_in + a.cr + a.cw + a.t_out + a.rs DESC, a.instance, a.project_id`, since)
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
		ORDER BY COALESCE(sum(`+allTokensOf("u")+`), 0) DESC, c.name, ar.tenant`, since)
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
	total := func(c ClientUsage) int64 { return c.Tokens.Total() }
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
		GROUP BY skill ORDER BY COALESCE(sum(`+allTokensSQL+`), 0) DESC, skill`,
		instance, projectID, since)
}

// UsageByModel returns what each model used in a repository since the given
// time, busiest first; one provider's model is apart from another's of the
// same name. Runs counts a run once, however many skills it ran. A multi
// review counts each Job on its own model (splitUsageSQL).
func (s *Store) UsageByModel(ctx context.Context, instance string, projectID int64, since time.Time) ([]NamedUsage, error) {
	return s.namedUsage(ctx, `SELECT provider, model, `+tokenSumColumns+`, count(DISTINCT run_id), 0::bigint
		FROM `+splitUsageSQL+` WHERE instance = $1::text AND project_id = $2::bigint AND reported_at >= $3::timestamptz
		GROUP BY provider, model ORDER BY COALESCE(sum(`+allTokensSQL+`), 0) DESC, model, provider`,
		instance, projectID, since)
}

func (s *Store) namedUsage(ctx context.Context, query string, args ...any) ([]NamedUsage, error) {
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanNamedUsage)
}

// scanNamedUsage scans a row of provider, name, tokenSumColumns, runs and
// failed.
func scanNamedUsage(row pgx.CollectableRow) (NamedUsage, error) {
	var u NamedUsage
	err := row.Scan(append([]any{&u.Provider, &u.Name}, append(u.Tokens.dests(), &u.Runs, &u.Failed)...)...)
	return u, err
}

// TeamUsage is what the repositories a team owned used: the total, each UTC
// day oldest first, the repositories and the models busiest first. Reviews
// counts a run once. A repository's GlobalTokens and TeamTokens are 0.
type TeamUsage struct {
	Tokens  TokenSum
	Reviews int64
	Daily   []DayUsage
	Repos   []RepoUsage
	Models  []NamedUsage
}

// teamUsageRows are the splitUsageSQL rows of owner team $1 from $2 on:
// every part of TeamUsage reads them, so the parts add up.
const teamUsageRows = splitUsageSQL + ` WHERE owner_team_id = $1::bigint AND reported_at >= $2::timestamptz`

// TeamUsage returns what team used from since on, by reported time: the
// splitUsageSQL rows whose owner team is the team (the owner of the
// repository when the row was written, so a repository that moved leaves its
// earlier rows with the old team). A multi review counts each Job on its own
// model, once. Repositories show the path they were last reported under; the
// order of repositories and models, ties included, is UsageByRepository's
// and UsageByModel's. It reads in one repeatable-read transaction, so a
// write in between can't make the parts disagree.
func (s *Store) TeamUsage(ctx context.Context, team int64, since time.Time) (TeamUsage, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return TeamUsage{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only
	var out TeamUsage
	if err := tx.QueryRow(ctx, `SELECT `+tokenSumColumns+`, count(DISTINCT run_id) FROM `+teamUsageRows, team, since).
		Scan(append(out.Tokens.dests(), &out.Reviews)...); err != nil {
		return TeamUsage{}, err
	}
	rows, err := tx.Query(ctx, `SELECT ((reported_at AT TIME ZONE 'UTC')::date)::timestamp AT TIME ZONE 'UTC' AS day, `+tokenSumColumns+`
		FROM `+teamUsageRows+` GROUP BY day ORDER BY day`, team, since)
	if err != nil {
		return TeamUsage{}, err
	}
	if out.Daily, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (DayUsage, error) {
		var u DayUsage
		err := row.Scan(append([]any{&u.Day}, u.Tokens.dests()...)...)
		u.Day = u.Day.UTC()
		return u, err
	}); err != nil {
		return TeamUsage{}, err
	}
	rows, err = tx.Query(ctx, `SELECT instance, project_id, (array_agg(project_path ORDER BY reported_at DESC))[1], `+tokenSumColumns+`,
			count(DISTINCT run_id)
		FROM `+teamUsageRows+` GROUP BY instance, project_id
		ORDER BY COALESCE(sum(`+allTokensSQL+`), 0) DESC, instance, project_id`, team, since)
	if err != nil {
		return TeamUsage{}, err
	}
	if out.Repos, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (RepoUsage, error) {
		var u RepoUsage
		err := row.Scan(append([]any{&u.Instance, &u.ProjectID, &u.ProjectPath}, append(u.Tokens.dests(), &u.Reviews)...)...)
		return u, err
	}); err != nil {
		return TeamUsage{}, err
	}
	rows, err = tx.Query(ctx, `SELECT provider, model, `+tokenSumColumns+`, count(DISTINCT run_id), 0::bigint
		FROM `+teamUsageRows+` GROUP BY provider, model
		ORDER BY COALESCE(sum(`+allTokensSQL+`), 0) DESC, model, provider`, team, since)
	if err != nil {
		return TeamUsage{}, err
	}
	if out.Models, err = pgx.CollectRows(rows, scanNamedUsage); err != nil {
		return TeamUsage{}, err
	}
	return out, nil
}

// OwnerTeamUsageStart returns when the oldest run_usage row with an owner
// team was reported; found is false when no row has one. Rows written before
// owner_team_id was (migration 0051) have none, so a team's usage before then
// is not known.
func (s *Store) OwnerTeamUsageStart(ctx context.Context) (start time.Time, found bool, err error) {
	var at *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT min(reported_at) FROM run_usage WHERE owner_team_id IS NOT NULL`).Scan(&at); err != nil {
		return time.Time{}, false, err
	}
	if at == nil {
		return time.Time{}, false, nil
	}
	return at.UTC(), true, nil
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
		ORDER BY COALESCE(sum(`+allTokensOf("u")+`), 0) DESC, u.mr_iid
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
	// A failed skill, or a multi review's member tokens when its judge left
	// no result (multiReviewNoResult): both are wasted.
	failed := `(verdict = 'failed' OR ` + multiReviewNoResult + `)`
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
	// A review with no result has no usage rows of its own (a multi
	// review's has its members', counted as wasted above, never as a
	// review), so these
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
				sum(`+allTokensSQL+`)::bigint AS total
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
		var r ReviewTokens
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
	Skill  string
	Tokens TokenSum
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
		SELECT u.skill, `+tokenSumColumnsOfUsage+`
		FROM run_usage u JOIN decided d ON d.mr_iid = u.mr_iid
		WHERE u.instance = $1::text AND u.project_id = $2::bigint
		GROUP BY u.skill ORDER BY u.skill`, instance, projectID, since)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (SkillTokens, error) {
		var t SkillTokens
		err := row.Scan(append([]any{&t.Skill}, t.Tokens.dests()...)...)
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
	// A multi review counts each Job on its own model (splitUsageSQL).
	rows, err := s.pool.Query(ctx, `SELECT instance, project_path, skill, model, `+tokenSumColumns+`, count(*)
		FROM `+splitUsageSQL+` WHERE instance <> '' GROUP BY instance, project_path, skill, model`)
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
		FROM run_usage WHERE instance <> '' AND `+notFellBack+` AND NOT `+multiReviewNoResult+`
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

	// Reviews with no result have no usage rows of their own (a multi
	// review's members' rows are left out of the review and skill counts), so these
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
		FROM run_usage WHERE instance <> '' AND `+notFellBack+` AND NOT `+multiReviewNoResult+`
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

// multiReviewNoResult is a run_usage row of a multi review whose judge run
// left no result (or whose every reviewer failed), or of a legacy review
// panel (panel_members, a main build left it) whose merge left none: only
// its members' tokens are there, with no verdict. Such a review is a review
// with no result, as a single review with none is, so the review counts
// leave it out (runs count it as no result) and its tokens are wasted. An
// off row is a single review: it never matches.
const multiReviewNoResult = `EXISTS (SELECT 1 FROM runs nr WHERE nr.id = run_usage.run_id AND nr.result IS NULL
	AND nr.failure_reason <> '' AND (EXISTS (SELECT 1 FROM panel_members pm
		WHERE pm.run_id = COALESCE(nr.attempt_of, nr.id) AND pm.state <> 'off')
	  OR EXISTS (SELECT 1 FROM review_members rm
		WHERE rm.run_id = COALESCE(nr.attempt_of, nr.id) AND rm.state <> 'off')))`

// notFellBack keeps the run_usage rows of runs not dropped for the next entry
// of their chain.
const notFellBack = `NOT EXISTS (SELECT 1 FROM runs fb WHERE fb.id = run_usage.run_id AND fb.outcome = 'fell_back')`
