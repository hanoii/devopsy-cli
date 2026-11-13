package setup

import (
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// A mount unit's name is systemd-escape --path's: slashes as -, a - and every character but
// letters, digits, : _ . as \x and its hex, a leading . too; the unit sits beside the others,
// enabled by local-fs.target.
func TestMountUnitName(t *testing.T) {
	t.Parallel()
	for p, want := range map[string]string{
		DefaultSpace:          `var-lib-devopsy\x2ddoor.mount`,
		"/srv/door/":          "srv-door.mount",
		"/srv//door":          "srv-door.mount",
		"/.hidden/a_b:c.d":    `\x2ehidden-a_b:c.d.mount`,
		"/srv/door two":       `srv-door\x20two.mount`,
		"/":                   "-.mount",
		"/var/lib/devopsy+do": `var-lib-devopsy\x2bdo.mount`,
	} {
		if got := MountUnitName(p); got != want {
			t.Errorf("%s: %s, want %s", p, got, want)
		}
	}
	if SpaceUnit(DefaultSpace) != `/etc/systemd/system/var-lib-devopsy\x2ddoor.mount` || SpaceUnitLink(DefaultSpace) != `/etc/systemd/system/local-fs.target.wants/var-lib-devopsy\x2ddoor.mount` {
		t.Errorf("the unit %s, its link %s", SpaceUnit(DefaultSpace), SpaceUnitLink(DefaultSpace))
	}
}

// The units' texts are those tried by hand on Debian 13 and Ubuntu 26.04, each after devopsy's
// header: the space bound on itself with nosuid and nodev at every boot; /proc remounted with
// hidepid=invisible and the group's number early at every boot; polkit in the group, by a
// drop-in named for what it is for, a path of its own: a drop-in named for devopsy alone
// (90-devopsy.conf) is one another tool may write with other text.
func TestDoorUnitTexts(t *testing.T) {
	t.Parallel()
	if PolkitDropIn != "/etc/systemd/system/polkit.service.d/90-devopsy-proc.conf" {
		t.Errorf("polkit's drop-in at %s", PolkitDropIn)
	}
	if got := SpaceUnitText(DefaultSpace); got != DoorHeader+`
[Mount]
What=/var/lib/devopsy-door
Where=/var/lib/devopsy-door
Type=none
Options=bind,nosuid,nodev

[Install]
WantedBy=local-fs.target
` {
		t.Errorf("the space's unit:\n%s", got)
	}
	if got := ProcUnitText("990"); got != DoorHeader+`
[Unit]
DefaultDependencies=no
After=systemd-remount-fs.service
Before=sysinit.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/bin/mount -o remount,hidepid=invisible,gid=990 /proc

[Install]
WantedBy=sysinit.target
` {
		t.Errorf("/proc's unit:\n%s", got)
	}
	if strings.Count(ProcUnitText(GIDMark), GIDMark) != 1 {
		t.Error("the root run puts the group's number in one place")
	}
	if got := PolkitDropInText(); got != DoorHeader+"\n[Service]\nSupplementaryGroups=devopsy-proc\n" {
		t.Errorf("polkit's drop-in:\n%s", got)
	}
	if got, err := MountsDropInText("/srv", DefaultSpace); err != nil || got != Header+"\n[Unit]\nRequiresMountsFor=/srv /var/lib/devopsy-door\n" {
		t.Errorf("the drop-in with a space (%v):\n%s", err, got)
	}
}

// door: space: goes into the deploy user's config as text, the rest as it is (its comments
// included), only where the file has no door: key and the lines then read back as door:
// space: the way the loader reads the file (its first YAML document's top mapping): a file
// with none, one without a final newline, no file at all take them; one with a door:, with a
// space or not, is refused, and so is one whose YAML ends before its last line (a flow
// mapping, an indented one, a ... end marker, a second document), where the lines would land
// unread and every run would append them again.
func TestAppendDoorConfig(t *testing.T) {
	t.Parallel()
	lines := "door:\n  space: /var/lib/devopsy-door\n"
	for name, c := range map[string]struct {
		have, want string
		fails      bool
	}{
		"no door:":           {have: "# mine\nreleases:\n  root: /srv\n", want: "# mine\nreleases:\n  root: /srv\n" + lines},
		"no final newline":   {have: "releases:\n  root: /srv", want: "releases:\n  root: /srv\n" + lines},
		"no file":            {have: "", want: lines},
		"a door: with space": {have: "door:\n  space: /srv/door\n", fails: true},
		"a door: without":    {have: "door:\n  vets: []\n", fails: true},
		"no mapping":         {have: "- a\n", fails: true},
		"a flow mapping":     {have: "{releases: {root: /srv}}\n", fails: true},
		"a mapping indented": {have: "  releases:\n    root: /srv\n", fails: true},
		"an end marker":      {have: "releases:\n  root: /srv\n...\n", fails: true},
		"two documents":      {have: "---\nreleases:\n  root: /srv\n---\nother: 1\n", fails: true},
	} {
		got, err := AppendDoorConfig([]byte(c.have), DefaultSpace)
		if c.fails != (err != nil) || !c.fails && string(got) != c.want {
			t.Errorf("%s: %q, %v", name, got, err)
		}
		if err != nil {
			continue
		}
		var doc struct {
			Door struct {
				Space string `yaml:"space"`
			} `yaml:"door"`
		}
		if err := yaml.Unmarshal(got, &doc); err != nil || doc.Door.Space != DefaultSpace {
			t.Errorf("%s: appended, and the config reads door: space: %q (%v):\n%s", name, doc.Door.Space, err, got)
		}
	}
	if got, err := AppendDoorConfig(nil, DefaultSpace); err != nil || string(got) != lines {
		t.Errorf("no file (nil): %q, %v", got, err)
	}
}

// A space devopsy writes into a unit and the config is a plain path: absolute, clean, not /,
// letters, digits and . _ - / only, each refusal saying plainly which.
func TestPlainPath(t *testing.T) {
	t.Parallel()
	for _, p := range []string{DefaultSpace, "/srv/door_1.d"} {
		if err := PlainPath(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	for p, want := range map[string]string{
		"srv/door":      "srv/door is not an absolute path",
		"/srv/../door":  "/srv/../door is not written plainly, as /door",
		"/":             "/ cannot be the directory",
		"/srv/door two": `/srv/door two holds ' ', and devopsy takes only letters, digits and . _ - /`,
		"/srv/%h":       `/srv/%h holds '%', and devopsy takes only letters, digits and . _ - /`,
		`/srv/a\b`:      `/srv/a\b holds '\\', and devopsy takes only letters, digits and . _ - /`,
		"/srv/é":        `/srv/é holds 'é', and devopsy takes only letters, digits and . _ - /`,
	} {
		if err := PlainPath(p); err == nil || err.Error() != want {
			t.Errorf("%s: %v, want %s", p, err, want)
		}
	}
}

// Each item's state as the facts hold it: what changes, and what a refusal stops (the tools
// and Docker every step, the space its steps, /proc another way's its own).
func TestDoorFactsSteps(t *testing.T) {
	t.Parallel()
	inSync := DoorFacts{Space: "/srv/door", Guarded: true,
		Processes: ProcFact{HidePID: "invisible", GID: "990", Group: "990", Member: true},
		Files:     []FileFact{{Item: ItemSpace, InSync: true}, {Item: ItemMounts, InSync: true}, {Item: ItemProcesses, InSync: true}}}
	if c := inSync.Changing(); len(c) != 0 || inSync.Applies() {
		t.Errorf("in sync: changing %v", c)
	}
	other := inSync
	other.Processes = ProcFact{HidePID: "2", GID: "4", Other: true, Member: true}
	other.Files = []FileFact{{Item: ItemMounts, InSync: true}}
	if c := other.Changing(); len(c) != 0 {
		t.Errorf("set another way: changing %v", c)
	}
	for name, c := range map[string]struct {
		change func(*DoorFacts)
		step   Step
	}{
		"space missing":      {func(f *DoorFacts) { f.SpaceAction = RootMake }, StepSpace},
		"space's mode":       {func(f *DoorFacts) { f.SpaceMode = "700" }, StepSpace},
		"space unguarded":    {func(f *DoorFacts) { f.Guarded = false }, StepSpace},
		"space's unit":       {func(f *DoorFacts) { f.Files[0].InSync = false }, StepSpace},
		"drop-in":            {func(f *DoorFacts) { f.Files[1].InSync = false }, StepDoorDrop},
		"no hidepid":         {func(f *DoorFacts) { f.Processes.HidePID = "off" }, StepProcesses},
		"another gid":        {func(f *DoorFacts) { f.Processes.GID = "4" }, StepProcesses},
		"no group":           {func(f *DoorFacts) { f.Processes.Group = "" }, StepProcesses},
		"no member":          {func(f *DoorFacts) { f.Processes.Member = false }, StepProcesses},
		"/proc's unit":       {func(f *DoorFacts) { f.Files[2].InSync = false }, StepProcesses},
		"no door: in config": {func(f *DoorFacts) { f.Config = DoorConfigLines(f.Space) }, StepConfig},
	} {
		f := inSync
		f.Files = slices.Clone(inSync.Files)
		c.change(&f)
		if got := f.Changing(); !slices.Equal(got, []Step{c.step}) {
			t.Errorf("%s: changing %v, want %s", name, got, c.step)
		}
	}
	all := inSync
	all.SpaceAction, all.Config, all.Processes.HidePID = RootMake, "x", "off"
	all.Files = []FileFact{{Item: ItemSpace}, {Item: ItemMounts}, {Item: ItemProcesses}}
	for name, c := range map[string]struct {
		r     Refusal
		stops []string
	}{
		"tools":     {Refusal{Item: RefusalTools, Stops: RefusalDoorTools}, []string{string(StepSpace), string(StepDoorDrop), string(StepProcesses), string(StepConfig)}},
		"docker":    {Refusal{Item: RefusalDocker}, []string{string(StepSpace), string(StepDoorDrop), string(StepProcesses), string(StepConfig)}},
		"floor":     {Refusal{Item: RefusalDocker, Stops: RefusalPlugins}, []string{string(StepSpace), string(StepDoorDrop), string(StepProcesses), string(StepConfig)}},
		"space":     {Refusal{Item: ItemSpace, Stops: RefusalSpace}, []string{string(StepSpace), string(StepDoorDrop), string(StepConfig)}},
		"config":    {Refusal{Item: ItemConfig, Stops: RefusalSpace}, []string{string(StepSpace), string(StepDoorDrop), string(StepConfig)}},
		"path":      {Refusal{Item: RefusalRoot, Stops: RefusalMountsPath}, []string{string(StepDoorDrop)}},
		"processes": {Refusal{Item: ItemProcesses, Stops: RefusalProcesses}, []string{string(StepProcesses)}},
	} {
		f := all
		f.Refusals = []Refusal{c.r}
		if got := f.Stopped(); !slices.Equal(got, c.stops) {
			t.Errorf("%s: stops %v, want %v", name, got, c.stops)
		}
		if f.NoneGoes() != (len(c.stops) == len(DoorSteps)) {
			t.Errorf("%s: none goes %v", name, f.NoneGoes())
		}
	}
}
