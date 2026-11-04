package store

import (
	"sync"
	"testing"
	"time"
)

func command(kind, arg string, comment int64) Command {
	return Command{MR: mr1, ProjectPath: "acme/widgets", Platform: "gitlab", Kind: kind, Arg: arg,
		CommentID: comment, Author: "alice", AuthorID: 11, CreatedAt: t0}
}

func TestInsertCommandRedelivery(t *testing.T) {
	s := newStore(t)
	c := command("dismiss", "abc123", 100)
	c.Reason = "not a bug"
	if ok, err := s.InsertCommand(ctx, c); err != nil || !ok {
		t.Fatalf("InsertCommand = %v, %v", ok, err)
	}
	// The same comment delivered again is not stored twice, whatever it says now.
	c.Reason = "edited"
	if ok, err := s.InsertCommand(ctx, c); err != nil || ok {
		t.Fatalf("redelivery: InsertCommand = %v, %v; want false", ok, err)
	}
	// Another comment, and the same comment id on another MR, are new.
	if ok, err := s.InsertCommand(ctx, command("help", "", 101)); err != nil || !ok {
		t.Fatalf("another comment = %v, %v", ok, err)
	}
	same := command("help", "", 100)
	same.MR = mr2
	if ok, err := s.InsertCommand(ctx, same); err != nil || !ok {
		t.Fatalf("same comment id on another MR = %v, %v", ok, err)
	}

	got, err := s.ClaimCommands(ctx, 10, t0)
	if err != nil || len(got) != 3 {
		t.Fatalf("ClaimCommands = %+v, %v", got, err)
	}
	first := got[0]
	if first.ID == 0 || first.MR != mr1 || first.ProjectPath != "acme/widgets" || first.Platform != "gitlab" ||
		first.Kind != "dismiss" || first.Arg != "abc123" || first.Reason != "not a bug" || first.CommentID != 100 ||
		first.Author != "alice" || first.AuthorID != 11 || !first.CreatedAt.Equal(t0) {
		t.Errorf("command = %+v", first)
	}
}

func TestInsertCommandRefusesAnUnknownKind(t *testing.T) {
	s := newStore(t)
	if _, err := s.InsertCommand(ctx, command("explode", "", 1)); err == nil {
		t.Error("InsertCommand accepted an unknown kind")
	}
	for i, kind := range []string{"review", "review_failed", "dismiss", "help", "unknown"} {
		if ok, err := s.InsertCommand(ctx, command(kind, "", int64(i+1))); err != nil || !ok {
			t.Errorf("kind %q: %v, %v", kind, ok, err)
		}
	}
}

func TestClaimCommandsOnce(t *testing.T) {
	s := newStore(t)
	const total = 12
	for i := int64(1); i <= total; i++ {
		if ok, err := s.InsertCommand(ctx, command("review", "", i)); err != nil || !ok {
			t.Fatalf("InsertCommand %d = %v, %v", i, ok, err)
		}
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		seen = map[int64]int{}
	)
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				got, err := s.ClaimCommands(ctx, 2, t0)
				if err != nil {
					t.Error(err)
					return
				}
				if len(got) == 0 {
					return
				}
				mu.Lock()
				for _, c := range got {
					seen[c.ID]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != total {
		t.Errorf("claimed %d distinct commands, want %d", len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("command %d claimed %d times", id, n)
		}
	}
	if got, err := s.ClaimCommands(ctx, 5, t0); err != nil || len(got) != 0 {
		t.Errorf("ClaimCommands after all claimed = %+v, %v", got, err)
	}
	if got, err := s.ClaimCommands(ctx, 0, t0); err != nil || got != nil {
		t.Errorf("ClaimCommands(limit 0) = %+v, %v", got, err)
	}
}

// Commands are claimed oldest first, at most limit at a time.
func TestClaimCommandsOrderAndLimit(t *testing.T) {
	s := newStore(t)
	for i := int64(1); i <= 3; i++ {
		if _, err := s.InsertCommand(ctx, command("review", "", i)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ClaimCommands(ctx, 2, t0)
	if err != nil || len(got) != 2 || got[0].CommentID != 1 || got[1].CommentID != 2 {
		t.Fatalf("first claim = %+v, %v", got, err)
	}
	got, err = s.ClaimCommands(ctx, 2, t0)
	if err != nil || len(got) != 1 || got[0].CommentID != 3 {
		t.Fatalf("second claim = %+v, %v", got, err)
	}
}

func TestRecentCommand(t *testing.T) {
	s := newStore(t)
	insert := func(mr MRKey, kind, arg string, comment int64, at time.Time) int64 {
		t.Helper()
		c := command(kind, arg, comment)
		c.MR, c.CreatedAt = mr, at
		if ok, err := s.InsertCommand(ctx, c); err != nil || !ok {
			t.Fatalf("InsertCommand = %v, %v", ok, err)
		}
		var id int64
		if err := s.pool.QueryRow(ctx, `SELECT id FROM commands WHERE comment_id = $1 AND mr_iid = $2`, comment, mr.IID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	window := 30 * time.Second
	recent := func(mr MRKey, kind, arg string, at time.Time) bool {
		t.Helper()
		ok, err := s.RecentCommand(ctx, mr, kind, arg, at.Add(-window))
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	earlier := insert(mr1, "dismiss", "abc", 1, t0)
	// A command that has not been answered yet, like the one being
	// checked, is not a recent command.
	if recent(mr1, "dismiss", "abc", t0.Add(10*time.Second)) {
		t.Error("an unanswered command counted")
	}
	if _, err := s.ClaimCommands(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishCommand(ctx, earlier, CommandDone, t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	if !recent(mr1, "dismiss", "abc", t0.Add(10*time.Second)) {
		t.Error("an answered command 10s ago was not found")
	}
	if recent(mr1, "dismiss", "abc", t0.Add(40*time.Second)) {
		t.Error("a command 40s ago was found")
	}
	if recent(mr1, "dismiss", "xyz", t0.Add(10*time.Second)) {
		t.Error("another finding ID matched")
	}
	if recent(mr1, "review", "abc", t0.Add(10*time.Second)) {
		t.Error("another command matched")
	}
	if recent(mr2, "dismiss", "abc", t0.Add(10*time.Second)) {
		t.Error("another MR matched")
	}

	// A command that did nothing but be answered (a help, an "already
	// dismissed") is answered all the same.
	noop := insert(mr1, "help", "", 3, t0)
	if _, err := s.ClaimCommands(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishCommand(ctx, noop, CommandNoop("help"), t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if !recent(mr1, "help", "", t0.Add(10*time.Second)) {
		t.Error("a command answered with nothing to do was not found")
	}

	// A command refused or dropped does not hold others off.
	refused := insert(mr1, "review", "", 2, t0)
	if _, err := s.ClaimCommands(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishCommand(ctx, refused, "refused", t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if recent(mr1, "review", "", t0.Add(10*time.Second)) {
		t.Error("a refused command counted")
	}
}

// RecentAnswered tells whether the author had a command on the change
// answered since a time, whatever the command, unless it was carried out
// (CommandDone) or throttled.
func TestRecentAnswered(t *testing.T) {
	s := newStore(t)
	finish := func(mr MRKey, author int64, comment int64, outcome string, at time.Time) {
		t.Helper()
		c := command("help", "", comment)
		c.MR, c.AuthorID = mr, author
		if ok, err := s.InsertCommand(ctx, c); err != nil || !ok {
			t.Fatalf("InsertCommand = %v, %v", ok, err)
		}
		got, err := s.ClaimCommands(ctx, 100, t0)
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range got {
			if g.CommentID == comment && g.MR == mr {
				if err := s.FinishCommand(ctx, g.ID, outcome, at); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	answered := func(mr MRKey, author int64, since time.Time) bool {
		t.Helper()
		ok, err := s.RecentAnswered(ctx, mr, author, since)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	if answered(mr1, 11, t0) {
		t.Fatal("no command at all counted")
	}
	// Claimed and not finished: not answered.
	c := command("help", "", 1)
	if _, err := s.InsertCommand(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimCommands(ctx, 100, t0); err != nil {
		t.Fatal(err)
	}
	if answered(mr1, 11, t0.Add(-time.Hour)) {
		t.Error("an unanswered command counted")
	}

	finish(mr1, 12, 2, "refused: no permission", t0.Add(time.Minute))
	if !answered(mr1, 12, t0) {
		t.Error("a refused command was not found")
	}
	if !answered(mr1, 12, t0.Add(time.Minute)) {
		t.Error("the boundary is inclusive")
	}
	if answered(mr1, 12, t0.Add(time.Minute+time.Second)) {
		t.Error("an older answer was found")
	}
	if answered(mr1, 13, t0) || answered(mr2, 12, t0) {
		t.Error("another author or change matched")
	}

	// A throttled command is not an answer: throttling never extends itself.
	finish(mr1, 14, 3, CommandThrottled, t0.Add(time.Minute))
	if answered(mr1, 14, t0) {
		t.Error("a throttled command counted")
	}

	// A command that was carried out is no answer to hold others off for ...
	finish(mr1, 15, 4, CommandDone, t0.Add(time.Minute))
	if answered(mr1, 15, t0) {
		t.Error("a carried-out command counted")
	}
	// ... but one answered without doing anything is: a no-op, a drop, a
	// command given up on.
	for i, outcome := range []string{CommandNoop("help"), CommandNoop("nothing failed"), "dropped: the author is a bot", "gave up: 502"} {
		author := int64(20 + i)
		finish(mr1, author, int64(10+i), outcome, t0.Add(time.Minute))
		if !answered(mr1, author, t0) {
			t.Errorf("a command finished with %q was not found", outcome)
		}
	}
}

func TestFinishCommand(t *testing.T) {
	s := newStore(t)
	if _, err := s.InsertCommand(ctx, command("review", "", 1)); err != nil {
		t.Fatal(err)
	}
	got, err := s.ClaimCommands(ctx, 1, t0)
	if err != nil || len(got) != 1 {
		t.Fatalf("ClaimCommands = %+v, %v", got, err)
	}
	if err := s.FinishCommand(ctx, got[0].ID, CommandDone, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	var outcome string
	var finished *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT outcome, finished_at FROM commands WHERE id = $1`, got[0].ID).Scan(&outcome, &finished); err != nil {
		t.Fatal(err)
	}
	if outcome != CommandDone || finished == nil || !finished.Equal(t0.Add(time.Minute)) {
		t.Errorf("outcome, finished_at = %q, %v", outcome, finished)
	}
	// A finished command is not claimed again.
	if again, _ := s.ClaimCommands(ctx, 1, t0); len(again) != 0 {
		t.Errorf("finished command claimed again: %+v", again)
	}
}

// DeferCommand puts a claimed command back for later with its attempts as
// they were.
func TestDeferCommand(t *testing.T) {
	s := newStore(t)
	if _, err := s.InsertCommand(ctx, command("review", "", 1)); err != nil {
		t.Fatal(err)
	}
	got, err := s.ClaimCommands(ctx, 1, t0)
	if err != nil || len(got) != 1 {
		t.Fatalf("ClaimCommands = %+v, %v", got, err)
	}
	later := t0.Add(time.Minute)
	if err := s.DeferCommand(ctx, got[0].ID, later); err != nil {
		t.Fatal(err)
	}
	if again, err := s.ClaimCommands(ctx, 1, t0.Add(30*time.Second)); err != nil || len(again) != 0 {
		t.Fatalf("claimed before not_before: %+v, %v", again, err)
	}
	again, err := s.ClaimCommands(ctx, 1, later)
	if err != nil || len(again) != 1 || again[0].Attempts != 0 {
		t.Fatalf("claim at not_before = %+v, %v; want the command, still at 0 attempts", again, err)
	}
	// A finished command is left alone.
	if err := s.FinishCommand(ctx, again[0].ID, CommandDone, later); err != nil {
		t.Fatal(err)
	}
	if err := s.DeferCommand(ctx, again[0].ID, later.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if third, _ := s.ClaimCommands(ctx, 1, later.Add(2*time.Hour)); len(third) != 0 {
		t.Errorf("a finished command was offered again: %+v", third)
	}
}

func TestReleaseCommand(t *testing.T) {
	s := newStore(t)
	if _, err := s.InsertCommand(ctx, command("review", "", 1)); err != nil {
		t.Fatal(err)
	}
	got, err := s.ClaimCommands(ctx, 1, t0)
	if err != nil || len(got) != 1 || got[0].Attempts != 0 {
		t.Fatalf("ClaimCommands = %+v, %v", got, err)
	}
	later := t0.Add(time.Minute)
	if err := s.ReleaseCommand(ctx, got[0].ID, later); err != nil {
		t.Fatal(err)
	}
	// Not offered before not_before, offered from then on with one more attempt.
	if again, err := s.ClaimCommands(ctx, 1, t0.Add(30*time.Second)); err != nil || len(again) != 0 {
		t.Fatalf("claimed before not_before: %+v, %v", again, err)
	}
	again, err := s.ClaimCommands(ctx, 1, later)
	if err != nil || len(again) != 1 || again[0].ID != got[0].ID || again[0].Attempts != 1 {
		t.Fatalf("claim at not_before = %+v, %v; want the command with attempts 1", again, err)
	}
	if err := s.ReleaseCommand(ctx, again[0].ID, later); err != nil {
		t.Fatal(err)
	}
	if third, _ := s.ClaimCommands(ctx, 1, later); len(third) != 1 || third[0].Attempts != 2 {
		t.Errorf("third claim = %+v, want attempts 2", third)
	}

	// A finished command is not released.
	if err := s.FinishCommand(ctx, got[0].ID, CommandDone, later); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseCommand(ctx, got[0].ID, later); err != nil {
		t.Fatal(err)
	}
	if left, _ := s.ClaimCommands(ctx, 1, later.Add(time.Hour)); len(left) != 0 {
		t.Errorf("finished command offered: %+v", left)
	}
}

// A claim not renewed within five minutes is taken to be lost; a fresh one
// holds the command.
func TestClaimCommandsReclaimsStaleClaim(t *testing.T) {
	s := newStore(t)
	if _, err := s.InsertCommand(ctx, command("review", "", 1)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ClaimCommands(ctx, 1, t0); len(got) != 1 {
		t.Fatalf("first claim = %+v", got)
	}
	if got, err := s.ClaimCommands(ctx, 1, t0.Add(4*time.Minute)); err != nil || len(got) != 0 {
		t.Fatalf("a fresh claim was taken: %+v, %v", got, err)
	}
	got, err := s.ClaimCommands(ctx, 1, t0.Add(6*time.Minute))
	if err != nil || len(got) != 1 || got[0].Attempts != 0 {
		t.Fatalf("stale claim = %+v, %v; want it reclaimed", got, err)
	}
	// The new claim is fresh again.
	if again, _ := s.ClaimCommands(ctx, 1, t0.Add(7*time.Minute)); len(again) != 0 {
		t.Errorf("reclaimed command offered at once: %+v", again)
	}
}

func TestClaimCommandsSkipsInstances(t *testing.T) {
	s := newStore(t)
	other := command("review", "", 2)
	other.MR = MRKey{Instance: "https://other.example.com", ProjectID: 7, IID: 3}
	for _, c := range []Command{command("review", "", 1), other} {
		if _, err := s.InsertCommand(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ClaimCommands(ctx, 10, t0, mr1.Instance)
	if err != nil || len(got) != 1 || got[0].MR.Instance != "https://other.example.com" {
		t.Fatalf("claim skipping %s = %+v, %v", mr1.Instance, got, err)
	}
	// The skipped one stayed unclaimed.
	got, err = s.ClaimCommands(ctx, 10, t0)
	if err != nil || len(got) != 1 || got[0].MR.Instance != mr1.Instance {
		t.Fatalf("claim of the rest = %+v, %v", got, err)
	}
}

func TestPruneCommands(t *testing.T) {
	s := newStore(t)
	for i := int64(1); i <= 3; i++ {
		if _, err := s.InsertCommand(ctx, command("review", "", i)); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := s.ClaimCommands(ctx, 3, t0)
	// 1 finished long ago, 2 finished recently, 3 never finished.
	if err := s.FinishCommand(ctx, got[0].ID, CommandDone, t0.Add(-100*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishCommand(ctx, got[1].ID, CommandDone, t0); err != nil {
		t.Fatal(err)
	}
	n, err := s.PruneCommands(ctx, t0.Add(-90*24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("PruneCommands = %d, %v; want 1", n, err)
	}
	var left int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM commands`).Scan(&left); err != nil || left != 2 {
		t.Errorf("commands left = %d, %v; want 2", left, err)
	}
	// The pruned comment is stored again if redelivered, which is fine.
}

// Deleting a project or an instance's data takes its dismissals and
// commands with it, and only those.
func TestDeletesRemoveDismissalsAndCommands(t *testing.T) {
	s := newStore(t)
	otherProject := MRKey{Instance: mr1.Instance, ProjectID: 8, IID: 3}
	otherInstance := MRKey{Instance: "lab", ProjectID: 7, IID: 3}
	if err := s.CreateInstance(ctx, InstanceRow{Name: mr1.Instance, Platform: "gitlab"}, map[string][]byte{"review_token": []byte("x")}, t0); err != nil {
		t.Fatal(err)
	}
	seed := func() {
		t.Helper()
		for i, mr := range []MRKey{mr1, otherProject, otherInstance} {
			if _, err := s.Dismiss(ctx, Dismissal{MR: mr, Fingerprint: "fp", Severity: "blocking", Reason: "r"}); err != nil {
				t.Fatal(err)
			}
			c := command("review", "", int64(i+1))
			c.MR = mr
			if _, err := s.InsertCommand(ctx, c); err != nil {
				t.Fatal(err)
			}
		}
	}
	count := func(table string) int {
		t.Helper()
		var n int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	seed()
	if err := s.DeleteProject(ctx, mr1.Instance, mr1.ProjectID); err != nil {
		t.Fatal(err)
	}
	if count("dismissals") != 2 || count("commands") != 2 {
		t.Errorf("after DeleteProject: %d dismissals, %d commands; want 2, 2", count("dismissals"), count("commands"))
	}
	if got, _ := s.Dismissals(ctx, mr1); len(got) != 0 {
		t.Errorf("the project's dismissals remain: %+v", got)
	}

	if err := s.DeleteInstanceData(ctx, mr1.Instance); err != nil {
		t.Fatal(err)
	}
	if count("dismissals") != 1 || count("commands") != 1 {
		t.Errorf("after DeleteInstanceData: %d dismissals, %d commands; want 1, 1", count("dismissals"), count("commands"))
	}
	if got, _ := s.Dismissals(ctx, otherInstance); len(got) != 1 {
		t.Errorf("another instance's dismissal was removed: %+v", got)
	}
}

// A command written as a reply in a thread keeps the discussion and the
// comment it replies to; a plain comment has neither.
func TestCommandThreadFields(t *testing.T) {
	s := newStore(t)
	plain := command("review", "", 1)
	gitlab := command("dismiss", "abc123", 2)
	gitlab.Discussion = "d3adbeef"
	github := command("dismiss", "abc123", 3)
	github.ReplyTo = 555
	for _, c := range []Command{plain, gitlab, github} {
		if ok, err := s.InsertCommand(ctx, c); err != nil || !ok {
			t.Fatalf("InsertCommand(%d) = %v, %v", c.CommentID, ok, err)
		}
	}
	got, err := s.ClaimCommands(ctx, 10, t0)
	if err != nil || len(got) != 3 {
		t.Fatalf("ClaimCommands = %+v, %v", got, err)
	}
	if got[0].Discussion != "" || got[0].ReplyTo != 0 || got[0].InThread() {
		t.Errorf("plain comment = %+v", got[0])
	}
	if got[1].Discussion != "d3adbeef" || got[1].ReplyTo != 0 || !got[1].InThread() {
		t.Errorf("GitLab reply = %+v", got[1])
	}
	if got[2].Discussion != "" || got[2].ReplyTo != 555 || !got[2].InThread() {
		t.Errorf("GitHub reply = %+v", got[2])
	}
}

// RefusedBefore tells whether an earlier, finished command of the author on
// the change ended with the outcome.
func TestRefusedBefore(t *testing.T) {
	s := newStore(t)
	const why = "refused: not the author or a maintainer"
	finish := func(mr MRKey, author, comment int64, outcome string, at time.Time) {
		t.Helper()
		c := command("review", "", comment)
		c.MR, c.AuthorID = mr, author
		if ok, err := s.InsertCommand(ctx, c); err != nil || !ok {
			t.Fatalf("InsertCommand = %v, %v", ok, err)
		}
		got, err := s.ClaimCommands(ctx, 100, t0)
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range got {
			if g.CommentID == comment && g.MR == mr {
				if outcome != "" {
					if err := s.FinishCommand(ctx, g.ID, outcome, at); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
	}
	before := func(mr MRKey, author int64, outcome string, at time.Time) bool {
		t.Helper()
		ok, err := s.RefusedBefore(ctx, mr, author, outcome, at)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	if before(mr1, 11, why, t0.Add(time.Hour)) {
		t.Fatal("no command at all counted")
	}
	finish(mr1, 11, 1, why, t0.Add(time.Minute))
	if !before(mr1, 11, why, t0.Add(time.Hour)) {
		t.Error("an earlier refusal was not found")
	}
	if before(mr1, 11, why, t0.Add(time.Minute)) || before(mr1, 11, why, t0) {
		t.Error("a refusal finished at or after the time counted")
	}
	if before(mr1, 11, "refused: no permission", t0.Add(time.Hour)) {
		t.Error("another outcome matched")
	}
	if before(mr1, 12, why, t0.Add(time.Hour)) {
		t.Error("another author matched")
	}
	if before(mr2, 11, why, t0.Add(time.Hour)) {
		t.Error("another change matched")
	}

	// A command claimed and not finished has no outcome yet.
	finish(mr1, 13, 2, "", time.Time{})
	if before(mr1, 13, why, t0.Add(time.Hour)) || before(mr1, 13, "", t0.Add(time.Hour)) {
		t.Error("an unfinished command counted")
	}
}
