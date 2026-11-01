package forge_test

import (
	"testing"

	"github.com/gopaytech/pergent.sh/internal/forge"
	"github.com/gopaytech/pergent.sh/internal/inputs"
)

// forge can't import inputs, so its copy of the cap is kept equal here.
func TestMaxSourceFileIsTheConfigMapLimit(t *testing.T) {
	if forge.MaxSourceFile != inputs.ConfigMapLimit {
		t.Fatalf("forge.MaxSourceFile = %d, inputs.ConfigMapLimit = %d", forge.MaxSourceFile, inputs.ConfigMapLimit)
	}
}
