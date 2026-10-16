package config

import (
	"fmt"
	"slices"
	"strings"
)

// httpKind is the condition-key kind of http entries.
const httpKind = "http"

// resolveDependsOn rejects http entries whose depends_on names the entry
// itself, a condition no configured check raises, or a cycle: such a check
// would be skipped forever once its own condition is bad, or the dependency
// would silently never apply. Rejecting an entry can orphan another that
// depends on it, so it repeats until nothing changes.
func resolveDependsOn(r *reporter, cfg *Config) {
	for {
		bad := map[string]string{}

		for i := range cfg.HTTP {
			if reason := dependsOnProblem(cfg, &cfg.HTTP[i]); reason != "" {
				bad[cfg.HTTP[i].Name] = reason
			}
		}

		if len(bad) == 0 {
			return
		}

		cfg.HTTP = slices.DeleteFunc(cfg.HTTP, func(h HTTP) bool {
			reason, ok := bad[h.Name]
			if ok {
				r.add(httpKind+"/"+h.Name, reason)
			}

			return ok
		})
	}
}

// dependsOnProblem returns why the entry's depends_on is unusable, or "".
func dependsOnProblem(cfg *Config, h *HTTP) string {
	if h.DependsOn == "" {
		return ""
	}

	if h.DependsOn == httpKind+"/"+h.Name {
		return "depends_on must not name the entry itself"
	}

	kind, name, _ := strings.Cut(h.DependsOn, "/")
	if known, ok := dependsOnTargets(cfg)[kind]; !ok || !known(name) {
		return fmt.Sprintf("depends_on %q names no configured check", h.DependsOn)
	}

	if inCycle(cfg.HTTP, h.Name) {
		return fmt.Sprintf("depends_on %q forms a cycle", h.DependsOn)
	}

	return ""
}

// dependsOnTargets maps each configured check kind to whether it can raise
// the condition kind/name. The network keys include the 001 aliases the
// runner maps to network/connectivity. Kernel, containers and ups derive
// their keys from what they find on the host, so any name is accepted once
// the section is configured; disks and temperatures are refused earlier, as
// their keys have no kind/name form.
func dependsOnTargets(cfg *Config) map[string]func(name string) bool {
	anyName := func(string) bool { return true }
	out := map[string]func(string) bool{
		httpKind: func(n string) bool {
			return slices.ContainsFunc(cfg.HTTP, func(h HTTP) bool { return h.Name == n })
		},
		"freshness": func(n string) bool {
			return slices.ContainsFunc(cfg.Freshness, func(f Freshness) bool { return f.Name == n })
		},
		"command": func(n string) bool {
			return slices.ContainsFunc(cfg.Command, func(c Command) bool { return c.Name == n })
		},
	}

	if cfg.Network != nil {
		networkKeys := []string{"connectivity", "router", "internet"}
		out["network"] = func(n string) bool { return slices.Contains(networkKeys, n) }
	}

	if cfg.ModemProbe != nil {
		out["modem_probe"] = func(n string) bool { return n == "roundtrip" }
	}

	for kind, configured := range map[string]bool{
		"ups": cfg.UPS != nil, "kernel": cfg.Kernel != nil, "containers": cfg.Containers != nil,
	} {
		if configured {
			out[kind] = anyName
		}
	}

	return out
}

// inCycle reports whether following depends_on from the http entry start
// through other http entries leads back to it.
func inCycle(entries []HTTP, start string) bool {
	next := make(map[string]string, len(entries))

	for i := range entries {
		if target, ok := strings.CutPrefix(entries[i].DependsOn, httpKind+"/"); ok {
			next[entries[i].Name] = target
		}
	}

	cur := start
	for range entries {
		target, ok := next[cur]
		if !ok {
			return false
		}

		if target == start {
			return true
		}

		cur = target
	}

	return false
}
