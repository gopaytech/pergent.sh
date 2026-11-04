package diffmap

import (
	"reflect"
	"testing"
)

// twoHunks has one added line in the first hunk and one removed line in the
// second; the old and new sides drift by +1 after the first hunk and by 0
// after the second.
const twoHunks = `@@ -10,4 +10,5 @@ func first() {
 ctx10
 ctx11
+added12
 ctx13
 ctx14
@@ -40,3 +41,2 @@ func second() {
 keep41
-gone
 keep42
`

func TestParseHunks(t *testing.T) {
	f := Parse("a.go", "a.go", twoHunks, false)
	if f.Path != "a.go" || f.OldPath != "a.go" || f.Deleted {
		t.Fatalf("file fields = %+v", f)
	}
	if len(f.Hunks) != 2 {
		t.Fatalf("hunks = %d, want 2", len(f.Hunks))
	}
	h0, h1 := f.Hunks[0], f.Hunks[1]
	if h0.OldStart != 10 || h0.OldLines != 4 || h0.NewStart != 10 || h0.NewLines != 5 {
		t.Errorf("hunk 0 header = %+v", h0)
	}
	if h1.OldStart != 40 || h1.OldLines != 3 || h1.NewStart != 41 || h1.NewLines != 2 {
		t.Errorf("hunk 1 header = %+v", h1)
	}
	want0 := []Pair{{10, 10}, {11, 11}, {0, 12}, {12, 13}, {13, 14}}
	if !reflect.DeepEqual(h0.Lines, want0) {
		t.Errorf("hunk 0 lines = %v, want %v", h0.Lines, want0)
	}
	want1 := []Pair{{40, 41}, {42, 42}}
	if !reflect.DeepEqual(h1.Lines, want1) {
		t.Errorf("hunk 1 lines = %v, want %v", h1.Lines, want1)
	}
}

func TestHunkOf(t *testing.T) {
	f := Parse("a.go", "a.go", twoHunks, false)
	tests := []struct {
		line int
		hunk int
		ok   bool
	}{
		{9, 0, false},
		{10, 0, true},
		{12, 0, true},
		{14, 0, true},
		{15, 0, false},
		{41, 1, true},
		{42, 1, true},
		{43, 0, false},
	}
	for _, tt := range tests {
		got, ok := f.HunkOf(tt.line)
		if ok != tt.ok || (ok && got != tt.hunk) {
			t.Errorf("HunkOf(%d) = %d, %v; want %d, %v", tt.line, got, ok, tt.hunk, tt.ok)
		}
	}
}

func TestOldLine(t *testing.T) {
	f := Parse("a.go", "a.go", twoHunks, false)
	tests := []struct {
		line int
		old  int
		ok   bool
	}{
		{10, 10, true},
		{12, 0, false}, // added
		{13, 12, true},
		{14, 13, true},
		{41, 40, true},
		{42, 42, true},
		{5, 0, false}, // outside the hunks
		{43, 0, false},
	}
	for _, tt := range tests {
		got, ok := f.OldLine(tt.line)
		if ok != tt.ok || got != tt.old {
			t.Errorf("OldLine(%d) = %d, %v; want %d, %v", tt.line, got, ok, tt.old, tt.ok)
		}
	}
}

func TestMapOld(t *testing.T) {
	f := Parse("a.go", "a.go", twoHunks, false)
	tests := []struct {
		name string
		old  int
		new  int
		ok   bool
	}{
		{"above both hunks", 5, 5, true},
		{"unchanged in hunk 0", 11, 11, true},
		{"unchanged after the added line", 12, 13, true},
		{"last line of hunk 0", 13, 14, true},
		{"between hunks", 20, 21, true},
		{"unchanged in hunk 1", 40, 41, true},
		{"removed line", 41, 0, false},
		{"unchanged after the removed line", 42, 42, true},
		{"below both hunks", 50, 50, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := f.MapOld(tt.old)
			if ok != tt.ok || got != tt.new {
				t.Errorf("MapOld(%d) = %d, %v; want %d, %v", tt.old, got, ok, tt.new, tt.ok)
			}
		})
	}
}

func TestMapOldInsertionAndDeletedFile(t *testing.T) {
	// A pure insertion: old start is the line before the insertion.
	f := Parse("a.go", "a.go", "@@ -5,0 +6,2 @@\n+x\n+y\n", false)
	for old, want := range map[int]int{5: 5, 6: 8, 10: 12} {
		if got, ok := f.MapOld(old); !ok || got != want {
			t.Errorf("MapOld(%d) = %d, %v; want %d, true", old, got, ok, want)
		}
	}
	d := Parse("a.go", "a.go", "@@ -1,2 +0,0 @@\n-a\n-b\n", true)
	if got, ok := d.MapOld(1); ok {
		t.Errorf("MapOld on a deleted file = %d, true; want false", got)
	}
}

func TestParseEmptyAndMalformed(t *testing.T) {
	if f := Parse("a.go", "a.go", "", false); len(f.Hunks) != 0 {
		t.Errorf("empty patch hunks = %d, want 0", len(f.Hunks))
	}

	// A header without numbers is skipped, with the lines below it.
	f := Parse("a.go", "a.go", "@@ nonsense @@\n+x\n@@ -1,1 +1,2 @@\n a\n+b\n", false)
	if len(f.Hunks) != 1 || f.Hunks[0].NewStart != 1 || f.Hunks[0].NewLines != 2 {
		t.Fatalf("hunks = %+v", f.Hunks)
	}
	if want := []Pair{{1, 1}, {0, 2}}; !reflect.DeepEqual(f.Hunks[0].Lines, want) {
		t.Errorf("lines = %v, want %v", f.Hunks[0].Lines, want)
	}

	// "\ No newline at end of file" is not a line.
	f = Parse("a.go", "a.go", "@@ -1,2 +1,2 @@\n a\n-b\n\\ No newline at end of file\n+c\n\\ No newline at end of file\n", false)
	if want := []Pair{{1, 1}, {0, 2}}; len(f.Hunks) != 1 || !reflect.DeepEqual(f.Hunks[0].Lines, want) {
		t.Errorf("hunks = %+v", f.Hunks)
	}

	// A new file.
	f = Parse("new.go", "", "@@ -0,0 +1,3 @@\n+a\n+b\n+c\n", false)
	if len(f.Hunks) != 1 {
		t.Fatalf("hunks = %d, want 1", len(f.Hunks))
	}
	h := f.Hunks[0]
	if h.OldStart != 0 || h.OldLines != 0 || h.NewStart != 1 || h.NewLines != 3 {
		t.Errorf("header = %+v", h)
	}
	if want := []Pair{{0, 1}, {0, 2}, {0, 3}}; !reflect.DeepEqual(h.Lines, want) {
		t.Errorf("lines = %v, want %v", h.Lines, want)
	}
	for line := 1; line <= 3; line++ {
		if _, ok := f.OldLine(line); ok {
			t.Errorf("OldLine(%d) on a new file = true, want false", line)
		}
	}
}

func TestParseToleratesFileHeadersAndOmittedCounts(t *testing.T) {
	patch := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -3 +3 @@\n-old\n+new\n"
	f := Parse("a.go", "a.go", patch, false)
	if len(f.Hunks) != 1 {
		t.Fatalf("hunks = %d, want 1", len(f.Hunks))
	}
	h := f.Hunks[0]
	if h.OldLines != 1 || h.NewLines != 1 || !reflect.DeepEqual(h.Lines, []Pair{{0, 3}}) {
		t.Errorf("hunk = %+v", h)
	}
}
