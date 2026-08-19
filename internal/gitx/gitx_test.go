package gitx

import "testing"

func TestParseReleaseBranch(t *testing.T) {
	cases := []struct {
		name, prefix string
		want         string
		ok           bool
	}{
		{"release-1.26", "release-", "1.26", true},
		{"release-10.0", "release-", "10.0", true},
		{"develop", "release-", "", false},
		{"release-1.26.1", "release-", "", false},
		{"release-x.y", "release-", "", false},
		{"rel/1.2", "rel/", "1.2", true},
	}
	for _, c := range cases {
		v, ok := ParseReleaseBranch(c.name, c.prefix)
		if ok != c.ok {
			t.Errorf("%s: ok = %v, want %v", c.name, ok, c.ok)
			continue
		}
		if ok && v.String() != c.want {
			t.Errorf("%s: got %s, want %s", c.name, v, c.want)
		}
	}
}

func TestLatestReleaseBranch(t *testing.T) {
	branches := []string{"develop", "release-1.9", "release-1.26", "release-2.0", "release-1.25", "master"}
	name, v, ok := LatestReleaseBranch(branches, "release-")
	if !ok || name != "release-2.0" || v.Major != 2 || v.Minor != 0 {
		t.Fatalf("got %q %v %v", name, v, ok)
	}
	if _, _, ok := LatestReleaseBranch([]string{"develop"}, "release-"); ok {
		t.Fatal("expected no release branch")
	}
}

func TestParseTag(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"1.26.0", "1.26.0", true},
		{"v1.0.0", "1.0.0", true},
		{"6.7.0-rc.2", "6.7.0-rc.2", true},
		{"1.26.0-rc.10", "1.26.0-rc.10", true},
		{"nightly", "", false},
		{"1.26", "", false},
		{"1.26.0-beta.1", "", false},
	}
	for _, c := range cases {
		v, ok := ParseTag(c.in)
		if ok != c.ok {
			t.Errorf("%s: ok = %v, want %v", c.in, ok, c.ok)
			continue
		}
		if ok && v.String() != c.want {
			t.Errorf("%s: got %s, want %s", c.in, v, c.want)
		}
	}
}

func TestNextRc(t *testing.T) {
	rel := ReleaseVer{1, 26}
	cases := []struct {
		name string
		tags []string
		want string
	}{
		{"no tags at all", nil, "1.26.0-rc.0"},
		{"only other releases", []string{"1.25.7", "1.25.0-rc.1"}, "1.26.0-rc.0"},
		{"rc series in progress", []string{"1.26.0-rc.0", "1.26.0-rc.1"}, "1.26.0-rc.2"},
		{"after a final release", []string{"1.26.0-rc.0", "1.26.0"}, "1.26.1-rc.0"},
		{"after several finals", []string{"1.26.0", "1.26.1", "1.26.2"}, "1.26.3-rc.0"},
		{"rc after final of previous patch", []string{"1.26.0", "1.26.1-rc.0"}, "1.26.1-rc.1"},
	}
	for _, c := range cases {
		if got := NextRc(c.tags, rel).String(); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestNextFinal(t *testing.T) {
	rel := ReleaseVer{1, 26}
	cases := []struct {
		name string
		tags []string
		want string
	}{
		{"only rc tags", []string{"1.26.0-rc.0", "1.26.0-rc.1"}, "1.26.0"},
		{"after one final", []string{"1.26.0"}, "1.26.1"},
		{"ignores other minors", []string{"1.25.9", "1.26.0"}, "1.26.1"},
	}
	for _, c := range cases {
		if got := NextFinal(c.tags, rel).String(); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

// The first tag on a freshly cut branch has no tag of its own line to count
// from, and the branch carries nothing the dev branch does not — so the message
// counts from the previous release line, which is what the release contains.
func TestPreviousTag(t *testing.T) {
	tags := []string{"0.1.0", "0.1.1-rc.0", "0.1.1", "0.2.0-rc.0", "1.0.0", "not-a-tag"}

	got, ok := PreviousTag(tags, ReleaseVer{Major: 0, Minor: 2})
	if !ok || got.String() != "0.1.1" {
		t.Errorf("got %v (%v), want 0.1.1", got, ok)
	}

	// Candidates of the same line are earlier than its release.
	got, ok = PreviousTag([]string{"0.1.0-rc.0", "0.1.0-rc.1"}, ReleaseVer{Major: 0, Minor: 2})
	if !ok || got.String() != "0.1.0-rc.1" {
		t.Errorf("got %v (%v), want 0.1.0-rc.1", got, ok)
	}

	// Nothing before the first line ever released.
	if _, ok := PreviousTag(tags, ReleaseVer{Major: 0, Minor: 1}); ok {
		t.Error("0.1 is the first line, nothing precedes it")
	}
}
