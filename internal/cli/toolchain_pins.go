package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
)

// pinHit is one version line that is not on the configured value.
type pinHit struct {
	File    string
	Line    int
	Current string
	Want    string
}

func (h pinHit) String() string {
	return fmt.Sprintf("%s:%d: %s -> %s", h.File, h.Line, h.Current, h.Want)
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
		pattern := pin.Regexp()
		found := false

		for _, file := range pin.Files(paths) {
			fileHits, matched, err := applyPin(dir, pin, pattern, file, expand(pin.Value, vars), write)
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
	file, want string,
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

	hits, updated, matched := rewriteGroups(file, content, want, pattern.FindAllStringSubmatchIndex(content, -1))
	if !matched {
		if pin.Optional {
			return nil, false, nil
		}

		return nil, false, fmt.Errorf("pin %s: %q %w", file, pin.Match, errPinNotFound)
	}

	if write && len(hits) > 0 {
		if err := rewriteFile(gitx.Repo{Dir: dir}, file, updated); err != nil {
			return hits, true, fmt.Errorf("pin %s: %w", file, err)
		}
	}

	return hits, true, nil
}

// rewriteGroups puts want into the capture group of every match in
// content: the hits where it differs, the rewritten content, and whether
// any match had the group at all.
func rewriteGroups(file, content, want string, matches [][]int) ([]pinHit, string, bool) {
	var (
		hits    []pinHit
		out     strings.Builder
		last    int
		matched bool
	)

	for _, m := range matches {
		start, end := m[2], m[3] // the one capture group
		if start < 0 {
			continue // the group took no part in this match: (\S+)? on an empty tag
		}

		if end > start && content[end-1] == '\r' {
			end-- // $ stands before the \n of a CRLF line, the \r is the line's
		}

		matched = true

		if current := content[start:end]; current != want {
			hits = append(hits, pinHit{
				File: file, Line: strings.Count(content[:start], "\n") + 1, Current: current, Want: want,
			})
		}

		out.WriteString(content[last:start])
		out.WriteString(want)

		last = end
	}

	out.WriteString(content[last:])

	return hits, out.String(), matched
}

// pinsAt are the pins that have a file at ref — the branch the bump works
// on, so the plan says what will happen there rather than in the checkout.
// A ref that cannot be listed is an error, not a project with nothing to pin.
func pinsAt(r gitx.Repo, ref string, pins []config.Pin) ([]config.Pin, error) {
	paths, err := r.FilesAt(ref)
	if err != nil {
		return nil, fmt.Errorf("files on %s: %w", ref, err)
	}

	var present []config.Pin

	for _, pin := range pins {
		if len(pin.Files(paths)) > 0 {
			present = append(present, pin)
		}
	}

	return present, nil
}

// pinPlanLines is one line per pin for the plan: the file, the pattern and
// the version that goes into it.
func pinPlanLines(pins []config.Pin, vars map[string]string) []string {
	lines := make([]string, 0, len(pins))

	for _, pin := range pins {
		lines = append(lines, fmt.Sprintf("pin %s: %s -> %s", pin.File, pin.Match, expand(pin.Value, vars)))
	}

	return lines
}
