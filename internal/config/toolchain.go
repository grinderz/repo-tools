package config

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The toolchain is the config's word on which language and tool versions the
// projects should be on: go, go_toolchain, python, uv, whatever the pins and
// the commands of cmds.toolchain write. The keys mean nothing to rt; each
// becomes the placeholders {<key>}, {<key>_minor} and {<key>_next_minor} for
// the pin values, the commands and toolchain_commit_message — the last one
// for an upper bound like requires-python's <3.14 that has to move with the
// version.

var toolchainKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// toolchainVersionRe is what a version may look like: one word — 1.26.2,
// 0.12, a release codename like trixie — that a command can write into a
// file. "latest" is the one word that is not a version: it moves.
var toolchainVersionRe = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._-]*$`)

// placeholderRe matches a {placeholder} as a template would write one —
// wider than a key's shape, so {Go} is refused rather than written into a
// file as it is; a brace with a digit, a dot or a space inside ({2},
// {{.Id}}, { print }) is the shell's, sed's or go template's.
var placeholderRe = regexp.MustCompile(`\{([A-Za-z][A-Za-z0-9_-]*)\}`)

// placeholdersIn names the placeholders of a template. A shell parameter
// of the same shape, ${home}, is the shell's: expand would still replace
// one named like a key, so a command keeps to parameters that are not.
func placeholdersIn(tpl string) []string {
	var names []string

	for _, m := range placeholderRe.FindAllStringSubmatchIndex(tpl, -1) {
		// ${home} is the shell's, {{end}} a go template's
		if m[0] > 0 && (tpl[m[0]-1] == '$' || tpl[m[0]-1] == '{') {
			continue
		}

		names = append(names, tpl[m[2]:m[3]])
	}

	return names
}

// commonPlaceholders are the placeholders every command template may use
// regardless of the toolchain; anything else in a toolchain command has to
// be a toolchain key.
//
//nolint:gochecknoglobals // a fixed table
var commonPlaceholders = map[string]bool{
	"project": true, "branch": true, "task": true, "product_version": true, "rt": true,
}

// applyToolingDefaults is the renovate and toolchain part of applyDefaults.
func (c *Config) applyToolingDefaults() error {
	if err := c.Renovate.applyDefaults(); err != nil {
		return err
	}

	if c.ToolchainCommitMessage == "" {
		c.ToolchainCommitMessage = "chore: bump toolchain"
	}

	return c.validateToolchain()
}

// validateToolchain checks the keys and values of every toolchain map and
// that the toolchain commands only ask for versions the config gives.
func (c *Config) validateToolchain() error {
	if err := validateToolchainMap(c.Toolchain, "toolchain"); err != nil {
		return err
	}

	if err := c.validatePins(c.Pins, "pins"); err != nil {
		return err
	}

	for _, kind := range slices.Sorted(maps.Keys(c.Cmds.Toolchain)) {
		if _, ok := c.Cmds.Deps[kind]; !ok {
			return fmt.Errorf(
				"cmds.toolchain: %q %w (defined: %s)",
				kind,
				errNotDepsKind,
				strings.Join(c.DepsKinds(), ", "),
			)
		}
	}

	for _, kind := range slices.Sorted(maps.Keys(c.Cmds.ToolchainCheck)) {
		if _, ok := c.Cmds.Deps[kind]; !ok {
			return fmt.Errorf("cmds.toolchain_check: %q %w (defined: %s)",
				kind, errNotDepsKind, strings.Join(c.DepsKinds(), ", "))
		}
	}

	return nil
}

func validateToolchainMap(versions map[string]string, where string) error {
	for _, key := range slices.Sorted(maps.Keys(versions)) {
		if !toolchainKeyRe.MatchString(key) {
			return fmt.Errorf("%s: %q %w", where, key, errBadToolchainKey)
		}

		// {branch} is every template's: a key of that name would take it over.
		if commonPlaceholders[key] {
			return fmt.Errorf("%s: %q %w: a placeholder of every template", where, key, errBadToolchainKey)
		}

		// go_minor is derived from go: a key of that name would race it.
		for _, suffix := range ToolchainSuffixes {
			if strings.HasSuffix(key, suffix) {
				return fmt.Errorf("%s: %q %w: %s is a derived form", where, key, errBadToolchainKey, suffix)
			}
		}

		if !toolchainVersionRe.MatchString(versions[key]) || versions[key] == "latest" {
			return fmt.Errorf("%s: %s: %q %w", where, key, versions[key], errNotAVersion)
		}
	}

	return nil
}

// validateProjectKinds checks that a project's own toolchain lists are
// keyed by deps kind like the global ones.
func (c *Config) validateProjectKinds(p *Project) error {
	for name, lists := range map[string]map[string][]string{
		"cmds.toolchain": p.Cmds.Toolchain, "cmds.toolchain_check": p.Cmds.ToolchainCheck,
	} {
		for _, kind := range slices.Sorted(maps.Keys(lists)) {
			if _, ok := c.Cmds.Deps[kind]; !ok {
				return fmt.Errorf("project %s: %s: %q %w (defined: %s)",
					p.Name, name, kind, errNotDepsKind, strings.Join(c.DepsKinds(), ", "))
			}
		}
	}

	return nil
}

// validateProjectToolchain checks a project's own versions and pins, and
// that every pin value, toolchain command and the commit message can be
// expanded for it.
func (c *Config) validateProjectToolchain(p *Project) error {
	if err := validateToolchainMap(p.Toolchain, "project "+p.Name+": toolchain"); err != nil {
		return err
	}

	if err := c.validatePins(p.Pins, "project "+p.Name+": pins"); err != nil {
		return err
	}

	if err := c.validateProjectKinds(p); err != nil {
		return err
	}

	versions := c.ToolchainFor(p)
	templates := append(c.pinValues(p), c.ToolchainCommands(p)...)
	templates = append(templates, c.ToolchainCheckCommands(p)...)

	if len(templates) > 0 {
		templates = append(templates, c.ToolchainCommitMessageFor(p))
	}

	for _, tpl := range templates {
		for _, name := range placeholdersIn(tpl) {
			if commonPlaceholders[name] {
				continue
			}

			key := ToolchainKey(name)

			version, ok := versions[key]
			if !ok {
				return fmt.Errorf("project %s: {%s} %w", p.Name, name, errNoToolchainVersion)
			}

			if _, ok := NextMinorOf(version); !ok && strings.HasSuffix(name, toolchainNextMinor) {
				return fmt.Errorf("project %s: {%s}: %s %q %w", p.Name, name, key, version, errNoNextMinor)
			}
		}
	}

	return nil
}

// The derived forms of a toolchain key.
const (
	toolchainMinor     = "_minor"
	toolchainNextMinor = "_next_minor"
)

// ToolchainSuffixes are the derived forms of a toolchain key, longest first
// so _next_minor is not read as _minor.
//
//nolint:gochecknoglobals // a fixed table
var ToolchainSuffixes = []string{toolchainNextMinor, toolchainMinor}

// MinorOf is the first two components of a version: 1.26.2 -> 1.26, a
// version with one component as it is.
func MinorOf(version string) string {
	parts := strings.SplitN(version, ".", 3) //nolint:mnd // major, minor, the rest
	if len(parts) < 2 {                      //nolint:mnd // major and minor
		return version
	}

	return parts[0] + "." + parts[1]
}

// NextMinorOf is the minor after a version's: 3.13 -> 3.14, for the upper
// bound of a range. A version whose minor is not a number (3.14rc1, 4) has
// none.
func NextMinorOf(version string) (string, bool) {
	parts := strings.SplitN(version, ".", 3) //nolint:mnd // major, minor, the rest
	if len(parts) < 2 {                      //nolint:mnd // major and minor
		return "", false
	}

	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", false
	}

	return parts[0] + "." + strconv.Itoa(minor+1), true
}

// ToolchainKeysIn names the toolchain keys the templates ask for, through
// any of the placeholder forms, each once and sorted.
func ToolchainKeysIn(templates []string) []string {
	keys := map[string]bool{}

	for _, tpl := range templates {
		for _, name := range placeholdersIn(tpl) {
			if !commonPlaceholders[name] {
				keys[ToolchainKey(name)] = true
			}
		}
	}

	return slices.Sorted(maps.Keys(keys))
}

// ToolchainKey is the toolchain key a placeholder name refers to.
func ToolchainKey(name string) string {
	for _, suffix := range ToolchainSuffixes {
		if key, ok := strings.CutSuffix(name, suffix); ok {
			return key
		}
	}

	return name
}

// ToolchainFor is the versions one project is held to: the global map with
// the project's own entries over it.
func (c *Config) ToolchainFor(p *Project) map[string]string {
	versions := make(map[string]string, len(c.Toolchain)+len(p.Toolchain))
	maps.Copy(versions, c.Toolchain)
	maps.Copy(versions, p.Toolchain)

	return versions
}

// ToolchainCommitMessageFor is the project's own commit message, else the
// global one.
func (c *Config) ToolchainCommitMessageFor(p *Project) string {
	return firstNonEmpty(p.ToolchainCommitMessage, c.ToolchainCommitMessage)
}

// ToolchainCommands are the commands that put a project on the configured
// toolchain: its deps kind's list from cmds.toolchain, replaced by the
// project's own list for that kind, plus the project's lists for any other
// kind — a tooling repository with deps: none still carries the go pins.
func (c *Config) ToolchainCommands(p *Project) []string {
	return mergeKindLists(p.Deps, c.Cmds.Toolchain, p.Cmds.Toolchain)
}

// ToolchainCheckCommands are the read-only checks that say whether a project
// is on the configured toolchain, resolved like ToolchainCommands.
func (c *Config) ToolchainCheckCommands(p *Project) []string {
	return mergeKindLists(p.Deps, c.Cmds.ToolchainCheck, p.Cmds.ToolchainCheck)
}

// ToolchainKinds names the kinds a project's toolchain commands come from,
// for the plan.
func (c *Config) ToolchainKinds(p *Project) []string {
	return mergeKinds(p.Deps, c.Cmds.Toolchain, p.Cmds.Toolchain)
}

func mergeKindLists(deps string, global, own map[string][]string) []string {
	var cmds []string

	for _, kind := range mergeKinds(deps, global, own) {
		if list, ok := own[kind]; ok {
			cmds = append(cmds, list...)
		} else {
			cmds = append(cmds, global[kind]...)
		}
	}

	return cmds
}

// mergeKinds is the project's kind first (when either map has commands for
// it), then its own other kinds, sorted.
func mergeKinds(deps string, global, own map[string][]string) []string {
	var kinds []string

	if deps != DepsNone {
		if _, ok := own[deps]; ok || len(global[deps]) > 0 {
			kinds = append(kinds, deps)
		}
	}

	for _, kind := range slices.Sorted(maps.Keys(own)) {
		if kind != deps {
			kinds = append(kinds, kind)
		}
	}

	return kinds
}
