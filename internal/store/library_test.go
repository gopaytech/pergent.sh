package store

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

func newSkill(t *testing.T, s *Store, name, content string) {
	t.Helper()
	if _, err := s.CreateLibrarySkill(ctx, NewSkill{Name: name, Content: content, Actor: "alice", At: t0}); err != nil {
		t.Fatalf("CreateLibrarySkill(%s): %v", name, err)
	}
}

func TestLibrarySkillVersions(t *testing.T) {
	s := newStore(t)
	v, err := s.CreateLibrarySkill(ctx, NewSkill{Name: "style", Content: "one", Actor: "alice", At: t0})
	if err != nil || v != 1 {
		t.Fatalf("CreateLibrarySkill = %d, %v; want 1", v, err)
	}
	v, err = s.UpdateLibrarySkill(ctx, "style", "two!", "bob", 1, t0.Add(time.Hour))
	if err != nil || v != 2 {
		t.Fatalf("UpdateLibrarySkill = %d, %v; want 2", v, err)
	}

	got, versions, err := s.LibrarySkill(ctx, "style")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "style" || got.Version != 2 || got.Content != "two!" || got.Size != 4 || got.UpdatedBy != "bob" ||
		!got.UpdatedAt.Equal(t0.Add(time.Hour)) || got.Source != nil {
		t.Errorf("skill = %+v", got)
	}
	if len(versions) != 2 || versions[0].Version != 2 || versions[1].Version != 1 {
		t.Fatalf("versions = %+v; want 2 then 1", versions)
	}
	for _, v := range versions {
		if v.Content != "" {
			t.Errorf("version %d lists its content", v.Version)
		}
	}
	if versions[1].Actor != "alice" || !versions[1].At.Equal(t0) || versions[0].Actor != "bob" {
		t.Errorf("versions = %+v", versions)
	}

	v1, err := s.LibrarySkillVersion(ctx, "style", 1)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("one"))
	if v1.Content != "one" || !bytes.Equal(v1.SHA256, sum[:]) || v1.Name != "style" || v1.Version != 1 {
		t.Errorf("version 1 = %+v", v1)
	}

	list, err := s.LibrarySkills(ctx)
	if err != nil || len(list) != 1 || list[0].Name != "style" || list[0].Content != "" || list[0].Size != 4 || list[0].Version != 2 {
		t.Errorf("LibrarySkills = %+v, %v", list, err)
	}

	if _, _, err := s.LibrarySkill(ctx, "nope"); !errors.Is(err, ErrSkillNotFound) {
		t.Errorf("LibrarySkill(nope) = %v; want ErrSkillNotFound", err)
	}
	if _, err := s.LibrarySkillVersion(ctx, "style", 9); !errors.Is(err, ErrSkillNotFound) {
		t.Errorf("LibrarySkillVersion(9) = %v; want ErrSkillNotFound", err)
	}
	if _, err := s.UpdateLibrarySkill(ctx, "nope", "x", "bob", 1, t0); !errors.Is(err, ErrSkillNotFound) {
		t.Errorf("UpdateLibrarySkill(nope) = %v; want ErrSkillNotFound", err)
	}
}

func TestCreateLibrarySkillRefusals(t *testing.T) {
	s := newStore(t)
	newSkill(t, s, "style", "x")
	if _, err := s.CreateLibrarySkill(ctx, NewSkill{Name: "style", Content: "y", At: t0}); !errors.Is(err, ErrSkillExists) {
		t.Errorf("create of a taken name = %v; want ErrSkillExists", err)
	}
	for _, bad := range []string{"", "-x", ".x", "a b", "a/b", "é"} {
		if _, err := s.CreateLibrarySkill(ctx, NewSkill{Name: bad, Content: "y", At: t0}); err == nil || errors.Is(err, ErrSkillExists) {
			t.Errorf("create of name %q = %v; want the name check to refuse it", bad, err)
		}
	}
	if list, _ := s.LibrarySkills(ctx); len(list) != 1 {
		t.Errorf("library = %+v after refusals", list)
	}
}

func TestVersionsContinueAfterDelete(t *testing.T) {
	s := newStore(t)
	newSkill(t, s, "style", "one")
	if _, err := s.UpdateLibrarySkill(ctx, "style", "two", "bob", 1, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteLibrarySkill(ctx, "style", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.LibrarySkill(ctx, "style"); !errors.Is(err, ErrSkillNotFound) {
		t.Fatalf("LibrarySkill after delete = %v; want ErrSkillNotFound", err)
	}
	// The history outlives the skill.
	if v, err := s.LibrarySkillVersion(ctx, "style", 2); err != nil || v.Content != "two" {
		t.Errorf("version 2 after delete = %+v, %v", v, err)
	}
	v, err := s.CreateLibrarySkill(ctx, NewSkill{Name: "style", Content: "three", Actor: "alice", At: t0})
	if err != nil || v != 3 {
		t.Fatalf("create again = %d, %v; want 3", v, err)
	}
	if err := s.DeleteLibrarySkill(ctx, "style", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteLibrarySkill(ctx, "style", nil); !errors.Is(err, ErrSkillNotFound) {
		t.Errorf("second delete = %v; want ErrSkillNotFound", err)
	}
}

func TestUpdateStaleVersion(t *testing.T) {
	s := newStore(t)
	newSkill(t, s, "style", "one")
	if _, err := s.UpdateLibrarySkill(ctx, "style", "two", "bob", 1, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateLibrarySkill(ctx, "style", "mine", "carol", 1, t0); !errors.Is(err, ErrStaleVersion) {
		t.Fatalf("update of a stale version = %v; want ErrStaleVersion", err)
	}
	got, versions, _ := s.LibrarySkill(ctx, "style")
	if got.Content != "two" || got.Version != 2 || len(versions) != 2 {
		t.Errorf("a refused update changed the skill: %+v, %d versions", got, len(versions))
	}
}

func TestUpdateInstalledRefused(t *testing.T) {
	s := newStore(t)
	src := SkillSource{Kind: SourceKindInstance, Instance: "lab", Repo: "acme/skills", Ref: "v1.0", Path: "style.md", Commit: "abc"}
	v, err := s.CreateLibrarySkill(ctx, NewSkill{Name: "style", Content: "one", Actor: "alice", Source: &src, At: t0})
	if err != nil || v != 1 {
		t.Fatalf("install = %d, %v", v, err)
	}
	if _, err := s.UpdateLibrarySkill(ctx, "style", "edit", "bob", 1, t0); !errors.Is(err, ErrSkillInstalled) {
		t.Fatalf("edit of an installed skill = %v; want ErrSkillInstalled", err)
	}
	got, _, _ := s.LibrarySkill(ctx, "style")
	if got.Source == nil || *got.Source != src || got.Content != "one" {
		t.Errorf("skill = %+v; source %+v", got, got.Source)
	}
	if ver, _ := s.LibrarySkillVersion(ctx, "style", 1); ver.Commit != "abc" {
		t.Errorf("version commit = %q; want abc", ver.Commit)
	}

	// An update from the source takes a new version, the new ref and commit.
	next := SkillSource{Kind: SourceKindInstance, Instance: "lab", Repo: "acme/skills", Ref: "v1.1", Path: "style.md", Commit: "def"}
	v, err = s.UpdateInstalledSkill(ctx, "style", "two", "bob", next, t0.Add(time.Hour))
	if err != nil || v != 2 {
		t.Fatalf("UpdateInstalledSkill = %d, %v; want 2", v, err)
	}
	got, _, _ = s.LibrarySkill(ctx, "style")
	if got.Version != 2 || got.Content != "two" || got.Source == nil || *got.Source != next || got.UpdatedBy != "bob" {
		t.Errorf("skill = %+v; source %+v", got, got.Source)
	}
	if ver, _ := s.LibrarySkillVersion(ctx, "style", 2); ver.Commit != "def" || ver.Actor != "bob" {
		t.Errorf("version 2 = %+v", ver)
	}

	// Not for a skill written in the browser, and not for a missing one.
	newSkill(t, s, "mine", "x")
	if _, err := s.UpdateInstalledSkill(ctx, "mine", "y", "bob", next, t0); err == nil {
		t.Error("UpdateInstalledSkill of a written skill succeeded")
	}
	if _, err := s.UpdateInstalledSkill(ctx, "nope", "y", "bob", next, t0); !errors.Is(err, ErrSkillNotFound) {
		t.Errorf("UpdateInstalledSkill(nope) = %v; want ErrSkillNotFound", err)
	}
}

func TestCreateFull(t *testing.T) {
	s := newStore(t)
	for i := 0; i < 200; i++ {
		newSkill(t, s, fmt.Sprintf("skill-%03d", i), "x")
	}
	_, err := s.CreateLibrarySkill(ctx, NewSkill{Name: "one-too-many", Content: "x", At: t0})
	if !errors.Is(err, ErrLibraryFull) {
		t.Fatalf("the 201st skill = %v; want ErrLibraryFull", err)
	}
	if _, err := s.CreateLibrarySkill(ctx, NewSkill{Name: "skill-000", Content: "x", At: t0}); !errors.Is(err, ErrSkillExists) {
		t.Errorf("a taken name in a full library = %v; want ErrSkillExists", err)
	}
	// Room again after a delete.
	if err := s.DeleteLibrarySkill(ctx, "skill-000", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateLibrarySkill(ctx, NewSkill{Name: "one-too-many", Content: "x", At: t0}); err != nil {
		t.Errorf("create after a delete: %v", err)
	}
}

func TestDeleteInUse(t *testing.T) {
	s := newStore(t)
	for _, n := range []string{"a", "b", "c", "d"} {
		newSkill(t, s, n, "x")
	}
	if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "lab", Project: "acme/web", Skills: []string{"a", "b"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutRepoDefaults(ctx, RepoDefaults{Skills: []string{"b", "c"}}); err != nil {
		t.Fatal(err)
	}
	cfg := []RepoRef{{Instance: "lab", Project: "acme/api"}}

	err := s.DeleteLibrarySkill(ctx, "a", cfg)
	var inUse *SkillInUseError
	if !errors.As(err, &inUse) || inUse.InDefaults ||
		!slices.Equal(inUse.Repos, []RepoRef{{Instance: "lab", Project: "acme/web"}, {Instance: "lab", Project: "acme/api"}}) {
		t.Fatalf("delete of a skill a dashboard repository and a config repository name = %#v; want those two", err)
	}
	err = s.DeleteLibrarySkill(ctx, "b", nil)
	if !errors.As(err, &inUse) || !inUse.InDefaults || !slices.Equal(inUse.Repos, []RepoRef{{Instance: "lab", Project: "acme/web"}}) {
		t.Fatalf("delete of a skill a repository and the defaults name = %#v", err)
	}
	err = s.DeleteLibrarySkill(ctx, "c", nil)
	if !errors.As(err, &inUse) || !inUse.InDefaults || len(inUse.Repos) != 0 {
		t.Fatalf("delete of a default skill = %#v", err)
	}
	// A config repository alone refuses too.
	if err := s.DeleteLibrarySkill(ctx, "d", cfg); !errors.As(err, &inUse) || len(inUse.Repos) != 1 || inUse.InDefaults {
		t.Fatalf("delete of a skill a config repository names = %#v", err)
	}
	if err := s.DeleteLibrarySkill(ctx, "d", nil); err != nil {
		t.Fatalf("delete of an unused skill: %v", err)
	}
	if list, _ := s.LibrarySkills(ctx); len(list) != 3 {
		t.Errorf("library = %+v; want a, b, c still there", list)
	}
}

// race runs a and b together and returns how many returned nil.
func race(a, b func() error) (nils int, errs [2]error) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, f := range []func() error{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = f()
		}()
	}
	close(start)
	wg.Wait()
	for _, e := range errs {
		if e == nil {
			nils++
		}
	}
	return nils, errs
}

func TestDeleteRacesRepoSave(t *testing.T) {
	s := newStore(t)
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("racy-%d", i)
		newSkill(t, s, name, "x")
		project := fmt.Sprintf("acme/repo-%d", i)
		if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "lab", Project: project}); err != nil {
			t.Fatal(err)
		}
		nils, errs := race(
			func() error { return s.DeleteLibrarySkill(ctx, name, nil) },
			func() error {
				_, err := s.UpdateDashboardRepo(ctx, "lab", project, func(r *DashboardRepo) { r.Skills = []string{name} })
				return err
			})
		if nils != 1 {
			t.Fatalf("round %d: delete = %v, save = %v; want exactly one to succeed", i, errs[0], errs[1])
		}
		_, exists := skillExists(t, s, name)
		repo, _, _ := s.DashboardRepo(ctx, "lab", project)
		if exists != (len(repo.Skills) == 1) {
			t.Fatalf("round %d: skill exists = %v but the repository names %v", i, exists, repo.Skills)
		}
	}
}

func TestDeleteRacesRepoInsert(t *testing.T) {
	s := newStore(t)
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("racy-%d", i)
		newSkill(t, s, name, "x")
		project := fmt.Sprintf("acme/repo-%d", i)
		nils, errs := race(
			func() error { return s.DeleteLibrarySkill(ctx, name, nil) },
			func() error {
				_, err := s.InsertDashboardRepo(ctx, DashboardRepo{Instance: "lab", Project: project, Skills: []string{name}})
				return err
			})
		if nils != 1 {
			t.Fatalf("round %d: delete = %v, insert = %v; want exactly one to succeed", i, errs[0], errs[1])
		}
		_, exists := skillExists(t, s, name)
		_, stored, _ := s.DashboardRepo(ctx, "lab", project)
		if exists != stored {
			t.Fatalf("round %d: skill exists = %v but the repository stored = %v", i, exists, stored)
		}
	}
}

func TestDeleteRacesRepoUpsert(t *testing.T) {
	s := newStore(t)
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("racy-%d", i)
		newSkill(t, s, name, "x")
		project := fmt.Sprintf("acme/repo-%d", i)
		nils, errs := race(
			func() error { return s.DeleteLibrarySkill(ctx, name, nil) },
			func() error {
				return s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "lab", Project: project, Skills: []string{name}})
			})
		if nils != 1 {
			t.Fatalf("round %d: delete = %v, upsert = %v; want exactly one to succeed", i, errs[0], errs[1])
		}
		_, exists := skillExists(t, s, name)
		_, stored, _ := s.DashboardRepo(ctx, "lab", project)
		if exists != stored {
			t.Fatalf("round %d: skill exists = %v but the repository stored = %v", i, exists, stored)
		}
	}
}

func TestInsertNamingMissingSkill(t *testing.T) {
	s := newStore(t)
	created, err := s.InsertDashboardRepo(ctx, DashboardRepo{Instance: "lab", Project: "acme/web", Skills: []string{"gone"}})
	if !errors.Is(err, ErrSkillNotFound) || created {
		t.Fatalf("InsertDashboardRepo naming a missing skill = %v, %v; want ErrSkillNotFound", created, err)
	}
	if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "lab", Project: "acme/web", Skills: []string{"gone"}}); !errors.Is(err, ErrSkillNotFound) {
		t.Fatalf("UpsertDashboardRepo naming a missing skill = %v; want ErrSkillNotFound", err)
	}
	if _, ok, _ := s.DashboardRepo(ctx, "lab", "acme/web"); ok {
		t.Error("a refused insert stored the repository")
	}
}

// A save that names a missing skill names it in the error, the first one
// missing in name order, and still matches ErrSkillNotFound.
func TestMissingSkillErrorNamesSkill(t *testing.T) {
	s := newStore(t)
	newSkill(t, s, "a", "x")
	err := s.PutRepoDefaults(ctx, RepoDefaults{Skills: []string{"zed", "a", "gone"}})
	var missing *MissingSkillError
	if !errors.Is(err, ErrSkillNotFound) || !errors.As(err, &missing) || missing.Name != "gone" {
		t.Fatalf("PutRepoDefaults = %v; want a MissingSkillError for \"gone\" that is ErrSkillNotFound", err)
	}
}

func TestDeleteRacesDefaultsSave(t *testing.T) {
	s := newStore(t)
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("racy-%d", i)
		newSkill(t, s, name, "x")
		nils, errs := race(
			func() error { return s.DeleteLibrarySkill(ctx, name, nil) },
			func() error { return s.PutRepoDefaults(ctx, RepoDefaults{Skills: []string{name}}) })
		if nils != 1 {
			t.Fatalf("round %d: delete = %v, save = %v; want exactly one to succeed", i, errs[0], errs[1])
		}
		d, _ := s.RepoDefaults(ctx)
		_, exists := skillExists(t, s, name)
		if exists != (len(d.Skills) == 1) {
			t.Fatalf("round %d: skill exists = %v but the defaults name %v", i, exists, d.Skills)
		}
		// Clear the defaults so the next round starts clean.
		if err := s.PutRepoDefaults(ctx, RepoDefaults{}); err != nil {
			t.Fatal(err)
		}
	}
}

func skillExists(t *testing.T, s *Store, name string) (LibrarySkill, bool) {
	t.Helper()
	sk, _, err := s.LibrarySkill(ctx, name)
	if errors.Is(err, ErrSkillNotFound) {
		return LibrarySkill{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return sk, true
}

func TestSaveNamingMissingSkill(t *testing.T) {
	s := newStore(t)
	newSkill(t, s, "a", "x")
	if err := s.PutRepoDefaults(ctx, RepoDefaults{Skills: []string{"a", "gone"}}); !errors.Is(err, ErrSkillNotFound) {
		t.Fatalf("PutRepoDefaults naming a missing skill = %v; want ErrSkillNotFound", err)
	}
	if d, _ := s.RepoDefaults(ctx); d.Skills != nil {
		t.Errorf("defaults = %+v after a refused save", d)
	}
	if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "lab", Project: "acme/web"}); err != nil {
		t.Fatal(err)
	}
	found, err := s.UpdateDashboardRepo(ctx, "lab", "acme/web", func(r *DashboardRepo) { r.Skills = []string{"gone"} })
	if !errors.Is(err, ErrSkillNotFound) {
		t.Fatalf("UpdateDashboardRepo naming a missing skill = %v, %v; want ErrSkillNotFound", found, err)
	}
	if r, _, _ := s.DashboardRepo(ctx, "lab", "acme/web"); r.Skills != nil {
		t.Errorf("repository = %+v after a refused save", r)
	}

	// The defaults and the repository's detected skills round-trip.
	if err := s.PutRepoDefaults(ctx, RepoDefaults{Skills: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.RepoDefaults(ctx); !slices.Equal(d.Skills, []string{"a"}) {
		t.Errorf("defaults skills = %v", d.Skills)
	}
	if _, err := s.UpdateDashboardRepo(ctx, "lab", "acme/web", func(r *DashboardRepo) {
		r.Skills = []string{"a"}
		r.RepoSkills = []string{"detected", "other"}
	}); err != nil {
		t.Fatal(err)
	}
	r, _, _ := s.DashboardRepo(ctx, "lab", "acme/web")
	if !slices.Equal(r.Skills, []string{"a"}) || !slices.Equal(r.RepoSkills, []string{"detected", "other"}) {
		t.Errorf("repository = %+v", r)
	}
	// Detected names are not library names: a repository may name any.
	if _, err := s.UpdateDashboardRepo(ctx, "lab", "acme/web", func(r *DashboardRepo) { r.Skills = nil; r.RepoSkills = nil }); err != nil {
		t.Fatal(err)
	}
	if r, _, _ := s.DashboardRepo(ctx, "lab", "acme/web"); r.Skills != nil || r.RepoSkills != nil {
		t.Errorf("cleared repository = %+v", r)
	}
}

func TestResolveLibrary(t *testing.T) {
	s := newStore(t)
	newSkill(t, s, "a", "alpha")
	newSkill(t, s, "b", "bravo!")
	if _, err := s.UpdateLibrarySkill(ctx, "b", "bravo!!", "bob", 1, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.PutRepoDefaults(ctx, RepoDefaults{Skills: []string{"b"}}); err != nil {
		t.Fatal(err)
	}

	got, err := s.ResolveLibrary(ctx, nil)
	if err != nil || len(got.Missing) != 0 || len(got.Skills) != 1 || got.Skills["b"].Content != "bravo!!" || got.Skills["b"].Version != 2 {
		t.Fatalf("ResolveLibrary(nil) = %+v, %v; want the defaults {b}", got, err)
	}
	got, err = s.ResolveLibrary(ctx, []string{"a", "gone"})
	if err != nil || len(got.Skills) != 1 || got.Skills["a"].Content != "alpha" || !slices.Equal(got.Missing, []string{"gone"}) {
		t.Fatalf("ResolveLibrary(a, gone) = %+v, %v", got, err)
	}
	got, err = s.ResolveLibrary(ctx, []string{"gone", "a", "also-gone", "gone"})
	if err != nil || !slices.Equal(got.Missing, []string{"gone", "also-gone"}) {
		t.Errorf("Missing = %v, %v; want each missing name once, in order", got.Missing, err)
	}

	// No defaults set: nothing, not everything.
	if err := s.PutRepoDefaults(ctx, RepoDefaults{}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ResolveLibrary(ctx, nil); err != nil || len(got.Skills) != 0 || len(got.Missing) != 0 {
		t.Errorf("ResolveLibrary(nil) without defaults = %+v, %v", got, err)
	}
}

func TestLibrarySize(t *testing.T) {
	s := newStore(t)
	newSkill(t, s, "a", "12345")
	newSkill(t, s, "b", "héllo") // 6 bytes
	newSkill(t, s, "c", "xxxxxxxxxx")
	n, err := s.LibrarySize(ctx, []string{"a", "b", "gone"})
	if err != nil || n != 11 {
		t.Errorf("LibrarySize(a, b, gone) = %d, %v; want 11", n, err)
	}
	if n, err := s.LibrarySize(ctx, []string{}); err != nil || n != 0 {
		t.Errorf("LibrarySize(none) = %d, %v; want 0", n, err)
	}
	if err := s.PutRepoDefaults(ctx, RepoDefaults{Skills: []string{"c"}}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.LibrarySize(ctx, nil); err != nil || n != 10 {
		t.Errorf("LibrarySize(nil) = %d, %v; want the defaults' 10", n, err)
	}
}

func TestRunSkillVersionsRoundTrip(t *testing.T) {
	s := newStore(t)
	id := enqueue(t, s, mr1, "a1")
	if r := getRun(t, s, id); r.SkillVersions != nil {
		t.Fatalf("new run skill versions = %v; want none", r.SkillVersions)
	}
	if err := s.SetRunInputs(ctx, id, "m", 0, map[string]string{"a": "library", "b": "repo"}, map[string]int{"a": 3}); err != nil {
		t.Fatal(err)
	}
	r := getRun(t, s, id)
	if len(r.SkillVersions) != 1 || r.SkillVersions["a"] != 3 || r.SkillSources["b"] != "repo" {
		t.Errorf("run = versions %v sources %v", r.SkillVersions, r.SkillSources)
	}
	if err := s.SetRunInputs(ctx, id, "m", 0, nil, nil); err != nil {
		t.Fatal(err)
	}
	if r := getRun(t, s, id); r.SkillVersions == nil || len(r.SkillVersions) != 0 {
		t.Errorf("run skill versions = %#v; want empty and recorded", r.SkillVersions)
	}
}

func TestSkipRun(t *testing.T) {
	s := newStore(t)
	id := enqueue(t, s, mr1, "A")
	if err := s.SetApprovedSHA(ctx, mr1, "A", "main"); err != nil {
		t.Fatal(err)
	}
	ok, err := s.SkipRun(ctx, id, "this repository has no skills", t0)
	if err != nil || !ok {
		t.Fatalf("SkipRun of a queued run = %v, %v", ok, err)
	}
	r := getRun(t, s, id)
	if r.State != Skipped || r.SkipReason != "this repository has no skills" {
		t.Errorf("run = state %q reason %q", r.State, r.SkipReason)
	}
	if ok, err := s.SkipRun(ctx, id, "again", t0); err != nil || ok {
		t.Errorf("SkipRun of a skipped run = %v, %v; want false", ok, err)
	}
	if ok, err := s.SkipRun(ctx, "bad", "x", t0); err == nil || ok {
		t.Errorf("SkipRun of a malformed id = %v, %v; want an error", ok, err)
	}

	// A running run can be skipped too.
	run2 := enqueue(t, s, mr2, "B")
	if claimed, err := s.ClaimQueued(ctx, 10, t0); err != nil || len(claimed) != 1 {
		t.Fatalf("ClaimQueued = %v, %v", claimed, err)
	}
	if ok, err := s.SkipRun(ctx, run2, "none", t0); err != nil || !ok {
		t.Fatalf("SkipRun of a running run = %v, %v", ok, err)
	}
	// A reported one cannot.
	rep := reported(t, s, MRKey{Instance: mr1.Instance, ProjectID: 9, IID: 1}, "R", `{}`)
	if ok, err := s.SkipRun(ctx, rep, "none", t0); err != nil || ok {
		t.Errorf("SkipRun of a reported run = %v, %v; want false", ok, err)
	}

	// The skipped run is the newest of its merge request, so an approval of
	// an older head is stale, like for any other run.
	if got, err := s.StaleApprovals(ctx, time.Now()); err != nil || len(got) != 0 {
		t.Fatalf("approval of the skipped run's own head is stale: %v, %v", got, err)
	}
	enqueue(t, s, mr1, "B")
	idB := getRunByHead(t, s, mr1, "B")
	if ok, err := s.SkipRun(ctx, idB, "none", t0); err != nil || !ok {
		t.Fatalf("SkipRun = %v, %v", ok, err)
	}
	got, err := s.StaleApprovals(ctx, time.Now())
	if err != nil || len(got) != 1 || got[0] != (Approval{MR: mr1, ApprovedSHA: "A", ApprovedTarget: "main"}) {
		t.Fatalf("StaleApprovals with a skipped run on a new head = %v, %v; want the MR", got, err)
	}
}

func getRunByHead(t *testing.T, s *Store, mr MRKey, head string) string {
	t.Helper()
	var id string
	err := s.pool.QueryRow(ctx, `SELECT id::text FROM runs WHERE instance = $1 AND project_id = $2 AND mr_iid = $3 AND head_sha = $4
		AND state <> 'superseded'`, mr.Instance, mr.ProjectID, mr.IID, head).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSkillFetchesTable(t *testing.T) {
	s := newStore(t)
	insert := func(purpose, status string) error {
		_, err := s.pool.Exec(ctx, `INSERT INTO skill_fetches (id, kind, purpose, status, created_at, expires_at)
			VALUES (gen_random_uuid(), 'url', $1, $2, $3, $3)`, purpose, status, t0)
		return err
	}
	if err := insert("install", "running"); err != nil {
		t.Fatalf("a valid fetch: %v", err)
	}
	if err := insert("update", "failed"); err != nil {
		t.Fatalf("a valid update fetch: %v", err)
	}
	if err := insert("delete", "running"); err == nil {
		t.Error("an unknown purpose was stored")
	}
	if err := insert("install", "stuck"); err == nil {
		t.Error("an unknown status was stored")
	}
}
