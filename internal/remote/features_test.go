package remote

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// A feature a release's project needs (projectFeatures) is one the server's devopsy reports,
// and is added to what the session checks beside its scripts' own needs, where an older
// server refuses it before the upload.
func TestProjectFeatures(t *testing.T) {
	t.Parallel()
	if !slices.Contains(strings.Fields(FeaturesList()), UIDsFeature) {
		t.Errorf("%s is not reported: %s", UIDsFeature, FeaturesList())
	}
	ctx := WithAlsoNeeded(WithAlsoNeeds(context.Background(), "devopsy --say info x"), UIDsFeature)
	if got := alsoNeeds(ctx); !slices.Contains(got, "say") || !slices.Contains(got, UIDsFeature) {
		t.Errorf("needs %v", got)
	}
	if got := needsLine("true\n", alsoNeeds(ctx)...); !strings.Contains(got, UIDsFeature) {
		t.Errorf("the prelude's line %q lacks %s", got, UIDsFeature)
	}
}
