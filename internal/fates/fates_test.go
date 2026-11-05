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
	files map[[2]string][]diffmap.File
	errs  map[[2]string]error
}

func (f *fake) compare(from, to string) ([]diffmap.File, error) {
	f.calls++
	p := [2]string{from, to}
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
	got := decide(t, f, nil,
		review("h1", sk(a, fnd("x", "a.go", 10))),
		review("h2", sk(a)),
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
