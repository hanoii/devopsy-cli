package images

import (
	"fmt"
	"testing"
)

// TestDataRandom in image mode: every release pulls what the project
// builds (a database FROM mariadb pushed under a commit tag), so a hold
// on a built data service goes by its registry digest, a rollback pulls
// its tag, and nothing is ever built on the server.
func TestDataRandomImageMode(t *testing.T) {
	t.Parallel()
	for seed := int64(1); seed <= int64(seeds(300)); seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) { dataRandomMode(t, seed, ModeImage) })
	}
}
