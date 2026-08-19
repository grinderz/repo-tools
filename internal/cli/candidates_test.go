package cli

import (
	"strings"
	"testing"
)

const (
	feat  = "feat/AB-3151: add the thing"
	fix   = "fix/AB-3595: stop the crash"
	build = "build/AB-1335: bump the image"
	other = "refactor/AB-9000: unrelated"
)

func mustFilter(t *testing.T, tasks, greps []string) pickFilter {
	t.Helper()

	f, err := newPickFilter(tasks, greps, "", "")
	if err != nil {
		t.Fatal(err)
	}

	return f
}

func TestPickFilterEmptyKeepsEverything(t *testing.T) {
	f := mustFilter(t, nil, nil)

	if !f.empty() {
		t.Error("a filter without terms should be empty")
	}

	for _, subject := range []string{feat, fix, other} {
		if !f.matches(subject) {
			t.Errorf("empty filter rejected %q", subject)
		}
	}
}

// The motivating case: three tickets, each with a different commit type.
func TestPickFilterByTasks(t *testing.T) {
	f := mustFilter(t, []string{"AB-3151", "AB-3595", "AB-1335"}, nil)

	for _, subject := range []string{feat, fix, build} {
		if !f.matches(subject) {
			t.Errorf("task filter missed %q", subject)
		}
	}

	if f.matches(other) {
		t.Errorf("task filter accepted %q", other)
	}
}

func TestPickFilterTaskIsCaseInsensitive(t *testing.T) {
	if !mustFilter(t, []string{"ab-3151"}, nil).matches(feat) {
		t.Error("ticket ids should match regardless of case")
	}
}

// A ticket id must not match a longer one that merely starts with it.
func TestPickFilterTaskDoesNotMatchLongerId(t *testing.T) {
	if mustFilter(t, []string{"AB-315"}, nil).matches(feat) {
		t.Errorf("AB-315 must not select %q", feat)
	}

	if !mustFilter(t, []string{"AB-3151"}, nil).matches(feat) {
		t.Errorf("AB-3151 must select %q", feat)
	}
}

func TestPickFilterByGrep(t *testing.T) {
	f := mustFilter(t, nil, []string{`^fix/`})

	if !f.matches(fix) {
		t.Errorf("grep missed %q", fix)
	}

	if f.matches(feat) {
		t.Errorf("grep accepted %q", feat)
	}
}

func TestPickFilterCombinesTermsWithOr(t *testing.T) {
	f := mustFilter(t, []string{"AB-1335"}, []string{`^fix/`})

	for _, subject := range []string{build, fix} {
		if !f.matches(subject) {
			t.Errorf("combined filter missed %q", subject)
		}
	}

	if f.matches(feat) {
		t.Errorf("combined filter accepted %q", feat)
	}
}

func TestPickFilterRejectsBadRegexp(t *testing.T) {
	_, err := newPickFilter(nil, []string{"("}, "", "")
	if err == nil {
		t.Fatal("expected an error for an invalid regexp")
	}

	if !strings.Contains(err.Error(), "--grep") {
		t.Errorf("error should name the flag: %v", err)
	}
}

func TestPickFilterDescribe(t *testing.T) {
	got := mustFilter(t, []string{"AB-1"}, []string{`^fix/`}).describe()
	if got != "AB-1, ^fix/" {
		t.Errorf("describe() = %q", got)
	}

	dated, err := newPickFilter(nil, nil, "2026-08-01", "2026-08-31")
	if err != nil {
		t.Fatal(err)
	}

	if got := dated.describe(); got != "since 2026-08-01, until 2026-08-31" {
		t.Errorf("describe() with dates = %q", got)
	}
}

// Dates scope the list; only subject terms decide what gets picked.
func TestPickFilterDatesScopeButDoNotSelect(t *testing.T) {
	dated, err := newPickFilter(nil, nil, "2026-08-01", "")
	if err != nil {
		t.Fatal(err)
	}

	if dated.selects() {
		t.Error("a date must not count as a selection")
	}

	if dated.empty() {
		t.Error("a date does narrow the list, so the filter is not empty")
	}

	// With no subject terms every subject stays; git applies the window.
	if !dated.matches(other) {
		t.Error("date-only filters must not reject subjects")
	}

	if !mustFilter(t, []string{"AB-1"}, nil).selects() {
		t.Error("a task filter is a selection")
	}
}
