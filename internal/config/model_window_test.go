package config

import (
	"strings"
	"testing"
)

func setModelWindowMin(t *testing.T, v string) {
	t.Helper()
	old := ModelWindowMinVersion
	ModelWindowMinVersion = v
	t.Cleanup(func() { ModelWindowMinVersion = old })
}

func TestModelWindowImage(t *testing.T) {
	for _, tc := range []struct {
		min, image string
		ok         bool
		reason     string // a substring; "" means no refusal
	}{
		{"", "ghcr.io/x/pergent:v0.14.0", false, "no pergent image reads them yet"},
		{"", "ghcr.io/x/pergent:v99.0.0", false, "no pergent image reads them yet"},
		{"v0.14.0", "ghcr.io/x/pergent:v0.14.0", true, ""},
		{"v0.14.0", "ghcr.io/x/pergent:v0.15.1", true, ""},
		{"v0.14.0", "ghcr.io/x/pergent:v0.13.0", false, `image tag "v0.13.0" is older than v0.14.0, the first pergent that reads them`},
		{"v0.14.0", "ghcr.io/x/pergent:main", false, `image tag "main" is not a version (vX.Y.Z); they need v0.14.0 or later`},
		{"v0.14.0", "ghcr.io/x/pergent@sha256:abc123", false, "the image has no tag, so its pergent version is not known; they need v0.14.0 or later"},
		{"v0.14.0", "host:5000/pergent", false, "no tag"},
	} {
		setModelWindowMin(t, tc.min)
		ok, reason := ModelWindowImage(tc.image)
		if ok != tc.ok || (tc.ok && reason != "") || (!tc.ok && !strings.Contains(reason, tc.reason)) {
			t.Errorf("ModelWindowImage(%q) with floor %q = %v, %q; want %v, %q", tc.image, tc.min, ok, reason, tc.ok, tc.reason)
		}
		// A run's page shows the reason: it names the tag, never the registry.
		if strings.Contains(reason, "ghcr.io") || strings.Contains(reason, "host:5000") {
			t.Errorf("ModelWindowImage(%q) reason %q names the image", tc.image, reason)
		}
	}
}
