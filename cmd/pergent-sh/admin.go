package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/gopaytech/pergent.sh/internal/dashboard"
	"github.com/gopaytech/pergent.sh/internal/store"
)

// resetPassword runs "admin reset-password <username>": it gives the
// password account username a new random password, which must be changed
// at the next sign-in, and ends its sessions. It needs only DATABASE_URL.
// The password goes to stdout, with fmt (never the JSON log); errors go to
// stderr. It returns the exit code: 0, or 1 on an error.
func resetPassword(ctx context.Context, username string, stdout, stderr io.Writer) int {
	pw, err := runResetPassword(ctx, username)
	if errors.Is(err, dashboard.ErrNotPasswordAccount) {
		fmt.Fprintf(stderr, "pergent-sh: no password account named %q\n", username)
		return 1
	}
	if err != nil {
		fmt.Fprintf(stderr, "pergent-sh: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "new password for %s: %s\n", username, pw)
	return 0
}

func runResetPassword(ctx context.Context, username string) (string, error) {
	dbURL, err := requireEnv("DATABASE_URL")
	if err != nil {
		return "", err
	}
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		return "", err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return "", err
	}
	return dashboard.ResetPassword(ctx, st, username, time.Now())
}
