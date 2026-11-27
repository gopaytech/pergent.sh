package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The severities a security finding may have (SecurityFinding.Severity).
const (
	SeverityCritical = "critical"
	SeverityHigh     = "high"
	SeverityMedium   = "medium"
	SeverityLow      = "low"
)

// The severities, most severe first.
var securitySeverities = []string{SeverityCritical, SeverityHigh, SeverityMedium, SeverityLow}

// Where a security finding stands (SecurityFinding.State).
const (
	FindingOpen      = "open"
	FindingDismissed = "dismissed"
	FindingFixed     = "fixed"
)

// What happened to a security finding (SecurityFindingEvent.Kind).
const (
	FindingEventFound     = "found"
	FindingEventReopened  = "reopened"
	FindingEventFixed     = "fixed"
	FindingEventDismissed = "dismissed"
	FindingEventRestored  = "restored"
)

// The reasons a person may give to dismiss a security finding.
const (
	DismissFalsePositive  = "false_positive"
	DismissWontFix        = "wont_fix"
	DismissUsedInTests    = "used_in_tests"
	DismissMitigated      = "mitigated"
	DismissAcceptableRisk = "acceptable_risk"
)

// SecurityDismissReasons are the dismiss reasons, in the order a person
// picks from.
var SecurityDismissReasons = []string{DismissFalsePositive, DismissWontFix, DismissUsedInTests, DismissMitigated,
	DismissAcceptableRisk}

// securityOtherCommit is the reason of a scan whose result is for another
// commit than the one it was started on: a result it cannot use.
const securityOtherCommit = "pergent sent a result pergent.sh couldn't use: the result is for another commit"

// securityKeptScans is how many of a repository's newest finished scans keep
// their SARIF and state. The last complete scan keeps them too.
const securityKeptScans = 10

// securityFindingsMax is the most findings SecurityFindings returns, also
// when the filter asks for more or sets no limit.
const securityFindingsMax = 1000

// SecurityFindingInput is one finding of a scan's result. Fingerprint is the
// finding's identity from scan to scan; Severity is one of critical, high,
// medium and low. The skill sends no end line.
type SecurityFindingInput struct {
	Fingerprint, RuleID, CWE                                         string
	Tags                                                             []string
	Severity, Title, Description, Exploit, FixHint, Confidence, File string
	StartLine                                                        int
	NeedsValidation                                                  bool
}

// SecurityResult is what FinishSecurityScan stores of a scan Job's result.
// Status is complete, partial or failed; Reason says why for the last two.
// Commit must be the scan's own commit: a result for another one is
// unusable, and the scan fails keeping none of it. A nil State or SARIF is
// none. SkillName and SkillCommit are empty when the result does not say.
type SecurityResult struct {
	Status, Reason, Commit, UsageSource string
	CoverageDone, CoveragePlanned       *int
	State                               map[string]string
	SARIF                               []byte
	SkillName, SkillCommit              string
	Findings                            []SecurityFindingInput
}

// SecurityFinding is one finding of a repository, open, dismissed or fixed.
// FirstScanID found it first, LastScanID found it last, and FixedScanID is
// the complete scan that no longer found it (nil unless fixed).
// FirstScanNumber is the number of FirstScanID and FirstSeenAt the time it
// ended. The dismiss fields are set while it is dismissed.
type SecurityFinding struct {
	ID                                                               int64
	Instance, Project, Fingerprint, State, RuleID, CWE               string
	Tags                                                             []string
	Severity, Title, Description, Exploit, FixHint, Confidence, File string
	StartLine                                                        int
	NeedsValidation                                                  bool
	FirstScanID, LastScanID                                          string
	FixedScanID                                                      *string
	FirstScanNumber                                                  int64
	FirstSeenAt                                                      time.Time
	DismissReason, DismissComment, DismissedBy                       string
	DismissedAt                                                      *time.Time
	UpdatedAt                                                        time.Time
}

// SecurityFindingEvent is one thing that happened to a finding: found,
// reopened or fixed by the scan ScanID (number ScanNumber), or dismissed or
// restored by Actor (ScanID empty, ScanNumber 0). Reason and Comment are a
// dismissal's.
type SecurityFindingEvent struct {
	Kind, ScanID, Actor, Reason, Comment string
	At                                   time.Time
	ScanNumber                           int64
}

// SecurityFindingFilter narrows SecurityFindings. An empty field matches
// every finding. Rule is the rule ID, an attack class. Query matches a text
// in the file or the title, ignoring case; % and _ in it are plain
// characters. Limit 0 or over the maximum is the maximum.
type SecurityFindingFilter struct {
	State, Severity, Rule, Query string
	Limit                        int
}

// SecurityCounts counts a repository's open findings by severity, and the
// open ones that need a person to check them.
type SecurityCounts struct {
	Critical, High, Medium, Low, NeedsValidation int
}

// findingColumns are a finding's columns, read FROM security_findings. The
// first scan is read by its key, so a list costs one lookup per row.
const findingColumns = `id, instance, project, fingerprint, state, rule_id, cwe, tags, severity, title, description,
	exploit, fix_hint, confidence, file, start_line, needs_validation, first_scan_id::text, last_scan_id::text,
	fixed_scan_id::text,
	(SELECT fs.number FROM security_scans fs WHERE fs.id = security_findings.first_scan_id),
	(SELECT COALESCE(fs.finished_at, fs.queued_at) FROM security_scans fs WHERE fs.id = security_findings.first_scan_id),
	COALESCE(dismiss_reason, ''), dismiss_comment, dismissed_by, dismissed_at, updated_at`

// findingRepo matches a repository's findings, the project ignoring case.
const findingRepo = `instance = $1 AND lower(project) = lower($2)`

// findingOrder lists open findings first, then dismissed, then fixed; each
// by severity, most severe first, then by file and line.
const findingOrder = `ORDER BY CASE state WHEN 'open' THEN 0 WHEN 'dismissed' THEN 1 ELSE 2 END,
	CASE severity WHEN 'critical' THEN 0 WHEN 'high' THEN 1 WHEN 'medium' THEN 2 ELSE 3 END, file, start_line, id`

func scanFinding(row pgx.Row) (SecurityFinding, error) {
	var f SecurityFinding
	err := row.Scan(&f.ID, &f.Instance, &f.Project, &f.Fingerprint, &f.State, &f.RuleID, &f.CWE, &f.Tags, &f.Severity,
		&f.Title, &f.Description, &f.Exploit, &f.FixHint, &f.Confidence, &f.File, &f.StartLine, &f.NeedsValidation,
		&f.FirstScanID, &f.LastScanID, &f.FixedScanID, &f.FirstScanNumber, &f.FirstSeenAt, &f.DismissReason,
		&f.DismissComment, &f.DismissedBy,
		&f.DismissedAt, &f.UpdatedAt)
	return f, err
}

func (s *Store) queryFindings(ctx context.Context, sql string, args ...any) ([]SecurityFinding, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (SecurityFinding, error) { return scanFinding(row) })
}

// upsertFindingSQL records one finding of the scan $3 by its fingerprint
// $4: a new one is open, with a found event; a fixed one is open again, with
// a reopened event; an open or dismissed one keeps its state. Each takes the
// scan's fields and the scan as its last. It returns the event's row, so
// the rows it changes count the findings that are new to the scan. prev
// reads the row as it was before the statement. No lock is needed: only one
// scan of a repository runs at a time, and a dismiss or restore beside it
// never makes or ends a fixed finding.
const upsertFindingSQL = `WITH prev AS (
		SELECT id, state FROM security_findings WHERE ` + findingRepo + ` AND fingerprint = $4
	), up AS (
		INSERT INTO security_findings AS f (instance, project, fingerprint, state, rule_id, cwe, tags, severity, title,
			description, exploit, fix_hint, confidence, file, start_line, needs_validation, first_scan_id,
			last_scan_id, updated_at)
		VALUES ($1, $2, $4, 'open', $5, $6, $7::text[], $8, $9, $10, $11, $12, $13, $14, $15, $16, $3::uuid,
			$3::uuid, $17)
		ON CONFLICT (instance, (lower(project)), fingerprint) DO UPDATE SET
			state = CASE WHEN f.state = 'fixed' THEN 'open' ELSE f.state END,
			fixed_scan_id = CASE WHEN f.state = 'fixed' THEN NULL ELSE f.fixed_scan_id END,
			rule_id = EXCLUDED.rule_id, cwe = EXCLUDED.cwe, tags = EXCLUDED.tags, severity = EXCLUDED.severity,
			title = EXCLUDED.title, description = EXCLUDED.description, exploit = EXCLUDED.exploit,
			fix_hint = EXCLUDED.fix_hint, confidence = EXCLUDED.confidence, file = EXCLUDED.file,
			start_line = EXCLUDED.start_line, needs_validation = EXCLUDED.needs_validation,
			last_scan_id = EXCLUDED.last_scan_id, updated_at = EXCLUDED.updated_at
		RETURNING f.id
	)
	INSERT INTO security_finding_events (finding_id, scan_id, kind, at)
	SELECT up.id, $3::uuid, CASE WHEN prev.id IS NULL THEN 'found' ELSE 'reopened' END, $17
	FROM up LEFT JOIN prev ON true
	WHERE prev.id IS NULL OR prev.state = 'fixed'`

// fixFindingsSQL fixes the repository's open findings that the complete
// scan $3 did not find (their last scan is another one), each with a fixed
// event. Dismissed findings stay dismissed.
const fixFindingsSQL = `WITH fixed AS (
		UPDATE security_findings SET state = 'fixed', fixed_scan_id = $3::uuid, updated_at = $4
		WHERE ` + findingRepo + ` AND state = 'open' AND last_scan_id <> $3::uuid
		RETURNING id
	)
	INSERT INTO security_finding_events (finding_id, scan_id, kind, at) SELECT id, $3::uuid, 'fixed', $4 FROM fixed`

// pruneSecuritySQL drops the SARIF and state of the repository's finished
// scans, except the newest few and the last complete one, which the next
// scan reads its state from.
const pruneSecuritySQL = `WITH newest AS (
		SELECT id FROM security_scans WHERE ` + findingRepo + ` AND state IN ('complete', 'partial', 'failed')
		ORDER BY number DESC LIMIT $3
	), last_complete AS (
		SELECT id FROM security_scans WHERE ` + findingRepo + ` AND state = 'complete'
		ORDER BY number DESC LIMIT 1
	)
	UPDATE security_scans SET sarif = NULL, state_files = NULL
	WHERE ` + findingRepo + ` AND state IN ('complete', 'partial', 'failed')
		AND (sarif IS NOT NULL OR state_files IS NOT NULL)
		AND id NOT IN (SELECT id FROM newest) AND id NOT IN (SELECT id FROM last_complete)`

// CheckSecurityResult refuses a result FinishSecurityScan cannot store: a
// status that is not an end, or a finding with no fingerprint or an unknown
// severity. The engine checks first, so a result it can't store fails the
// scan rather than leaving it running.
func CheckSecurityResult(res SecurityResult) error { return checkSecurityResult(res) }

func checkSecurityResult(res SecurityResult) error {
	if !slices.Contains([]string{SecurityComplete, SecurityPartial, SecurityFailed}, res.Status) {
		return fmt.Errorf("store: security result status %q is not complete, partial or failed", res.Status)
	}
	for _, f := range res.Findings {
		if f.Fingerprint == "" {
			return errors.New("store: a security finding has no fingerprint")
		}
		if !slices.Contains(securitySeverities, f.Severity) {
			return fmt.Errorf("store: security finding %q has severity %q", f.Fingerprint, f.Severity)
		}
	}
	return nil
}

// jsonParam is v as JSON text for a jsonb column; nil is NULL.
func jsonParam(v map[string]string) (*string, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	s := string(b)
	return &s, nil
}

// FinishSecurityScan stores the result of a running scan, in one
// transaction. ok is false, and nothing changes, when the scan is not
// running or, with a hash, the hash is not its token. A result with a bad
// status or finding is an error and changes nothing either.
//
// A result for another commit than the scan's is unusable: the scan fails
// with a reason that says so and keeps none of it but its usage source.
// Otherwise, in this order:
//  1. The scan ends with the status, reason, usage source, coverage, skill,
//     state and SARIF; its token is spent.
//  2. Each finding is recorded by its fingerprint (see upsertFindingSQL); a
//     fingerprint sent twice counts once, as first sent. NewCount counts the
//     new and the reopened ones.
//  3. A complete scan is SkillUpdated when the repository's last complete
//     scan with a skill named another skill name or commit. A scan with no
//     skill (name and commit both empty) is no change, and later scans do
//     not compare with it.
//  4. Only a complete scan that names its skill and is not SkillUpdated
//     fixes the open findings it did not find; FixedCount counts them. A
//     scan with no skill fixes nothing: an unknown skill version cannot show
//     its fingerprints match the old ones. A dismissed finding is never
//     fixed.
//  5. The repository's finished scans past the newest 10 lose their SARIF
//     and state, except the last complete one.
func (s *Store) FinishSecurityScan(ctx context.Context, id string, hash []byte, res SecurityResult, now time.Time) (bool, error) {
	if !fetchID.MatchString(id) {
		return false, nil
	}
	if err := checkSecurityResult(res); err != nil {
		return false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var instance, project, commit string
	err = tx.QueryRow(ctx, `SELECT instance, project, commit FROM security_scans
		WHERE id = $1::uuid AND state = 'running' AND ($2::bytea IS NULL OR token_hash = $2::bytea)
		FOR UPDATE`, id, hash).Scan(&instance, &project, &commit)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if res.Commit != commit {
		res = SecurityResult{Status: SecurityFailed, Reason: securityOtherCommit, UsageSource: res.UsageSource}
	}

	state, err := jsonParam(res.State)
	if err != nil {
		return false, err
	}
	var sarif *string
	if res.SARIF != nil {
		v := string(res.SARIF)
		sarif = &v
	}
	skillName, skillCommit := cutText(res.SkillName), cutText(res.SkillCommit)
	if _, err := tx.Exec(ctx, `UPDATE security_scans SET state = $2, reason = $3, usage_source = $4,
			coverage_done = $5, coverage_planned = $6, skill_name = $7, skill_commit = $8, state_files = $9::jsonb,
			sarif = $10::jsonb, finished_at = $11, token_hash = NULL
		WHERE id = $1::uuid`,
		id, res.Status, cutText(res.Reason), cutText(res.UsageSource), res.CoverageDone, res.CoveragePlanned,
		skillName, skillCommit, state, sarif, now); err != nil {
		return false, err
	}

	newCount, err := upsertFindings(ctx, tx, instance, project, id, res.Findings, now)
	if err != nil {
		return false, err
	}

	hasSkill := skillName != "" || skillCommit != ""
	skillUpdated := false
	if res.Status == SecurityComplete && hasSkill {
		var lastName, lastCommit string
		err := tx.QueryRow(ctx, `SELECT skill_name, skill_commit FROM security_scans
			WHERE `+findingRepo+` AND state = 'complete' AND id <> $3::uuid AND (skill_name <> '' OR skill_commit <> '')
			ORDER BY number DESC LIMIT 1`, instance, project, id).Scan(&lastName, &lastCommit)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return false, err
		default:
			skillUpdated = lastName != skillName || lastCommit != skillCommit
		}
	}

	fixedCount := 0
	if res.Status == SecurityComplete && hasSkill && !skillUpdated {
		tag, err := tx.Exec(ctx, fixFindingsSQL, instance, project, id, now)
		if err != nil {
			return false, err
		}
		fixedCount = int(tag.RowsAffected())
	}

	if _, err := tx.Exec(ctx, `UPDATE security_scans SET skill_updated = $2, new_count = $3, fixed_count = $4
		WHERE id = $1::uuid`, id, skillUpdated, newCount, fixedCount); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, pruneSecuritySQL, instance, project, securityKeptScans); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// upsertFindings records the findings of the scan id in one round trip and
// returns how many are new or reopened. A fingerprint sent twice counts once,
// as first sent.
func upsertFindings(ctx context.Context, tx pgx.Tx, instance, project, id string, findings []SecurityFindingInput, now time.Time) (int, error) {
	seen := map[string]bool{}
	b := &pgx.Batch{}
	for _, f := range findings {
		if seen[f.Fingerprint] {
			continue
		}
		seen[f.Fingerprint] = true
		tags := f.Tags
		if tags == nil {
			tags = []string{}
		}
		b.Queue(upsertFindingSQL, instance, project, id, f.Fingerprint, f.RuleID, f.CWE, tags, f.Severity, f.Title,
			f.Description, f.Exploit, f.FixHint, f.Confidence, f.File, f.StartLine, f.NeedsValidation, now)
	}
	if b.Len() == 0 {
		return 0, nil
	}
	br := tx.SendBatch(ctx, b)
	n := 0
	for range b.Len() {
		tag, err := br.Exec()
		if err != nil {
			_ = br.Close()
			return 0, err
		}
		n += int(tag.RowsAffected())
	}
	return n, br.Close()
}

// SecurityFindings returns a repository's findings that match f, open first,
// then by severity and file. truncated is whether more matched than the
// limit. The project is matched ignoring case.
func (s *Store) SecurityFindings(ctx context.Context, instance, project string, f SecurityFindingFilter) (rows []SecurityFinding, truncated bool, err error) {
	limit := f.Limit
	if limit <= 0 || limit > securityFindingsMax {
		limit = securityFindingsMax
	}
	pattern := ""
	if f.Query != "" {
		pattern = "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(f.Query) + "%"
	}
	rows, err = s.queryFindings(ctx, `SELECT `+findingColumns+` FROM security_findings
		WHERE `+findingRepo+` AND ($3 = '' OR state = $3) AND ($4 = '' OR severity = $4) AND ($5 = '' OR rule_id = $5)
			AND ($6 = '' OR file ILIKE $6 OR title ILIKE $6)
		`+findingOrder+` LIMIT $7`, instance, project, f.State, f.Severity, f.Rule, pattern, limit+1)
	if err != nil {
		return nil, false, err
	}
	if len(rows) > limit {
		return rows[:limit], true, nil
	}
	return rows, false, nil
}

// SecurityScanFindings returns the findings of a repository that the scan
// scanID found new (found or reopened) and those it fixed, each at most the
// SecurityFindings maximum, in its order. A scan of another repository has
// none. The project is matched ignoring case.
func (s *Store) SecurityScanFindings(ctx context.Context, instance, project, scanID string) (found, fixed []SecurityFinding, err error) {
	if !fetchID.MatchString(scanID) {
		return nil, nil, nil
	}
	of := func(kinds ...string) ([]SecurityFinding, error) {
		return s.queryFindings(ctx, `SELECT `+findingColumns+` FROM security_findings
			WHERE `+findingRepo+` AND EXISTS (SELECT 1 FROM security_finding_events e
				WHERE e.finding_id = security_findings.id AND e.scan_id = $3::uuid AND e.kind = ANY($4::text[]))
			`+findingOrder+` LIMIT $5`, instance, project, scanID, kinds, securityFindingsMax)
	}
	if found, err = of(FindingEventFound, FindingEventReopened); err != nil {
		return nil, nil, err
	}
	if fixed, err = of(FindingEventFixed); err != nil {
		return nil, nil, err
	}
	return found, fixed, nil
}

// SecurityFinding returns a finding of a repository; found is false when the
// repository has no finding id. The project is matched ignoring case.
func (s *Store) SecurityFinding(ctx context.Context, instance, project string, id int64) (SecurityFinding, bool, error) {
	f, err := scanFinding(s.pool.QueryRow(ctx, `SELECT `+findingColumns+` FROM security_findings
		WHERE `+findingRepo+` AND id = $3`, instance, project, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return SecurityFinding{}, false, nil
	}
	if err != nil {
		return SecurityFinding{}, false, err
	}
	return f, true, nil
}

// SecurityFindingEvents returns what happened to a finding, oldest first.
// The caller has checked the finding is of a repository it may see.
func (s *Store) SecurityFindingEvents(ctx context.Context, id int64) ([]SecurityFindingEvent, error) {
	rows, err := s.pool.Query(ctx, `SELECT e.kind, COALESCE(e.scan_id::text, ''), e.actor, e.reason, e.comment, e.at,
			COALESCE(sc.number, 0)
		FROM security_finding_events e LEFT JOIN security_scans sc ON sc.id = e.scan_id
		WHERE e.finding_id = $1 ORDER BY e.at, e.id`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[SecurityFindingEvent])
}

// DismissSecurityFinding dismisses an open finding of a repository, with
// one of the dismiss reasons, a comment and who did it, and records a
// dismissed event. changed is false when the repository has no open finding
// id. An unknown reason is an error. The project is matched ignoring case.
func (s *Store) DismissSecurityFinding(ctx context.Context, instance, project string, id int64, reason, comment, by string, now time.Time) (bool, error) {
	if !slices.Contains(SecurityDismissReasons, reason) {
		return false, fmt.Errorf("store: %q is not a reason to dismiss a security finding", reason)
	}
	comment, by = cutText(comment), cutText(by)
	tag, err := s.pool.Exec(ctx, `WITH d AS (
			UPDATE security_findings SET state = 'dismissed', dismiss_reason = $4, dismiss_comment = $5,
				dismissed_by = $6, dismissed_at = $7, updated_at = $7
			WHERE `+findingRepo+` AND id = $3 AND state = 'open'
			RETURNING id
		)
		INSERT INTO security_finding_events (finding_id, kind, actor, reason, comment, at)
		SELECT id, 'dismissed', $6, $4, $5, $7 FROM d`, instance, project, id, reason, comment, by, now)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// RestoreSecurityFinding makes a dismissed finding of a repository open
// again, clears its dismissal and records a restored event by who did it.
// changed is false when the repository has no dismissed finding id. The
// project is matched ignoring case.
func (s *Store) RestoreSecurityFinding(ctx context.Context, instance, project string, id int64, by string, now time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `WITH r AS (
			UPDATE security_findings SET state = 'open', dismiss_reason = NULL, dismiss_comment = '',
				dismissed_by = '', dismissed_at = NULL, updated_at = $5
			WHERE `+findingRepo+` AND id = $3 AND state = 'dismissed'
			RETURNING id
		)
		INSERT INTO security_finding_events (finding_id, kind, actor, at)
		SELECT id, 'restored', $4, $5 FROM r`, instance, project, id, cutText(by), now)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// SecurityCountsOf counts a repository's open findings. The project is
// matched ignoring case.
func (s *Store) SecurityCountsOf(ctx context.Context, instance, project string) (SecurityCounts, error) {
	var c SecurityCounts
	err := s.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE severity = 'critical'),
			count(*) FILTER (WHERE severity = 'high'), count(*) FILTER (WHERE severity = 'medium'),
			count(*) FILTER (WHERE severity = 'low'), count(*) FILTER (WHERE needs_validation)
		FROM security_findings WHERE `+findingRepo+` AND state = 'open'`, instance, project).
		Scan(&c.Critical, &c.High, &c.Medium, &c.Low, &c.NeedsValidation)
	return c, err
}

// LastCompleteSecurityState returns the state of a repository's last
// complete scan; found is false when it has none, or that scan sent no
// state. The project is matched ignoring case.
func (s *Store) LastCompleteSecurityState(ctx context.Context, instance, project string) (map[string]string, bool, error) {
	var raw *string
	err := s.pool.QueryRow(ctx, `SELECT state_files::text FROM security_scans
		WHERE `+findingRepo+` AND state = 'complete' ORDER BY number DESC LIMIT 1`, instance, project).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && raw == nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var state map[string]string
	if err := json.Unmarshal([]byte(*raw), &state); err != nil {
		return nil, false, fmt.Errorf("store: security scan state: %w", err)
	}
	return state, true, nil
}

// DismissedSecurityFindings returns a repository's dismissed findings, by
// file. The project is matched ignoring case.
func (s *Store) DismissedSecurityFindings(ctx context.Context, instance, project string) ([]SecurityFinding, error) {
	return s.queryFindings(ctx, `SELECT `+findingColumns+` FROM security_findings
		WHERE `+findingRepo+` AND state = 'dismissed' ORDER BY file, start_line, id`, instance, project)
}

// OpenSecurityFindingsIn returns a repository's open findings in the given
// files, most severe first. No files is no findings. The project is matched
// ignoring case.
func (s *Store) OpenSecurityFindingsIn(ctx context.Context, instance, project string, files []string) ([]SecurityFinding, error) {
	if len(files) == 0 {
		return nil, nil
	}
	return s.queryFindings(ctx, `SELECT `+findingColumns+` FROM security_findings
		WHERE `+findingRepo+` AND state = 'open' AND file = ANY($3::text[]) `+findingOrder, instance, project, files)
}

// SecurityScanSARIF returns the SARIF a scan kept; found is false when the
// scan does not exist or no longer keeps it.
func (s *Store) SecurityScanSARIF(ctx context.Context, id string) ([]byte, bool, error) {
	if !fetchID.MatchString(id) {
		return nil, false, nil
	}
	var raw *string
	err := s.pool.QueryRow(ctx, `SELECT sarif::text FROM security_scans WHERE id = $1::uuid`, id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && raw == nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return []byte(*raw), true, nil
}
