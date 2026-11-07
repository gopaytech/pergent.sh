package fates

import (
	"errors"
	"reflect"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/diffmap"
)

// fake is a Compare that counts its calls and answers by pair of heads.
type fake struct {
	calls int
	pairs [][2]string
	files map[[2]string][]diffmap.File
	errs  map[[2]string]error
}

func (f *fake) compare(from, to string) ([]diffmap.File, error) {
	f.calls++
	p := [2]string{from, to}
	f.pairs = append(f.pairs, p)
	if err := f.errs[p]; err != nil {
		return nil, err
	}
	return f.files[p], nil
}

func newFake() *fake {
	return &fake{files: map[[2]string][]diffmap.File{}, errs: map[[2]string]error{}}
}

// edit is a diff that changes only old line n of path.
func edit(path string, n int) diffmap.File {
	patch := "@@ -" + itoa(n-1) + ",3 +" + itoa(n-1) + ",3 @@\n a\n-b\n+c\n d\n"
	return diffmap.Parse(path, "", patch, false)
}

// insert is a diff that adds count lines before old line n of path.
func insert(path string, n, count int) diffmap.File {
	patch := "@@ -" + itoa(n-1) + ",2 +" + itoa(n-1) + "," + itoa(2+count) + " @@\n a\n"
	for i := 0; i < count; i++ {
		patch += "+x\n"
	}
	patch += " b\n"
	return diffmap.Parse(path, "", patch, false)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for ; n > 0; n /= 10 {
		s = string(rune('0'+n%10)) + s
	}
	return s
}

func fnd(fp, file string, line int) Finding {
	return Finding{Fingerprint: fp, File: file, Line: line, Severity: "blocking"}
}

// sk is a skill "s" that reviewed the files and reported the findings.
func sk(files []string, fs ...Finding) Skill {
	rev := map[string]bool{}
	for _, f := range files {
		rev[f] = true
	}
	return Skill{Name: "s", Hash: "h", Reviewed: rev, Findings: fs}
}

func review(head string, skills ...Skill) Review { return Review{Head: head, Skills: skills} }

var a = []string{"a.go"}

func decide(t *testing.T, f *fake, dismissals []Dismissal, reviews ...Review) []Fate {
	t.Helper()
	got, err := Decide(Input{Reviews: reviews, Dismissals: dismissals, Compare: f.compare})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	return got
}

// one fails unless exactly one fate came back, and returns it.
func one(t *testing.T, got []Fate) Fate {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("got %d fates, want 1: %+v", len(got), got)
	}
	return got[0]
}

func TestReportedUntilTheEndIsOpen(t *testing.T) {
	f := newFake()
	got := decide(t, f, nil,
		review("h1", sk(a, fnd("x", "a.go", 10))),
		review("h2", sk(a, fnd("x", "a.go", 12))),
	)
	fate := one(t, got)
	want := Fate{Fingerprint: "x", Skill: "s", SkillHash: "h", Severity: "blocking", Fate: Open}
	if fate != want {
		t.Errorf("got %+v, want %+v", fate, want)
	}
	if f.calls != 0 {
		t.Errorf("compares = %d, want 0", f.calls)
	}
}

func TestLinesChangedIsFixed(t *testing.T) {
	f := newFake()
	f.files[[2]string{"h1", "h2"}] = []diffmap.File{edit("a.go", 10)}
	got := decide(t, f, nil,
		review("h1", sk(a, fnd("x", "a.go", 10))),
		review("h2", sk(a)),
	)
	if fate := one(t, got); fate.Fate != Fixed {
		t.Errorf("fate = %s, want fixed", fate.Fate)
	}
}

func TestLinesUntouchedWentAway(t *testing.T) {
	f := newFake()
	f.files[[2]string{"h1", "h2"}] = []diffmap.File{edit("a.go", 50), edit("b.go", 10)}
	got := decide(t, f, nil,
		review("h1", sk(a, fnd("x", "a.go", 10))),
		review("h2", sk(a)),
	)
	if fate := one(t, got); fate.Fate != WentAway {
		t.Errorf("fate = %s, want went_away", fate.Fate)
	}
}

func TestNearbyEditIsFixed(t *testing.T) {
	tests := []struct {
		name      string
		line, end int
		edit      int
		want      string
	}{
		{"3 below", 10, 0, 13, Fixed},
		{"4 below", 10, 0, 14, WentAway},
		{"3 above", 10, 0, 7, Fixed},
		{"4 above", 10, 0, 6, WentAway},
		{"3 past the end", 10, 15, 18, Fixed},
		{"4 past the end", 10, 15, 19, WentAway},
		{"inside the range", 10, 15, 12, Fixed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			f.files[[2]string{"h1", "h2"}] = []diffmap.File{edit("a.go", tt.edit)}
			x := fnd("x", "a.go", tt.line)
			x.EndLine = tt.end
			got := decide(t, f, nil,
				review("h1", sk(a, x)),
				review("h2", sk(a)),
			)
			if fate := one(t, got); fate.Fate != tt.want {
				t.Errorf("fate = %s, want %s", fate.Fate, tt.want)
			}
		})
	}
}

func TestDeletedFileIsFixed(t *testing.T) {
	f := newFake()
	f.files[[2]string{"h1", "h2"}] = []diffmap.File{diffmap.Parse("a.go", "", "", true)}
	// A deleted file is skipped by the review, so no skill has it in Reviewed.
	got := decide(t, f, nil,
		review("h1", sk(a, fnd("x", "a.go", 10))),
		review("h2", sk(nil)),
	)
	if fate := one(t, got); fate.Fate != Fixed {
		t.Errorf("fate = %s, want fixed", fate.Fate)
	}
}

func TestNoLineAnyFileChangeIsFixed(t *testing.T) {
	tests := []struct {
		name  string
		files []diffmap.File
		want  string
	}{
		{"edited far away", []diffmap.File{edit("a.go", 900)}, Fixed},
		{"another file only", []diffmap.File{edit("b.go", 5)}, WentAway},
		{"not in the diff", nil, WentAway},
		// A rename with no content change is not a fix; there is no telling.
		{"renamed unchanged", []diffmap.File{{Path: "c.go", OldPath: "a.go"}}, WentAway},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			f.files[[2]string{"h1", "h2"}] = tt.files
			got := decide(t, f, nil,
				review("h1", sk(a, fnd("x", "a.go", 0))),
				review("h2", sk(a)),
			)
			if fate := one(t, got); fate.Fate != tt.want {
				t.Errorf("fate = %s, want %s", fate.Fate, tt.want)
			}
		})
	}
}

func TestSameHeadVanishedWentAwayNoCompare(t *testing.T) {
	f := newFake()
	f.files[[2]string{"h1", "h1"}] = []diffmap.File{edit("a.go", 10)}
	got := decide(t, f, nil,
		review("h1", sk(a, fnd("x", "a.go", 10))),
		review("h1", sk(a)),
	)
	if fate := one(t, got); fate.Fate != WentAway {
		t.Errorf("fate = %s, want went_away", fate.Fate)
	}
	if f.calls != 0 {
		t.Errorf("compares = %d, want 0", f.calls)
	}
}

func TestVanishedAndBackIsOneRow(t *testing.T) {
	f := newFake()
	f.files[[2]string{"h1", "h2"}] = []diffmap.File{edit("a.go", 10)}
	got := decide(t, f, nil,
		review("h1", sk(a, fnd("x", "a.go", 10))),
		review("h2", sk(a)),
		review("h3", sk(a, fnd("x", "a.go", 10))),
	)
	if fate := one(t, got); fate.Fate != Open {
		t.Errorf("fate = %s, want open", fate.Fate)
	}
}

func TestFailedSkillIsSkipped(t *testing.T) {
	f := newFake()
	f.files[[2]string{"h1", "h2"}] = []diffmap.File{edit("a.go", 10)}
	failed := sk(a)
	failed.Failed = true
	got := decide(t, f, nil,
		review("h1", sk(a, fnd("x", "a.go", 10))),
		review("h2", failed),
	)
	if fate := one(t, got); fate.Fate != Open {
		t.Errorf("fate = %s, want open", fate.Fate)
	}
}

func TestRemovedSkillIsSkipped(t *testing.T) {
	f := newFake()
	f.files[[2]string{"h1", "h2"}] = []diffmap.File{edit("a.go", 10)}
	other := sk(a)
	other.Name = "other"
	for _, later := range []Review{review("h2"), review("h2", other)} {
		got := decide(t, f, nil, review("h1", sk(a, fnd("x", "a.go", 10))), later)
		if fate := one(t, got); fate.Fate != Open {
			t.Errorf("fate = %s, want open", fate.Fate)
		}
	}
}

func TestFileNotReviewedIsSkipped(t *testing.T) {
	f := newFake()
	f.files[[2]string{"h1", "h2"}] = []diffmap.File{edit("a.go", 10)}
	got := decide(t, f, nil,
		review("h1", sk(a, fnd("x", "a.go", 10))),
		review("h2", sk([]string{"b.go"})),
	)
	if fate := one(t, got); fate.Fate != Open {
		t.Errorf("fate = %s, want open", fate.Fate)
	}
}

func TestLeftOutIsWentAwayEvenIfLinesChanged(t *testing.T) {
	f := newFake()
	f.files[[2]string{"h1", "h2"}] = []diffmap.File{edit("a.go", 10)}
	left := sk(a)
	left.LeftOut = true
	got := decide(t, f, nil,
		review("h1", sk(a, fnd("x", "a.go", 10))),
		review("h2", left),
	)
	if fate := one(t, got); fate.Fate != WentAway {
		t.Errorf("fate = %s, want went_away", fate.Fate)
	}
	if f.calls != 0 {
		t.Errorf("compares = %d, want 0", f.calls)
	}
}

func TestRewordedIsTheSameFinding(t *testing.T) {
	tests := []struct {
		name  string
		diff  []diffmap.File
		line  int
		extra int // rows expected beyond the one
	}{
		{"same lines", nil, 10, 0},
		{"file untouched, lines not overlapping", nil, 12, 1},
		{"lines moved by an insertion above", []diffmap.File{insert("a.go", 2, 5)}, 15, 0},
		{"lines not where the old ones went", []diffmap.File{insert("a.go", 2, 5)}, 10, 1},
		{"unmapped, within reach", []diffmap.File{edit("a.go", 10)}, 13, 0},
		{"unmapped, past reach", []diffmap.File{edit("a.go", 10)}, 14, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			f.files[[2]string{"h1", "h2"}] = tt.diff
			got := decide(t, f, nil,
				review("h1", sk(a, fnd("old", "a.go", 10))),
				review("h2", sk(a, fnd("new", "a.go", tt.line))),
			)
			if len(got) != 1+tt.extra {
				t.Fatalf("got %d fates, want %d: %+v", len(got), 1+tt.extra, got)
			}
			if tt.extra > 0 {
				// Sorted by fingerprint: new (open), old (nothing near it changed
				// or it was fixed).
				if got[0].Fingerprint != "new" || got[0].Fate != Open || got[1].Fingerprint != "old" || got[1].Fate == Open {
					t.Errorf("got %+v, want new/open and old/not open", got)
				}
				return
			}
			if got[0].Fingerprint != "new" || got[0].Fate != Open {
				t.Errorf("got %+v, want new/open", got[0])
			}
		})
	}
}

func TestRewordedOnAnotherFileIsNew(t *testing.T) {
	f := newFake()
	both := []string{"a.go", "b.go"}
	got := decide(t, f, nil,
		review("h1", sk(both, fnd("old", "a.go", 10))),
		review("h2", sk(both, fnd("new", "b.go", 10))),
	)
	if len(got) != 2 {
		t.Fatalf("got %d fates, want 2: %+v", len(got), got)
	}
	// Sorted by fingerprint: new, old.
	if got[0].Fingerprint != "new" || got[0].Fate != Open {
		t.Errorf("got %+v, want new/open", got[0])
	}
	if got[1].Fingerprint != "old" || got[1].Fate != WentAway {
		t.Errorf("got %+v, want old/went_away", got[1])
	}
}

func TestRewordedAcrossARename(t *testing.T) {
	f := newFake()
	f.files[[2]string{"h1", "h2"}] = []diffmap.File{{Path: "b.go", OldPath: "a.go"}}
	got := decide(t, f, nil,
		review("h1", sk(a, fnd("old", "a.go", 10))),
		review("h2", sk([]string{"b.go"}, fnd("new", "b.go", 10))),
	)
	fate := one(t, got)
	if fate.Fingerprint != "new" || fate.Fate != Open {
		t.Errorf("got %+v, want new/open", fate)
	}
}

func TestRenamedAndNotReportedIsNotSkipped(t *testing.T) {
	// The file was renamed and reviewed under its new name; the finding is
	// gone and the rename also changed lines near it.
	f := newFake()
	moved := diffmap.Parse("b.go", "a.go", "@@ -9,3 +9,3 @@\n a\n-b\n+c\n d\n", false)
	f.files[[2]string{"h1", "h2"}] = []diffmap.File{moved}
	got := decide(t, f, nil,
		review("h1", sk(a, fnd("x", "a.go", 10))),
		review("h2", sk([]string{"b.go"})),
	)
	if fate := one(t, got); fate.Fate != Fixed {
		t.Errorf("fate = %s, want fixed", fate.Fate)
	}
}

func TestRewordedFingerprintReportedAgainIsTheSameFinding(t *testing.T) {
	f := newFake()
	got := decide(t, f, nil,
		review("h1", sk(a, fnd("x", "a.go", 10))),
		review("h2", sk(a, fnd("y", "a.go", 10))),
		review("h3", sk(a, fnd("x", "a.go", 10))),
	)
	fate := one(t, got)
	if fate.Fingerprint != "x" || fate.Fate != Open {
		t.Errorf("got %+v, want x/open", fate)
	}
}

func TestDismissedWins(t *testing.T) {
	f := newFake()
	f.files[[2]string{"h1", "h2"}] = []diffmap.File{edit("a.go", 10)}
	d := []Dismissal{{Fingerprint: "x", Severity: "blocking"}, {Fingerprint: "nowhere", Severity: "blocking"}}
	got := decide(t, f, d,
		review("h1", sk(a, fnd("x", "a.go", 10))),
		review("h2", sk(a)),
	)
	// The dismissal of a finding in no review gets no row.
	if fate := one(t, got); fate.Fingerprint != "x" || fate.Fate != Dismissed {
		t.Errorf("got %+v, want x/dismissed", fate)
	}
}

func TestDismissalOfAnEarlierWordingCovers(t *testing.T) {
	f := newFake()
	got := decide(t, f, []Dismissal{{Fingerprint: "old", Severity: "blocking"}},
		review("h1", sk(a, fnd("old", "a.go", 10))),
		review("h2", sk(a, fnd("new", "a.go", 10))),
	)
	if fate := one(t, got); fate.Fingerprint != "new" || fate.Fate != Dismissed {
		t.Errorf("got %+v, want new/dismissed", fate)
	}
}

func TestSuggestionDismissalDoesNotCoverABlocker(t *testing.T) {
	tests := []struct {
		name     string
		severity string // of the finding
		dismissa string
		want     string
	}{
		{"suggestion dismissal, blocking finding", "blocking", "suggestion", Open},
		{"suggestion dismissal, suggestion finding", "suggestion", "suggestion", Dismissed},
		{"blocking dismissal, suggestion finding", "suggestion", "blocking", Dismissed},
		{"blocking dismissal, blocking finding", "blocking", "blocking", Dismissed},
		{"suggestion dismissal, other severity is a blocker", "", "suggestion", Open},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			x := fnd("x", "a.go", 10)
			x.Severity = tt.severity
			got := decide(t, newFake(), []Dismissal{{Fingerprint: "x", Severity: tt.dismissa}},
				review("h1", sk(a, x)),
			)
			if fate := one(t, got); fate.Fate != tt.want {
				t.Errorf("fate = %s, want %s", fate.Fate, tt.want)
			}
		})
	}
}

func TestDismissalCoversByTheLastSeverityTheRowShowsTheHighest(t *testing.T) {
	// Reported as a blocker, then as a suggestion, then a suggestion is
	// dismissed: the finding the comment shows is the suggestion, which that
	// dismissal covers. The row is still the highest severity it had.
	tests := []struct {
		name     string
		sevs     []string
		dismissa string
		want     string
	}{
		{"blocking then suggestion, suggestion dismissed", []string{"blocking", "suggestion"}, "suggestion", Dismissed},
		{"suggestion then blocking, suggestion dismissed", []string{"suggestion", "blocking"}, "suggestion", Open},
		{"blocking then suggestion, blocking dismissed", []string{"blocking", "suggestion"}, "blocking", Dismissed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reviews []Review
			for i, sev := range tt.sevs {
				x := fnd("x", "a.go", 10)
				x.Severity = sev
				reviews = append(reviews, review("h"+itoa(i+1), sk(a, x)))
			}
			got := decide(t, newFake(), []Dismissal{{Fingerprint: "x", Severity: tt.dismissa}}, reviews...)
			fate := one(t, got)
			if fate.Fate != tt.want {
				t.Errorf("fate = %s, want %s", fate.Fate, tt.want)
			}
			if fate.Severity != "blocking" {
				t.Errorf("severity = %s, want blocking", fate.Severity)
			}
		})
	}
}

func TestHighestSeverityCounts(t *testing.T) {
	tests := []struct {
		name string
		sevs []string
		want string
	}{
		{"suggestion then blocking", []string{"suggestion", "blocking"}, "blocking"},
		{"blocking then suggestion", []string{"blocking", "suggestion"}, "blocking"},
		{"suggestion only", []string{"suggestion", "suggestion"}, "suggestion"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reviews []Review
			for i, sev := range tt.sevs {
				x := fnd("x", "a.go", 10)
				x.Severity = sev
				reviews = append(reviews, review("h"+itoa(i+1), sk(a, x)))
			}
			got := decide(t, newFake(), nil, reviews...)
			if fate := one(t, got); fate.Severity != tt.want {
				t.Errorf("severity = %s, want %s", fate.Severity, tt.want)
			}
		})
	}
}

func TestOneFingerprintOnTwoLinesIsOneRow(t *testing.T) {
	x1, x2 := fnd("x", "a.go", 10), fnd("x", "a.go", 40)
	x2.Severity = "suggestion"
	got := decide(t, newFake(), nil, review("h1", sk(a, x1, x2)))
	fate := one(t, got)
	if fate.Severity != "blocking" || fate.Fate != Open {
		t.Errorf("got %+v, want blocking/open", fate)
	}
}

func TestSameFingerprintInTwoSkillsIsTwoRows(t *testing.T) {
	second := sk(a, fnd("x", "a.go", 10))
	second.Name = "t"
	got := decide(t, newFake(), nil, review("h1", sk(a, fnd("x", "a.go", 10)), second))
	if len(got) != 2 || got[0].Skill != "s" || got[1].Skill != "t" {
		t.Errorf("got %+v, want one row per skill", got)
	}
}

func TestErrGoneIsWentAway(t *testing.T) {
	f := newFake()
	f.errs[[2]string{"h1", "h2"}] = ErrGone
	got := decide(t, f, nil,
		review("h1", sk(a, fnd("x", "a.go", 10))),
		review("h2", sk(a)),
	)
	if fate := one(t, got); fate.Fate != WentAway {
		t.Errorf("fate = %s, want went_away", fate.Fate)
	}
}

func TestOtherCompareErrorReturnsNoFates(t *testing.T) {
	boom := errors.New("503")
	f := newFake()
	f.errs[[2]string{"h1", "h2"}] = boom
	got, err := Decide(Input{
		Reviews: []Review{
			review("h1", sk(a, fnd("x", "a.go", 10))),
			review("h2", sk(a)),
		},
		Compare: f.compare,
	})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want %v", err, boom)
	}
	if got != nil {
		t.Errorf("fates = %+v, want none", got)
	}
}

func TestComparesAreCapped(t *testing.T) {
	f := newFake()
	var reviews []Review
	const n = MaxCompares + 1
	for i := 0; i < n; i++ {
		file := "f" + itoa(i) + ".go"
		head := "h" + itoa(i)
		next := "h" + itoa(i+1)
		files := []string{file}
		var fs []Finding
		fs = append(fs, fnd("x"+itoa(i), file, 10))
		reviews = append(reviews, review(head, sk(files, fs...)))
		f.files[[2]string{head, next}] = []diffmap.File{edit(file, 10)}
		// The next review reports the next finding, so each pair of heads is
		// one finding's pair.
		if i == n-1 {
			reviews = append(reviews, review(next, sk(files)))
		}
	}
	// Each review i+1 (reporting finding i+1 in its own file) must also be
	// reviewing finding i's file, or finding i would be skipped.
	for i := 1; i < len(reviews); i++ {
		prev := "f" + itoa(i-1) + ".go"
		reviews[i].Skills[0].Reviewed[prev] = true
	}
	got := decide(t, f, nil, reviews...)
	if f.calls != MaxCompares {
		t.Errorf("compares = %d, want %d", f.calls, MaxCompares)
	}
	counts := map[string]int{}
	for _, fate := range got {
		counts[fate.Fate]++
	}
	if counts[Fixed] != MaxCompares || counts[WentAway] != 1 || len(got) != n {
		t.Errorf("fates = %v, want %d fixed and 1 went_away", counts, MaxCompares)
	}
}

func TestComparesAreCached(t *testing.T) {
	f := newFake()
	f.files[[2]string{"h1", "h2"}] = []diffmap.File{edit("a.go", 10)}
	got := decide(t, f, nil,
		review("h1", sk(a, fnd("x", "a.go", 10), fnd("y", "a.go", 80))),
		review("h2", sk(a)),
	)
	if len(got) != 2 {
		t.Fatalf("got %d fates, want 2", len(got))
	}
	if f.calls != 1 {
		t.Errorf("compares = %d, want 1", f.calls)
	}
	want := map[string]string{"x": Fixed, "y": WentAway}
	for _, fate := range got {
		if fate.Fate != want[fate.Fingerprint] {
			t.Errorf("%s = %s, want %s", fate.Fingerprint, fate.Fate, want[fate.Fingerprint])
		}
	}
}

func TestNoReviews(t *testing.T) {
	got, err := Decide(Input{Dismissals: []Dismissal{{Fingerprint: "x", Severity: "blocking"}}})
	if err != nil || len(got) != 0 {
		t.Errorf("got %+v, %v; want no fates", got, err)
	}
}

func TestSortedByFingerprint(t *testing.T) {
	got := decide(t, newFake(), nil,
		review("h1", sk(a, fnd("c", "a.go", 1), fnd("a", "a.go", 50), fnd("b", "a.go", 90))),
	)
	var fps []string
	for _, fate := range got {
		fps = append(fps, fate.Fingerprint)
	}
	if !reflect.DeepEqual(fps, []string{"a", "b", "c"}) {
		t.Errorf("order = %v", fps)
	}
}

// removed is a diff where old lines 10 to 12 are replaced, so none of them maps.
func removed(path string) diffmap.File {
	return diffmap.Parse(path, "", "@@ -9,5 +9,5 @@\n a\n-b\n-c\n-d\n+B\n+C\n+D\n e\n", false)
}

func TestMovedLineThenGone(t *testing.T) {
	f := newFake()
	// Only the line the finding had last (20) is near this edit.
	f.files[[2]string{"h2", "h3"}] = []diffmap.File{edit("a.go", 20)}
	got := decide(t, f, nil,
		review("h1", sk(a, fnd("x", "a.go", 10))),
		review("h2", sk(a, fnd("x", "a.go", 20))),
		review("h3", sk(a)),
	)
	if fate := one(t, got); fate.Fate != Fixed {
		t.Errorf("fate = %s, want fixed", fate.Fate)
	}
	if want := [][2]string{{"h2", "h3"}}; !reflect.DeepEqual(f.pairs, want) {
		t.Errorf("compares = %v, want %v", f.pairs, want)
	}

	// And the other way round: an edit near the old line 10 is not near it.
	f = newFake()
	f.files[[2]string{"h2", "h3"}] = []diffmap.File{edit("a.go", 10)}
	got = decide(t, f, nil,
		review("h1", sk(a, fnd("x", "a.go", 10))),
		review("h2", sk(a, fnd("x", "a.go", 20))),
		review("h3", sk(a)),
	)
	if fate := one(t, got); fate.Fate != WentAway {
		t.Errorf("fate = %s, want went_away", fate.Fate)
	}
}

func TestReportedSkippedThenGone(t *testing.T) {
	f := newFake()
	// The head is the one of the last review that reported it (h2), not h1 or h3.
	f.files[[2]string{"h2", "h4"}] = []diffmap.File{edit("a.go", 10)}
	failed := sk(a)
	failed.Failed = true
	got := decide(t, f, nil,
		review("h1", sk(a, fnd("x", "a.go", 10))),
		review("h2", sk(a, fnd("x", "a.go", 10))),
		review("h3", failed),
		review("h4", sk(a)),
	)
	if fate := one(t, got); fate.Fate != Fixed {
		t.Errorf("fate = %s, want fixed", fate.Fate)
	}
	if want := [][2]string{{"h2", "h4"}}; !reflect.DeepEqual(f.pairs, want) {
		t.Errorf("compares = %v, want %v", f.pairs, want)
	}
}

func TestFailedSkillOnADeletedFileIsSkipped(t *testing.T) {
	// A failed skill says nothing, deleted file or not.
	f := newFake()
	f.files[[2]string{"h1", "h2"}] = []diffmap.File{diffmap.Parse("a.go", "", "", true)}
	failed := sk(nil)
	failed.Failed = true
	got := decide(t, f, nil, review("h1", sk(a, fnd("x", "a.go", 10))), review("h2", failed))
	if fate := one(t, got); fate.Fate != Open {
		t.Errorf("fate = %s, want open", fate.Fate)
	}
}

func TestRewordedInASkippedReviewIsTheSameFinding(t *testing.T) {
	f := newFake()
	failed := sk(a, fnd("y", "a.go", 10))
	failed.Failed = true
	got := decide(t, f, nil,
		review("h1", sk(a, fnd("x", "a.go", 10))),
		review("h2", failed),
		review("h3", sk(a, fnd("y", "a.go", 10))),
	)
	fate := one(t, got)
	if fate.Fingerprint != "y" || fate.Fate != Open {
		t.Errorf("got %+v, want y/open", fate)
	}

	// Also from a file the skill didn't review.
	got = decide(t, newFake(), nil,
		review("h1", sk(a, fnd("x", "a.go", 10))),
		review("h2", sk([]string{"b.go"}, fnd("y", "a.go", 10))),
	)
	if fate := one(t, got); fate.Fingerprint != "y" || fate.Fate != Open {
		t.Errorf("got %+v, want y/open", fate)
	}
}

func TestRewordedOnTheSameHead(t *testing.T) {
	tests := []struct {
		name string
		line int
		rows int
	}{
		{"same lines", 10, 1},
		{"overlapping", 11, 1},
		{"not overlapping", 13, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			x := fnd("x", "a.go", 10)
			x.EndLine = 11
			got := decide(t, f, nil,
				review("h1", sk(a, x)),
				review("h1", sk(a, fnd("y", "a.go", tt.line))),
			)
			if len(got) != tt.rows {
				t.Fatalf("got %d fates, want %d: %+v", len(got), tt.rows, got)
			}
			if tt.rows == 1 && (got[0].Fingerprint != "y" || got[0].Fate != Open) {
				t.Errorf("got %+v, want y/open", got[0])
			}
			if f.calls != 0 {
				t.Errorf("compares = %d, want 0", f.calls)
			}
		})
	}
}

func TestAVanishedFindingReportedReworded(t *testing.T) {
	// F is reported, left out of a review with an edit beside its lines (fixed
	// or went away), and then reported again under a new title: one row.
	t.Run("on two heads", func(t *testing.T) {
		f := newFake()
		f.files[[2]string{"h1", "h2"}] = []diffmap.File{edit("a.go", 12)}
		got := decide(t, f, nil,
			review("h1", sk(a, fnd("f1", "a.go", 10))),
			review("h2", sk(a)),
			review("h2", sk(a, fnd("f2", "a.go", 10))),
		)
		if fate := one(t, got); fate.Fingerprint != "f2" || fate.Fate != Open {
			t.Errorf("got %+v, want f2/open", fate)
		}
	})
	t.Run("on one head", func(t *testing.T) {
		f := newFake()
		got := decide(t, f, nil,
			review("h1", sk(a, fnd("f1", "a.go", 10))),
			review("h1", sk(a)),
			review("h1", sk(a, fnd("f2", "a.go", 10))),
		)
		if fate := one(t, got); fate.Fingerprint != "f2" || fate.Fate != Open {
			t.Errorf("got %+v, want f2/open", fate)
		}
		if f.calls != 0 {
			t.Errorf("compares = %d, want 0", f.calls)
		}
	})
}

func TestAVanishedFindingReportedRewordedOnALaterHead(t *testing.T) {
	// The main route: the finding vanished on one head, and comes back
	// reworded in its own file on a third, which needs a comparison of its own.
	f := newFake()
	f.files[[2]string{"h1", "h2"}] = []diffmap.File{edit("a.go", 12)}
	f.files[[2]string{"h1", "h3"}] = []diffmap.File{edit("a.go", 30)}
	got := decide(t, f, nil,
		review("h1", sk(a, fnd("f1", "a.go", 10))),
		review("h2", sk(a)),
		review("h3", sk(a, fnd("f2", "a.go", 10))),
	)
	if fate := one(t, got); fate.Fingerprint != "f2" || fate.Fate != Open {
		t.Errorf("got %+v, want f2/open", fate)
	}
}

func TestAVanishedFindingRenamedInTheGapIsReworded(t *testing.T) {
	// The file was renamed (and edited near the finding) in the review that
	// left the finding out, so it is Fixed there; it comes back reworded under
	// the new name on a later head.
	f := newFake()
	moved := diffmap.Parse("b.go", "a.go", "@@ -9,3 +9,3 @@\n a\n-b\n+c\n d\n", false)
	f.files[[2]string{"h1", "h2"}] = []diffmap.File{moved}
	f.files[[2]string{"h1", "h3"}] = []diffmap.File{moved}
	b := []string{"b.go"}
	got := decide(t, f, nil,
		review("h1", sk(a, fnd("f1", "a.go", 10))),
		review("h2", sk(b)),
		review("h3", sk(b, fnd("f2", "b.go", 10))),
	)
	if fate := one(t, got); fate.Fingerprint != "f2" || fate.Fate != Open {
		t.Errorf("got %+v, want f2/open", fate)
	}
}

func TestAVanishedFindingFollowsARenameInAComparisonAlreadyMade(t *testing.T) {
	// A comparison made for a live finding is free for a vanished one: here it
	// shows a.go renamed to b.go, where the vanished finding comes back.
	// c.go is not reviewed by h2, so its finding is skipped, not gone, and
	// still looks at h1 when h3 reports it reworded.
	f := newFake()
	f.files[[2]string{"h1", "h2"}] = []diffmap.File{edit("a.go", 12)}
	f.files[[2]string{"h1", "h3"}] = []diffmap.File{{Path: "b.go", OldPath: "a.go"}}
	got := decide(t, f, nil,
		review("h1", sk([]string{"a.go", "c.go"}, fnd("g", "a.go", 10), fnd("l", "c.go", 5))),
		review("h2", sk(a)),
		review("h3", sk([]string{"b.go", "c.go"}, fnd("n1", "c.go", 5), fnd("n2", "b.go", 10))),
	)
	want := []Fate{
		{Fingerprint: "n1", Skill: "s", SkillHash: "h", Severity: "blocking", Fate: Open},
		{Fingerprint: "n2", Skill: "s", SkillHash: "h", Severity: "blocking", Fate: Open},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestALiveTrackBeatsAVanishedOneForACandidate(t *testing.T) {
	// x and w are reported; w alone stays reported, then neither is, and c sits
	// on the lines of both. x came first, but w is live: w is c's.
	w := fnd("w", "a.go", 12)
	got := decide(t, newFake(), nil,
		review("h1", sk(a, fnd("x", "a.go", 11), w)),
		review("h1", sk(a, w)),
		review("h1", sk(a, func() Finding { c := fnd("c", "a.go", 11); c.EndLine = 12; return c }())),
	)
	want := []Fate{
		{Fingerprint: "c", Skill: "s", SkillHash: "h", Severity: "blocking", Fate: Open},
		{Fingerprint: "x", Skill: "s", SkillHash: "h", Severity: "blocking", Fate: WentAway},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestRewordedWithoutALine(t *testing.T) {
	t.Run("retitled, edit far away", func(t *testing.T) {
		f := newFake()
		f.files[[2]string{"h1", "h2"}] = []diffmap.File{edit("a.go", 900)}
		got := decide(t, f, nil,
			review("h1", sk(a, fnd("x", "a.go", 0))),
			review("h2", sk(a, fnd("y", "a.go", 0))),
		)
		if fate := one(t, got); fate.Fingerprint != "y" || fate.Fate != Open {
			t.Errorf("got %+v, want y/open", fate)
		}
	})
	t.Run("renamed with an edit", func(t *testing.T) {
		f := newFake()
		moved := diffmap.Parse("b.go", "a.go", "@@ -9,3 +9,3 @@\n a\n-b\n+c\n d\n", false)
		f.files[[2]string{"h1", "h2"}] = []diffmap.File{moved}
		got := decide(t, f, nil,
			review("h1", sk(a, fnd("x", "a.go", 0))),
			review("h2", sk([]string{"b.go"}, fnd("y", "b.go", 0))),
		)
		if fate := one(t, got); fate.Fingerprint != "y" || fate.Fate != Open {
			t.Errorf("got %+v, want y/open", fate)
		}
	})
	t.Run("another file is new", func(t *testing.T) {
		both := []string{"a.go", "b.go"}
		got := decide(t, newFake(), nil,
			review("h1", sk(both, fnd("x", "a.go", 0))),
			review("h2", sk(both, fnd("y", "b.go", 0))),
		)
		if len(got) != 2 {
			t.Errorf("got %d fates, want 2: %+v", len(got), got)
		}
	})
	t.Run("a finding without a line is not a candidate for one with a line", func(t *testing.T) {
		// Line 2 is rewritten, so Reach applies, and z at "line 0" is 2 away.
		f := newFake()
		f.files[[2]string{"h1", "h2"}] = []diffmap.File{edit("a.go", 2)}
		got := decide(t, f, nil,
			review("h1", sk(a, fnd("x", "a.go", 2))),
			review("h2", sk(a, fnd("z", "a.go", 0), fnd("y", "a.go", 50))),
		)
		if len(got) != 3 {
			t.Errorf("got %d fates, want 3: %+v", len(got), got)
		}
	})
	t.Run("a finding with a line is not a candidate", func(t *testing.T) {
		got := decide(t, newFake(), nil,
			review("h1", sk(a, fnd("x", "a.go", 0))),
			review("h2", sk(a, fnd("y", "a.go", 10))),
		)
		if len(got) != 2 {
			t.Errorf("got %d fates, want 2: %+v", len(got), got)
		}
	})
}

func TestRewordedAfterAnEditedLineFollowsTheShift(t *testing.T) {
	// Five lines are added at the top and old line 10 is rewritten, so it has
	// no new line; it sat at 15 after the insertion.
	patch := "@@ -1,2 +1,7 @@\n a\n+x\n+x\n+x\n+x\n+x\n b\n@@ -9,3 +14,3 @@\n a\n-b\n+c\n d\n"
	f := newFake()
	f.files[[2]string{"h1", "h2"}] = []diffmap.File{diffmap.Parse("a.go", "", patch, false)}
	got := decide(t, f, nil,
		review("h1", sk(a, fnd("x", "a.go", 10))),
		review("h2", sk(a, fnd("y", "a.go", 15))),
	)
	if fate := one(t, got); fate.Fingerprint != "y" || fate.Fate != Open {
		t.Errorf("got %+v, want y/open", fate)
	}
}

func TestRewordedBothEndsMustMap(t *testing.T) {
	// Line 9 is unchanged and maps; line 10 is rewritten and does not, so the
	// range is not mapped and Reach applies to the old lines.
	f := newFake()
	f.files[[2]string{"h1", "h2"}] = []diffmap.File{edit("a.go", 10)}
	x := fnd("x", "a.go", 9)
	x.EndLine = 10
	got := decide(t, f, nil,
		review("h1", sk(a, x)),
		review("h2", sk(a, fnd("y", "a.go", 13))),
	)
	if fate := one(t, got); fate.Fingerprint != "y" || fate.Fate != Open {
		t.Errorf("got %+v, want y/open", fate)
	}
}

func TestRewordedAcrossAPureRenameUsesReach(t *testing.T) {
	// A file in the diff with no hunks has no mapping to trust: Reach applies.
	f := newFake()
	f.files[[2]string{"h1", "h2"}] = []diffmap.File{{Path: "b.go", OldPath: "a.go"}}
	got := decide(t, f, nil,
		review("h1", sk(a, fnd("x", "a.go", 10))),
		review("h2", sk([]string{"b.go"}, fnd("y", "b.go", 12))),
	)
	if fate := one(t, got); fate.Fingerprint != "y" || fate.Fate != Open {
		t.Errorf("got %+v, want y/open", fate)
	}
}

func TestRewordedWithTheCompareGone(t *testing.T) {
	tests := []struct {
		name string
		line int
		rows int
	}{
		{"within reach", 12, 1},
		{"past reach", 14, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			f.errs[[2]string{"h1", "h2"}] = ErrGone
			got := decide(t, f, nil,
				review("h1", sk(a, fnd("x", "a.go", 10))),
				review("h2", sk(a, fnd("y", "a.go", tt.line))),
			)
			if len(got) != tt.rows {
				t.Errorf("got %d fates, want %d: %+v", len(got), tt.rows, got)
			}
		})
	}
}

func TestTheClosestCandidateWins(t *testing.T) {
	f := newFake()
	f.files[[2]string{"h1", "h2"}] = []diffmap.File{removed("a.go")}
	x := fnd("x", "a.go", 12)
	// far comes first by line, but near is closer.
	far, near := fnd("far", "a.go", 9), fnd("near", "a.go", 13)
	far.Severity, near.Severity = "suggestion", "suggestion"
	got := decide(t, f, nil, review("h1", sk(a, x)), review("h2", sk(a, far, near)))
	// The one that joined x keeps its severity, blocking.
	want := []Fate{
		{Fingerprint: "far", Skill: "s", SkillHash: "h", Severity: "suggestion", Fate: Open},
		{Fingerprint: "near", Skill: "s", SkillHash: "h", Severity: "blocking", Fate: Open},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestTheNearestTrackTakesACandidate(t *testing.T) {
	f := newFake()
	f.files[[2]string{"h1", "h2"}] = []diffmap.File{removed("a.go")}
	got := decide(t, f, nil,
		review("h1", sk(a, fnd("x", "a.go", 10), fnd("w", "a.go", 12))),
		review("h2", sk(a, fnd("y", "a.go", 13))),
	)
	// y is 1 line from w and 3 from x: it is w's, and x, whose lines were
	// rewritten, is fixed. Under w's old fingerprint there is no row.
	want := []Fate{
		{Fingerprint: "x", Skill: "s", SkillHash: "h", Severity: "blocking", Fate: Fixed},
		{Fingerprint: "y", Skill: "s", SkillHash: "h", Severity: "blocking", Fate: Open},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestAnOldWordingBesideTheNewKeepsItsSeverity(t *testing.T) {
	x := fnd("x", "a.go", 10)
	x.Severity = "suggestion"
	y := fnd("y", "a.go", 10)
	y.Severity = "suggestion"
	old := fnd("x", "a.go", 10) // blocking
	got := decide(t, newFake(), nil,
		review("h1", sk(a, x)),
		review("h2", sk(a, y)),
		review("h3", sk(a, y, old)),
	)
	fate := one(t, got)
	if fate.Fingerprint != "y" || fate.Severity != "blocking" || fate.Fate != Open {
		t.Errorf("got %+v, want y/blocking/open", fate)
	}
}

func TestDismissalCoversByTheSeveritiesOfTheLastReport(t *testing.T) {
	// The comment judges what the last review that reported the finding shows:
	// a wording of it reported beside the newest one is a finding there too,
	// and a suggestion dismissal doesn't cover a blocker.
	y := fnd("y", "a.go", 10)
	y.Severity = "suggestion"
	old := fnd("x", "a.go", 10) // blocking
	dismissal := []Dismissal{{Fingerprint: "y", Severity: "suggestion"}}

	t.Run("an old wording beside the new one", func(t *testing.T) {
		got := decide(t, newFake(), dismissal,
			review("h1", sk(a, old)),
			review("h2", sk(a, y)),
			review("h3", sk(a, y, old)),
		)
		if fate := one(t, got); fate.Fate != Open || fate.Severity != "blocking" {
			t.Errorf("got %+v, want open/blocking", fate)
		}
	})
	t.Run("the old wording no longer reported", func(t *testing.T) {
		got := decide(t, newFake(), dismissal,
			review("h1", sk(a, old)),
			review("h2", sk(a, y)),
			review("h3", sk(a, y, old)),
			review("h4", sk(a, y)),
		)
		if fate := one(t, got); fate.Fate != Dismissed || fate.Severity != "blocking" {
			t.Errorf("got %+v, want dismissed/blocking", fate)
		}
	})
}

func TestSkillHashIsFromTheLastReport(t *testing.T) {
	with := func(hash string, fs ...Finding) Skill {
		s := sk(a, fs...)
		s.Hash = hash
		return s
	}
	got := decide(t, newFake(), nil,
		review("h1", with("one", fnd("x", "a.go", 10))),
		review("h2", with("two", fnd("x", "a.go", 10))),
		review("h3", with("three")),
	)
	if fate := one(t, got); fate.SkillHash != "two" {
		t.Errorf("hash = %s, want two", fate.SkillHash)
	}
}

func TestTheLowestLineOfAFingerprintIsItsLine(t *testing.T) {
	f := newFake()
	f.files[[2]string{"h1", "h2"}] = []diffmap.File{edit("a.go", 10)}
	got := decide(t, f, nil,
		review("h1", sk(a, fnd("x", "a.go", 40), fnd("x", "a.go", 10))),
		review("h2", sk(a)),
	)
	if fate := one(t, got); fate.Fate != Fixed {
		t.Errorf("fate = %s, want fixed", fate.Fate)
	}
}

func TestAFindingReportedByAFailedSkillCounts(t *testing.T) {
	failed := func(fs ...Finding) Skill {
		s := sk(a, fs...)
		s.Failed = true
		return s
	}
	t.Run("starts a track", func(t *testing.T) {
		got := decide(t, newFake(), nil, review("h1", failed(fnd("x", "a.go", 10))))
		if fate := one(t, got); fate.Fate != Open {
			t.Errorf("fate = %s, want open", fate.Fate)
		}
	})
	t.Run("moves the head", func(t *testing.T) {
		f := newFake()
		f.files[[2]string{"h2", "h3"}] = []diffmap.File{edit("a.go", 10)}
		got := decide(t, f, nil,
			review("h1", sk(a, fnd("x", "a.go", 10))),
			review("h2", failed(fnd("x", "a.go", 10))),
			review("h3", sk(a)),
		)
		if fate := one(t, got); fate.Fate != Fixed {
			t.Errorf("fate = %s, want fixed", fate.Fate)
		}
		if want := [][2]string{{"h2", "h3"}}; !reflect.DeepEqual(f.pairs, want) {
			t.Errorf("compares = %v, want %v", f.pairs, want)
		}
	})
	t.Run("from a file it didn't review", func(t *testing.T) {
		got := decide(t, newFake(), nil, review("h1", sk([]string{"b.go"}, fnd("x", "a.go", 10))))
		if fate := one(t, got); fate.Fate != Open {
			t.Errorf("fate = %s, want open", fate.Fate)
		}
	})
}

func TestATrackedFindingIsNotACandidate(t *testing.T) {
	got := decide(t, newFake(), nil,
		review("h1", sk(a, fnd("x", "a.go", 10), fnd("z", "a.go", 10))),
		review("h2", sk(a, fnd("z", "a.go", 10))),
	)
	want := []Fate{
		{Fingerprint: "x", Skill: "s", SkillHash: "h", Severity: "blocking", Fate: WentAway},
		{Fingerprint: "z", Skill: "s", SkillHash: "h", Severity: "blocking", Fate: Open},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// shifted is a diff that adds five lines above old line 10 and rewrites old
// line 11: old 10 is new 15, and old 11 has no new line.
func shifted(path string) diffmap.File {
	return diffmap.Parse(path, "", "@@ -9,4 +9,9 @@\n a\n+x\n+x\n+x\n+x\n+x\n b\n-c\n+C\n d\n", false)
}

func TestRewordedInsideItsOwnHunkFollowsTheShift(t *testing.T) {
	tests := []struct {
		name          string
		line, end     int
		yLine, yEnd   int
		wantRows      int
		wantFirstFate string
	}{
		{"one end maps, the other follows the shift", 10, 11, 15, 16, 1, Open},
		{"rewritten line follows the shift", 11, 0, 16, 0, 1, Open},
		{"not where the shifted lines are", 11, 0, 11, 0, 2, Open},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			f.files[[2]string{"h1", "h2"}] = []diffmap.File{shifted("a.go")}
			x := fnd("x", "a.go", tt.line)
			x.EndLine = tt.end
			y := fnd("y", "a.go", tt.yLine)
			y.EndLine = tt.yEnd
			got := decide(t, f, nil, review("h1", sk(a, x)), review("h2", sk(a, y)))
			if len(got) != tt.wantRows {
				t.Fatalf("got %d fates, want %d: %+v", len(got), tt.wantRows, got)
			}
			if tt.wantRows == 1 && (got[0].Fingerprint != "y" || got[0].Fate != Open) {
				t.Errorf("got %+v, want y/open", got[0])
			}
		})
	}
}

func TestShiftAt(t *testing.T) {
	insertion := diffmap.Parse("a.go", "", "@@ -5,0 +6,3 @@\n+x\n+x\n+x\n", false)
	// Old lines 9 to 11, the last removed and three lines added in its place.
	ending := diffmap.Parse("a.go", "", "@@ -9,3 +9,5 @@\n a\n b\n-c\n+C\n+C\n+C\n", false)
	tests := []struct {
		name string
		d    diffmap.File
		line int
		want int
	}{
		{"no hunks", diffmap.File{}, 10, 0},
		{"before a hunk", ending, 8, 0},
		{"a hunk ends exactly at the line", ending, 11, 0},
		{"after a hunk", ending, 12, 2},
		{"the line before a pure insertion", insertion, 5, 0},
		{"the line after a pure insertion", insertion, 6, 3},
		{"inside a hunk, after added lines", shifted("a.go"), 11, 5},
		{"inside a hunk, before any context above", diffmap.Parse("a.go", "", "@@ -9,2 +9,2 @@\n-a\n+A\n b\n", false), 9, 0},
	}
	for _, tt := range tests {
		if got := shiftAt(tt.d, tt.line); got != tt.want {
			t.Errorf("%s: shiftAt(%d) = %d, want %d", tt.name, tt.line, got, tt.want)
		}
	}
}

func TestMapRangeEndsHaveTheirOwnShift(t *testing.T) {
	// Old 3 is rewritten in a hunk that adds a line (shifting what follows by
	// one), and old 21 in a later hunk.
	d := diffmap.Parse("a.go", "", "@@ -2,3 +2,4 @@\n a\n-b\n+B\n+B\n c\n@@ -20,3 +21,3 @@\n a\n-b\n+B\n c\n", false)
	lo, hi, limit, path := mapRange([]diffmap.File{d}, true, &track{file: "a.go", line: 3, end: 21})
	if lo != 3 || hi != 22 || limit != Reach || path != "a.go" {
		t.Errorf("got %d..%d limit %d %s, want 3..22 limit %d a.go", lo, hi, limit, path, Reach)
	}
}

func TestNoCompareForACandidateOfAnotherKind(t *testing.T) {
	// x has no line and the only candidate has one, so there is nothing to
	// compare the files for; a left-out skill needs no compare to vanish.
	f := newFake()
	left := sk(a, fnd("y", "a.go", 10))
	left.LeftOut = true
	got := decide(t, f, nil, review("h1", sk(a, fnd("x", "a.go", 0))), review("h2", left))
	if len(got) != 2 {
		t.Errorf("got %d fates, want 2: %+v", len(got), got)
	}
	if f.calls != 0 {
		t.Errorf("compares = %d, want 0", f.calls)
	}
}

// A skill that looked at the whole change lists no file: when it finished, a
// finding it no longer reports is gone, as for any skill that reviewed the file.
func TestWholeSkillCoversEveryFile(t *testing.T) {
	whole := func(fs ...Finding) Skill {
		s := sk(nil, fs...)
		s.Whole = true
		return s
	}
	t.Run("a finding it stops reporting went away", func(t *testing.T) {
		f := newFake()
		f.files[[2]string{"h1", "h2"}] = []diffmap.File{edit("b.go", 10)}
		got := decide(t, f, nil, review("h1", whole(fnd("x", "a.go", 10))), review("h2", whole()))
		if fate := one(t, got); fate.Fate != WentAway {
			t.Errorf("fate = %s, want went_away", fate.Fate)
		}
	})
	t.Run("and fixed when the lines changed", func(t *testing.T) {
		f := newFake()
		f.files[[2]string{"h1", "h2"}] = []diffmap.File{edit("a.go", 10)}
		got := decide(t, f, nil, review("h1", whole(fnd("x", "a.go", 10))), review("h2", whole()))
		if fate := one(t, got); fate.Fate != Fixed {
			t.Errorf("fate = %s, want fixed", fate.Fate)
		}
	})
	t.Run("a failed check leaves it open", func(t *testing.T) {
		f := newFake()
		f.files[[2]string{"h1", "h2"}] = []diffmap.File{edit("a.go", 10)}
		failed := whole()
		failed.Failed = true
		got := decide(t, f, nil, review("h1", whole(fnd("x", "a.go", 10))), review("h2", failed))
		if fate := one(t, got); fate.Fate != Open {
			t.Errorf("fate = %s, want open", fate.Fate)
		}
	})
	t.Run("a review without the check leaves it open", func(t *testing.T) {
		f := newFake()
		f.files[[2]string{"h1", "h2"}] = []diffmap.File{edit("a.go", 10)}
		other := sk(a)
		other.Name = "other"
		got := decide(t, f, nil, review("h1", whole(fnd("x", "a.go", 10))), review("h2", other))
		if fate := one(t, got); fate.Fate != Open {
			t.Errorf("fate = %s, want open", fate.Fate)
		}
	})
	t.Run("one that is not whole still needs the file", func(t *testing.T) {
		f := newFake()
		f.files[[2]string{"h1", "h2"}] = []diffmap.File{edit("a.go", 10)}
		got := decide(t, f, nil, review("h1", sk(nil, fnd("x", "a.go", 10))), review("h2", sk(nil)))
		if fate := one(t, got); fate.Fate != Open {
			t.Errorf("fate = %s, want open", fate.Fate)
		}
	})
}
