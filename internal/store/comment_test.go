package store

import "testing"

func TestCommentFooterNote(t *testing.T) {
	s := newStore(t)
	if note, err := s.CommentFooterNote(ctx); err != nil || note != "" {
		t.Fatalf("unset: %q, %v", note, err)
	}
	if err := s.PutCommentFooterNote(ctx, "Managed by the platform team", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.PutCommentFooterNote(ctx, "Run by the platform team", t0); err != nil {
		t.Fatal(err)
	}
	if note, err := s.CommentFooterNote(ctx); err != nil || note != "Run by the platform team" {
		t.Errorf("after two puts: %q, %v", note, err)
	}
	if err := s.PutCommentFooterNote(ctx, "", t0); err != nil {
		t.Fatal(err)
	}
	if note, err := s.CommentFooterNote(ctx); err != nil || note != "" {
		t.Errorf("after clearing: %q, %v", note, err)
	}
}
