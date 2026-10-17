package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
	"github.com/grinderz/repo-tools/internal/run"
)

// pinHit is one version line a pin found: what it holds and what the pin
// wants there, the same when the line is on the configured value.
type pinHit struct {
	File    string
	Line    int
	Label   string // the toolchain key the pin writes, for the eye
	Current string
	Want    string
}

// Changed says whether the line is not on the configured value.
func (h pinHit) Changed() bool { return h.Current != h.Want }

func (h pinHit) String() string {
	if !h.Changed() {
		return fmt.Sprintf("%s:%d %s: %s", h.File, h.Line, h.Label, h.Current)
	}

	return fmt.Sprintf("%s:%d %s: %s", h.File, h.Line, h.Label, run.Change(h.Current, h.Want))
}

// applyPins reads each pin's files among paths — the files git tracks in
// dir, so a pin is applied to what the commit will carry and not to an
// ignored or untracked file a glob would reach — finds the line and
// compares the version there with the pin's value; with write the files
// are rewritten. A file that is not there is skipped; a pin that matches
// nothing in a file that is there is an error unless it is optional.
// applied counts the pins that found a file, so a caller knows whether
// anything was checked. A check goes on past a pin's error and returns
// them all, so one line that is missing does not hide the rest; a write
// stops at the first.
func applyPins(
	dir string,
	paths []string,
	pins []config.Pin,
	vars map[string]string,
	write bool,
) ([]pinHit, int, error) {
	var (
		hits    []pinHit
		applied int
		errs    []error
	)

	for _, pin := range pins {
		pattern, label, want := pin.Regexp(), pinLabel(pin), expand(pin.Value, vars)
		found := false

		for _, file := range pin.Files(paths) {
			fileHits, matched, err := applyPin(dir, pin, pattern, label, file, want, write)
			if err != nil && write {
				return hits, applied, err
			}

			if err != nil {
				errs = append(errs, err)

				continue
			}

			found = found || matched

			hits = append(hits, fileHits...)
		}

		if found {
			applied++
		}
	}

	return hits, applied, errors.Join(errs...)
}

// applyPin is applyPins for one pin in one file git tracks: matched says
// the file has the line. A tracked file deleted from the working tree is
// not there to pin.
func applyPin(
	dir string,
	pin config.Pin,
	pattern *regexp.Regexp,
	label, file, want string,
	write bool,
) ([]pinHit, bool, error) {
	data, err := os.ReadFile(filepath.Join(dir, file))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}

	if err != nil {
		return nil, false, fmt.Errorf("pin %s: %w", file, err)
	}

	content := string(data)

	sites, updated := rewriteGroups(file, label, content, want, pattern.FindAllStringSubmatchIndex(content, -1))
	if len(sites) == 0 {
		if pin.Optional {
			return nil, false, nil
		}

		return nil, false, fmt.Errorf("pin %s: %q %w", file, pin.Match, errPinNotFound)
	}

	hits := slices.DeleteFunc(sites, func(h pinHit) bool { return !h.Changed() })

	if write && len(hits) > 0 {
		if err := rewriteFile(gitx.Repo{Dir: dir}, file, updated); err != nil {
			return hits, true, fmt.Errorf("pin %s: %w", file, err)
		}
	}

	return hits, true, nil
}

// pinLabel names what a pin writes: the toolchain keys of its value, as
// written ({go_minor} is go_minor), the value itself when it has none.
func pinLabel(pin config.Pin) string {
	if names := config.PlaceholdersIn(pin.Value); len(names) > 0 {
		return strings.Join(names, "+")
	}

	return pin.Value
}

// rewriteGroups puts want into the capture group of every match in
// content: every line the group was found on, with what it holds, and
// the rewritten content. No sites means no match had the group at all.
func rewriteGroups(file, label, content, want string, matches [][]int) ([]pinHit, string) {
	var (
		sites []pinHit
		out   strings.Builder
		last  int
	)

	for _, m := range matches {
		start, end := m[2], m[3] // the one capture group
		if start < 0 {
			continue // the group took no part in this match: (\S+)? on an empty tag
		}

		if end > start && content[end-1] == '\r' {
			end-- // $ stands before the \n of a CRLF line, the \r is the line's
		}

		sites = append(sites, pinHit{
			File: file, Line: strings.Count(content[:start], "\n") + 1, Label: label,
			Current: content[start:end], Want: want,
		})

		out.WriteString(content[last:start])
		out.WriteString(want)

		last = end
	}

	out.WriteString(content[last:])

	return sites, out.String()
}

// pinsAt are the pins that have a file at ref — the branch the bump works
// on, so the plan says what will happen there rather than in the checkout
// — and the files there. A ref that cannot be listed is an error, not a
// project with nothing to pin.
func pinsAt(r gitx.Repo, ref string, pins []config.Pin) ([]config.Pin, []string, error) {
	paths, err := r.FilesAt(ref)
	if err != nil {
		return nil, nil, fmt.Errorf("files on %s: %w", ref, err)
	}

	var present []config.Pin

	for _, pin := range pins {
		if len(pin.Files(paths)) > 0 {
			present = append(present, pin)
		}
	}

	return present, paths, nil
}

// pinPlanLines reads the pins' files on ref and says what the bump would
// do to them: one line per version line, current -> wanted where it
// moves, the value alone and dim where it stays, and one for a required
// line that is not there (the bump would stop on it).
func pinPlanLines(r gitx.Repo, ref string, paths []string, pins []config.Pin, vars map[string]string) []string {
	var (
		lines []string
		texts = map[string]string{} // a file once, however many pins it carries
	)

	for _, pin := range pins {
		pattern, label, want := pin.Regexp(), pinLabel(pin), expand(pin.Value, vars)

		for _, file := range pin.Files(paths) {
			text, read := texts[file]
			if !read {
				// A ref's file is absent only when the listing and the read
				// disagree, which one ref cannot; an empty text then says
				// "matches nothing", the honest answer.
				text, _, _ = r.FileAt(ref, file)
				texts[file] = text
			}

			sites, _ := rewriteGroups(file, label, text, want, pattern.FindAllStringSubmatchIndex(text, -1))
			if len(sites) == 0 && !pin.Optional {
				lines = append(
					lines,
					fmt.Sprintf("pin %s: %q matches nothing, the bump would stop here", file, pin.Match),
				)
			}

			for _, site := range sites {
				line := "pin " + site.String()
				if !site.Changed() {
					line = run.Dim(line)
				}

				lines = append(lines, line)
			}
		}
	}

	return lines
}
