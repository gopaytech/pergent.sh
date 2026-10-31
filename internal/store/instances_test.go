package store

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func TestCreateInstanceAndSecrets(t *testing.T) {
	s := newStore(t)
	row := InstanceRow{
		Name: "lab", Platform: "github", APIURL: "https://api.example.com", ReviewAppID: 11, ApproveAppID: 12,
		OAuthClientID: "client-1",
	}
	secrets := map[string][]byte{"review_token": []byte("sealed-a"), "webhook_secret": {0, 1, 2, 255}}
	if err := s.CreateInstance(ctx, row, secrets, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateInstance(ctx, InstanceRow{Name: "lab", Platform: "gitlab"}, nil, t0); !errors.Is(err, ErrExists) {
		t.Fatalf("CreateInstance of a taken name = %v; want ErrExists", err)
	}
	if err := s.CreateInstance(ctx, InstanceRow{Name: "bad", Platform: "bitbucket"}, nil, t0); err == nil {
		t.Fatal("CreateInstance with an unknown platform succeeded")
	}
	if err := s.CreateInstance(ctx, InstanceRow{Name: "gl", Platform: "gitlab", URL: "https://gitlab.example.com"}, nil, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	got, found, err := s.InstanceRow(ctx, "lab")
	if err != nil || !found {
		t.Fatalf("InstanceRow = %v, %v", found, err)
	}
	if !got.CreatedAt.Equal(t0) {
		t.Fatalf("CreatedAt = %v; want %v", got.CreatedAt, t0)
	}
	got.CreatedAt = row.CreatedAt
	if got != row {
		t.Fatalf("InstanceRow = %+v; want %+v", got, row)
	}
	if _, found, err := s.InstanceRow(ctx, "nope"); found || err != nil {
		t.Fatalf("InstanceRow(nope) = %v, %v", found, err)
	}
	all, err := s.Instances(ctx)
	if err != nil || len(all) != 2 || all[0].Name != "gl" || all[1].Name != "lab" || all[0].URL != "https://gitlab.example.com" {
		t.Fatalf("Instances = %+v, %v; want gl then lab", all, err)
	}

	got2, err := s.InstanceSecrets(ctx, "lab")
	if err != nil || len(got2) != 2 {
		t.Fatalf("InstanceSecrets = %+v, %v", got2, err)
	}
	if !bytes.Equal(got2["review_token"].Value, []byte("sealed-a")) || !got2["review_token"].UpdatedAt.Equal(t0) ||
		!bytes.Equal(got2["webhook_secret"].Value, []byte{0, 1, 2, 255}) || !got2["webhook_secret"].UpdatedAt.Equal(t0) {
		t.Fatalf("secrets = %+v", got2)
	}
	if none, err := s.InstanceSecrets(ctx, "gl"); err != nil || len(none) != 0 {
		t.Fatalf("InstanceSecrets of an instance without any = %+v, %v", none, err)
	}
}

func TestCreateInstanceTakenLeavesNoSecrets(t *testing.T) {
	s := newStore(t)
	if err := s.CreateInstance(ctx, InstanceRow{Name: "lab", Platform: "gitlab"}, map[string][]byte{"review_token": []byte("one")}, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateInstance(ctx, InstanceRow{Name: "lab", Platform: "gitlab"}, map[string][]byte{"review_token": []byte("two")}, t0.Add(time.Hour)); !errors.Is(err, ErrExists) {
		t.Fatalf("second CreateInstance = %v; want ErrExists", err)
	}
	got, _ := s.InstanceSecrets(ctx, "lab")
	if !bytes.Equal(got["review_token"].Value, []byte("one")) || !got["review_token"].UpdatedAt.Equal(t0) {
		t.Fatalf("the first instance's secret was changed: %+v", got)
	}
}

func TestUpdateInstanceKeepsOtherSecrets(t *testing.T) {
	s := newStore(t)
	err := s.CreateInstance(ctx, InstanceRow{Name: "lab", Platform: "gitlab", URL: "https://gitlab.example.com", OAuthClientID: "client-1"},
		map[string][]byte{"review_token": []byte("old-review"), "approve_token": []byte("approve")}, t0)
	if err != nil {
		t.Fatal(err)
	}
	t1 := t0.Add(time.Hour)

	// A nil oauthClientID keeps it; a key in secrets replaces only that field.
	ok, err := s.UpdateInstance(ctx, "lab", nil, map[string][]byte{"review_token": []byte("new-review")}, t1)
	if err != nil || !ok {
		t.Fatalf("UpdateInstance = %v, %v", ok, err)
	}
	row, _, _ := s.InstanceRow(ctx, "lab")
	if row.OAuthClientID != "client-1" || row.URL != "https://gitlab.example.com" {
		t.Fatalf("row after a secret-only update = %+v", row)
	}
	got, _ := s.InstanceSecrets(ctx, "lab")
	if !bytes.Equal(got["review_token"].Value, []byte("new-review")) || !got["review_token"].UpdatedAt.Equal(t1) ||
		!bytes.Equal(got["approve_token"].Value, []byte("approve")) || !got["approve_token"].UpdatedAt.Equal(t0) {
		t.Fatalf("secrets = %+v; want review_token replaced and approve_token untouched", got)
	}

	// A new field is added; an empty (non-nil) client ID clears sign-in.
	empty := ""
	ok, err = s.UpdateInstance(ctx, "lab", &empty, map[string][]byte{"oauth_client_secret": []byte("sec")}, t1)
	if err != nil || !ok {
		t.Fatalf("UpdateInstance = %v, %v", ok, err)
	}
	row, _, _ = s.InstanceRow(ctx, "lab")
	got, _ = s.InstanceSecrets(ctx, "lab")
	if row.OAuthClientID != "" || len(got) != 3 || !bytes.Equal(got["oauth_client_secret"].Value, []byte("sec")) {
		t.Fatalf("row = %+v, secrets = %+v", row, got)
	}

	// A client ID is set; an unknown instance is not found.
	id := "client-2"
	if ok, err := s.UpdateInstance(ctx, "lab", &id, nil, t1); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if row, _, _ = s.InstanceRow(ctx, "lab"); row.OAuthClientID != "client-2" {
		t.Fatalf("OAuthClientID = %q", row.OAuthClientID)
	}
	if ok, err := s.UpdateInstance(ctx, "nope", &id, map[string][]byte{"x": []byte("y")}, t1); ok || err != nil {
		t.Fatalf("UpdateInstance(nope) = %v, %v; want not found", ok, err)
	}
	if got, _ := s.InstanceSecrets(ctx, "nope"); len(got) != 0 {
		t.Fatalf("secrets of an unknown instance = %+v", got)
	}
}

func TestMarkInstanceRemovingAndDelete(t *testing.T) {
	s := newStore(t)
	if err := s.CreateInstance(ctx, InstanceRow{Name: "lab", Platform: "gitlab"}, map[string][]byte{"review_token": []byte("x")}, t0); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.MarkInstanceRemoving(ctx, "nope"); ok || err != nil {
		t.Fatalf("MarkInstanceRemoving(nope) = %v, %v", ok, err)
	}
	if ok, err := s.MarkInstanceRemoving(ctx, "lab"); !ok || err != nil {
		t.Fatalf("MarkInstanceRemoving = %v, %v", ok, err)
	}
	if row, _, _ := s.InstanceRow(ctx, "lab"); !row.Removing {
		t.Fatal("the instance is not marked removing")
	}
	if err := s.DeleteInstance(ctx, "lab"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.InstanceRow(ctx, "lab"); found {
		t.Fatal("the instance row is still there")
	}
	if got, _ := s.InstanceSecrets(ctx, "lab"); len(got) != 0 {
		t.Fatalf("secrets after DeleteInstance = %+v", got)
	}
	if err := s.DeleteInstance(ctx, "lab"); err != nil {
		t.Fatalf("DeleteInstance of a missing instance = %v; want nil", err)
	}
}

func TestDeleteInstanceDataCascades(t *testing.T) {
	s := newStore(t)
	for _, name := range []string{"lab", "main"} {
		if err := s.CreateInstance(ctx, InstanceRow{Name: name, Platform: "gitlab"}, map[string][]byte{"review_token": []byte("x")}, t0); err != nil {
			t.Fatal(err)
		}
	}
	admin := mustPassword(t, s, "admin")
	labUser := mustPlatform(t, s, "lab", 1, "alice")
	mainUser := mustPlatform(t, s, "main", 1, "alice")
	for i, a := range []Account{labUser, mainUser} {
		se := Session{
			IDHash: bytes.Repeat([]byte{byte(i + 1)}, 32), AccountID: a.ID, Platform: "gitlab", Instance: a.Instance,
			UserID: 1, Username: "alice", TokenSealed: []byte("sealed"), CSRF: "csrf", LastSeenAt: t0, CreatedAt: t0,
		}
		if err := s.CreateSession(ctx, se); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateSession(ctx, Session{IDHash: bytes.Repeat([]byte{9}, 32), AccountID: admin.ID, CSRF: "csrf", LastSeenAt: t0, CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	team, err := s.CreateTeam(ctx, "devs", RoleViewer, t0)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.UpdateTeam(ctx, team.ID, func(tm *Team) error {
		tm.Members = []int64{labUser.ID, mainUser.ID}
		tm.Repos = []TeamRepo{{"lab", "acme/widgets"}, {"lab", ""}, {"main", "acme/widgets"}}
		return nil
	}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	for _, inst := range []string{"lab", "main"} {
		if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: inst, Project: "acme/widgets"}); err != nil {
			t.Fatal(err)
		}
		if err := s.SetAttention(ctx, inst, "acme/widgets", "webhook", "missing"); err != nil {
			t.Fatal(err)
		}
		if err := s.SetAttention(ctx, inst, "", "token", "expired"); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.DeleteInstanceData(ctx, "lab"); err != nil {
		t.Fatal(err)
	}

	if _, found, _ := s.Account(ctx, labUser.ID); found {
		t.Fatal("lab's account is still there")
	}
	if _, found, _ := s.Account(ctx, mainUser.ID); !found {
		t.Fatal("main's account is gone")
	}
	if _, found, _ := s.Account(ctx, admin.ID); !found {
		t.Fatal("the password account is gone")
	}
	if _, found, _ := s.Session(ctx, bytes.Repeat([]byte{1}, 32)); found {
		t.Fatal("lab's session is still there")
	}
	for _, b := range []byte{2, 9} {
		if _, found, _ := s.Session(ctx, bytes.Repeat([]byte{b}, 32)); !found {
			t.Fatalf("session %d is gone", b)
		}
	}
	tm, _, _ := s.Team(ctx, team.ID)
	if len(tm.Members) != 1 || tm.Members[0] != mainUser.ID || len(tm.Repos) != 1 || tm.Repos[0] != (TeamRepo{"main", "acme/widgets"}) {
		t.Fatalf("team = %+v; want only main's member and repository", tm)
	}
	repos, _ := s.DashboardRepos(ctx)
	if len(repos) != 1 || repos[0].Instance != "main" {
		t.Fatalf("dashboard repos = %+v; want main's only", repos)
	}
	att, _ := s.Attention(ctx)
	if len(att) != 2 || att[0].Instance != "main" || att[1].Instance != "main" {
		t.Fatalf("attention = %+v; want main's two", att)
	}
	// The instance row and its secrets stay: it is still Removing.
	if _, found, _ := s.InstanceRow(ctx, "lab"); !found {
		t.Fatal("DeleteInstanceData deleted the instance row")
	}
	if got, _ := s.InstanceSecrets(ctx, "lab"); len(got) != 1 {
		t.Fatalf("DeleteInstanceData changed the secrets: %+v", got)
	}
}

func TestLLMRowKeepsKey(t *testing.T) {
	s := newStore(t)
	if _, found, err := s.LLM(ctx); found || err != nil {
		t.Fatalf("LLM before any put = %v, %v", found, err)
	}
	if err := s.PutLLM(ctx, "anthropic", "claude-sonnet-4", "", []byte("sealed-key"), t0); err != nil {
		t.Fatal(err)
	}
	got, found, err := s.LLM(ctx)
	if err != nil || !found || got.Provider != "anthropic" || got.Model != "claude-sonnet-4" || got.BaseURL != "" ||
		!bytes.Equal(got.APIKeySealed, []byte("sealed-key")) || !got.UpdatedAt.Equal(t0) {
		t.Fatalf("LLM = %+v, %v, %v", got, found, err)
	}

	// A nil key keeps the earlier one.
	t1 := t0.Add(time.Hour)
	if err := s.PutLLM(ctx, "openai", "gpt-x", "https://llm.example.com/v1", nil, t1); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.LLM(ctx)
	if got.Provider != "openai" || got.Model != "gpt-x" || got.BaseURL != "https://llm.example.com/v1" ||
		!bytes.Equal(got.APIKeySealed, []byte("sealed-key")) || !got.UpdatedAt.Equal(t1) {
		t.Fatalf("LLM after a put without a key = %+v", got)
	}

	// A new key replaces it.
	if err := s.PutLLM(ctx, "openai", "gpt-x", "", []byte("sealed-2"), t1); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.LLM(ctx)
	if !bytes.Equal(got.APIKeySealed, []byte("sealed-2")) || got.BaseURL != "" {
		t.Fatalf("LLM after a new key = %+v", got)
	}
}

func TestLLMWithoutKey(t *testing.T) {
	s := newStore(t)
	if err := s.PutLLM(ctx, "ollama", "llama", "http://ollama:11434", nil, t0); err != nil {
		t.Fatal(err)
	}
	got, found, err := s.LLM(ctx)
	if err != nil || !found || got.Provider != "ollama" || len(got.APIKeySealed) != 0 {
		t.Fatalf("LLM = %+v, %v, %v", got, found, err)
	}
}

func TestRepoDefaultsRoundTrip(t *testing.T) {
	s := newStore(t)
	got, err := s.RepoDefaults(ctx)
	if err != nil || got.Approve != nil || got.ReviewDrafts != nil || got.FailureStatus != nil ||
		got.JobTimeoutSeconds != nil || got.JobMemory != nil || !got.UpdatedAt.IsZero() {
		t.Fatalf("RepoDefaults when unset = %+v, %v; want the zero value", got, err)
	}

	yes, no := true, false
	status, memory, timeout := "neutral", "2Gi", 900
	in := RepoDefaults{Approve: &yes, ReviewDrafts: &no, FailureStatus: &status, JobTimeoutSeconds: &timeout, JobMemory: &memory}
	if err := s.PutRepoDefaults(ctx, in); err != nil {
		t.Fatal(err)
	}
	got, err = s.RepoDefaults(ctx)
	if err != nil || got.Approve == nil || !*got.Approve || got.ReviewDrafts == nil || *got.ReviewDrafts ||
		got.FailureStatus == nil || *got.FailureStatus != "neutral" || got.JobTimeoutSeconds == nil || *got.JobTimeoutSeconds != 900 ||
		got.JobMemory == nil || *got.JobMemory != "2Gi" || got.UpdatedAt.IsZero() {
		t.Fatalf("RepoDefaults = %+v, %v", got, err)
	}

	// A put replaces every field, nil clearing it.
	if err := s.PutRepoDefaults(ctx, RepoDefaults{JobMemory: &memory}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.RepoDefaults(ctx)
	if got.Approve != nil || got.ReviewDrafts != nil || got.FailureStatus != nil || got.JobTimeoutSeconds != nil ||
		got.JobMemory == nil || *got.JobMemory != "2Gi" {
		t.Fatalf("RepoDefaults after a second put = %+v", got)
	}

	bad := "maybe"
	if err := s.PutRepoDefaults(ctx, RepoDefaults{FailureStatus: &bad}); err == nil {
		t.Fatal("PutRepoDefaults accepted an unknown failure status")
	}
}

// SupersedeInstance supersedes every live run (queued, running, reported) of
// the instance, one merge request at a time, and leaves other instances'
// runs alone.
func TestSupersedeInstance(t *testing.T) {
	s := newStore(t)
	enqueue := func(instance string, project, iid int64, head string) string {
		id, _, err := s.EnqueueRun(ctx, MRKey{Instance: instance, ProjectID: project, IID: iid}, "acme/x", head, "main")
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	head := func(c byte) string { return string(bytes.Repeat([]byte{c}, 40)) }
	running := enqueue("lab", 1, 2, head('b'))
	reported := enqueue("lab", 2, 1, head('c'))
	if _, err := s.ClaimQueued(ctx, 10, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ReportFailure(ctx, reported, "no clone token", t0); err != nil || !ok {
		t.Fatalf("ReportFailure = %v, %v", ok, err)
	}
	queued := enqueue("lab", 1, 1, head('a'))
	other := enqueue("main", 1, 1, head('d'))

	n, err := s.SupersedeInstance(ctx, "lab")
	if err != nil {
		t.Fatal(err)
	}
	state := func(id string) RunState {
		r, ok, err := s.Run(ctx, id)
		if err != nil || !ok {
			t.Fatalf("Run(%s) = %v, %v", id, ok, err)
		}
		return r.State
	}
	if n != 3 {
		t.Errorf("SupersedeInstance = %d, want 3", n)
	}
	for name, id := range map[string]string{"queued": queued, "running": running, "reported": reported} {
		if got := state(id); got != Superseded {
			t.Errorf("%s run = %s, want superseded", name, got)
		}
	}
	if got := state(other); got != Queued {
		t.Errorf("a run of another instance = %s, want queued", got)
	}
	if again, err := s.SupersedeInstance(ctx, "lab"); err != nil || again != 0 {
		t.Errorf("SupersedeInstance again = %d, %v; want 0", again, err)
	}
}

// A removing instance can't be changed: the update finds it removing, and
// leaves its row and secrets as they were.
func TestUpdateInstanceRefusesARemovingInstance(t *testing.T) {
	s := newStore(t)
	if err := s.CreateInstance(ctx, InstanceRow{Name: "lab", Platform: "gitlab", OAuthClientID: "c1"},
		map[string][]byte{"review_token": []byte("old")}, t0); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.MarkInstanceRemoving(ctx, "lab"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	id := "c2"
	ok, err := s.UpdateInstance(ctx, "lab", &id, map[string][]byte{"review_token": []byte("new")}, t0.Add(time.Hour))
	if !errors.Is(err, ErrRemoving) || ok {
		t.Fatalf("UpdateInstance of a removing instance = %v, %v; want ErrRemoving", ok, err)
	}
	row, _, _ := s.InstanceRow(ctx, "lab")
	got, _ := s.InstanceSecrets(ctx, "lab")
	if row.OAuthClientID != "c1" || !bytes.Equal(got["review_token"].Value, []byte("old")) {
		t.Fatalf("row = %+v, secrets = %+v; want them unchanged", row, got)
	}
	if ok, err := s.UpdateInstance(ctx, "nope", &id, nil, t0); ok || err != nil {
		t.Fatalf("UpdateInstance(nope) = %v, %v; want not found", ok, err)
	}
}
