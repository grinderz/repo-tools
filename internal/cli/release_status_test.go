package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/grinderz/repo-tools/internal/gitx"
)

// The line's rc and final columns are independent: a final does not hide the
// rc that tested it, and tags of other lines stay out.
func TestReleaseLineTags(t *testing.T) {
	f := newFixture(t)
	r := gitx.Repo{Dir: f.parent}

	for _, tag := range []string{"1.0.0-rc.1", "1.0.0-rc.2", "1.0.0", "2.0.0"} {
		git(t, f.parent, "tag", tag)
	}

	rcTag, finalTag := releaseLineTags(r, gitx.ReleaseVer{Major: 1, Minor: 0})
	if rcTag != "1.0.0-rc.2" || finalTag != "1.0.0" {
		t.Errorf("rc = %q, final = %q", rcTag, finalTag)
	}

	rcTag, finalTag = releaseLineTags(r, gitx.ReleaseVer{Major: 3, Minor: 0})
	if rcTag != "-" || finalTag != "-" {
		t.Errorf("an untagged line must show dashes: %q %q", rcTag, finalTag)
	}
}

// head says "tagged" only while the branch head is what the last tag marks.
func TestHeadStatus(t *testing.T) {
	f := newFixture(t)
	r := gitx.Repo{Dir: f.parent}

	git(t, f.parent, "checkout", "--quiet", "release-1.0")
	git(t, f.parent, "update-ref", "refs/remotes/origin/release-1.0", "release-1.0")

	ver := gitx.ReleaseVer{Major: 1, Minor: 0}

	if head, _ := headStatus(r, ver, "release-1.0"); head != "untagged" {
		t.Errorf("no tags yet: head = %q", head)
	}

	git(t, f.parent, "tag", "1.0.0-rc.1")

	if head, _ := headStatus(r, ver, "release-1.0"); head != "tagged" {
		t.Errorf("head just tagged: head = %q", head)
	}

	writeFile(t, f.parent, "hot.txt", "fix\n")
	commit(t, f.parent, "parent: hotfix after rc")
	git(t, f.parent, "update-ref", "refs/remotes/origin/release-1.0", "release-1.0")

	if head, _ := headStatus(r, ver, "release-1.0"); head != "+1 commits" {
		t.Errorf("one commit past the rc: head = %q", head)
	}
}

// The compare column carries git compare's counters and turns yellow only
// while dev commits are still waiting to be picked.
func TestCompareCell(t *testing.T) {
	f, r, _ := compareFixture(t)
	p := f.project(t)

	text, _ := compareCell(r, p, "release-1.0")
	if text != "1/1/2" {
		t.Errorf("cell = %q, want 1 to pick, 1 release-only, 2 shared", text)
	}

	if text, _ := compareCell(r, p, "release-9.9"); text != "-" {
		t.Errorf("a pending branch has nothing to count: %q", text)
	}
}

// The pins column folds every pin's drift into one cell and repeats the
// drifted lines under the row.
func TestPinsCell(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	r := gitx.Repo{Dir: f.parent}

	git(t, f.parent, "checkout", "--quiet", "release-1.0")
	git(t, f.parent, "submodule", "update", "--quiet", "--", "sub")
	git(t, f.parent, "update-ref", "refs/remotes/origin/release-1.0", "release-1.0")

	text, _, drifted := pinsCell(testCtx(), p, r, "release-1.0", map[string]bool{})
	if text != "ok" || drifted != nil {
		t.Errorf("pin at head: %q %v", text, drifted)
	}

	// The submodule's branch moves on; the pin is now one commit back.
	git(t, f.sub, "checkout", "--quiet", "release-1.0")
	writeFile(t, f.sub, "lib.txt", "v1-hotfix-2\n")
	commit(t, f.sub, "sub: second hotfix")
	git(t, filepath.Join(f.parent, "sub"), "fetch", "--quiet", "origin")

	text, _, drifted = pinsCell(testCtx(), p, r, "release-1.0", map[string]bool{})
	if text != "drift(1)" || len(drifted) != 1 || !strings.Contains(drifted[0], "behind 1 commit(s)") {
		t.Errorf("pin behind: %q %v", text, drifted)
	}
}

// Freeze has nothing to say for a project that does not freeze.
func TestFreezeStatusDisabled(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	r := gitx.Repo{Dir: f.parent}

	text, _, stale := freezeStatus(testCtx(), r, p, "release-1.0")
	if text != "-" || stale != nil {
		t.Errorf("freeze = %q, stale = %v", text, stale)
	}
}

// --ref accepts a branch on origin or a tag; anything else is named plainly.
func TestCITarget(t *testing.T) {
	f := newFixture(t)
	p := f.project(t)
	r := gitx.Repo{Dir: f.parent}

	git(t, f.parent, "update-ref", "refs/remotes/origin/develop", "develop")
	git(t, f.parent, "tag", "1.0.0-rc.1", "release-1.0")

	sha, ref, err := ciTarget(r, p, "")
	if err != nil || ref != "develop" || sha != git(t, f.parent, "rev-parse", "origin/develop") {
		t.Errorf("default target: sha=%s ref=%q err=%v", sha, ref, err)
	}

	sha, ref, err = ciTarget(r, p, "1.0.0-rc.1")
	if err != nil || ref != "1.0.0-rc.1" || sha != git(t, f.parent, "rev-parse", "release-1.0") {
		t.Errorf("tag target: sha=%s ref=%q err=%v", sha, ref, err)
	}

	if _, _, err = ciTarget(r, p, "nope"); err == nil || !strings.Contains(err.Error(), "neither a branch") {
		t.Errorf("unknown ref: err = %v", err)
	}
}

// One pipeline, one coloured word — the details stay off the table row.
func TestCISummary(t *testing.T) {
	word, detail := ciSummary(ciPipeline{State: ciRunning, Progress: "3/7 jobs", URL: "http://x"})
	if !strings.Contains(word, "running 3/7 jobs") || detail != "http://x" {
		t.Errorf("running: word=%q detail=%q", word, detail)
	}

	word, detail = ciSummary(ciPipeline{State: ciSuccess})
	if !strings.Contains(word, "success") || detail != "" {
		t.Errorf("success: word=%q detail=%q", word, detail)
	}

	if word, detail = ciSummary(ciPipeline{State: ciFailed, URL: "http://f"}); detail != "http://f" {
		t.Errorf("failed must carry its URL: word=%q detail=%q", word, detail)
	}
}

// {task} is the ticket id the branch name carries; a branch without one
// renders the placeholder empty rather than inventing something.
func TestTaskFromBranch(t *testing.T) {
	cases := map[string]string{
		"feat/AB-000":          "AB-000",
		"build/AB-00000":       "AB-00000",
		"fix/AB-9855-retry":    "AB-9855",
		"release-1.4":          "",
		"develop":              "",
		"feature/no-ticket-42": "",
	}
	for branch, want := range cases {
		if got := taskFromBranch(branch); got != want {
			t.Errorf("taskFromBranch(%q) = %q, want %q", branch, got, want)
		}
	}
}
