package store

import (
	"errors"
	"slices"
	"testing"
	"time"
)

var t1 = t0.Add(time.Hour)

// saveReviewers saves a repository's reviewers 2.. and judge on main.
func saveReviewers(t *testing.T, s *Store, project string, admin bool, at time.Time, judge []EntryRef, extra ...[]EntryRef) RepoReviewers {
	t.Helper()
	before, err := s.SaveReviewers(ctx, ReviewersSave{Instance: "main", Project: project, Extra: extra, Judge: judge, SavedByAdmin: admin}, at)
	if err != nil {
		t.Fatalf("SaveReviewers(%s): %v", project, err)
	}
	return before
}

func mustReviewers(t *testing.T, s *Store, project string) RepoReviewers {
	t.Helper()
	r, err := s.Reviewers(ctx, "main", project)
	if err != nil {
		t.Fatalf("Reviewers(%s): %v", project, err)
	}
	return r
}

// extraEntries is each extra reviewer's entries, in order.
func extraEntries(r RepoReviewers) [][]EntryRef {
	out := make([][]EntryRef, len(r.Extra))
	for i, c := range r.Extra {
		out[i] = c.Entries
	}
	return out
}

func sameEntryLists(a, b [][]EntryRef) bool {
	return slices.EqualFunc(a, b, func(x, y []EntryRef) bool { return slices.Equal(x, y) })
}

func reviewerRows(t *testing.T, s *Store) (chains, entries int) {
	t.Helper()
	if err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM reviewer_chains), (SELECT count(*) FROM reviewer_chain_entries)`).
		Scan(&chains, &entries); err != nil {
		t.Fatal(err)
	}
	return chains, entries
}

// Reviewers 2 and 3 and the judge are saved and read back in slot order;
// reviewer 1's chain A and the repository's row are left as they were.
func TestReviewersRoundTrip(t *testing.T) {
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	g1 := newModel(t, s, "anthropic", "sonnet", "", nil)
	g2 := newModel(t, s, "openai", "gpt-5", "", nil)
	tm := newTeamModel(t, s, team, "One", nil)
	G1, G2, T := EntryRef{SourceGlobal, g1.ID}, EntryRef{SourceGlobal, g2.ID}, EntryRef{SourceTeam, tm.ID}
	addRepos(t, s, "acme/api")
	if err := s.SaveChain(ctx, Chain{Scope: ScopeRepo, Instance: "main", Project: "acme/api", Entries: []EntryRef{G1}}, t0); err != nil {
		t.Fatal(err)
	}
	chainA, _ := mustChain(t, s, ScopeRepo, 0, "main", "acme/api")
	repo := mustRepo(t, s, "acme/api")

	before := saveReviewers(t, s, "acme/api", true, t1, []EntryRef{G1}, []EntryRef{G2}, []EntryRef{T, G1})
	if len(before.Extra) != 0 || before.JudgeFound {
		t.Errorf("before the first save = %+v; want nothing", before)
	}
	r := mustReviewers(t, s, "ACME/api")
	if !sameEntryLists(extraEntries(r), [][]EntryRef{{G2}, {T, G1}}) {
		t.Errorf("extra reviewers = %+v", r.Extra)
	}
	for i, slot := range []string{"r2", "r3"} {
		if c := r.Extra[i]; c.Slot != slot || c.Scope != ScopeRepo || c.Instance != "main" || c.Project != "acme/api" ||
			!c.SavedByAdmin || !c.UpdatedAt.Equal(t1) {
			t.Errorf("reviewer %d = %+v; want slot %s", i+2, c, slot)
		}
	}
	if !r.JudgeFound || r.Judge.Slot != SlotJudge || !slices.Equal(r.Judge.Entries, []EntryRef{G1}) {
		t.Errorf("judge = %+v, %v", r.Judge, r.JudgeFound)
	}
	if again := saveReviewers(t, s, "acme/api", true, t1, []EntryRef{G1}, []EntryRef{G2}, []EntryRef{T, G1}); !sameEntryLists(extraEntries(again), extraEntries(r)) || !again.JudgeFound {
		t.Errorf("before the second save = %+v; want the first save", again)
	}
	if c, _ := mustChain(t, s, ScopeRepo, 0, "main", "acme/api"); !slices.Equal(c.Entries, chainA.Entries) || !c.UpdatedAt.Equal(chainA.UpdatedAt) {
		t.Errorf("chain A = %+v; want %+v", c, chainA)
	}
	if after := mustRepo(t, s, "acme/api"); after.ChainMode != repo.ChainMode || !samePtr(after.ModelID, repo.ModelID) {
		t.Errorf("repository = chain %s, model %v; want %s, %v", after.ChainMode, after.ModelID, repo.ChainMode, repo.ModelID)
	}
	if all, err := s.Chains(ctx); err != nil || len(all) != 1 {
		t.Errorf("model_chains = %+v, %v; want only chain A", all, err)
	}
	if other := mustReviewers(t, s, "acme/web"); len(other.Extra) != 0 || other.JudgeFound {
		t.Errorf("a repository with none = %+v", other)
	}
	if SlotOf(2) != "r2" || SlotOf(5) != "r5" {
		t.Errorf("SlotOf = %q, %q", SlotOf(2), SlotOf(5))
	}
}

// Each refused save stores nothing.
func TestSaveReviewersRefusals(t *testing.T) {
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	g := newModel(t, s, "openai", "gpt-5", "", nil)
	tm := newTeamModel(t, s, team, "Gone", nil)
	if _, _, err := s.DeleteTeamModel(ctx, team, tm.ID); err != nil {
		t.Fatal(err)
	}
	G, Gone := EntryRef{SourceGlobal, g.ID}, EntryRef{SourceTeam, tm.ID}
	one := []EntryRef{G}
	six := []EntryRef{G, G, G, G, G, G}
	for _, c := range []struct {
		name  string
		extra [][]EntryRef
		judge []EntryRef
		want  error
	}{
		{"5 extra", [][]EntryRef{one, one, one, one, one}, one, ErrTooManyReviewers},
		{"an empty extra", [][]EntryRef{one, {}}, one, ErrEmptyReviewer},
		{"6 entries", [][]EntryRef{six}, one, ErrChainTooLong},
		{"a judge of 6 entries", [][]EntryRef{one}, six, ErrChainTooLong},
		{"no judge", [][]EntryRef{one}, nil, ErrNoJudge},
		{"a judge with no extra", nil, one, ErrJudgeWithoutReviewers},
		{"a deleted team entry", [][]EntryRef{{G, Gone}}, one, ErrModelNotFound},
		{"a deleted team judge", [][]EntryRef{one}, []EntryRef{Gone}, ErrModelNotFound},
	} {
		_, err := s.SaveReviewers(ctx, ReviewersSave{Instance: "main", Project: "acme/api", Extra: c.extra, Judge: c.judge}, t0)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: %v; want %v", c.name, err, c.want)
		}
		if chains, entries := reviewerRows(t, s); chains != 0 || entries != 0 {
			t.Errorf("%s: %d chains and %d entries stored", c.name, chains, entries)
		}
	}
}

// A maintainer's save that changes only reviewer 3 keeps the flag and date
// of the admin's reviewer 2 and judge.
func TestSaveReviewersKeepsUnchangedSlotFlag(t *testing.T) {
	s := newStore(t)
	g1 := newModel(t, s, "anthropic", "sonnet", "", nil)
	g2 := newModel(t, s, "openai", "gpt-5", "", nil)
	g3 := newModel(t, s, "openai", "gpt-6", "", nil)
	G1, G2, G3 := EntryRef{SourceGlobal, g1.ID}, EntryRef{SourceGlobal, g2.ID}, EntryRef{SourceGlobal, g3.ID}
	saveReviewers(t, s, "acme/api", true, t0, []EntryRef{G1}, []EntryRef{G1}, []EntryRef{G2})
	saveReviewers(t, s, "acme/api", false, t1, []EntryRef{G1}, []EntryRef{G1}, []EntryRef{G3})
	r := mustReviewers(t, s, "acme/api")
	if c := r.Extra[0]; !c.SavedByAdmin || !c.UpdatedAt.Equal(t0) || !slices.Equal(c.Entries, []EntryRef{G1}) {
		t.Errorf("r2 = %+v; want the admin's, unchanged", c)
	}
	if c := r.Extra[1]; c.SavedByAdmin || !c.UpdatedAt.Equal(t1) || !slices.Equal(c.Entries, []EntryRef{G3}) {
		t.Errorf("r3 = %+v; want the maintainer's", c)
	}
	if c := r.Judge; !c.SavedByAdmin || !c.UpdatedAt.Equal(t0) {
		t.Errorf("judge = %+v; want the admin's, unchanged", c)
	}
}

// A save with no extra reviewer deletes every slot, the judge too.
func TestSaveReviewersRemoveLastDropsJudge(t *testing.T) {
	s := newStore(t)
	g := newModel(t, s, "openai", "gpt-5", "", nil)
	G := EntryRef{SourceGlobal, g.ID}
	saveReviewers(t, s, "acme/api", false, t0, []EntryRef{G}, []EntryRef{G})
	before := saveReviewers(t, s, "acme/api", false, t1, nil)
	if !sameEntryLists(extraEntries(before), [][]EntryRef{{G}}) || !before.JudgeFound {
		t.Errorf("before = %+v; want reviewer 2 and the judge", before)
	}
	if chains, entries := reviewerRows(t, s); chains != 0 || entries != 0 {
		t.Errorf("%d chains and %d entries left; want none", chains, entries)
	}
	if r := mustReviewers(t, s, "acme/api"); len(r.Extra) != 0 || r.JudgeFound {
		t.Errorf("Reviewers = %+v; want none", r)
	}
}

// [P9] A maintainer removes reviewer 2: the admin's reviewers 3 and 4 move
// down to 2 and 3 with their flag, as unchanged.
func TestSaveReviewersRemoveKeepsMovedFlag(t *testing.T) {
	s := newStore(t)
	g1 := newModel(t, s, "anthropic", "sonnet", "", nil)
	g2 := newModel(t, s, "openai", "gpt-5", "", nil)
	g3 := newModel(t, s, "openai", "gpt-6", "", nil)
	g4 := newModel(t, s, "openai", "gpt-7", "", nil)
	G1, G2, G3, G4 := EntryRef{SourceGlobal, g1.ID}, EntryRef{SourceGlobal, g2.ID}, EntryRef{SourceGlobal, g3.ID}, EntryRef{SourceGlobal, g4.ID}
	J := []EntryRef{G1}
	saveReviewers(t, s, "acme/api", true, t0, J, []EntryRef{G1}, []EntryRef{G2}, []EntryRef{G3})
	saveReviewers(t, s, "acme/api", false, t0, J, []EntryRef{G4}, []EntryRef{G2}, []EntryRef{G3})
	if r := mustReviewers(t, s, "acme/api"); r.Extra[0].SavedByAdmin || !r.Extra[1].SavedByAdmin || !r.Extra[2].SavedByAdmin {
		t.Fatalf("setup = %+v; want r2 the maintainer's, r3 and r4 the admin's", r.Extra)
	}
	saveReviewers(t, s, "acme/api", false, t1, J, []EntryRef{G2}, []EntryRef{G3})
	r := mustReviewers(t, s, "acme/api")
	if !sameEntryLists(extraEntries(r), [][]EntryRef{{G2}, {G3}}) {
		t.Fatalf("extra reviewers = %+v", r.Extra)
	}
	for i, c := range r.Extra {
		if c.Slot != SlotOf(i+2) || !c.SavedByAdmin || !c.UpdatedAt.Equal(t0) {
			t.Errorf("reviewer %d = %+v; want the admin's chain moved down, unchanged", i+2, c)
		}
	}
	// A remove that also edits the moved chain is a change: the saver's flag.
	saveReviewers(t, s, "acme/api", false, t1, J, []EntryRef{G3, G4})
	if c := mustReviewers(t, s, "acme/api").Extra[0]; c.SavedByAdmin || !c.UpdatedAt.Equal(t1) {
		t.Errorf("an edited moved chain = %+v; want the maintainer's", c)
	}
}

// A team entry that is reviewer 3's only entry and reviewer 2's second goes:
// reviewer 3 goes, reviewer 4 and 5 move to 3 and 4 with their flag, reviewer
// 2 loses the entry, and the emptied judge stays with no entries. Chain A
// loses it as before.
func TestDeleteTeamModelCompactsReviewers(t *testing.T) {
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	g1 := newModel(t, s, "anthropic", "sonnet", "", nil)
	g2 := newModel(t, s, "openai", "gpt-5", "", nil)
	g3 := newModel(t, s, "openai", "gpt-6", "", nil)
	tm := newTeamModel(t, s, team, "One", nil)
	G1, G2, G3, T := EntryRef{SourceGlobal, g1.ID}, EntryRef{SourceGlobal, g2.ID}, EntryRef{SourceGlobal, g3.ID}, EntryRef{SourceTeam, tm.ID}
	addRepos(t, s, "acme/api", "acme/web")
	if err := s.SaveChain(ctx, Chain{Scope: ScopeRepo, Instance: "main", Project: "acme/api", Entries: []EntryRef{T, G1}}, t0); err != nil {
		t.Fatal(err)
	}
	saveReviewers(t, s, "acme/api", true, t0, []EntryRef{T}, []EntryRef{G1, T}, []EntryRef{T}, []EntryRef{G2}, []EntryRef{G3})
	saveReviewers(t, s, "acme/web", false, t0, []EntryRef{G1}, []EntryRef{G2})

	changed, changedR, err := s.DeleteTeamModel(ctx, team, tm.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed[0].Scope != ScopeRepo || !slices.Equal(changed[0].Entries, []EntryRef{G1}) {
		t.Errorf("chain A changed = %+v; want acme/api's [G1]", changed)
	}
	if c, _ := mustChain(t, s, ScopeRepo, 0, "main", "acme/api"); !slices.Equal(c.Entries, []EntryRef{G1}) {
		t.Errorf("chain A = %+v", c.Entries)
	}
	if len(changedR) != 3 {
		t.Errorf("reviewer chains changed = %+v; want r2, r3 and the judge", changedR)
	}
	for _, c := range changedR {
		if c.Project != "acme/api" || slices.Contains(c.Entries, T) {
			t.Errorf("changed reviewer chain = %+v", c)
		}
	}
	r := mustReviewers(t, s, "acme/api")
	if !sameEntryLists(extraEntries(r), [][]EntryRef{{G1}, {G2}, {G3}}) {
		t.Errorf("extra reviewers = %+v; want [G1] [G2] [G3]", r.Extra)
	}
	for i, c := range r.Extra {
		if c.Slot != SlotOf(i+2) || !c.SavedByAdmin {
			t.Errorf("reviewer %d = %+v; want slot %s, the admin's", i+2, c, SlotOf(i+2))
		}
	}
	if !r.JudgeFound || len(r.Judge.Entries) != 0 || !r.Judge.SavedByAdmin {
		t.Errorf("judge = %+v, %v; want the row with no entries", r.Judge, r.JudgeFound)
	}
	if w := mustReviewers(t, s, "acme/web"); !sameEntryLists(extraEntries(w), [][]EntryRef{{G2}}) || !slices.Equal(w.Judge.Entries, []EntryRef{G1}) {
		t.Errorf("acme/web = %+v; want it untouched", w)
	}
	if cs, err := s.ChainsNamingOf(ctx, ChainReviewers, T); err != nil || len(cs) != 0 {
		t.Errorf("reviewer chains naming the entry = %+v, %v", cs, err)
	}
}

// The entry is the only entry of the only extra reviewer: no reviewer row
// and no judge row is left.
func TestDeleteTeamModelLastExtraDropsJudge(t *testing.T) {
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	g := newModel(t, s, "openai", "gpt-5", "", nil)
	tm := newTeamModel(t, s, team, "One", nil)
	saveReviewers(t, s, "acme/api", false, t0, []EntryRef{{SourceGlobal, g.ID}}, []EntryRef{{SourceTeam, tm.ID}})
	_, changedR, err := s.DeleteTeamModel(ctx, team, tm.ID)
	if err != nil || len(changedR) != 1 {
		t.Fatalf("DeleteTeamModel = %+v, %v; want reviewer 2", changedR, err)
	}
	if chains, entries := reviewerRows(t, s); chains != 0 || entries != 0 {
		t.Errorf("%d chains and %d entries left; want none", chains, entries)
	}
}

// Deleting a team takes its entries out of the reviewer chains of every
// repository, with the same compaction.
func TestDeleteTeamCompactsReviewers(t *testing.T) {
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	g := newModel(t, s, "openai", "gpt-5", "", nil)
	tm := newTeamModel(t, s, team, "One", nil)
	G, T := EntryRef{SourceGlobal, g.ID}, EntryRef{SourceTeam, tm.ID}
	addRepos(t, s, "acme/api")
	saveReviewers(t, s, "acme/api", false, t0, []EntryRef{T}, []EntryRef{T}, []EntryRef{G})
	saveReviewers(t, s, "acme/web", false, t0, []EntryRef{G}, []EntryRef{G, T})
	if ok, err := s.DeleteTeam(ctx, team, nil); err != nil || !ok {
		t.Fatalf("DeleteTeam = %v, %v", ok, err)
	}
	a := mustReviewers(t, s, "acme/api")
	if !sameEntryLists(extraEntries(a), [][]EntryRef{{G}}) || a.Extra[0].Slot != "r2" || !a.JudgeFound || len(a.Judge.Entries) != 0 {
		t.Errorf("acme/api = %+v; want r2 [G] and an empty judge", a)
	}
	w := mustReviewers(t, s, "acme/web")
	if !sameEntryLists(extraEntries(w), [][]EntryRef{{G}}) || !slices.Equal(w.Judge.Entries, []EntryRef{G}) {
		t.Errorf("acme/web = %+v; want r2 [G] and judge [G]", w)
	}
}

// A removed global entry stays in the reviewer chains, as in chain A: the
// resolver skips it.
func TestRemoveGlobalModelKeepsReviewerEntry(t *testing.T) {
	s := newStore(t)
	g1 := newModel(t, s, "anthropic", "sonnet", "", nil)
	g2 := newModel(t, s, "openai", "gpt-5", "", nil)
	if defaultsOf(mustModels(t, s))[0] == g2.ID {
		t.Fatal("g2 is the default; the test needs a non-default entry")
	}
	want := []EntryRef{{SourceGlobal, g2.ID}, {SourceGlobal, g1.ID}}
	saveReviewers(t, s, "acme/api", false, t0, want[:1], want)
	if err := s.DeleteLLMModel(ctx, g2.ID, false); err != nil {
		t.Fatal(err)
	}
	r := mustReviewers(t, s, "acme/api")
	if !sameEntryLists(extraEntries(r), [][]EntryRef{want}) || !slices.Equal(r.Judge.Entries, want[:1]) {
		t.Errorf("reviewers = %+v; want the entry kept", r)
	}
}

// Removing a repository drops its reviewer chains, so one added again has
// one reviewer.
func TestRemoveRepositoryDropsReviewers(t *testing.T) {
	s := newStore(t)
	g := newModel(t, s, "openai", "gpt-5", "", nil)
	G := []EntryRef{{SourceGlobal, g.ID}}
	addRepos(t, s, "acme/api", "acme/web")
	saveReviewers(t, s, "acme/api", false, t0, G, G, G)
	saveReviewers(t, s, "acme/web", false, t0, G, G)
	if found, err := s.RemoveDashboardRepo(ctx, "main", "ACME/api"); err != nil || !found {
		t.Fatalf("RemoveDashboardRepo = %v, %v", found, err)
	}
	if r := mustReviewers(t, s, "acme/api"); len(r.Extra) != 0 || r.JudgeFound {
		t.Errorf("the removed repository's reviewers = %+v", r)
	}
	if r := mustReviewers(t, s, "acme/web"); len(r.Extra) != 1 || !r.JudgeFound {
		t.Errorf("another repository's reviewers = %+v; want them kept", r)
	}
}

// A team entry's removal locks the repository's row before its reviewer
// chains, as a save does, so a save that holds the row and then writes the
// chains does not deadlock with it.
func TestDeleteTeamModelLocksTheRepoBeforeReviewers(t *testing.T) {
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	g := newModel(t, s, "openai", "gpt-5", "", nil)
	tm := newTeamModel(t, s, team, "One", nil)
	addRepos(t, s, "acme/api")
	saveReviewers(t, s, "acme/api", false, t0, []EntryRef{{SourceGlobal, g.ID}}, []EntryRef{{SourceTeam, tm.ID}})
	holder, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck // committed below
	var holderPID int32
	if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `SELECT 1 FROM dashboard_repos WHERE instance = 'main' AND project = 'acme/api' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	deleted := make(chan error, 1)
	go func() {
		_, _, err := s.DeleteTeamModel(ctx, team, tm.ID)
		deleted <- err
	}()
	waitBlocked(t, s, 0, holderPID)
	if _, err := holder.Exec(ctx, `UPDATE `+ChainReviewers.chains+` SET updated_at = now()
		WHERE instance = 'main' AND project = 'acme/api'`); err != nil {
		t.Fatalf("the save's chain write: %v", err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-deleted; err != nil {
		t.Fatalf("DeleteTeamModel: %v", err)
	}
	if chains, _ := reviewerRows(t, s); chains != 0 {
		t.Errorf("%d reviewer chains left; want none", chains)
	}
}

// Two team entries of one team, removed at once, with the repositories
// crosswise: repository a has chain A [m1] and reviewer 2 [m2], repository b
// has chain A [m2] and reviewer 2 [m1]. Without the team lock each removal
// holds one repository row from its chain A pass and wants the other for
// its reviewer pass: a deadlock (40P01). The holder stalls both removals
// between the 2 passes (it holds the reviewer entries table).
func TestDeleteTeamModelOneAtATimePerTeamReviewers(t *testing.T) {
	s := newStore(t)
	team := newTeam(t, s, "Payments")
	g := newModel(t, s, "openai", "gpt-5", "", nil)
	m1 := newTeamModel(t, s, team, "One", nil)
	m2 := newTeamModel(t, s, team, "Two", nil)
	G := []EntryRef{{SourceGlobal, g.ID}}
	addRepos(t, s, "acme/a", "acme/b")
	for _, x := range []struct {
		project string
		a, r    TeamModel
	}{{"acme/a", m1, m2}, {"acme/b", m2, m1}} {
		if err := s.SaveChain(ctx, Chain{Scope: ScopeRepo, Instance: "main", Project: x.project, Entries: []EntryRef{{SourceTeam, x.a.ID}}}, t0); err != nil {
			t.Fatal(err)
		}
		saveReviewers(t, s, x.project, false, t0, G, []EntryRef{{SourceTeam, x.r.ID}})
	}
	holder, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck // committed below
	var holderPID int32
	if err := holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `LOCK TABLE `+ChainReviewers.entries+` IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	first, second := make(chan error, 1), make(chan error, 1)
	go func() {
		_, _, err := s.DeleteTeamModel(ctx, team, m1.ID)
		first <- err
	}()
	firstPID := waitBlocked(t, s, 0, holderPID)
	go func() {
		_, _, err := s.DeleteTeamModel(ctx, team, m2.ID)
		second <- err
	}()
	waitBlocked(t, s, firstPID, holderPID, firstPID)
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-first; err != nil {
		t.Errorf("first DeleteTeamModel: %v", err)
	}
	if err := <-second; err != nil {
		t.Errorf("second DeleteTeamModel: %v", err)
	}
	if chains, _ := reviewerRows(t, s); chains != 0 {
		t.Errorf("%d reviewer chains left; want none", chains)
	}
}
