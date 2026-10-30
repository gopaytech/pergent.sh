package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gopaytech/pergent.sh/internal/config"
	"github.com/gopaytech/pergent.sh/internal/inputs"
	"github.com/gopaytech/pergent.sh/internal/server"
)

func TestParseArgs(t *testing.T) {
	if got, err := parseArgs([]string{"serve"}, io.Discard); err != nil || got != "/etc/pergent-sh/config.yaml" {
		t.Errorf("serve = %q, %v; want the default path", got, err)
	}
	if got, err := parseArgs([]string{"serve", "--config", "/tmp/c.yaml"}, io.Discard); err != nil || got != "/tmp/c.yaml" {
		t.Errorf("serve --config = %q, %v", got, err)
	}
	for _, args := range [][]string{nil, {"run"}, {"serve", "extra"}, {"serve", "--nope"}} {
		if _, err := parseArgs(args, io.Discard); err == nil {
			t.Errorf("parseArgs(%q) succeeded, want an error", args)
		}
	}
}

func TestCheckSkills(t *testing.T) {
	cfg := &config.Config{SkillsDir: "/skills", Repositories: []config.Repository{
		{GitLab: "main", Project: "acme/widgets", Skills: []string{"security"}},
		{GitLab: "main", Project: "acme/gadgets"},
	}}
	if err := checkSkills(cfg, inputs.Library{"security": nil}); err != nil {
		t.Errorf("checkSkills = %v, want nil", err)
	}
	err := checkSkills(cfg, inputs.Library{"style": nil})
	if err == nil || !strings.Contains(err.Error(), `skill "security"`) || !strings.Contains(err.Error(), "acme/widgets") {
		t.Errorf("checkSkills = %v, want an error naming the skill and repository", err)
	}
}

// The routes answer while the loops are stuck (as in a startup reconcile
// against a GitLab that doesn't answer), and cancelling stops both.
func TestServeUntilDoneAnswersWhileLoopsBlock(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Routes(server.Deps{Config: &config.Config{}})
	loopsStarted := make(chan struct{})
	loopsStopped := make(chan struct{})
	loops := func(ctx context.Context) {
		close(loopsStarted)
		<-ctx.Done() // blocked, like a reconcile waiting on GitLab
		close(loopsStopped)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- serveUntilDone(ctx, ln, handler, loops, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()

	<-loopsStarted
	resp, err := http.Get("http://" + ln.Addr().String() + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz while the loops block: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Errorf("GET /healthz = %d %q, want 200 ok", resp.StatusCode, body)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serveUntilDone = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveUntilDone didn't return after cancel")
	}
	select {
	case <-loopsStopped:
	default:
		t.Errorf("the loops were not stopped")
	}
}
