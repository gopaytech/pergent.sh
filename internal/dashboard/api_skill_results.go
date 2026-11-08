package dashboard

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// minAddressed is how many findings a row needs before it shows a percentage.
const minAddressed = 10

// --- GET /api/repositories/{instance}/{project...}/skill-results ---

// repositorySkillResults serves repo's Skill results tab: what became of the
// findings of each skill, decided in the last 30 or 90 days. Anyone who can
// see the repository may read it.
func (d *Dashboard) repositorySkillResults(w http.ResponseWriter, r *http.Request, se Session, repo settings.Effective) {
	days, ok := skillResultsDays(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	// A repository the platform hasn't told us the ID of has no result.
	id, found, err := d.projectID(ctx, se, repo.Instance, repo.Project)
	if err != nil {
		d.failed(w, r, "reading skill results", err)
		return
	}
	var rows []store.SkillResult
	if found {
		rows, err = d.Store.SkillResults(ctx, repo.Instance, id, d.now().Add(-time.Duration(days)*day))
		if err != nil {
			d.failed(w, r, "reading skill results", err)
			return
		}
	}
	writeJSON(w, http.StatusOK, skillResultViews(rows))
}

// skillResultsDays reads the window of the Skill results tab: 30 days if none
// is given, else 30 or 90. Anything else is answered with a 400.
func skillResultsDays(w http.ResponseWriter, r *http.Request) (int, bool) {
	days := 30
	if r.URL.Query().Has("days") {
		var err error
		if days, err = strconv.Atoi(r.URL.Query().Get("days")); err != nil || (days != 30 && days != 90) {
			writeError(w, http.StatusBadRequest, "days must be 30 or 90")
			return 0, false
		}
	}
	return days, true
}

// --- GET /api/repositories/{instance}/{project...}/skill-results/tokens ---

// repositorySkillTokens serves what each skill used on the changes repo's
// Skill results tab counts for the same window. Only admins may read it:
// anyone else gets the 404 of a repository that is not there.
func (d *Dashboard) repositorySkillTokens(w http.ResponseWriter, r *http.Request, se Session, repo settings.Effective) {
	if !se.Admin {
		notFound(w)
		return
	}
	days, ok := skillResultsDays(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	// A repository the platform hasn't told us the ID of has no usage.
	id, found, err := d.projectID(ctx, se, repo.Instance, repo.Project)
	if err != nil {
		d.failed(w, r, "reading skill tokens", err)
		return
	}
	out := []SkillTokensView{}
	if found {
		rows, err := d.Store.UsageOnDecidedChanges(ctx, repo.Instance, id, d.now().Add(-time.Duration(days)*day))
		if err != nil {
			d.failed(w, r, "reading skill tokens", err)
			return
		}
		for _, t := range rows {
			out = append(out, SkillTokensView{Skill: t.Skill, Tokens: TokensView{In: t.In, Out: t.Out}})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// skillResultViews is rows as the tab shows them: Addressed worked out, and
// the skills that need work first.
func skillResultViews(rows []store.SkillResult) []SkillResultView {
	out := make([]SkillResultView, 0, len(rows))
	for _, r := range rows {
		v := SkillResultView{Skill: r.Skill, Severity: r.Severity, Raised: r.Raised, Fixed: r.Fixed,
			Dismissed: r.Dismissed, Open: r.Open, WentAway: r.WentAway, EditedAt: r.EditedAt}
		if v.EditedAt != nil {
			t := v.EditedAt.UTC()
			v.EditedAt = &t
		}
		if r.Raised >= minAddressed {
			// Fixed*100/Raised, rounded half up.
			a := (r.Fixed*200 + r.Raised) / (2 * r.Raised)
			v.Addressed = &a
		}
		out = append(out, v)
	}
	// Lowest Addressed first, rows with none last; then by skill, and a
	// blocking row before a suggestion (the words sort that way).
	slices.SortStableFunc(out, func(a, b SkillResultView) int {
		switch {
		case a.Addressed == nil && b.Addressed != nil:
			return 1
		case a.Addressed != nil && b.Addressed == nil:
			return -1
		case a.Addressed != nil && *a.Addressed != *b.Addressed:
			return *a.Addressed - *b.Addressed
		}
		if c := strings.Compare(a.Skill, b.Skill); c != 0 {
			return c
		}
		// "blocking" sorts before "suggestion".
		return strings.Compare(a.Severity, b.Severity)
	})
	return out
}
