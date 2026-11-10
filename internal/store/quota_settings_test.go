package store

import (
	"errors"
	"testing"
)

// The repository's quota is stored by the upsert and the update, and nil
// clears it.
func TestDashboardRepoKeepsQuota(t *testing.T) {
	s := newStore(t)
	read := func() *int64 {
		t.Helper()
		r, ok, err := s.DashboardRepo(ctx, "main", "acme/widgets")
		if err != nil || !ok {
			t.Fatalf("DashboardRepo = %v, %v", ok, err)
		}
		return r.MonthlyTokenQuota
	}
	q := int64(42)
	if err := s.UpsertDashboardRepo(ctx, DashboardRepo{Instance: "main", Project: "acme/widgets", CreatedBy: "admin", MonthlyTokenQuota: &q}); err != nil {
		t.Fatal(err)
	}
	if got := read(); got == nil || *got != 42 {
		t.Fatalf("after upsert quota = %v, want 42", got)
	}
	seven := int64(7)
	if found, err := s.UpdateDashboardRepo(ctx, "main", "acme/widgets", func(r *DashboardRepo) { r.MonthlyTokenQuota = &seven }); err != nil || !found {
		t.Fatalf("UpdateDashboardRepo = %v, %v", found, err)
	}
	if got := read(); got == nil || *got != 7 {
		t.Fatalf("after update quota = %v, want 7", got)
	}
	if found, err := s.UpdateDashboardRepo(ctx, "main", "acme/widgets", func(r *DashboardRepo) { r.MonthlyTokenQuota = nil }); err != nil || !found {
		t.Fatalf("UpdateDashboardRepo = %v, %v", found, err)
	}
	if got := read(); got != nil {
		t.Fatalf("after clearing quota = %d, want nil", *got)
	}
	// An insert of a new repository keeps it too.
	nine := int64(9)
	if created, err := s.InsertDashboardRepo(ctx, DashboardRepo{Instance: "main", Project: "acme/other", CreatedBy: "admin", MonthlyTokenQuota: &nine}); err != nil || !created {
		t.Fatalf("InsertDashboardRepo = %v, %v", created, err)
	}
	if r, _, _ := s.DashboardRepo(ctx, "main", "acme/other"); r.MonthlyTokenQuota == nil || *r.MonthlyTokenQuota != 9 {
		t.Errorf("after insert quota = %v, want 9", r.MonthlyTokenQuota)
	}
	// The database refuses a quota of zero.
	zero := int64(0)
	if _, err := s.UpdateDashboardRepo(ctx, "main", "acme/widgets", func(r *DashboardRepo) { r.MonthlyTokenQuota = &zero }); err == nil {
		t.Error("a quota of 0 was stored")
	}
}

func TestSetInstanceTokenQuota(t *testing.T) {
	s := newStore(t)
	if err := s.CreateInstance(ctx, InstanceRow{Name: "lab", Platform: "gitlab"}, nil, t0); err != nil {
		t.Fatal(err)
	}
	row, _, err := s.InstanceRow(ctx, "lab")
	if err != nil || row.MonthlyTokenQuota != nil {
		t.Fatalf("new instance quota = %v, %v; want nil", row.MonthlyTokenQuota, err)
	}
	nine := int64(9)
	if found, err := s.SetInstanceTokenQuota(ctx, "lab", &nine); err != nil || !found {
		t.Fatalf("SetInstanceTokenQuota = %v, %v", found, err)
	}
	if row, _, _ := s.InstanceRow(ctx, "lab"); row.MonthlyTokenQuota == nil || *row.MonthlyTokenQuota != 9 {
		t.Errorf("quota = %v, want 9", row.MonthlyTokenQuota)
	}
	if found, err := s.SetInstanceTokenQuota(ctx, "lab", nil); err != nil || !found {
		t.Fatalf("SetInstanceTokenQuota(nil) = %v, %v", found, err)
	}
	if row, _, _ := s.InstanceRow(ctx, "lab"); row.MonthlyTokenQuota != nil {
		t.Errorf("quota = %d, want nil", *row.MonthlyTokenQuota)
	}
	if found, err := s.SetInstanceTokenQuota(ctx, "nope", &nine); err != nil || found {
		t.Errorf("unknown instance = %v, %v; want not found", found, err)
	}
	zero := int64(0)
	if _, err := s.SetInstanceTokenQuota(ctx, "lab", &zero); err == nil {
		t.Error("a quota of 0 was stored")
	}

	// An instance being removed keeps its quota.
	if found, err := s.SetInstanceTokenQuota(ctx, "lab", &nine); err != nil || !found {
		t.Fatal(found, err)
	}
	if ok, err := s.MarkInstanceRemoving(ctx, "lab"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	seven := int64(7)
	found, err := s.SetInstanceTokenQuota(ctx, "lab", &seven)
	if !errors.Is(err, ErrRemoving) || !found {
		t.Fatalf("SetInstanceTokenQuota of a removing instance = %v, %v; want found and ErrRemoving", found, err)
	}
	if row, _, _ := s.InstanceRow(ctx, "lab"); row.MonthlyTokenQuota == nil || *row.MonthlyTokenQuota != 9 {
		t.Errorf("quota = %v, want it unchanged at 9", row.MonthlyTokenQuota)
	}
}
