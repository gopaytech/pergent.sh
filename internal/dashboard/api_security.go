package dashboard

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gopaytech/pergent.sh/internal/chain"
	"github.com/gopaytech/pergent.sh/internal/instances"
	"github.com/gopaytech/pergent.sh/internal/security"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// The security scan routes, under /api/repositories/{instance}/{project...}
// /security. They hang off the repository's {rest...} routes: securityRoute
// serves one when rest ends in one of their shapes and the project part is a
// repository the session sees, before the repository's other routes.

const (
	// securityScansShown is how many scans the Scans list shows, newest
	// first.
	securityScansShown = 100
	// maxSecurityComment is the longest dismiss comment, in characters.
	maxSecurityComment = 1000
	// maxSecurityFilter is the longest rule and q of the findings list, in
	// characters.
	maxSecurityFilter = 200
	// sourceRepositoryChain is ChainView.source for a scan chain with none of
	// its own: it shows the repository's model chain in effect.
	sourceRepositoryChain = "chain"

	msgBadSecurityState    = "state must be open, dismissed or fixed"
	msgBadSecuritySeverity = "severity must be critical, high, medium or low"
	msgSecurityFilterLong  = "rule and q must be at most 200 characters"
	msgBadSecurityDismiss  = `the body must be {"reason": ..., "comment": ...}`
	msgBadSecurityReason   = "reason must be false_positive, wont_fix, used_in_tests, mitigated or acceptable_risk"
	msgSecurityCommentLong = "the comment is over 1000 characters"
	msgFixedNotDismissed   = "a fixed finding can't be dismissed"
	msgFixedNotRestored    = "a fixed finding can't be restored"
	msgBadSecuritySettings = `the body must be {"maxSteps", "subagentMaxSteps", "timeoutSeconds", "findingsInReviews"}: numbers, true or false, or null for the default`
	msgBadSecurityTimeout  = "the scan timeout must be from 600 to 21600 seconds (10 minutes to 6 hours)"
	msgEmptyScanChain      = "a scan chain needs an entry: reset it to use the repository's model chain"
	msgNeedsRoleSecurity   = msgNeedsRoleReview
)

// securityPath is one route under .../security: Kind is "" (the tab),
// "findings", "scans", "settings" or "chain"; ID is a finding's number or a
// scan's UUID; Action is "dismiss", "restore", "sarif", "cancel" or "reset".
type securityPath struct {
	Kind   string
	ID     string
	Action string
}

var (
	securityFindingID = regexp.MustCompile(`^[1-9][0-9]{0,17}$`)
	securityScanID    = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// parseSecurityPath matches the end of rest against the security routes:
// .../security, .../security/{findings,scans,settings,chain},
// .../security/findings/{id}[/dismiss|/restore],
// .../security/scans/{id}[/sarif|/cancel] and .../security/chain/reset. A
// finding's ID is a positive number and a scan's a UUID, so no rest matches
// two shapes. project is what comes before /security; ok is false when rest
// is no such route or project is empty.
func parseSecurityPath(rest string) (project string, p securityPath, ok bool) {
	parts := strings.Split(rest, "/")
	n := len(parts)
	// at reads the route from part i ("security") on.
	at := func(i int, p securityPath) (string, securityPath, bool) {
		if i < 1 || parts[i] != "security" {
			return "", securityPath{}, false
		}
		project := strings.Join(parts[:i], "/")
		return project, p, project != ""
	}
	last := parts[n-1]
	switch {
	case last == "security":
		return at(n-1, securityPath{})
	case last == "findings" || last == "scans" || last == "settings" || last == "chain":
		return at(n-2, securityPath{Kind: last})
	case n >= 3 && last == "reset" && parts[n-2] == "chain":
		return at(n-3, securityPath{Kind: "chain", Action: last})
	case n >= 3 && parts[n-2] == "findings" && securityFindingID.MatchString(last):
		return at(n-3, securityPath{Kind: "findings", ID: last})
	case n >= 3 && parts[n-2] == "scans" && securityScanID.MatchString(last):
		return at(n-3, securityPath{Kind: "scans", ID: last})
	case n >= 4 && (last == "dismiss" || last == "restore") && parts[n-3] == "findings" &&
		securityFindingID.MatchString(parts[n-2]):
		return at(n-4, securityPath{Kind: "findings", ID: parts[n-2], Action: last})
	case n >= 4 && (last == "sarif" || last == "cancel") && parts[n-3] == "scans" && securityScanID.MatchString(parts[n-2]):
		return at(n-4, securityPath{Kind: "scans", ID: parts[n-2], Action: last})
	}
	return "", securityPath{}, false
}

// securityHandles reports whether method is a route of p.
func securityHandles(method string, p securityPath) bool {
	switch method {
	case http.MethodGet:
		return p.Action == "" || p.Action == "sarif"
	case http.MethodPost:
		switch p.Kind {
		case "findings":
			return p.Action == "dismiss" || p.Action == "restore"
		case "scans":
			return (p.ID == "" && p.Action == "") || p.Action == "cancel"
		case "chain":
			return p.Action == "reset"
		}
	case http.MethodPut:
		return (p.Kind == "settings" || p.Kind == "chain") && p.Action == ""
	}
	return false
}

// securityRoute serves the request when it is a security route of a
// repository the session sees, and reports whether it did. It does nothing,
// and the caller goes on with the repository's other routes, when there are
// no security scans (no engine), rest is no security route for the method,
// or the part before /security is not such a repository: a repository whose
// own path ends in /security keeps its routes.
func (d *Dashboard) securityRoute(w http.ResponseWriter, r *http.Request) bool {
	if d.securityScans == nil {
		return false
	}
	project, p, ok := parseSecurityPath(r.PathValue("rest"))
	if !ok || !securityHandles(r.Method, p) {
		return false
	}
	ctx, se := r.Context(), session(r)
	repos, err := d.visibleRepos(ctx, se)
	if err != nil {
		d.failed(w, r, "listing repositories", err)
		return true
	}
	instance := r.PathValue("instance")
	i := slices.IndexFunc(repos, func(e settings.Effective) bool {
		return e.Instance == instance && strings.EqualFold(e.Project, project)
	})
	if i < 0 {
		return false
	}
	d.serveSecurity(w, r, se, repos[i], p)
	return true
}

// serveSecurity serves route p of repo; securityHandles has checked the
// method.
func (d *Dashboard) serveSecurity(w http.ResponseWriter, r *http.Request, se Session, repo settings.Effective, p securityPath) {
	get := r.Method == http.MethodGet
	switch p.Kind {
	case "":
		d.securityTab(w, r, se, repo)
	case "findings":
		switch {
		case p.ID == "":
			d.securityFindings(w, r, repo)
		case get:
			d.securityFinding(w, r, se, repo, p.ID)
		default:
			d.securityFindingAction(w, r, se, repo, p.ID, p.Action)
		}
	case "scans":
		switch {
		case p.ID == "" && get:
			d.securityScanList(w, r, repo)
		case p.ID == "":
			d.securityStart(w, r, se, repo)
		case p.Action == "sarif":
			d.securitySARIF(w, r, repo, p.ID)
		case get:
			d.securityScanDetail(w, r, repo, p.ID)
		default:
			d.securityCancel(w, r, se, repo, p.ID)
		}
	case "settings":
		if get {
			d.securitySettings(w, r, se, repo)
		} else {
			d.putSecuritySettings(w, r, se, repo)
		}
	case "chain":
		switch {
		case get:
			d.writeScanChain(w, r, se, repo)
		case p.Action == "reset":
			d.resetScanChain(w, r, se, repo)
		default:
			d.putScanChain(w, r, se, repo)
		}
	}
}

// securityWriter answers 403 and returns false when se is not a maintainer
// or an admin of repo.
func securityWriter(w http.ResponseWriter, se Session, repo settings.Effective) bool {
	if se.Role(repo.Instance, repo.Project) < RoleMaintainer {
		writeError(w, http.StatusForbidden, msgNeedsRoleSecurity)
		return false
	}
	return true
}

// --- views ---

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// securityScanView is sc as the API shows it, with its tokens from usage (by
// run ID) when they are there.
func securityScanView(sc store.SecurityScan, usage map[string]store.TokenSum) SecurityScanView {
	v := SecurityScanView{ID: sc.ID, Number: sc.Number, State: sc.State, Reason: sc.Reason, StartedBy: sc.StartedBy,
		QueuedAt: sc.QueuedAt.UTC(), StartedAt: utcPtr(sc.StartedAt), FinishedAt: utcPtr(sc.FinishedAt),
		Branch: sc.Branch, Commit: sc.Commit, Model: sc.EntryName, SkillName: sc.SkillName, SkillCommit: sc.SkillCommit,
		SkillUpdated: sc.SkillUpdated, UsageSource: sc.UsageSource, NewCount: sc.NewCount, FixedCount: sc.FixedCount,
		HasSARIF: sc.HasSARIF, CancelRequested: sc.CancelRequestedAt != nil}
	if sc.StartedAt != nil && sc.FinishedAt != nil {
		s := int64(sc.FinishedAt.Sub(*sc.StartedAt) / time.Second)
		v.DurationSeconds = &s
	}
	if sc.CoverageDone != nil && sc.CoveragePlanned != nil {
		v.Coverage = &SecurityCoverageView{Done: *sc.CoverageDone, Planned: *sc.CoveragePlanned}
	}
	if t, ok := usage[sc.RunID]; ok {
		u := usageTokens(t)
		v.Tokens = &u
	}
	return v
}

// scanUsage reads the tokens of scans, by run ID.
func (d *Dashboard) scanUsage(ctx context.Context, scans ...*store.SecurityScan) (map[string]store.TokenSum, error) {
	ids := make([]string, 0, len(scans))
	for _, sc := range scans {
		if sc != nil {
			ids = append(ids, sc.RunID)
		}
	}
	return d.Store.RunUsages(ctx, ids)
}

func securityFindingRow(f store.SecurityFinding) SecurityFindingRowView {
	return SecurityFindingRowView{ID: f.ID, State: f.State, Severity: f.Severity, RuleID: f.RuleID, CWE: f.CWE,
		Title: f.Title, File: f.File, StartLine: f.StartLine, NeedsValidation: f.NeedsValidation,
		FirstSeenAt: f.FirstSeenAt.UTC(), FirstScan: f.FirstScanNumber}
}

func securityFindingRows(list []store.SecurityFinding) []SecurityFindingRowView {
	out := make([]SecurityFindingRowView, 0, len(list))
	for _, f := range list {
		out = append(out, securityFindingRow(f))
	}
	return out
}

// scanModel is the name of the entry of repo's scan chain a scan would run
// on now ("" when none can be used): the chain resolved as the launch does,
// with the limits but not the quotas.
func (d *Dashboard) scanModel(ctx context.Context, repo settings.Effective) (string, error) {
	in, err := chain.LoadScan(ctx, d.Store, d.registry, d.now(), repo)
	if err != nil {
		return "", err
	}
	if dec := chain.Resolve(in); dec.Runs() {
		return dec.Entry.Name, nil
	}
	return "", nil
}

// --- GET .../security ---

// securityTab serves the Security tab: the running and the last scan, the
// open findings' counts, and what Start scan would run with.
func (d *Dashboard) securityTab(w http.ResponseWriter, r *http.Request, se Session, repo settings.Effective) {
	ctx := r.Context()
	scans, err := d.Store.SecurityScans(ctx, repo.Instance, repo.Project, securityScansShown)
	if err != nil {
		d.failed(w, r, "reading the security scans", err)
		return
	}
	var running, last, estimate *store.SecurityScan
	for i := range scans {
		sc := &scans[i]
		switch {
		case sc.State == store.SecurityQueued || sc.State == store.SecurityRunning:
			if running == nil {
				running = sc
			}
		default:
			if last == nil {
				last = sc
			}
			if estimate == nil && sc.State == store.SecurityComplete {
				estimate = sc
			}
		}
	}
	usage, err := d.scanUsage(ctx, running, last, estimate)
	if err != nil {
		d.failed(w, r, "reading the security scans' usage", err)
		return
	}
	counts, err := d.Store.SecurityCountsOf(ctx, repo.Instance, repo.Project)
	if err != nil {
		d.failed(w, r, "counting the security findings", err)
		return
	}
	set, err := d.Store.SecurityScanSettingsOf(ctx, repo.Instance, repo.Project)
	if err != nil {
		d.failed(w, r, "reading the security scan settings", err)
		return
	}
	model, err := d.scanModel(ctx, repo)
	if err != nil {
		d.failed(w, r, "reading the scan chain", err)
		return
	}
	off := d.securityScans.StartOff()
	if off == "" && repo.Paused {
		off = msgPaused
	}
	manage := se.Role(repo.Instance, repo.Project) >= RoleMaintainer
	view := SecurityView{
		CanStart: manage && !repo.Paused && running == nil && off == "", StartOff: off, CanManage: manage,
		Counts: SecurityCountsView{Critical: counts.Critical, High: counts.High, Medium: counts.Medium, Low: counts.Low,
			NeedsValidation: counts.NeedsValidation},
		Start: SecurityStartView{Model: model, MaxSteps: orDefault(set.MaxSteps, security.DefaultMaxSteps),
			SubagentMaxSteps:  orDefault(set.SubagentMaxSteps, security.DefaultSubagentMaxSteps),
			StepsFromSkill:    set.MaxSteps == nil && set.SubagentMaxSteps == nil,
			MaxStepsFromSkill: set.MaxSteps == nil, SubagentMaxStepsFromSkill: set.SubagentMaxSteps == nil,
			TimeoutSeconds: orDefault(set.TimeoutSeconds, int(security.DefaultTimeout/time.Second))},
	}
	if running != nil {
		v := securityScanView(*running, usage)
		view.Running = &v
	}
	if last != nil {
		v := securityScanView(*last, usage)
		view.Last = &v
	}
	if estimate != nil {
		e := securityScanView(*estimate, usage)
		if e.Tokens != nil {
			view.Start.LastTokens = &e.Tokens.Total
		}
		view.Start.LastSeconds = e.DurationSeconds
	}
	writeJSON(w, http.StatusOK, view)
}

func orDefault(v *int, def int) int {
	if v == nil {
		return def
	}
	return *v
}

// --- GET .../security/findings ---

// securityFindings lists the repository's findings that match the filters
// state, severity, rule (an attack class) and q (in the file or the title).
func (d *Dashboard) securityFindings(w http.ResponseWriter, r *http.Request, repo settings.Effective) {
	q := r.URL.Query()
	f := store.SecurityFindingFilter{State: q.Get("state"), Severity: q.Get("severity"), Rule: q.Get("rule"),
		Query: q.Get("q")}
	switch {
	case f.State != "" && !slices.Contains([]string{store.FindingOpen, store.FindingDismissed, store.FindingFixed}, f.State):
		writeError(w, http.StatusBadRequest, msgBadSecurityState)
		return
	case f.Severity != "" && !slices.Contains([]string{store.SeverityCritical, store.SeverityHigh, store.SeverityMedium,
		store.SeverityLow}, f.Severity):
		writeError(w, http.StatusBadRequest, msgBadSecuritySeverity)
		return
	case utf8.RuneCountInString(f.Rule) > maxSecurityFilter || utf8.RuneCountInString(f.Query) > maxSecurityFilter:
		writeError(w, http.StatusBadRequest, msgSecurityFilterLong)
		return
	}
	rows, truncated, err := d.Store.SecurityFindings(r.Context(), repo.Instance, repo.Project, f)
	if err != nil {
		d.failed(w, r, "listing the security findings", err)
		return
	}
	writeJSON(w, http.StatusOK, SecurityFindingsView{Findings: securityFindingRows(rows), Truncated: truncated})
}

// --- GET .../security/findings/{id} ---

// repoFinding is the finding id of repo; ok is false when it answered: 404
// for one the repository does not have.
func (d *Dashboard) repoFinding(w http.ResponseWriter, r *http.Request, repo settings.Effective, id string) (store.SecurityFinding, bool) {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		notFound(w)
		return store.SecurityFinding{}, false
	}
	f, found, err := d.Store.SecurityFinding(r.Context(), repo.Instance, repo.Project, n)
	if err != nil {
		d.failed(w, r, "loading a security finding", err)
		return store.SecurityFinding{}, false
	}
	if !found {
		notFound(w)
		return store.SecurityFinding{}, false
	}
	return f, true
}

func (d *Dashboard) securityFinding(w http.ResponseWriter, r *http.Request, se Session, repo settings.Effective, id string) {
	f, ok := d.repoFinding(w, r, repo, id)
	if !ok {
		return
	}
	d.writeFinding(w, r, se, repo, f)
}

// writeFinding answers with f, its history and its link to the code.
func (d *Dashboard) writeFinding(w http.ResponseWriter, r *http.Request, se Session, repo settings.Effective, f store.SecurityFinding) {
	ctx := r.Context()
	events, err := d.Store.SecurityFindingEvents(ctx, f.ID)
	if err != nil {
		d.failed(w, r, "loading a security finding's history", err)
		return
	}
	link, err := d.findingCodeURL(ctx, repo, f)
	if err != nil {
		d.failed(w, r, "loading a security finding's scan", err)
		return
	}
	v := SecurityFindingView{Finding: securityFindingRow(f), Description: f.Description, Exploit: f.Exploit,
		FixHint: f.FixHint, Confidence: f.Confidence, Tags: nonNil(slices.Clone(f.Tags)), CodeURL: link,
		History: make([]SecurityEventView, 0, len(events)), CanManage: se.Role(repo.Instance, repo.Project) >= RoleMaintainer}
	if f.State == store.FindingDismissed && f.DismissedAt != nil {
		v.Dismissal = &SecurityDismissalView{Reason: f.DismissReason, Comment: f.DismissComment, By: f.DismissedBy,
			At: f.DismissedAt.UTC()}
	}
	for _, e := range events {
		v.History = append(v.History, SecurityEventView{Kind: e.Kind, Scan: e.ScanNumber, Actor: e.Actor, Reason: e.Reason,
			Comment: e.Comment, At: e.At.UTC()})
	}
	writeJSON(w, http.StatusOK, v)
}

// findingCodeURL links f's line on the platform, at the commit of the last
// scan that found it: an http or https address, or "" when a part is missing.
func (d *Dashboard) findingCodeURL(ctx context.Context, repo settings.Effective, f store.SecurityFinding) (string, error) {
	sc, err := d.Store.SecurityScan(ctx, f.LastScanID)
	if errors.Is(err, store.ErrSecurityScanNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	p, found, err := d.Store.ProjectByPath(ctx, repo.Instance, repo.Project)
	if err != nil || !found {
		return "", err
	}
	platform, _ := d.registry.Platform(repo.Instance)
	return codeLink(security.CodeURL(platform, p.HTTPURLToRepo, sc.Commit, f.File, f.StartLine)), nil
}

// codeLink is u when it is an absolute http or https address with no user or
// password in it, else "": the page links to it.
func codeLink(u string) string {
	p, err := url.Parse(u)
	if err != nil || p.User != nil {
		return ""
	}
	return safeURL(u)
}

// --- POST .../security/findings/{id}/dismiss and .../restore ---

// securityFindingAction dismisses an open finding ({"reason", "comment"}) or
// restores a dismissed one ({}), for a maintainer or an admin. A finding in
// that state already changes nothing (200, no audit entry); a fixed one is
// 409.
func (d *Dashboard) securityFindingAction(w http.ResponseWriter, r *http.Request, se Session, repo settings.Effective, id, action string) {
	if !securityWriter(w, se, repo) {
		return
	}
	ctx := r.Context()
	var body struct {
		Reason  string `json:"reason"`
		Comment string `json:"comment"`
	}
	if action == "dismiss" {
		if err := decodeStrict(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, msgBadSecurityDismiss)
			return
		}
		if !slices.Contains(store.SecurityDismissReasons, body.Reason) {
			writeError(w, http.StatusBadRequest, msgBadSecurityReason)
			return
		}
		if utf8.RuneCountInString(body.Comment) > maxSecurityComment {
			writeError(w, http.StatusBadRequest, msgSecurityCommentLong)
			return
		}
	}
	f, ok := d.repoFinding(w, r, repo, id)
	if !ok {
		return
	}
	if f.State == store.FindingFixed {
		msg := msgFixedNotDismissed
		if action == "restore" {
			msg = msgFixedNotRestored
		}
		writeError(w, http.StatusConflict, msg)
		return
	}
	by, now := actorLabel(se.Kind, se.Username), d.now()
	var changed bool
	var err error
	if action == "dismiss" {
		changed, err = d.Store.DismissSecurityFinding(ctx, repo.Instance, repo.Project, f.ID, body.Reason, body.Comment, by, now)
	} else {
		changed, err = d.Store.RestoreSecurityFinding(ctx, repo.Instance, repo.Project, f.ID, by, now)
	}
	if err != nil {
		d.Log.Error("dashboard: changing a security finding failed", "instance", repo.Instance, "project", repo.Project,
			"finding", f.ID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	if changed {
		detail := "fingerprint=" + shortFingerprint(f.Fingerprint)
		what, auditAction := "restored", "security_finding_restored"
		if action == "dismiss" {
			detail += " reason=" + body.Reason
			what, auditAction = "dismissed", "security_finding_dismissed"
		}
		d.Log.Info("dashboard: security finding "+what, "instance", repo.Instance, "project", repo.Project,
			"finding", f.ID, "by", se.Username)
		d.audit(ctx, se, auditAction, repo.Instance, repo.Project, detail)
	}
	f, ok = d.repoFinding(w, r, repo, id)
	if !ok {
		return
	}
	d.writeFinding(w, r, se, repo, f)
}

// shortFingerprint is the first 12 characters of a fingerprint, as the
// audit log names a finding.
func shortFingerprint(fp string) string {
	if n := 12; utf8.RuneCountInString(fp) > n {
		return string([]rune(fp)[:n])
	}
	return fp
}

// --- GET .../security/scans and .../security/scans/{id} ---

func (d *Dashboard) securityScanList(w http.ResponseWriter, r *http.Request, repo settings.Effective) {
	ctx := r.Context()
	scans, err := d.Store.SecurityScans(ctx, repo.Instance, repo.Project, securityScansShown)
	if err != nil {
		d.failed(w, r, "reading the security scans", err)
		return
	}
	ptrs := make([]*store.SecurityScan, len(scans))
	for i := range scans {
		ptrs[i] = &scans[i]
	}
	usage, err := d.scanUsage(ctx, ptrs...)
	if err != nil {
		d.failed(w, r, "reading the security scans' usage", err)
		return
	}
	view := SecurityScansView{Scans: make([]SecurityScanView, 0, len(scans))}
	for _, sc := range scans {
		view.Scans = append(view.Scans, securityScanView(sc, usage))
	}
	writeJSON(w, http.StatusOK, view)
}

// repoScan is the scan id of repo; ok is false when it answered: 404 for a
// scan of another repository, or none.
func (d *Dashboard) repoScan(w http.ResponseWriter, r *http.Request, repo settings.Effective, id string) (store.SecurityScan, bool) {
	sc, err := d.Store.SecurityScan(r.Context(), id)
	if errors.Is(err, store.ErrSecurityScanNotFound) ||
		(err == nil && (sc.Instance != repo.Instance || !strings.EqualFold(sc.Project, repo.Project))) {
		notFound(w)
		return store.SecurityScan{}, false
	}
	if err != nil {
		d.failed(w, r, "loading a security scan", err)
		return store.SecurityScan{}, false
	}
	return sc, true
}

func (d *Dashboard) securityScanDetail(w http.ResponseWriter, r *http.Request, repo settings.Effective, id string) {
	ctx := r.Context()
	sc, ok := d.repoScan(w, r, repo, id)
	if !ok {
		return
	}
	usage, err := d.scanUsage(ctx, &sc)
	if err != nil {
		d.failed(w, r, "reading a security scan's usage", err)
		return
	}
	log, _, err := d.Store.RunLog(ctx, sc.RunID)
	if err != nil {
		d.failed(w, r, "loading a security scan's log", err)
		return
	}
	found, fixed, err := d.Store.SecurityScanFindings(ctx, repo.Instance, repo.Project, sc.ID)
	if err != nil {
		d.failed(w, r, "loading a security scan's findings", err)
		return
	}
	writeJSON(w, http.StatusOK, SecurityScanDetailView{Scan: securityScanView(sc, usage), Log: log,
		New: securityFindingRows(found), Fixed: securityFindingRows(fixed)})
}

// --- GET .../security/scans/{id}/sarif ---

// sarifName is what a scan's SARIF download is called: the last part of the
// project path, with only letters, digits, '.', '_' and '-', then
// "-scan-<number>.sarif".
func sarifName(project string, number int64) string {
	name := strings.Map(func(c rune) rune {
		if c < 128 && (c == '.' || c == '_' || c == '-' || ('0' <= c && c <= '9') || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')) {
			return c
		}
		return -1
	}, path.Base(project))
	if name == "" || strings.Trim(name, ".") == "" {
		name = "repository"
	}
	return fmt.Sprintf("%s-scan-%d.sarif", name, number)
}

// securitySARIF sends the SARIF a scan keeps, as a file to save; 404 once
// retention dropped it.
func (d *Dashboard) securitySARIF(w http.ResponseWriter, r *http.Request, repo settings.Effective, id string) {
	sc, ok := d.repoScan(w, r, repo, id)
	if !ok {
		return
	}
	sarif, found, err := d.Store.SecurityScanSARIF(r.Context(), sc.ID)
	if err != nil {
		d.failed(w, r, "loading a security scan's SARIF", err)
		return
	}
	if !found {
		notFound(w)
		return
	}
	w.Header().Set("Content-Type", "application/sarif+json")
	w.Header().Set("Content-Disposition", `attachment; filename="`+sarifName(repo.Project, sc.Number)+`"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(sarif)
}

// --- POST .../security/scans and .../security/scans/{id}/cancel ---

// securityConflict is a 409 of Start scan or Cancel: why, and the scan it is
// about when there is one (the active scan for busy, the ended one for a
// cancel).
type securityConflict struct {
	Error string            `json:"error"`
	Scan  *SecurityScanView `json:"scan,omitempty"`
}

// securityStart queues a scan for a maintainer or an admin; an admin's goes
// past the quotas and the admin limits. Only an accepted start is audited.
func (d *Dashboard) securityStart(w http.ResponseWriter, r *http.Request, se Session, repo settings.Effective) {
	if !securityWriter(w, se, repo) || refusePaused(w, repo) {
		return
	}
	ctx := r.Context()
	model, err := d.scanModel(ctx, repo)
	if err != nil {
		d.failed(w, r, "reading the scan chain", err)
		return
	}
	sc, err := d.securityScans.RequestSecurityScan(ctx, security.Start{Instance: repo.Instance, Project: repo.Project,
		Actor: actorLabel(se.Kind, se.Username), Admin: se.Admin})
	var team *security.TeamLimitError
	var refused *security.RefusedError
	switch {
	case errors.Is(err, security.ErrBusy):
		v := securityScanView(sc, nil)
		writeJSON(w, http.StatusConflict, securityConflict{Error: security.ErrBusy.Error(), Scan: &v})
	case errors.As(err, &team):
		writeError(w, http.StatusConflict, fmt.Sprintf(msgTeamLimitUsedUp, team.Team))
	case errors.As(err, &refused):
		writeError(w, http.StatusConflict, refused.Error())
	case errors.Is(err, security.ErrOff), errors.Is(err, security.ErrPaused), errors.Is(err, security.ErrNotOn),
		errors.Is(err, security.ErrQuota):
		writeError(w, http.StatusConflict, err.Error())
	case err != nil:
		d.failed(w, r, "queueing a security scan", err)
	default:
		if model == "" {
			model = "(none)"
		}
		d.Log.Info("dashboard: security scan started", "instance", repo.Instance, "project", repo.Project, "scan", sc.ID,
			"by", se.Username)
		d.audit(ctx, se, "security_scan_started", repo.Instance, repo.Project,
			fmt.Sprintf("scan=%d model=%s", sc.Number, model))
		v := securityScanView(sc, nil)
		writeJSON(w, http.StatusAccepted, struct {
			Scan SecurityScanView `json:"scan"`
		}{v})
	}
}

// securityCancel asks a queued or running scan to stop, for a maintainer or
// an admin: 200 with the scan, 409 with it when it has ended.
func (d *Dashboard) securityCancel(w http.ResponseWriter, r *http.Request, se Session, repo settings.Effective, id string) {
	if !securityWriter(w, se, repo) {
		return
	}
	ctx := r.Context()
	sc, err := d.securityScans.CancelSecurityScan(ctx, repo.Instance, repo.Project, id)
	switch {
	case errors.Is(err, store.ErrSecurityScanNotFound):
		notFound(w)
	case errors.Is(err, security.ErrEnded):
		v := securityScanView(sc, nil)
		writeJSON(w, http.StatusConflict, securityConflict{Error: security.ErrEnded.Error(), Scan: &v})
	case err != nil:
		d.failed(w, r, "cancelling a security scan", err)
	default:
		d.Log.Info("dashboard: security scan cancelled", "instance", repo.Instance, "project", repo.Project, "scan", sc.ID,
			"by", se.Username)
		d.audit(ctx, se, "security_scan_cancelled", repo.Instance, repo.Project, fmt.Sprintf("scan=%d", sc.Number))
		writeJSON(w, http.StatusOK, struct {
			Scan SecurityScanView `json:"scan"`
		}{securityScanView(sc, nil)})
	}
}

// --- GET and PUT .../security/settings ---

func (d *Dashboard) securitySettings(w http.ResponseWriter, r *http.Request, se Session, repo settings.Effective) {
	set, err := d.Store.SecurityScanSettingsOf(r.Context(), repo.Instance, repo.Project)
	if err != nil {
		d.failed(w, r, "reading the security scan settings", err)
		return
	}
	writeJSON(w, http.StatusOK, SecuritySettingsView{MaxSteps: set.MaxSteps, SubagentMaxSteps: set.SubagentMaxSteps,
		TimeoutSeconds: set.TimeoutSeconds, FindingsInReviews: set.FindingsInReviews,
		Defaults: SecurityDefaultsView{MaxSteps: security.DefaultMaxSteps, SubagentMaxSteps: security.DefaultSubagentMaxSteps,
			TimeoutSeconds: int(security.DefaultTimeout / time.Second)},
		CanEdit: se.Role(repo.Instance, repo.Project) >= RoleMaintainer})
}

// putSecuritySettings replaces the repository's scan settings, for a
// maintainer or an admin: {maxSteps, subagentMaxSteps, timeoutSeconds,
// findingsInReviews}, each null (or left out) for the default. A value out of
// its range is 400. Each changed setting is in one settings_updated entry;
// no change writes nothing.
func (d *Dashboard) putSecuritySettings(w http.ResponseWriter, r *http.Request, se Session, repo settings.Effective) {
	if !securityWriter(w, se, repo) {
		return
	}
	ctx := r.Context()
	var body struct {
		MaxSteps          *int  `json:"maxSteps"`
		SubagentMaxSteps  *int  `json:"subagentMaxSteps"`
		TimeoutSeconds    *int  `json:"timeoutSeconds"`
		FindingsInReviews *bool `json:"findingsInReviews"`
	}
	if err := decodeStrict(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, msgBadSecuritySettings)
		return
	}
	if err := security.CheckLimits(body.MaxSteps, body.SubagentMaxSteps, security.DefaultTimeout); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if t := body.TimeoutSeconds; t != nil &&
		(*t < int(security.MinTimeout/time.Second) || *t > int(security.MaxTimeout/time.Second)) {
		writeError(w, http.StatusBadRequest, msgBadSecurityTimeout)
		return
	}
	next := store.SecurityScanSettings{MaxSteps: body.MaxSteps, SubagentMaxSteps: body.SubagentMaxSteps,
		TimeoutSeconds: body.TimeoutSeconds, FindingsInReviews: body.FindingsInReviews != nil && *body.FindingsInReviews}
	cur, err := d.Store.SecurityScanSettingsOf(ctx, repo.Instance, repo.Project)
	if err != nil {
		d.failed(w, r, "reading the security scan settings", err)
		return
	}
	var changes []string
	for _, c := range []struct {
		key       string
		cur, next *int
	}{{"maxSteps", cur.MaxSteps, next.MaxSteps}, {"subagentMaxSteps", cur.SubagentMaxSteps, next.SubagentMaxSteps},
		{"timeoutSeconds", cur.TimeoutSeconds, next.TimeoutSeconds}} {
		if (c.cur == nil) != (c.next == nil) || (c.cur != nil && *c.cur != *c.next) {
			value := "default"
			if c.next != nil {
				value = strconv.Itoa(*c.next)
			}
			changes = append(changes, "security."+c.key+"="+value)
		}
	}
	if cur.FindingsInReviews != next.FindingsInReviews {
		changes = append(changes, "security.findingsInReviews="+strconv.FormatBool(next.FindingsInReviews))
	}
	if len(changes) > 0 {
		if err := d.Store.PutSecurityScanSettings(ctx, repo.Instance, repo.Project, next, d.now()); err != nil {
			d.Log.Error("dashboard: saving the security scan settings failed", "instance", repo.Instance,
				"project", repo.Project, "error", err.Error())
			writeError(w, http.StatusInternalServerError, msgWriteFailed)
			return
		}
		d.Log.Info("dashboard: security scan settings changed", "instance", repo.Instance, "project", repo.Project,
			"by", se.Username)
		d.audit(ctx, se, "settings_updated", repo.Instance, repo.Project, strings.Join(changes, ", "))
	}
	d.securitySettings(w, r, se, repo)
}

// --- GET, PUT .../security/chain and POST .../security/chain/reset ---

// scanShown is repo's scan chain as chain.LoadScan reads it: its own (source
// "repository"), else the repository's model chain in effect (source
// "chain", with that chain's team). owner is its owner team, 0 for none.
func (d *Dashboard) scanShown(ctx context.Context, repo settings.Effective) (c shownChain, owner int64, err error) {
	c, owner, err = d.repoShown(ctx, repo)
	if err != nil {
		return shownChain{}, 0, err
	}
	own, found, err := d.Store.ChainOf(ctx, store.ChainScan, store.ScopeRepo, 0, repo.Instance, repo.Project)
	if err != nil {
		return shownChain{}, 0, err
	}
	if found && len(own.Entries) > 0 {
		c.in.Scope, c.in.Refs, c.in.SavedByAdmin = store.ScopeRepo, own.Entries, own.SavedByAdmin
		c.source, c.team, c.fixed = "repository", "", 0
	} else {
		c.source = sourceRepositoryChain
	}
	c.scope = store.ScopeRepo
	return c, owner, nil
}

// writeScanChain answers with repo's scan chain as se sees it: anyone who
// sees the repository reads it; the repository chain's write rule says who
// may change it.
func (d *Dashboard) writeScanChain(w http.ResponseWriter, r *http.Request, se Session, repo settings.Effective) {
	ctx := r.Context()
	c, owner, err := d.scanShown(ctx, repo)
	if err != nil {
		d.failed(w, r, "reading the scan chain", err)
		return
	}
	var choices []instances.Model
	if se.Role(repo.Instance, repo.Project) >= RoleMaintainer {
		if choices, _, err = d.repoChoices(ctx, se, owner); err != nil {
			d.failed(w, r, "reading the owner team", err)
			return
		}
	}
	teams, err := d.teamNames(ctx)
	if err != nil {
		d.failed(w, r, "listing teams", err)
		return
	}
	writeJSON(w, http.StatusOK, d.chainView(c, choices, teams, repoClears(se, owner, choices, c.in)))
}

// scanChainChoices are the entries se may put in repo's scan chain, as for
// its model chain: 403 for anyone but an admin or a maintainer of its owner
// team. ok is false when it answered.
func (d *Dashboard) scanChainChoices(w http.ResponseWriter, r *http.Request, se Session, repo settings.Effective) ([]instances.Model, bool) {
	ctx := r.Context()
	owner, _, err := d.Store.OwnerTeam(ctx, repo.Instance, repo.Project)
	if err != nil {
		d.failed(w, r, "reading the owner team", err)
		return nil, false
	}
	choices, msg, err := d.repoChoices(ctx, se, owner)
	if err != nil {
		d.failed(w, r, "reading the owner team", err)
		return nil, false
	}
	if choices == nil {
		writeError(w, http.StatusForbidden, msg)
		return nil, false
	}
	return choices, true
}

// putScanChain gives the repository a scan chain of its own, which its scans
// use in place of its model chain. It records whether an admin saved it, as
// the model chain does.
func (d *Dashboard) putScanChain(w http.ResponseWriter, r *http.Request, se Session, repo settings.Effective) {
	ctx := r.Context()
	choices, ok := d.scanChainChoices(w, r, se, repo)
	if !ok {
		return
	}
	refs, msg := decodeChain(r)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if len(refs) == 0 {
		writeError(w, http.StatusBadRequest, msgEmptyScanChain)
		return
	}
	in, err := d.chainEntries(ctx)
	if err != nil {
		d.failed(w, r, "reading the models", err)
		return
	}
	if msg := checkEntries(refs, choices, in.Entries); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if !d.saveChainOf(ctx, w, store.ChainScan, store.Chain{Scope: store.ScopeRepo, Instance: repo.Instance,
		Project: repo.Project, Entries: refs, SavedByAdmin: se.Admin}) {
		return
	}
	d.Log.Info("dashboard: model chain changed", "chain", "security scan", "instance", repo.Instance,
		"project", repo.Project, "by", se.Username)
	d.audit(ctx, se, "chain_changed", repo.Instance, repo.Project, "security scan: "+entryNames(refs, in.Entries))
	d.writeScanChain(w, r, se, repo)
}

// resetScanChain drops the repository's own scan chain: its scans use its
// model chain again. Who may is who may save it.
func (d *Dashboard) resetScanChain(w http.ResponseWriter, r *http.Request, se Session, repo settings.Effective) {
	ctx := r.Context()
	if _, ok := d.scanChainChoices(w, r, se, repo); !ok {
		return
	}
	if err := d.Store.ResetScanChain(ctx, repo.Instance, repo.Project); err != nil {
		d.Log.Error("dashboard: resetting a scan chain failed", "instance", repo.Instance, "project", repo.Project,
			"error", err.Error())
		writeError(w, http.StatusInternalServerError, msgWriteFailed)
		return
	}
	d.Log.Info("dashboard: scan chain reset", "instance", repo.Instance, "project", repo.Project, "by", se.Username)
	d.audit(ctx, se, "chain_changed", repo.Instance, repo.Project, "security scan: inherit")
	d.writeScanChain(w, r, se, repo)
}
