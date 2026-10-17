package config

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
)

// A version lives on a line of a file: the go directive, GO_VERSION in a CI
// file, an ARG in a Dockerfile. A pin names that line — the file, a pattern
// that finds it, the version to write — and serves deps toolchain and deps
// check alike: the one rewrites the capture group, the other reads it. One
// entry for both, so the write and the check cannot drift apart, and a
// pattern that matches nothing is an error rather than a bump that quietly
// did not happen.

// Pin is one version line.
type Pin struct {
	// File is the path in the repository, or a glob (svc-*/Dockerfile) for
	// the same line in several files; a project without the file skips the
	// pin.
	File string `yaml:"file"`
	// Match is a regular expression with exactly one capture group, the
	// version. ^ and $ bind to lines; \n in the pattern crosses them.
	Match string `yaml:"match"`
	// Value is what the group becomes, with the toolchain placeholders.
	Value string `yaml:"value"`
	// Kinds limits the pin to projects of these deps kinds, none among them
	// for the projects without one; empty means every project that has the
	// file.
	Kinds []string `yaml:"kinds"`
	// Except names the projects a global pin passes over: the repository
	// that builds the ci-tools images has no CI_TOOLS_VERSION line.
	Except []string `yaml:"except"`
	// Optional allows the file to lack the line: nothing matched is then not
	// an error. Without it the pin has to find its line in every file it is
	// applied to.
	Optional bool `yaml:"optional"`
}

// IsGlob says whether File names files by pattern.
func (pin Pin) IsGlob() bool {
	return strings.ContainsAny(pin.File, "*?[")
}

// Files are the files of the pin among paths: the one it names, or those
// its glob matches.
func (pin Pin) Files(paths []string) []string {
	if !pin.IsGlob() {
		if slices.Contains(paths, pin.File) {
			return []string{pin.File}
		}

		return nil
	}

	var files []string

	for _, p := range paths {
		if ok, _ := path.Match(pin.File, p); ok {
			files = append(files, p)
		}
	}

	return files
}

// Regexp is the pattern as it is applied: over the whole file, ^ and $ at
// line boundaries. Load validated it.
func (pin Pin) Regexp() *regexp.Regexp {
	return regexp.MustCompile("(?m)" + pin.Match)
}

// Applies says whether the pin is for a project: one of its deps kinds,
// and not named in except.
func (pin Pin) Applies(p *Project) bool {
	if slices.Contains(pin.Except, p.Name) {
		return false
	}

	kind := p.Deps
	if kind == "" {
		kind = DepsNone
	}

	return len(pin.Kinds) == 0 || slices.Contains(pin.Kinds, kind)
}

// PinsFor is every pin a project is held to: the global ones for its kind,
// then its own.
func (c *Config) PinsFor(p *Project) []Pin {
	var pins []Pin

	for _, pin := range c.Pins {
		if pin.Applies(p) {
			pins = append(pins, pin)
		}
	}

	return append(pins, p.Pins...)
}

// validatePins checks that every pin can be applied: the three fields, a
// pattern that compiles with one group, kinds that are deps kinds, except
// naming projects of the config and only on the global pins — a project's
// own pin is for that project.
func (c *Config) validatePins(pins []Pin, where string) error {
	for i, pin := range pins {
		entry := fmt.Sprintf("%s[%d]", where, i)

		if err := c.validatePinExcept(pin, entry, where == "pins"); err != nil {
			return err
		}

		if pin.File == "" || pin.Match == "" || pin.Value == "" {
			return fmt.Errorf("%s: %w", entry, errBadPin)
		}

		if _, err := path.Match(pin.File, ""); err != nil {
			return fmt.Errorf("%s: file: %w", entry, err)
		}

		re, err := regexp.Compile("(?m)" + pin.Match)
		if err != nil {
			return fmt.Errorf("%s: match: %w", entry, err)
		}

		if re.NumSubexp() != 1 {
			return fmt.Errorf("%s: match %q %w", entry, pin.Match, errPinGroup)
		}

		for _, kind := range pin.Kinds {
			if _, ok := c.Cmds.Deps[kind]; !ok && kind != DepsNone {
				return fmt.Errorf("%s: kinds: %q %w (defined: %s, none)",
					entry, kind, errNotDepsKind, strings.Join(c.DepsKinds(), ", "))
			}
		}
	}

	return nil
}

// validatePinExcept checks a pin's except list.
func (c *Config) validatePinExcept(pin Pin, entry string, global bool) error {
	if len(pin.Except) == 0 {
		return nil
	}

	if !global {
		return fmt.Errorf("%s: except %w", entry, errExceptOnProjectPin)
	}

	for _, name := range pin.Except {
		if !slices.ContainsFunc(c.Projects, func(p *Project) bool { return p.Name == name }) {
			return fmt.Errorf("%s: except: %q %w", entry, name, errNotAProject)
		}
	}

	return nil
}

// validatePinsReach refuses a global pin whose except leaves no project it
// applies to: a pin nobody is held to is a bump that quietly does not
// happen. It runs once the projects carry their defaults, since a kind may
// come from there.
func (c *Config) validatePinsReach() error {
	for i, pin := range c.Pins {
		if len(pin.Except) > 0 && !slices.ContainsFunc(c.Projects, pin.Applies) {
			return fmt.Errorf("pins[%d]: except %w", i, errExceptEveryone)
		}
	}

	return nil
}

// pinValues are the value templates of a project's pins, for the placeholder
// check.
func (c *Config) pinValues(p *Project) []string {
	return PinValues(c.PinsFor(p))
}

// PinValues are the value templates of pins, for a summary of the versions
// they use.
func PinValues(pins []Pin) []string {
	values := make([]string, 0, len(pins))

	for _, pin := range pins {
		values = append(values, pin.Value)
	}

	return values
}
