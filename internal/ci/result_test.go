package ci

import "testing"

func TestFromGitLab(t *testing.T) {
	cases := []struct {
		in    string
		want  Result
		ended bool
	}{
		{"success", Passed, true},
		{"failed", Failed, true},
		{"canceled", Cancelled, true},
		{"canceling", Cancelled, true},
		{"skipped", Skipped, true},
		{"manual", Skipped, true},
		{"created", Running, false},
		{"pending", Running, false},
		{"preparing", Running, false},
		{"waiting_for_resource", Running, false},
		{"scheduled", Running, false},
		{"running", Running, false},
		{"something_new", Failed, true},
	}
	for _, c := range cases {
		got, ended := FromGitLab(c.in)
		if got != c.want || ended != c.ended {
			t.Errorf("FromGitLab(%q) = %q,%v; want %q,%v", c.in, got, ended, c.want, c.ended)
		}
	}
}

func TestFromGitHub(t *testing.T) {
	cases := []struct {
		status, conclusion string
		want               Result
		ended              bool
	}{
		{"completed", "success", Passed, true},
		{"completed", "neutral", Passed, true},
		{"completed", "failure", Failed, true},
		{"completed", "timed_out", Failed, true},
		{"completed", "action_required", Failed, true},
		{"completed", "startup_failure", Failed, true},
		{"completed", "stale", Failed, true},
		{"completed", "cancelled", Cancelled, true},
		{"completed", "skipped", Skipped, true},
		{"completed", "brand_new", Failed, true},
		{"completed", "", Failed, true},
		{"queued", "", Running, false},
		{"in_progress", "", Running, false},
		{"waiting", "", Running, false},
		{"requested", "", Running, false},
		{"pending", "", Running, false},
	}
	for _, c := range cases {
		got, ended := FromGitHub(c.status, c.conclusion)
		if got != c.want || ended != c.ended {
			t.Errorf("FromGitHub(%q,%q) = %q,%v; want %q,%v", c.status, c.conclusion, got, ended, c.want, c.ended)
		}
	}
}

func TestDone(t *testing.T) {
	ended := Job{Name: "a", Result: Passed, Ended: true}
	if (Snapshot{Jobs: []Job{ended, {Name: "b", Result: Running}}}).Done() {
		t.Error("a running job must not be done")
	}
	if !(Snapshot{Jobs: []Job{ended, {Name: "b", Result: Failed, Ended: true}}}).Done() {
		t.Error("all ended must be done")
	}
	missing := Job{Name: "b", Result: Missing}
	if (Snapshot{Jobs: []Job{ended, missing}}).Done() {
		t.Error("missing while not finished must not be done")
	}
	if !(Snapshot{Jobs: []Job{ended, missing}, Finished: true}).Done() {
		t.Error("missing when finished must be done")
	}
	if (Snapshot{Jobs: []Job{{Name: "c", Result: Running}}, Finished: true}).Done() {
		t.Error("running when finished must not be done")
	}
}

func TestResults(t *testing.T) {
	s := Snapshot{Jobs: []Job{{Name: "t", Read: "log", Result: Failed, Blocks: true, Log: "abc", Report: "de", Cut: true, Reason: "error", TextFailed: true}}}
	r := s.Results()
	if len(r) != 1 || r[0].Name != "t" || r[0].Result != "failed" || r[0].Size != 5 || !r[0].Cut || !r[0].Blocks || r[0].Read != "log" || r[0].Reason != "error" || !r[0].TextFailed {
		t.Fatalf("got %+v", r)
	}
}
