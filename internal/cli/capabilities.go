package cli

import "path/filepath"

// Capability describes an interface devopsy defines and a project may
// implement in .devopsy/capabilities/<Name>/<action>. This is what
// `devopsy --debug capabilities` prints, so it lives with the code that
// calls them; README's Capabilities section has the full contracts.
type Capability struct {
	Name string
	// Who implements it, and when devopsy calls it.
	Who, When string
	Actions   []CapabilityAction
}

// CapabilityAction is one executable of a capability.
type CapabilityAction struct {
	Name, Usage, Does string
}

// Capabilities are the capabilities this devopsy calls.
var Capabilities = []Capability{
	{
		Name: "domains",
		Who:  "the server's proxy (devopsy-traefik), in DEVOPSY_PROXY_DIR (default /srv/traefik)",
		When: "devopsy @<target> --domains [--retry], and every --release (wildcard-domain)",
		Actions: []CapabilityAction{
			{"facts", "facts <host>... | --all", `JSON {"version": 1, "ip", "retries", "hosts": [{"host", "probe", "routed", "resolver", "method": "http|dns-cname|dns-api", "record", "wildcard"}]}`},
			{"retry", "retry <name> <host>... | retry <name> --done", "ask the proxy for those certificates again, or withdraw the request; prints nothing"},
			{"wildcard-domain", "wildcard-domain", `JSON {"version": 1, "wildcard_domain": "vm1.example.com"}, "" for none`},
		},
	},
	{
		Name: "shell",
		Who:  "any project wanting its own --shell (labels cover service and user: devopsy.shell, devopsy.shell.user)",
		When: "devopsy [@<target>] --shell [service] [exec options...] [-- command...]",
		Actions: []CapabilityAction{
			{"open", "open [service] [exec options...] [-- command...]", "interactive shell, or the command after --; devopsy execs it with the terminal attached"},
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
