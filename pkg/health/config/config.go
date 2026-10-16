// Package config loads and validates the host-health YAML configuration file.
//
// The package is pure: Parse works on bytes and Load only reads the file.
// A check type that is absent from the file is represented by a nil section
// (or an empty list), which is distinct from a present section with defaults.
//
// YAML anchors and aliases are resolved only within one top-level section;
// referencing an anchor defined in a different top-level section is not
// supported and is reported as an invalid entry.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// MinInterval is the scheduler tick; no check may run more often.
const MinInterval = 30 * time.Second

// Mode selects how a PROBLEM is delivered.
type Mode string

// Delivery modes.
const (
	ModeTLG      Mode = "tlg"
	ModeSMS      Mode = "sms"
	ModeTLGOrSMS Mode = "tlg-or-sms"
)

// Content selects how a freshness file is interpreted.
type Content string

// Freshness content kinds.
const (
	ContentEpoch Content = "epoch"
	ContentMtime Content = "mtime"
)

// Report selects where a command check reports its failures.
type Report string

// Command report destinations.
const (
	ReportJournal Report = "journal"
	ReportAlert   Report = "alert"
)

// AuthRefusal selects the verdict for an HTTP 401 or 403 answer.
type AuthRefusal string

// HTTP auth-refusal verdicts.
const (
	AuthRefusalDown         AuthRefusal = "down"
	AuthRefusalCheckerError AuthRefusal = "checker_error"
)

// TimeoutVerdict selects the verdict for a command probe that timed out.
type TimeoutVerdict string

// Command timeout verdicts.
const (
	TimeoutBroken     TimeoutVerdict = "broken"
	TimeoutUnreadable TimeoutVerdict = "unreadable"
)

// EntryError describes one invalid entry (or an unreadable file when Entry is
// empty). The entry is skipped; the rest of the configuration stays usable.
type EntryError struct {
	File   string
	Entry  string
	Reason string
}

// Error implements error.
func (e EntryError) Error() string {
	if e.Entry == "" {
		return fmt.Sprintf("%s: %s", e.File, e.Reason)
	}

	return fmt.Sprintf("%s: %s: %s", e.File, e.Entry, e.Reason)
}

// Load reads and parses the file at path. When the file cannot be read or
// parsed the returned Config is nil and there is exactly one error.
func Load(path string) (*Config, []EntryError) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, []EntryError{{File: path, Reason: fmt.Sprintf("read: %v", err)}}
	}

	return Parse(data, path)
}

// Parse decodes and validates a configuration document. file is used only to
// label errors. Valid entries are kept; each invalid entry yields an
// EntryError and is skipped. An unparsable document yields a nil Config and a
// single error.
func Parse(data []byte, file string) (*Config, []EntryError) {
	var top map[string]yaml.Node

	if err := yaml.Unmarshal(data, &top); err != nil {
		return nil, []EntryError{{File: file, Reason: oneLine(err)}}
	}

	r := &reporter{file: file}

	reportUnknownSections(r, top)

	cfg := &Config{General: defaultGeneral()}

	if n, ok := top["general"]; ok {
		g := defaultGeneral()
		if decodeEntry(r, "general", &n, &g, validateGeneral) {
			cfg.General = g
		}
	}

	cfg.Summary = section(r, top, "summary", defaultSummary(), validateSummary)
	cfg.Network = networkSection(r, top)
	cfg.UPS = section(r, top, "ups", defaultUPS(), validateUPS)
	cfg.Disks = section(r, top, "disks", defaultDisks(), validateDisks)
	cfg.Temperatures = section(r, top, "temperatures", defaultTemperatures(), validateTemperatures)
	cfg.Kernel = section(r, top, "kernel", defaultKernel(), validateKernel)
	cfg.Containers = section(r, top, "containers", defaultContainers(), validateContainers)
	cfg.ModemProbe = section(r, top, "modem_probe", defaultModemProbe(), validateModemProbe)
	cfg.HTTP = list(r, top, "http", defaultHTTP, ignoreNode(validateHTTP))
	cfg.Freshness = list(r, top, "freshness", defaultFreshness, validateFreshness)
	cfg.Command = list(r, top, "command", defaultCommand, ignoreNode(validateCommand))

	resolveDependsOn(r, cfg)

	return cfg, r.errs
}

// reportUnknownSections reports every unknown top-level key as its own entry
// error so one typo does not disable the other checks.
func reportUnknownSections(r *reporter, top map[string]yaml.Node) {
	known := append([]string{"general", "summary"}, checkKinds()...)

	var unknown []string

	for k := range top {
		if !slices.Contains(known, k) {
			unknown = append(unknown, k)
		}
	}

	for _, k := range sortedStrings(unknown) {
		node := top[k]
		r.add(k, fmt.Sprintf("line %d: unknown top-level section", node.Line))
	}
}

// reporter collects entry errors for one file.
type reporter struct {
	file string
	errs []EntryError
}

func (r *reporter) add(entry, reason string) {
	r.errs = append(r.errs, EntryError{File: r.file, Entry: entry, Reason: reason})
}

// strictDecode decodes a YAML node into v, rejecting unknown keys.
func strictDecode(node *yaml.Node, v any) error {
	raw, err := yaml.Marshal(node)
	if err != nil {
		return fmt.Errorf("re-encode: %w", err)
	}

	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)

	// An empty or null node leaves the defaults untouched.
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode: %w", err)
	}

	return nil
}

// decodeEntry decodes node on top of the defaults already in v, validates it and reports problems
// under the entry label. ok is false when the entry is invalid.
func decodeEntry[T any](r *reporter, label string, node *yaml.Node, v *T, validate func(*T, *problems)) bool {
	if err := strictDecode(node, v); err != nil {
		r.add(label, decodeReason(err, node))

		return false
	}

	var p problems

	validate(v, &p)

	if len(p) > 0 {
		r.add(label, fmt.Sprintf("line %d: %s", node.Line, p.reason()))

		return false
	}

	return true
}

// section decodes an optional single-entry section; nil when absent or invalid.
func section[T any](r *reporter, top map[string]yaml.Node, name string, def T, validate func(*T, *problems)) *T {
	node, ok := top[name]
	if !ok {
		return nil
	}

	if !decodeEntry(r, name, &node, &def, validate) {
		return nil
	}

	return &def
}

// networkSection decodes the network section. Whether the file sets a
// threshold is read from the node itself, so no threshold value has to stand
// for "absent".
func networkSection(r *reporter, top map[string]yaml.Node) *Network {
	set := false
	if node, ok := top["network"]; ok {
		var probe struct {
			Threshold *Duration `yaml:"threshold"`
		}
		// A node that does not decode fails the strict decode below anyway.
		if node.Decode(&probe) == nil {
			set = probe.Threshold != nil
		}
	}

	return section(r, top, "network", defaultNetwork(), func(n *Network, p *problems) {
		validateNetwork(n, set, p)
	})
}

// named is implemented (via pointer) by list entries.
type named interface {
	entryName() string
}

func (h *HTTP) entryName() string      { return h.Name }
func (f *Freshness) entryName() string { return f.Name }
func (c *Command) entryName() string   { return c.Name }

// list decodes a list section entry by entry, keeping the valid ones.
func list[T any, PT interface {
	*T
	named
}](r *reporter, top map[string]yaml.Node, name string, def func() T, validate func(*T, *yaml.Node, *problems)) []T {
	node, ok := top[name]
	if !ok || node.Kind == 0 || (node.Kind == yaml.ScalarNode && node.Tag == "!!null") {
		return nil
	}

	if node.Kind != yaml.SequenceNode {
		r.add(name, "must be a list")

		return nil
	}

	var out []T

	seen := map[string]bool{}

	for i, item := range node.Content {
		label := entryLabel(name, i, item)

		v := def()
		if !decodeEntry(r, label, item, &v, func(v *T, p *problems) {
			validate(v, item, p)
			p.name(PT(v).entryName())
		}) {
			continue
		}

		n := PT(&v).entryName()
		if seen[n] {
			r.add(label, "duplicate name")

			continue
		}

		seen[n] = true

		out = append(out, v)
	}

	return out
}

// ignoreNode adapts an entry validation that needs only the decoded entry.
func ignoreNode[T any](validate func(*T, *problems)) func(*T, *yaml.Node, *problems) {
	return func(v *T, _ *yaml.Node, p *problems) { validate(v, p) }
}

// hasKey reports whether the mapping node sets key itself (not a nested one).
func hasKey(node *yaml.Node, key string) bool {
	if node.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return true
		}
	}

	return false
}

// entryLabel names a list entry "section/name", or "section[index]" when the
// entry has no readable name.
func entryLabel(section string, index int, item *yaml.Node) string {
	var probe struct {
		Name string `yaml:"name"`
	}

	if item.Decode(&probe) == nil && probe.Name != "" {
		return section + "/" + probe.Name
	}

	return fmt.Sprintf("%s[%d]", section, index)
}

// decodeReason turns a decoding error into a reason with real file line
// numbers. The decoder runs over a re-encoded copy of the node, so the "line N"
// it reports is meaningless; it is replaced by the line of the offending key
// when that can be found, or else by the line where the entry starts.
func decodeReason(err error, node *yaml.Node) string {
	var typeErr *yaml.TypeError
	if !errors.As(err, &typeErr) {
		return fmt.Sprintf("line %d: %s", node.Line, oneLine(err))
	}

	parts := make([]string, 0, len(typeErr.Errors))

	for _, msg := range typeErr.Errors {
		if rest, ok := strings.CutPrefix(msg, "line "); ok {
			if _, after, found := strings.Cut(rest, ": "); found {
				msg = after
			}
		}

		line := node.Line

		if f, ok := strings.CutPrefix(msg, "field "); ok {
			if key, _, found := strings.Cut(f, " not found"); found {
				if l := keyLine(node, key); l > 0 {
					line = l
				}
			}
		}

		parts = append(parts, fmt.Sprintf("line %d: %s", line, msg))
	}

	return strings.Join(parts, "; ")
}

// keyLine finds the line of the first mapping key called key within node.
func keyLine(node *yaml.Node, key string) int {
	if node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == key {
				return node.Content[i].Line
			}
		}
	}

	for _, c := range node.Content {
		if l := keyLine(c, key); l > 0 {
			return l
		}
	}

	return 0
}

func oneLine(err error) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(err.Error(), "\n", "; ")), " ")
}
