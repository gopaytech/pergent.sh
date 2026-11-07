package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// noteForge is a forge that reads comments back.
type noteForge struct {
	forge.Forge
	comment forge.Comment
	err     error
}

func (f noteForge) Mention() string { return "bot" }

func (f noteForge) Comment(context.Context, int64, int64, int64) (forge.Comment, error) {
	return f.comment, f.err
}

func TestCommentIsGenuine(t *testing.T) {
	cmd := store.Command{Kind: "dismiss", Arg: "abc123", Reason: "constant", Author: "mo", AuthorID: 7, CommentID: 9}
	real := forge.Comment{AuthorID: 7, Author: "mo", Body: "@bot dismiss abc123 constant"}
	for name, tc := range map[string]struct {
		change func(c *forge.Comment)
		err    error
		want   bool
	}{
		"as claimed":     {func(*forge.Comment) {}, nil, true},
		"another author": {func(c *forge.Comment) { c.AuthorID = 8 }, nil, false},
		"another name":   {func(c *forge.Comment) { c.Author = "eve" }, nil, false},
		"other text":     {func(c *forge.Comment) { c.Body = "LGTM" }, nil, false},
		"other finding":  {func(c *forge.Comment) { c.Body = "@bot dismiss fff000 constant" }, nil, false},
		"other reason":   {func(c *forge.Comment) { c.Body = "@bot dismiss abc123 nope" }, nil, false},
		"system note":    {func(c *forge.Comment) { c.System = true }, nil, false},
		"gone":           {func(*forge.Comment) {}, forge.ErrNotFound, false},
	} {
		t.Run(name, func(t *testing.T) {
			c := real
			tc.change(&c)
			got, err := commentIsGenuine(context.Background(), noteForge{comment: c, err: tc.err}, cmd)
			if err != nil || got != tc.want {
				t.Fatalf("got %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

// A platform that can't answer says nothing about the command: it is tried
// again, and never carried out on the payload's word.
func TestCommentIsGenuineReadFails(t *testing.T) {
	boom := errors.New("gitlab is down")
	got, err := commentIsGenuine(context.Background(), noteForge{err: boom}, store.Command{})
	if got || !errors.Is(err, boom) {
		t.Fatalf("got %v, %v", got, err)
	}
}

// plainForge is a platform with no comment reader: GitHub.
type plainForge struct{ forge.Forge }

func TestCommentIsGenuineWithoutReader(t *testing.T) {
	if got, err := commentIsGenuine(context.Background(), plainForge{}, store.Command{}); !got || err != nil {
		t.Fatalf("got %v, %v", got, err)
	}
}
