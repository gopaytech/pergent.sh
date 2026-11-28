package security

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestStartErrorTexts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		err  error
		want string
	}{
		{ErrOff, "Start scan is off: an admin must set securityJobEgress.cidrs, or networkPolicy.enabled: false, in the Helm values first"},
		{ErrPaused, "the repository is paused"},
		{ErrQuota, "the monthly token quota is used up"},
		{&TeamLimitError{Team: "Payments"},
			"every model of the chain is at the team's own limit: a maintainer of Payments can raise it on the team page"},
		{&RefusedError{Reason: "couldn't read the default branch"}, "couldn't read the default branch"},
	}
	for _, c := range cases {
		if got := c.err.Error(); got != c.want {
			t.Errorf("Error() = %q, want %q", got, c.want)
		}
	}
	var tl *TeamLimitError
	if !errors.As(error(&TeamLimitError{Team: "x"}), &tl) || tl.Team != "x" {
		t.Errorf("errors.As(*TeamLimitError) = %v", tl)
	}
}

func ptr(n int) *int { return &n }

func TestCheckLimits(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		steps, sub    *int
		timeout       time.Duration
		wantErr, word string
	}{
		{name: "the skill's own steps", timeout: DefaultTimeout},
		{name: "the ends of each range", steps: ptr(5), sub: ptr(300), timeout: MinTimeout},
		{name: "the top ends", steps: ptr(1000), sub: ptr(5), timeout: MaxTimeout},
		{name: "main steps too few", steps: ptr(4), timeout: DefaultTimeout, word: "max steps of the main agent"},
		{name: "main steps too many", steps: ptr(1001), timeout: DefaultTimeout, word: "max steps of the main agent"},
		{name: "sub-agent steps too many", sub: ptr(301), timeout: DefaultTimeout, word: "max steps of each sub-agent"},
		{name: "sub-agent steps too few", sub: ptr(4), timeout: DefaultTimeout, word: "max steps of each sub-agent"},
		{name: "timeout too short", timeout: MinTimeout - time.Second, word: "timeout"},
		{name: "timeout too long", timeout: MaxTimeout + time.Second, word: "timeout"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := CheckLimits(c.steps, c.sub, c.timeout)
			switch {
			case c.word == "" && err != nil:
				t.Fatalf("CheckLimits = %v, want nil", err)
			case c.word != "" && (err == nil || !strings.Contains(err.Error(), c.word)):
				t.Fatalf("CheckLimits = %v, want an error naming %q", err, c.word)
			}
		})
	}
}
