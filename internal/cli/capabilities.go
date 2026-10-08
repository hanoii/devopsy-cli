package cli

import "path/filepath"

// Capability describes an interface devopsy defines and a project may
// implement in .devopsy/capabilities/<Name>/<action>. This is what
// `devopsy --debug capabilities` prints, so it lives with the code that
// calls them; README's Capabilities section has the full contracts.
type Capability struct {
	Name string
	// What it lets devopsy do, in a sentence.
	What string
	// Who implements it, and what in devopsy calls it.
	Who, CalledBy string
	Actions       []CapabilityAction
}

// CapabilityAction is one executable of a capability.
type CapabilityAction struct {
	Name, Usage, Does string
	// Output is an example of what it prints, when devopsy reads it.
	Output string
}

// Capabilities are the capabilities this devopsy calls.
var Capabilities = []Capability{
	{
		Name:     "shell",
		What:     "replaces devopsy's own --shell, for a project needing more than its labels say (devopsy.shell picks the service, devopsy.shell.user the user)",
		Who:      "any project; most need only the labels",
		CalledBy: "devopsy [@<target>] --shell [service] [exec options...] [-- command...]",
		Actions: []CapabilityAction{
			{
				Name:  "open",
				Usage: "open [service] [exec options...] [-- command...]",
				Does:  "gets --shell's arguments and opens an interactive shell, or runs the command after --; devopsy execs it with the terminal attached",
			},
		},
	},
}

// ImplementedActions lists the actions of c the project at projectDir (its
// .devopsy/) has.
func ImplementedActions(projectDir string, c Capability) []string {
	var have []string
	if projectDir == "" {
		return nil
	}
	for _, a := range c.Actions {
		if isExecutableFile(filepath.Join(projectDir, "capabilities", c.Name, a.Name)) {
			have = append(have, a.Name)
		}
	}
	return have
}
