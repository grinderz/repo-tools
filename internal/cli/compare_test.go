package cli

import (
	"strings"
	"testing"

	"github.com/grinderz/repo-tools/internal/gitx"
)

// compareFixture builds the three fates a commit can have: one still to pick,
// one carried over with -x, one applied to both sides independently — plus the
// fixture's own freeze commit that only the release branch has.
func compareFixture(t *testing.T) (fixture, gitx.Repo, map[string]string) {
	t.Helper()

	f := newFixture(t)
	r := gitx.Repo{Dir: f.parent}
	shas := map[string]string{}

	git(t, f.parent, "checkout", "--quiet", "develop")
	writeFile(t, f.parent, "a.txt", "a\n")
	shas["devonly"] = commit(t, f.parent, "parent: AB-1 dev only")
	writeFile(t, f.parent, "b.txt", "b\n")
	shas["picked"] = commit(t, f.parent, "parent: AB-2 to pick")
	writeFile(t, f.parent, "eq.txt", "same\n")
	shas["equivalent"] = commit(t, f.parent, "parent: AB-3 same fix")

	git(t, f.parent, "checkout", "--quiet", "release-1.0")

	if err := pickOne(testCtx(), r, f.project(t), "release-1.0", shas["picked"]); err != nil {
		t.Fatal(err)
	}

	shas["copy"] = git(t, f.parent, "rev-parse", "HEAD")

	writeFile(t, f.parent, "eq.txt", "same\n")
	commit(t, f.parent, "parent: AB-3 applied directly")

	git(t, f.parent, "update-ref", "refs/remotes/origin/develop", "develop")
	git(t, f.parent, "update-ref", "refs/remotes/origin/release-1.0", "release-1.0")

	return f, r, shas
}

func TestCompareBranchesClassifiesBothSides(t *testing.T) {
	t.Parallel()

	f, r, shas := compareFixture(t)
	p := f.project(t)

	cmp, err := compareBranches(r, p, "release-1.0", mustFilter(t, nil, nil))
	if err != nil {
		t.Fatal(err)
	}

	if len(cmp.DevOnly) != 1 || cmp.DevOnly[0].SHA != shas["devonly"] {
		t.Errorf("DevOnly = %v, want only %s", cmp.DevOnly, shas["devonly"])
	}

	if len(cmp.Both) != 2 {
		t.Fatalf("Both = %v, want the picked and the equivalent commit", cmp.Both)
	}

	if cmp.Both[0].SHA != shas["picked"] || !strings.HasPrefix(cmp.Both[0].How, "picked as ") {
		t.Errorf("picked commit reported as %v", cmp.Both[0])
	}

	if !strings.Contains(cmp.Both[0].How, shorten(shas["copy"], shortSHALen)) {
		t.Errorf("How = %q, want the release copy %s named", cmp.Both[0].How, shas["copy"])
	}

	if cmp.Both[1].SHA != shas["equivalent"] || !strings.Contains(cmp.Both[1].How, "equivalent patch") {
		t.Errorf("equivalent commit reported as %v", cmp.Both[1])
	}

	// The release side keeps only its own work: the fixture's freeze commit.
	// The -x copy and the independently applied fix are shown from the dev side.
	if len(cmp.RelOnly) != 1 || !strings.Contains(cmp.RelOnly[0].Subject, "freeze deps") {
		t.Errorf("RelOnly = %v, want only the freeze commit", cmp.RelOnly)
	}
}

// The filter narrows every group, so one ticket's fate is one screen.
func TestCompareBranchesAppliesTheFilter(t *testing.T) {
	t.Parallel()

	f, r, shas := compareFixture(t)
	p := f.project(t)

	cmp, err := compareBranches(r, p, "release-1.0", mustFilter(t, []string{"AB-1"}, nil))
	if err != nil {
		t.Fatal(err)
	}

	if len(cmp.DevOnly) != 1 || cmp.DevOnly[0].SHA != shas["devonly"] {
		t.Errorf("DevOnly = %v", cmp.DevOnly)
	}

	if len(cmp.Both) != 0 || len(cmp.RelOnly) != 0 {
		t.Errorf("other groups must be empty: both=%v rel=%v", cmp.Both, cmp.RelOnly)
	}
}

// A release branch the config names before anyone created it is a state to
// report, not a git failure.
func TestCompareBranchesReportsAPendingReleaseBranch(t *testing.T) {
	t.Parallel()

	f, r, _ := compareFixture(t)
	p := f.project(t)

	_, err := compareBranches(r, p, "release-9.9", mustFilter(t, nil, nil))
	if err == nil || !strings.Contains(err.Error(), "does not exist yet") {
		t.Errorf("err = %v", err)
	}
}
