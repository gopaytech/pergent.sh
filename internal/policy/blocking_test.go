package policy

import (
	"strings"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// blocking_status neutral turns every failed status neutral and says so in
// the description; it never changes the approval, the kind or the comment.
// A status failure_status already made neutral keeps its text.
func TestDecideBlockingStatus(t *testing.T) {
	t.Parallel()
	const suffix = " · not blocking merge"
	scan := []store.CIJobResult{ciJob("cycode-scan", "failed", true)}
	cases := []struct {
		name    string
		o       func() Outcome
		failure config.FailureStatus
		kind    Kind
		approve bool
		// status and desc with blocking_status failed, then neutral.
		failedStatus, neutralStatus Status
		failedDesc, neutralDesc     string
	}{
		{name: "clean review", o: func() Outcome { return Outcome{Result: completed(skill("a", "no_blocking_issues", "f.go"))} },
			kind: KindApprove, approve: true, failedStatus: StatusSuccess, neutralStatus: StatusSuccess, failedDesc: descClean, neutralDesc: descClean},
		{name: "blocking findings", o: func() Outcome { return Outcome{Result: completed(skill("a", "has_blocking_issues", "f.go"))} },
			kind: KindBlocking, failedStatus: StatusFailed, neutralStatus: StatusNeutral, failedDesc: descBlocking, neutralDesc: descBlocking + suffix},
		{name: "a blocking CI job failed", o: func() Outcome {
			return Outcome{Result: completed(skill("a", "no_blocking_issues", "f.go")), CI: scan}
		}, kind: KindCI, failedStatus: StatusFailed, neutralStatus: StatusNeutral,
			failedDesc: "CI blocks approval: cycode-scan failed", neutralDesc: "CI blocks approval: cycode-scan failed" + suffix},
		{name: "blocking findings and a blocking CI job", o: func() Outcome {
			return Outcome{Result: completed(skill("a", "has_blocking_issues", "f.go")), CI: scan}
		}, kind: KindBlocking, failedStatus: StatusFailed, neutralStatus: StatusNeutral,
			failedDesc: "Blocking issues found; CI blocks approval: cycode-scan failed", neutralDesc: "Blocking issues found; CI blocks approval: cycode-scan failed" + suffix},
		{name: "a failed skill", o: func() Outcome {
			return Outcome{Result: completed(skill("a", "failed"), skill("b", "no_blocking_issues", "f.go"))}
		}, kind: KindFailed, failedStatus: StatusFailed, neutralStatus: StatusNeutral,
			failedDesc: "Review failed for 1 of 2 skills", neutralDesc: "Review failed for 1 of 2 skills" + suffix},
		{name: "a failed skill, failure_status neutral", o: func() Outcome {
			return Outcome{Result: completed(skill("a", "failed"), skill("b", "no_blocking_issues", "f.go"))}
		}, failure: config.FailureStatusNeutral, kind: KindFailed, failedStatus: StatusNeutral, neutralStatus: StatusNeutral,
			failedDesc: "Review failed for 1 of 2 skills", neutralDesc: "Review failed for 1 of 2 skills"},
		{name: "no result, failure_status neutral", o: func() Outcome { return Outcome{FailureReason: "the job timed out"} },
			failure: config.FailureStatusNeutral, kind: KindFailed, failedStatus: StatusNeutral, neutralStatus: StatusNeutral,
			failedDesc: descCouldNotReview, neutralDesc: descCouldNotReview},
		{name: "no result", o: func() Outcome { return Outcome{FailureReason: "the job timed out"} },
			kind: KindFailed, failedStatus: StatusFailed, neutralStatus: StatusNeutral, failedDesc: descCouldNotReview, neutralDesc: descCouldNotReview + suffix},
		{name: "a failed review held by CI, failure_status neutral", o: func() Outcome {
			return Outcome{Result: completed(skill("a", "failed")), CI: scan}
		}, failure: config.FailureStatusNeutral, kind: KindFailed, failedStatus: StatusFailed, neutralStatus: StatusNeutral,
			failedDesc: "Review failed for 1 of 1 skills; CI blocks approval: cycode-scan failed", neutralDesc: "Review failed for 1 of 1 skills; CI blocks approval: cycode-scan failed" + suffix},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, run := range []struct {
				blocking config.BlockingStatus
				status   Status
				desc     string
			}{
				{config.BlockingStatusFailed, tc.failedStatus, tc.failedDesc},
				{"", tc.failedStatus, tc.failedDesc}, // unset is failed
				{config.BlockingStatusNeutral, tc.neutralStatus, tc.neutralDesc},
			} {
				d := Decide(tc.o(), tc.failure, run.blocking)
				if d.Status != run.status || d.Description != run.desc || d.Kind != tc.kind || d.Approve != tc.approve {
					t.Errorf("blocking %q: got status %q desc %q kind %q approve %v\nwant %q %q %q %v",
						run.blocking, d.Status, d.Description, d.Kind, d.Approve, run.status, run.desc, tc.kind, tc.approve)
				}
			}
			if a, b := Decide(tc.o(), tc.failure, config.BlockingStatusFailed), Decide(tc.o(), tc.failure, config.BlockingStatusNeutral); a.Comment != b.Comment {
				t.Errorf("blocking_status changed the comment:\n%s\n---\n%s", a.Comment, b.Comment)
			}
		})
	}
}

// The suffix is always whole: the description before it is cut to fit 140
// characters.
func TestNotBlockingMerge(t *testing.T) {
	t.Parallel()
	if got := NotBlockingMerge("Blocking issues found"); got != "Blocking issues found · not blocking merge" {
		t.Errorf("short = %q", got)
	}
	long := NotBlockingMerge(strings.Repeat("x", 500))
	if n := len([]rune(long)); n != maxDescription {
		t.Errorf("long is %d characters, want %d", n, maxDescription)
	}
	if !strings.HasSuffix(long, "x… · not blocking merge") {
		t.Errorf("long = %q", long)
	}
	// Exactly at the limit: nothing is cut.
	exact := strings.Repeat("y", maxDescription-len([]rune(" · not blocking merge")))
	if got := NotBlockingMerge(exact); got != exact+" · not blocking merge" {
		t.Errorf("exact = %q", got)
	}
	// Through Decide, a long CI job name still fits.
	r := completed(skill("a", "has_blocking_issues", "f.go"))
	d := Decide(Outcome{Result: r, CI: []store.CIJobResult{ciJob(strings.Repeat("z", 500), "failed", true)}}, config.FailureStatusFailed, config.BlockingStatusNeutral)
	if n := len([]rune(d.Description)); n > maxDescription || !strings.HasSuffix(d.Description, " · not blocking merge") ||
		!strings.HasPrefix(d.Description, "Blocking issues found; CI blocks approval: zzz") {
		t.Errorf("description = %q (%d)", d.Description, n)
	}
}
