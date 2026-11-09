package metrics

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// dashboardFile is the example Grafana dashboard in the docs.
const dashboardFile = "../../docs/grafana/pergent-sh.json"

// dashboardExprs returns every query of the dashboard's panels.
func dashboardExprs(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(dashboardFile)
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Panels []struct {
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("%s: %v", dashboardFile, err)
	}
	var out []string
	for _, p := range d.Panels {
		for _, q := range p.Targets {
			out = append(out, q.Expr)
		}
	}
	if len(out) == 0 {
		t.Fatal("the dashboard has no queries")
	}
	return out
}

// The dashboard asks only for metrics pergent.sh serves, and uses every one.
func TestDashboardNamesRealMetrics(t *testing.T) {
	used := map[string]bool{}
	for _, expr := range dashboardExprs(t) {
		for _, m := range regexp.MustCompile(`pergent_[a-z_]+`).FindAllString(expr, -1) {
			if !slices.Contains(names, m) {
				t.Errorf("%s isn't a metric pergent.sh serves: %s", m, expr)
			}
			used[m] = true
		}
	}
	for _, n := range names {
		if !used[n] {
			t.Errorf("no panel uses %s", n)
		}
	}
}

// Switching a repository off moves its whole count to "(switched off)", which
// increase() reads as a burst of usage: no query counts that series.
func TestDashboardLeavesOutSwitchedOff(t *testing.T) {
	for _, expr := range dashboardExprs(t) {
		for _, sel := range regexp.MustCompile(`pergent_[a-z_]+\{[^}]*\}`).FindAllString(expr, -1) {
			if !strings.Contains(sel, `repository!="(switched off)"`) {
				t.Errorf("selector counts (switched off): %s", sel)
			}
		}
		if regexp.MustCompile(`pergent_[a-z_]+(\[|\)|\s|$)`).MatchString(expr) {
			t.Errorf("a metric with no selector: %s", expr)
		}
	}
}
