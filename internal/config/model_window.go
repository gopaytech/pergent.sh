package config

import (
	"fmt"

	"github.com/gopaytech/pergent.sh/internal/result"
)

// ModelWindowMinVersion is the first pergent release that reads
// PERGENT_CONTEXT_LIMIT and PERGENT_OUTPUT_LIMIT in reviews and scans, as
// "vX.Y.Z". An older pergent ignores them, so an image below it gets neither,
// and "" refuses every image. pergent v0.14.0 is the first that reads both
// and caps the review batch size at 40,000 tokens.
var ModelWindowMinVersion = "v0.14.0"

// ModelWindowImage says whether image runs a pergent that reads the context
// window and max output of a model entry: its tag is a version
// (result.VersionAtLeast rules) at or above ModelWindowMinVersion. reason is
// the refusal's text, "" when it passes. It reads after a prefix such as "Jobs
// get no context window or max output for now: ". A run's page shows it, so it
// names the tag only, never the registry.
func ModelWindowImage(image string) (ok bool, reason string) {
	min := ModelWindowMinVersion
	if min == "" {
		return false, "no pergent image reads them yet"
	}
	tag := ImageTag(image)
	if tag == "" {
		return false, fmt.Sprintf("the image has no tag, so its pergent version is not known; they need %s or later", min)
	}
	atLeast, known := result.VersionAtLeast(tag, min)
	if !known {
		return false, fmt.Sprintf("image tag %q is not a version (vX.Y.Z); they need %s or later", tag, min)
	}
	if !atLeast {
		return false, fmt.Sprintf("image tag %q is older than %s, the first pergent that reads them", tag, min)
	}
	return true, ""
}
