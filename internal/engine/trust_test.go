package engine

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/settings"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// trustForge answers the two questions the trust check asks; anything else
// panics (the embedded interface is nil).
type trustForge struct {
	forge.Forge
	access                forge.Access
	accessErr             error
	vis                   forge.Visibility
	visErr                error
	accessCalls, visCalls int
}

func (f *trustForge) AccessLevel(context.Context, int64, string, int64) (forge.Access, error) {
	f.accessCalls++
	return f.access, f.accessErr
}

func (f *trustForge) Visibility(context.Context, string) (forge.Visibility, error) {
	f.visCalls++
	return f.vis, f.visErr
}

func TestAuthorTrust(t *testing.T) {
	boom := errors.New("GitLab said 500")
	alice := forge.Change{Author: "alice", AuthorID: 5}
	for _, tc := range []struct {
		name       string
		mode       string
		list       []store.TrustedAuthor
		f          trustForge
		ch         forge.Change
		want       bool
		wantErr    bool
		wantAccess int
		wantVis    int
	}{
		{name: "off", mode: config.TrustOff, f: trustForge{access: forge.AccessNone}, ch: alice, want: true},
		{name: "on, allowlisted", mode: config.TrustOn, list: []store.TrustedAuthor{{ID: 5, Username: "old-name"}}, ch: alice, want: true},
		{name: "on, developer", mode: config.TrustOn, f: trustForge{access: forge.AccessWrite}, ch: alice, want: true, wantAccess: 1},
		{name: "on, maintainer", mode: config.TrustOn, f: trustForge{access: forge.AccessMaintain}, ch: alice, want: true, wantAccess: 1},
		{name: "on, reporter", mode: config.TrustOn, f: trustForge{access: forge.AccessRead}, ch: alice, want: false, wantAccess: 1},
		{name: "on, no author ID", mode: config.TrustOn, list: []store.TrustedAuthor{{ID: 0, Username: "alice"}}, f: trustForge{access: forge.AccessMaintain}, ch: forge.Change{Author: "alice"}, want: false},
		{name: "follow, private", mode: config.TrustFollowVisibility, f: trustForge{vis: forge.VisPrivate}, ch: alice, want: true, wantVis: 1},
		{name: "follow, internal", mode: config.TrustFollowVisibility, f: trustForge{vis: forge.VisInternal}, ch: alice, want: true, wantVis: 1},
		{name: "follow, public reporter", mode: config.TrustFollowVisibility, f: trustForge{vis: forge.VisPublic, access: forge.AccessRead}, ch: alice, want: false, wantVis: 1, wantAccess: 1},
		{name: "follow, visibility unreadable", mode: config.TrustFollowVisibility, f: trustForge{visErr: boom, access: forge.AccessRead}, ch: alice, want: false, wantVis: 1, wantAccess: 1},
		{name: "follow, unreadable but developer", mode: config.TrustFollowVisibility, f: trustForge{visErr: boom, access: forge.AccessWrite}, ch: alice, want: true, wantVis: 1, wantAccess: 1},
		{name: "access error", mode: config.TrustOn, f: trustForge{accessErr: boom}, ch: alice, wantErr: true, wantAccess: 1},
		{name: "unknown mode is on", mode: "", f: trustForge{access: forge.AccessRead}, ch: alice, want: false, wantAccess: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.f
			repo := settings.Effective{TrustedAuthorsOnly: tc.mode, TrustedAuthors: tc.list}
			got, err := authorTrust(context.Background(), &f, repo, 7, "acme/widgets", tc.ch, slog.New(slog.DiscardHandler))
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Errorf("authorTrust = %v, %v; want %v (error %v)", got, err, tc.want, tc.wantErr)
			}
			if f.accessCalls != tc.wantAccess || f.visCalls != tc.wantVis {
				t.Errorf("calls: access %d visibility %d; want %d and %d", f.accessCalls, f.visCalls, tc.wantAccess, tc.wantVis)
			}
		})
	}
}
