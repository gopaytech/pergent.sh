package store

import "testing"

// Migration 0017 adds inline_available: a row stored before it can be posted
// to, a GitLab project always can, and a GitHub one stores what it is given.
func TestProjectInlineAvailable(t *testing.T) {
	s := newStore(t)
	inst := "https://gitlab.example.com"
	if _, err := s.pool.Exec(ctx, `INSERT INTO projects (instance, project_id, path) VALUES ($1, 5, 'acme/old')`, inst); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.Project(ctx, inst, 5); err != nil || !ok || !got.InlineAvailable {
		t.Fatalf("a row from before the column = %+v, %v, %v; want inline available", got, ok, err)
	}

	// A GitLab row keeps true whatever the caller passes.
	if err := s.UpsertProject(ctx, Project{Instance: inst, ProjectID: 6, Path: "acme/new"}); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Project(ctx, inst, 6); !got.InlineAvailable {
		t.Errorf("GitLab project: inline available = false, want true")
	}

	// A GitHub row stores it as given, on insert and on update.
	gh := Project{Instance: "github", ProjectID: 900, Path: "acme/widgets", InstallationID: 55, ApproveAvailable: true}
	if err := s.UpsertGitHubProject(ctx, gh); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Project(ctx, "github", 900); got.InlineAvailable {
		t.Errorf("GitHub insert: inline available = true, want false as given")
	}
	gh.InlineAvailable = true
	if err := s.UpsertGitHubProject(ctx, gh); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Project(ctx, "github", 900); !got.InlineAvailable || !got.ApproveAvailable {
		t.Errorf("GitHub update = %+v; want inline and approve available", got)
	}
	gh.InlineAvailable = false
	if err := s.UpsertGitHubProject(ctx, gh); err != nil {
		t.Fatal(err)
	}
	all, err := s.Projects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range all {
		if p.Instance == "github" && p.InlineAvailable {
			t.Errorf("Projects: GitHub row = %+v; want inline unavailable", p)
		}
	}
}

// Migration 0018 adds protection_readable: a row stored before it counts as
// readable, a GitLab project always is, and a GitHub one stores what it is
// given.
func TestProjectProtectionReadable(t *testing.T) {
	s := newStore(t)
	inst := "https://gitlab.example.com"
	if _, err := s.pool.Exec(ctx, `INSERT INTO projects (instance, project_id, path) VALUES ($1, 5, 'acme/old')`, inst); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.Project(ctx, inst, 5); err != nil || !ok || !got.ProtectionReadable {
		t.Fatalf("a row from before the column = %+v, %v, %v; want protection readable", got, ok, err)
	}

	// A GitLab row keeps true whatever the caller passes.
	if err := s.UpsertProject(ctx, Project{Instance: inst, ProjectID: 6, Path: "acme/new"}); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Project(ctx, inst, 6); !got.ProtectionReadable {
		t.Errorf("GitLab project: protection readable = false, want true")
	}

	// A GitHub row stores it as given, on insert and on update.
	gh := Project{Instance: "github", ProjectID: 900, Path: "acme/widgets", InstallationID: 55}
	if err := s.UpsertGitHubProject(ctx, gh); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Project(ctx, "github", 900); got.ProtectionReadable {
		t.Errorf("GitHub insert: protection readable = true, want false as given")
	}
	gh.ProtectionReadable = true
	if err := s.UpsertGitHubProject(ctx, gh); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.Project(ctx, "github", 900); !got.ProtectionReadable {
		t.Errorf("GitHub update = %+v; want protection readable", got)
	}
	gh.ProtectionReadable = false
	if err := s.UpsertGitHubProject(ctx, gh); err != nil {
		t.Fatal(err)
	}
	all, err := s.Projects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range all {
		if p.Instance == "github" && p.ProtectionReadable {
			t.Errorf("Projects: GitHub row = %+v; want protection unreadable", p)
		}
	}
}
