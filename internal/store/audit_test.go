package store

import (
	"strings"
	"testing"
	"time"
)

func TestAuditLogPaging(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	actor := int64(7)
	for i, action := range []string{"team.create", "team.update", "account.delete"} {
		e := AuditEntry{At: t0.Add(time.Duration(i) * time.Minute), Actor: "admin", Action: action}
		if i == 0 {
			e.ActorID, e.Instance, e.Project, e.Detail = &actor, "main", "acme/widgets", "devs"
		}
		if err := s.Audit(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.AuditLog(ctx, 0, 2)
	if err != nil || len(page) != 2 || page[0].Action != "account.delete" || page[1].Action != "team.update" || page[0].ID <= page[1].ID {
		t.Fatalf("AuditLog(0, 2) = %+v, %v; want the newest two", page, err)
	}
	rest, err := s.AuditLog(ctx, page[1].ID, 2)
	if err != nil || len(rest) != 1 || rest[0].Action != "team.create" {
		t.Fatalf("AuditLog(last, 2) = %+v, %v; want the remaining one", rest, err)
	}
	e := rest[0]
	if e.ActorID == nil || *e.ActorID != 7 || e.Actor != "admin" || e.Instance != "main" || e.Project != "acme/widgets" ||
		e.Detail != "devs" || !e.At.Equal(t0) {
		t.Fatalf("entry = %+v", e)
	}
	if page[0].ActorID != nil || page[0].Instance != "" || page[0].Detail != "" {
		t.Fatalf("entry without an actor ID or target = %+v", page[0])
	}
}

func TestSearchAudit(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	for i, e := range []AuditEntry{
		{Actor: "alice", Action: "command_refused", Instance: "main", Project: "acme/widgets", Detail: "mr=12 command=dismiss"},
		{Actor: "bob (password)", Action: "user_added", Detail: "carol@example.com"},
		{Actor: "alice", Action: "settings_updated", Instance: "main", Project: "acme/gadgets", Detail: "skills"},
		{Actor: "dave", Action: "team_updated", Detail: "100%_done"},
	} {
		e.At = t0.Add(time.Duration(i) * time.Minute)
		if err := s.Audit(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	actions := func(text string, before int64) []string {
		t.Helper()
		got, err := s.SearchAudit(ctx, before, text, 10)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range got {
			out = append(out, e.Action)
		}
		return out
	}
	for _, c := range []struct {
		text string
		want []string
	}{
		{"", []string{"team_updated", "settings_updated", "user_added", "command_refused"}},
		{"ALICE", []string{"settings_updated", "command_refused"}},
		{"command refused", []string{"command_refused"}},
		{"main/acme/widgets", []string{"command_refused"}},
		{"acme/", []string{"settings_updated", "command_refused"}},
		{"carol@", []string{"user_added"}},
		{"password", []string{"user_added"}},
		{"%", []string{"team_updated"}},
		{"_done", []string{"team_updated"}},
		{"nobody", nil},
	} {
		if got := actions(c.text, 0); strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("SearchAudit(%q) = %v, want %v", c.text, got, c.want)
		}
	}
	// The search pages like the log: older than before.
	all, err := s.SearchAudit(ctx, 0, "alice", 1)
	if err != nil || len(all) != 1 {
		t.Fatal(all, err)
	}
	if got := actions("alice", all[0].ID); strings.Join(got, ",") != "command_refused" {
		t.Errorf("SearchAudit(alice, before newest) = %v", got)
	}
}

func TestAuditLogBigintCursor(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	if err := s.Audit(ctx, AuditEntry{At: t0, Actor: "admin", Action: "x"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.AuditLog(ctx, 3_000_000_000, 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("AuditLog(3e9, 10) = %+v, %v; want the entry below the cursor", got, err)
	}
}

func TestAuditEntriesOutliveAccounts(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	mustPassword(t, s, "admin")
	a := mustPlatform(t, s, "main", 1, "alice")
	if err := s.Audit(ctx, AuditEntry{At: t0, ActorID: &a.ID, Actor: "alice", Action: "team.create"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.DeleteAccount(ctx, a.ID); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if got, err := s.AuditLog(ctx, 0, 10); err != nil || len(got) != 1 {
		t.Fatalf("AuditLog after the account is gone = %+v, %v", got, err)
	}
}

func TestPruneAudit(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	for _, at := range []time.Time{t0, t0.Add(time.Hour), t0.Add(2 * time.Hour)} {
		if err := s.Audit(ctx, AuditEntry{At: at, Actor: "admin", Action: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.PruneAudit(ctx, t0.Add(time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("PruneAudit = %d, %v; want 1", n, err)
	}
	if got, _ := s.AuditLog(ctx, 0, 10); len(got) != 2 {
		t.Fatalf("entries left = %d; want 2", len(got))
	}
}
