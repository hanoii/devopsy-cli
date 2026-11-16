package images

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/report"
)

// A sharer has no build: and names the image a service's build has (its image:, else
// compose's <compose name>-<service>), as Docker shows names (a missing tag is latest); a
// service that builds never shares, and every builder of the name is given, sorted.
func TestSharedNamesEachBuildsSharers(t *testing.T) {
	t.Parallel()
	got := Shared("shop-prod", map[string]ImageSpec{
		"web":     {Image: "shop-web", Builds: true},
		"worker":  {Image: "shop-web:latest", Builds: true},
		"plain":   {Builds: true},
		"twin":    {Image: "shop-web", Builds: true},
		"migrate": {Image: "shop-web:latest"},
		"seed":    {Image: "shop-prod-plain"},
		"db":      {Image: "mariadb:11.8"},
		"empty":   {},
		"tagged":  {Image: "shop-web:2"},
	})
	want := map[string][]string{"migrate": {"twin", "web", "worker"}, "seed": {"plain"}}
	if !maps.EqualFunc(got, want, slices.Equal) {
		t.Errorf("sharers: %v, want %v", got, want)
	}
}

// The listing's active services are every profile's listing kept to the plain one's, as named
// there; an active sharer of a build only a profile has refuses, in words a person reads, with
// its detail and its fix.
func TestActiveOfKeepsThePlainServicesAndRefusesAnUnbuiltShare(t *testing.T) {
	t.Parallel()
	all := []Service{
		{Name: "migrate", Ref: "devopsy.internal/shop-prod/web", Shares: "web"},
		{Name: "tool", Ref: "devopsy.internal/shop-prod/web", Shares: "web", Profiled: true},
		{Name: "web", Ref: "devopsy.internal/shop-prod/web", Build: true},
	}
	got, err := activeOf(all, []Service{{Name: "migrate", Ref: "shop-web"}, {Name: "web"}})
	if err != nil || len(got) != 2 || got[0].Name != "migrate" || got[0].Ref != all[0].Ref || got[1].Name != "web" {
		t.Errorf("the active services: %+v (%v)", got, err)
	}
	_, err = activeOf(all[:2], []Service{{Name: "migrate"}})
	if err == nil || err.Error() != "migrate runs web's image, but this release does not build web" ||
		!slices.Equal(report.Details(err), []string{"web is only under a profile this release does not use"}) ||
		len(report.Remedies(err)) != 1 || !strings.Contains(report.Remedies(err)[0].Text, "take web out of its profile, or put migrate under the same profile") {
		t.Errorf("a sharer of a build no release builds: %v, %q, %+v", err, report.Details(err), report.Remedies(err))
	}
}
