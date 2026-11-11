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
// increase() reads as a burst of usage: no query counts that series. The API
// metrics have a client label and no repository, so they are left to the next
// test.
func TestDashboardLeavesOutSwitchedOff(t *testing.T) {
	for _, expr := range dashboardExprs(t) {
		for _, sel := range regexp.MustCompile(`pergent_[a-z_]+\{[^}]*\}`).FindAllString(expr, -1) {
			if strings.HasPrefix(sel, "pergent_api_") {
				continue
			}
			if !strings.Contains(sel, `repository!="(switched off)"`) {
				t.Errorf("selector counts (switched off): %s", sel)
			}
		}
		if regexp.MustCompile(`pergent_[a-z_]+(\[|\)|\s|$)`).MatchString(expr) {
			t.Errorf("a metric with no selector: %s", expr)
		}
	}
}

// The API metrics are by client: a query asks for a client and never filters
// by the repository labels they do not have, and none names a tenant.
func TestDashboardAPIQueriesUseClient(t *testing.T) {
	n := 0
	for _, expr := range dashboardExprs(t) {
		if !strings.Contains(expr, "pergent_api_") {
			continue
		}
		n++
		for _, bad := range []string{"repository", "platform_instance", "tenant", "$skill", "$model"} {
			if strings.Contains(expr, bad) {
				t.Errorf("an API query names %q: %s", bad, expr)
			}
		}
		if !strings.Contains(expr, "client=~") {
			t.Errorf("an API query has no client selector: %s", expr)
		}
	}
	if n == 0 {
		t.Error("the dashboard has no API query")
	}
}
