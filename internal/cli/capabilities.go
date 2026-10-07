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
		Name:     "domains",
		What:     "lets devopsy ask the server's proxy what it routes and how it gets each certificate, so --domains can check hosts from outside, and releases can learn the server's wildcard domain",
		Who:      "the server's proxy (devopsy-traefik does), found in DEVOPSY_PROXY_DIR, default /srv/traefik",
		CalledBy: "devopsy @<target> --domains [--retry], and every devopsy @<target> --release",
		Actions: []CapabilityAction{
			{
				Name:  "facts",
				Usage: "facts <host>... | facts --all",
				Does:  "prints what the proxy knows about these hosts (or all it routes): routed or not, its resolver, how the certificate is issued (method: http, dns-cname with the record to create, or dns-api), the wildcard covering it",
				Output: `{
  "version": 1,
  "ip": "203.0.113.10",
  "retries": ["shop"],
  "hosts": [
    {"host": "shop.vm1.example.com", "routed": true, "resolver": "letsencrypt1", "method": "http", "wildcard": "*.vm1.example.com"},
    {"host": "api.example.org", "routed": true, "resolver": "acmedns", "method": "dns-cname",
     "record": {"name": "_acme-challenge.api.example.org", "target": "abc.acme-vm1.example.com"}}
  ]
}`,
			},
			{
				Name:  "retry",
				Usage: "retry <name> <host>... | retry <name> --done",
				Does:  "asks the proxy for those certificates again (name keeps callers apart: the project, or server), or withdraws the request; prints nothing",
			},
			{
				Name:   "wildcard-domain",
				Usage:  "wildcard-domain",
				Does:   "prints the server's wildcard domain, empty for none: releases write it into each project's target.env",
				Output: `{"version": 1, "wildcard_domain": "vm1.example.com"}`,
			},
		},
	},
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
