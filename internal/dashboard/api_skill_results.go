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
	days := 30
	if r.URL.Query().Has("days") {
		var err error
		if days, err = strconv.Atoi(r.URL.Query().Get("days")); err != nil || (days != 30 && days != 90) {
			writeError(w, http.StatusBadRequest, "days must be 30 or 90")
			return
		}
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
