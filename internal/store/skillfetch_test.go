package store

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func newFetch(t *testing.T, s *Store, f SkillFetch, tokenHash []byte) string {
	t.Helper()
	if f.Kind == "" {
		f.Kind, f.Instance, f.Repo = SourceKindInstance, "main", "acme/skills"
	}
	if f.Purpose == "" {
		f.Purpose = "install"
	}
	if f.Ref == "" {
		f.Ref = "v1.0"
	}
	if f.CreatedAt.IsZero() {
		f.CreatedAt, f.ExpiresAt = t0, t0.Add(time.Hour)
	}
	id, err := s.CreateSkillFetch(ctx, f, tokenHash)
	if err != nil {
		t.Fatalf("CreateSkillFetch: %v", err)
	}
	return id
}

// A fetch is completed once, from running only: by the API (no token) or by
// the Job holding the one-time token.
func TestCompleteSkillFetchOnce(t *testing.T) {
	s := newStore(t)
	id := newFetch(t, s, SkillFetch{Kind: SourceKindInstance, Instance: "main", Repo: "acme/skills", Ref: "v1.1",
		FromRef: "v1.0", Path: "skills", Purpose: "update", RequestedBy: "alice (gitlab main)", RequestedByID: 7,
		CreatedAt: t0, ExpiresAt: t0.Add(time.Hour)}, nil)

	got, err := s.SkillFetch(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	want := SkillFetch{ID: id, Kind: SourceKindInstance, Instance: "main", Repo: "acme/skills", Ref: "v1.1",
		FromRef: "v1.0", Path: "skills", Purpose: "update", RequestedBy: "alice (gitlab main)", RequestedByID: 7,
		Status: "running", CreatedAt: t0, ExpiresAt: t0.Add(time.Hour)}
	if !reflect.DeepEqual(normal(got), want) {
		t.Fatalf("fetch = %+v\nwant  %+v", got, want)
	}

	files := []FetchedFile{{Name: "style", Path: "skills/style.md", Content: "Be kind."},
		{Name: "big", Path: "skills/big.md", Problem: "too big"}}
	ok, err := s.CompleteSkillFetch(ctx, id, nil, "abc123", "", files, t0.Add(time.Minute))
	if err != nil || !ok {
		t.Fatalf("CompleteSkillFetch = %v, %v; want true", ok, err)
	}
	ok, err = s.CompleteSkillFetch(ctx, id, nil, "def456", "", nil, t0.Add(time.Minute))
	if err != nil || ok {
		t.Fatalf("second CompleteSkillFetch = %v, %v; want false", ok, err)
	}
	got, err = s.SkillFetch(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "done" || got.Commit != "abc123" || got.Error != "" || !reflect.DeepEqual(got.Files, files) {
		t.Errorf("done fetch = %+v", got)
	}

	// An error fails it, with no files.
	bad := newFetch(t, s, SkillFetch{}, nil)
	if ok, err := s.CompleteSkillFetch(ctx, bad, nil, "", "the ref is gone", nil, t0); err != nil || !ok {
		t.Fatalf("failing CompleteSkillFetch = %v, %v", ok, err)
	}
	if got, _ := s.SkillFetch(ctx, bad); got.Status != "failed" || got.Error != "the ref is gone" || got.Files != nil {
		t.Errorf("failed fetch = %+v", got)
	}

	// A Job's fetch needs its token; the API path (nil) can't complete it,
	// and the token is spent once used.
	hash := []byte("hash-of-the-token")
	job := newFetch(t, s, SkillFetch{}, hash)
	for _, h := range [][]byte{nil, []byte("another-hash")} {
		if ok, err := s.CompleteSkillFetch(ctx, job, h, "abc", "", nil, t0); err != nil || ok {
			t.Errorf("CompleteSkillFetch with token %q = %v, %v; want false", h, ok, err)
		}
	}
	if ok, err := s.CompleteSkillFetch(ctx, job, hash, "abc", "", nil, t0); err != nil || !ok {
		t.Errorf("CompleteSkillFetch with the token = %v, %v; want true", ok, err)
	}
	// An API fetch can't be completed with a token.
	api := newFetch(t, s, SkillFetch{}, nil)
	if ok, err := s.CompleteSkillFetch(ctx, api, hash, "abc", "", nil, t0); err != nil || ok {
		t.Errorf("CompleteSkillFetch of an API fetch with a token = %v, %v; want false", ok, err)
	}
	// An expired fetch can't be completed.
	if ok, err := s.CompleteSkillFetch(ctx, api, nil, "abc", "", nil, t0.Add(time.Hour)); err != nil || ok {
		t.Errorf("CompleteSkillFetch after expiry = %v, %v; want false", ok, err)
	}

	for _, missing := range []string{"00000000-0000-0000-0000-000000000000", "not-a-uuid", ""} {
		if _, err := s.SkillFetch(ctx, missing); !errors.Is(err, ErrSkillFetchNotFound) {
			t.Errorf("SkillFetch(%q) = %v; want ErrSkillFetchNotFound", missing, err)
		}
		if ok, err := s.CompleteSkillFetch(ctx, missing, nil, "abc", "", nil, t0); err != nil || ok {
			t.Errorf("CompleteSkillFetch(%q) = %v, %v; want false, nil", missing, ok, err)
		}
	}
}

// normal is f with its times in UTC, as written.
func normal(f SkillFetch) SkillFetch {
	f.CreatedAt, f.ExpiresAt = f.CreatedAt.UTC(), f.ExpiresAt.UTC()
	return f
}

func TestFailStaleAndPruneSkillFetches(t *testing.T) {
	s := newStore(t)
	old := newFetch(t, s, SkillFetch{CreatedAt: t0, ExpiresAt: t0.Add(time.Hour)}, []byte("h"))
	fresh := newFetch(t, s, SkillFetch{CreatedAt: t0.Add(5 * time.Minute), ExpiresAt: t0.Add(time.Hour)}, nil)
	done := newFetch(t, s, SkillFetch{CreatedAt: t0, ExpiresAt: t0.Add(time.Hour)}, nil)
	if ok, err := s.CompleteSkillFetch(ctx, done, nil, "abc", "", nil, t0); err != nil || !ok {
		t.Fatal(ok, err)
	}

	n, err := s.FailStaleSkillFetches(ctx, t0.Add(time.Minute))
	if err != nil || n != 1 {
		t.Fatalf("FailStaleSkillFetches = %d, %v; want 1", n, err)
	}
	if got, _ := s.SkillFetch(ctx, old); got.Status != "failed" || got.Error == "" {
		t.Errorf("stale fetch = %+v; want failed with a reason", got)
	}
	// Its token is spent: the Job can no longer post.
	if ok, _ := s.CompleteSkillFetch(ctx, old, []byte("h"), "abc", "", nil, t0); ok {
		t.Error("a stale fetch was completed")
	}
	for _, id := range []string{fresh, done} {
		if got, _ := s.SkillFetch(ctx, id); got.Status == "failed" {
			t.Errorf("fetch %s failed", id)
		}
	}

	n, err = s.PruneSkillFetches(ctx, t0.Add(time.Hour))
	if err != nil || n != 3 {
		t.Fatalf("PruneSkillFetches = %d, %v; want 3", n, err)
	}
	if _, err := s.SkillFetch(ctx, fresh); !errors.Is(err, ErrSkillFetchNotFound) {
		t.Errorf("a pruned fetch = %v", err)
	}
}

// InstalledFrom lists the skills installed from one source at one ref, with
// their text.
func TestInstalledFrom(t *testing.T) {
	s := newStore(t)
	add := func(name string, src SkillSource) {
		t.Helper()
		if _, err := s.CreateLibrarySkill(ctx, NewSkill{Name: name, Content: name + " text", Actor: "alice", Source: &src, At: t0}); err != nil {
			t.Fatal(err)
		}
	}
	inst := SkillSource{Kind: SourceKindInstance, Instance: "main", Repo: "acme/skills", Ref: "v1.0", Path: "a.md", Commit: "c1"}
	add("a", inst)
	other := inst
	other.Ref = "v2.0"
	add("b", other)
	other = inst
	other.Instance = "gh"
	add("c", other)
	other = inst
	other.Repo = "Acme/Skills" // the platform ignores case
	add("d", other)
	url := SkillSource{Kind: SourceKindURL, URL: "https://example.com/skills.git", Ref: "v1.0", Path: "e.md", Commit: "c1"}
	add("e", url)
	newSkill(t, s, "written", "by hand")

	got, err := s.InstalledFrom(ctx, SkillSource{Kind: SourceKindInstance, Instance: "main", Repo: "acme/skills", Ref: "v1.0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "d" || got[0].Content != "a text" || got[0].Source.Path != "a.md" {
		t.Errorf("InstalledFrom(instance) = %+v; want a, d with their text", got)
	}
	got, err = s.InstalledFrom(ctx, SkillSource{Kind: SourceKindURL, URL: "https://example.com/skills.git", Ref: "v1.0"})
	if err != nil || len(got) != 1 || got[0].Name != "e" {
		t.Errorf("InstalledFrom(url) = %+v, %v; want e", got, err)
	}
}
