package engine

import (
	"os"
	"testing"
)

// TestMain sets the variable the test config's llm: block names once, for
// every test. t.Setenv in newTestEnv would stop its tests from running in
// parallel. A test that needs another value still uses t.Setenv (and so runs
// serially, before the parallel tests start).
func TestMain(m *testing.M) {
	if err := os.Setenv("OPENCODE_API_KEY", testLLMKey); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}
