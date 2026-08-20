// Package gitx wraps the git CLI and version/branch parsing helpers.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	// dirPerm is the mode new parent directories are created with.
	dirPerm = 0o755
	// keyValueFields is the field count of a "key value" config line.
	keyValueFields = 2
	// finalRC marks a tag without an -rc suffix.
	finalRC = -1
	// finalRCSort sorts a final release after every rc of the same patch.
	finalRCSort = 1 << 30
)

type Repo struct {
	Dir string
}

// Git runs git with args in the repo dir, returns trimmed combined output.
func (r Repo) Git(args ...string) (string, error) {
	return r.run(nil, args...)
}

// GitEnv runs git with extra environment entries ("KEY=value").
func (r Repo) GitEnv(env []string, args ...string) (string, error) {
	return r.run(env, args...)
}

// Lines runs git and splits non-empty output lines.
func (r Repo) Lines(args ...string) ([]string, error) {
	out, err := r.Git(args...)
	if err != nil {
		return nil, err
	}

	if out == "" {
		return nil, nil
	}

	return strings.Split(out, "\n"), nil
}

// Output runs an arbitrary program in the repo dir and returns its trimmed
// combined output — for the tools that read a repository's remote state the
// way git cannot, like glab and gh.
func (r Repo) Output(program string, args ...string) (string, error) {
	cmd := exec.CommandContext(context.Background(), program, args...)
	cmd.Dir = r.Dir

	var buf bytes.Buffer

	cmd.Stdout = &buf
	cmd.Stderr = &buf

	err := cmd.Run()

	out := strings.TrimSpace(buf.String())
	if err != nil {
		return out, fmt.Errorf("%s %s: %w\n%s", program, strings.Join(args, " "), err, out)
	}

	return out, nil
}

// Sh runs an arbitrary shell command in the repo dir, streaming output.
func (r Repo) Sh(command string) error {
	return r.ShEnv(command, nil)
}

// ShEnv runs a shell command in the repo dir with extra environment entries
// ("KEY=value"), streaming output.
func (r Repo) ShEnv(command string, env []string) error {
	return r.shell(nil, command, env)
}

// ShDirenv runs the command through "direnv exec", so a repository that keeps
// its GOPROXY, GOPRIVATE or index URLs in .envrc is built the way its own
// shell would build it.
func (r Repo) ShDirenv(command string, env []string) error {
	return r.shell([]string{"direnv", "exec", r.Dir}, command, env)
}

func (r Repo) Exists() bool {
	fi, err := os.Stat(r.Dir)

	return err == nil && fi.IsDir()
}

func (r Repo) IsRepo() bool {
	_, err := r.Git("rev-parse", "--git-dir")

	return err == nil
}

// IsRepoRoot reports whether Dir is the top of a working tree of its own, not
// merely somewhere inside one. An uninitialised submodule is an empty
// directory inside its parent, where rev-parse happily walks up and answers
// about the parent instead.
func (r Repo) IsRepoRoot() bool {
	top, err := r.Git("rev-parse", "--show-toplevel")
	if err != nil {
		return false
	}

	here, err := os.Stat(r.Dir)
	if err != nil {
		return false
	}

	there, err := os.Stat(top)
	if err != nil {
		return false
	}

	return os.SameFile(here, there)
}

// HasCommits reports whether HEAD resolves, which it does not in a repository
// that has never been committed to.
func (r Repo) HasCommits() bool {
	_, err := r.Git("rev-parse", "--verify", "--quiet", "HEAD")

	return err == nil
}

func (r Repo) CurrentBranch() (string, error) {
	return r.Git("rev-parse", "--abbrev-ref", "HEAD")
}

func (r Repo) IsClean() (bool, error) {
	out, err := r.Git("status", "--porcelain")
	if err != nil {
		return false, err
	}

	return out == "", nil
}

// AheadBehind returns (ahead, behind) of HEAD relative to ref.
func (r Repo) AheadBehind(ref string) (int, int, error) {
	out, err := r.Git("rev-list", "--left-right", "--count", ref+"...HEAD")
	if err != nil {
		return 0, 0, err
	}

	fields := strings.Fields(out)
	if len(fields) != keyValueFields {
		return 0, 0, fmt.Errorf("unexpected rev-list output: %q", out)
	}

	behind, _ := strconv.Atoi(fields[0])
	ahead, _ := strconv.Atoi(fields[1])

	return ahead, behind, nil
}

func (r Repo) RemoteBranchExists(branch string) bool {
	_, err := r.Git("rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch)

	return err == nil
}

func (r Repo) LocalBranchExists(branch string) bool {
	_, err := r.Git("rev-parse", "--verify", "--quiet", "refs/heads/"+branch)

	return err == nil
}

// RemoteBranches lists the branches of origin, without the refs/remotes/origin/
// prefix. Full ref names are read rather than short ones: origin/HEAD shortens
// to plain "origin", and another remote's branches would pass a prefix trim
// untouched and look like origin's own.
func (r Repo) RemoteBranches() ([]string, error) {
	const originRefs = "refs/remotes/origin/"

	lines, err := r.Lines("for-each-ref", "--format=%(refname)", originRefs)
	if err != nil {
		return nil, err
	}

	out := make([]string, 0, len(lines))

	for _, ref := range lines {
		name := strings.TrimPrefix(ref, originRefs)
		if name == ref || name == "HEAD" {
			continue
		}

		out = append(out, name)
	}

	return out, nil
}

func (r Repo) Tags() ([]string, error) {
	return r.Lines("tag", "--list")
}

// SubmodulePaths returns paths declared in the working tree's .gitmodules
// (empty if none).
func (r Repo) SubmodulePaths() ([]string, error) { return r.SubmodulePathsAt("") }

// SubmodulePathsAt returns the paths declared in .gitmodules as it exists on
// ref. An empty ref reads the working tree.
func (r Repo) SubmodulePathsAt(ref string) ([]string, error) {
	byName, err := r.gitmodulesAt(ref, "path")
	if err != nil {
		return nil, err
	}

	paths := make([]string, 0, len(byName))
	for _, path := range byName {
		paths = append(paths, path)
	}

	sort.Strings(paths)

	return paths, nil
}

// FileAt reads a file from a ref, or from the working tree when ref is empty.
// The second result is false when the file simply is not there, which for a
// branch that never had it is a state to report, not a failure.
func (r Repo) FileAt(ref, path string) (string, bool, error) {
	if ref == "" {
		content, err := os.ReadFile(filepath.Join(r.Dir, path))
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}

		if err != nil {
			return "", false, fmt.Errorf("read %s: %w", path, err)
		}

		return string(content), true, nil
	}

	content, err := r.Git("show", ref+":"+path)
	if err != nil {
		return "", false, nil //nolint:nilerr // absence is not an error here
	}

	return content, true, nil
}

// SubmoduleNameByPath resolves the .gitmodules section name for a path.
func (r Repo) SubmoduleNameByPath(path string) (string, error) {
	return r.submoduleNameByPathAt("", path)
}

// SubmoduleBranch returns the branch a submodule tracks, empty when unset.
func (r Repo) SubmoduleBranch(path string) (string, error) {
	return r.SubmoduleBranchAt("", path)
}

// SubmoduleBranchAt returns the branch a submodule tracks according to
// .gitmodules as it exists on ref. An empty ref reads the working tree.
func (r Repo) SubmoduleBranchAt(ref, path string) (string, error) {
	name, err := r.submoduleNameByPathAt(ref, path)
	if err != nil {
		return "", err
	}

	byName, err := r.gitmodulesAt(ref, "branch")
	if err != nil {
		return "", err
	}

	return byName[name], nil
}

func (r Repo) SubmoduleURL(path string) (string, error) { return r.SubmoduleURLAt("", path) }

// SubmoduleURLAt returns a submodule's url according to .gitmodules as it
// exists on ref. An empty ref reads the working tree.
func (r Repo) SubmoduleURLAt(ref, path string) (string, error) {
	name, err := r.submoduleNameByPathAt(ref, path)
	if err != nil {
		return "", err
	}

	byName, err := r.gitmodulesAt(ref, "url")
	if err != nil {
		return "", err
	}

	url, ok := byName[name]
	if !ok {
		return "", fmt.Errorf("submodule %q has no url in .gitmodules", path)
	}

	return url, nil
}

func (r Repo) submoduleNameByPathAt(ref, path string) (string, error) {
	byName, err := r.gitmodulesAt(ref, "path")
	if err != nil {
		return "", err
	}

	for name, p := range byName {
		if p == path {
			return name, nil
		}
	}

	return "", fmt.Errorf("submodule with path %q not found in .gitmodules", path)
}

func (r Repo) shell(prefix []string, command string, env []string) error {
	argv := append(append([]string{}, prefix...), "sh", "-c", command)

	cmd := exec.CommandContext(context.Background(), argv[0], argv[1:]...) //nolint:gosec // the config says what to run
	cmd.Dir = r.Dir
	cmd.Stdout, cmd.Stderr = childStdout(), childStderr()

	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("command %q: %w", command, err)
	}

	return nil
}

// run executes git in the repo dir and returns its trimmed combined output.
func (r Repo) run(env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = r.Dir

	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}

	var buf bytes.Buffer

	cmd.Stdout = &buf
	cmd.Stderr = &buf

	err := cmd.Run()

	out := strings.TrimSpace(buf.String())
	if err != nil {
		return out, fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, out)
	}

	return out, nil
}

// gitmodulesAt reads one attribute of every submodule declared in .gitmodules,
// keyed by submodule name. An empty ref reads the working tree; any other ref
// reads the file as it is committed there, which is what a command that is
// about to check out another branch has to go by. An absent .gitmodules yields
// an empty map.
func (r Repo) gitmodulesAt(ref, attr string) (map[string]string, error) {
	file, cleanup, err := r.gitmodulesFile(ref)
	if err != nil || file == "" {
		return map[string]string{}, err
	}

	defer cleanup()

	// git config exits 1 when the pattern matches nothing, which is not an
	// error here: a submodule simply may not set the attribute.
	out, _ := r.Git("config", "-f", file, "--get-regexp", `^submodule\..*\.`+attr+`$`)

	values := map[string]string{}
	if out == "" {
		return values, nil
	}

	for line := range strings.SplitSeq(out, "\n") {
		key, value, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}

		name := strings.TrimSuffix(strings.TrimPrefix(key, "submodule."), "."+attr)
		values[name] = value
	}

	return values, nil
}

// gitmodulesFile hands back a path git config can read, plus the cleanup for
// it. An empty file path means the ref simply has no .gitmodules.
func (r Repo) gitmodulesFile(ref string) (string, func(), error) {
	noop := func() {}

	if ref == "" {
		path := filepath.Join(r.Dir, ".gitmodules")
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return "", noop, nil
			}

			return "", noop, fmt.Errorf("stat .gitmodules: %w", err)
		}

		return ".gitmodules", noop, nil
	}

	// A committed .gitmodules is not on disk, and git config only reads files,
	// so the blob is spilled into a temporary one.
	// A ref without a .gitmodules is the ordinary case of a branch that has no
	// submodules, not a failure.
	content, err := r.Git("show", ref+":.gitmodules")
	if err != nil {
		return "", noop, nil //nolint:nilerr // absence is not an error here
	}

	tmp, err := os.CreateTemp("", "gitmodules-*")
	if err != nil {
		return "", noop, fmt.Errorf("temp .gitmodules: %w", err)
	}

	cleanup := func() { _ = os.Remove(tmp.Name()) }

	if _, err := tmp.WriteString(content + "\n"); err != nil {
		cleanup()

		return "", noop, fmt.Errorf("write temp .gitmodules: %w", err)
	}

	if err := tmp.Close(); err != nil {
		cleanup()

		return "", noop, fmt.Errorf("close temp .gitmodules: %w", err)
	}

	return tmp.Name(), cleanup, nil
}

// Clone clones url into dir, pulling submodules in as well.
func Clone(url, dir, branch string) error {
	if err := os.MkdirAll(filepath.Dir(dir), dirPerm); err != nil {
		return fmt.Errorf("create parent of %s: %w", dir, err)
	}

	args := []string{"clone", "--recurse-submodules"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}

	args = append(args, url, dir)

	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git clone %s: %w", url, err)
	}

	return nil
}

// LsRemoteBranches lists branch names of a remote URL (network).
func LsRemoteBranches(url string) ([]string, error) {
	cmd := exec.CommandContext(context.Background(), "git", "ls-remote", "--heads", url)

	var buf bytes.Buffer

	cmd.Stdout = &buf
	cmd.Stderr = &buf

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git ls-remote %s: %w\n%s", url, err, strings.TrimSpace(buf.String()))
	}

	var out []string

	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		fields := strings.Fields(line)
		if len(fields) == keyValueFields {
			out = append(out, strings.TrimPrefix(fields[1], "refs/heads/"))
		}
	}

	return out, nil
}

// ---- release branch / tag version parsing ----

type ReleaseVer struct {
	Major, Minor int
}

func (v ReleaseVer) String() string { return fmt.Sprintf("%d.%d", v.Major, v.Minor) }

func (v ReleaseVer) Less(o ReleaseVer) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}

	return v.Minor < o.Minor
}

// ParseReleaseBranch parses "<prefix>X.Y".
func ParseReleaseBranch(name, prefix string) (ReleaseVer, bool) {
	if !strings.HasPrefix(name, prefix) {
		return ReleaseVer{}, false
	}

	parts := strings.Split(strings.TrimPrefix(name, prefix), ".")
	if len(parts) != keyValueFields {
		return ReleaseVer{}, false
	}

	major, errMajor := strconv.Atoi(parts[0])
	minor, errMinor := strconv.Atoi(parts[1])

	if errMajor != nil || errMinor != nil {
		return ReleaseVer{}, false
	}

	return ReleaseVer{major, minor}, true
}

// LatestReleaseBranch returns the highest <prefix>X.Y among names.
func LatestReleaseBranch(names []string, prefix string) (string, ReleaseVer, bool) {
	var best ReleaseVer

	found := false

	for _, n := range names {
		v, ok := ParseReleaseBranch(n, prefix)
		if !ok {
			continue
		}

		if !found || best.Less(v) {
			best, found = v, true
		}
	}

	if !found {
		return "", ReleaseVer{}, false
	}

	return prefix + best.String(), best, true
}

type TagVer struct {
	Major, Minor, Patch int
	RC                  int // finalRC = final release
}

func (t TagVer) Base() string { return fmt.Sprintf("%d.%d.%d", t.Major, t.Minor, t.Patch) }

func (t TagVer) String() string {
	if t.RC < 0 {
		return t.Base()
	}

	return fmt.Sprintf("%s-rc.%d", t.Base(), t.RC)
}

var tagRe = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:-rc\.(\d+))?$`)

func ParseTag(s string) (TagVer, bool) {
	m := tagRe.FindStringSubmatch(s)
	if m == nil {
		return TagVer{}, false
	}

	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	patch, _ := strconv.Atoi(m[3])

	rc := finalRC
	if m[4] != "" {
		rc, _ = strconv.Atoi(m[4])
	}

	return TagVer{major, minor, patch, rc}, true
}

// TagsFor returns parsed tags matching release X.Y, sorted ascending.
func TagsFor(tags []string, rel ReleaseVer) []TagVer {
	out := make([]TagVer, 0, len(tags))

	for _, t := range tags {
		v, ok := ParseTag(t)
		if !ok || v.Major != rel.Major || v.Minor != rel.Minor {
			continue
		}

		out = append(out, v)
	}

	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Patch != b.Patch {
			return a.Patch < b.Patch
		}

		return rcSortKey(a) < rcSortKey(b)
	})

	return out
}

// LatestTag is the highest version tag of the repository as a whole — what a
// summary reports when its branch is not a release line.
func LatestTag(tags []string) (TagVer, bool) {
	var best TagVer

	found := false

	for _, t := range tags {
		v, ok := ParseTag(t)
		if !ok {
			continue
		}

		if !found || lessTag(best, v) {
			best, found = v, true
		}
	}

	return best, found
}

// PreviousTag is the highest tag of a release line before rel — what the first
// tag on a freshly cut branch has to count from, since that branch carries no
// tag of its own yet and nothing beyond the dev branch either.
func PreviousTag(tags []string, rel ReleaseVer) (TagVer, bool) {
	var best TagVer

	found := false

	for _, t := range tags {
		v, ok := ParseTag(t)
		if !ok {
			continue
		}

		line := ReleaseVer{Major: v.Major, Minor: v.Minor}
		if !line.Less(rel) {
			continue
		}

		if !found || lessTag(best, v) {
			best, found = v, true
		}
	}

	return best, found
}

// lessTag orders two tags by version, release before its own candidates.
func lessTag(one, other TagVer) bool {
	if one.Major != other.Major {
		return one.Major < other.Major
	}

	if one.Minor != other.Minor {
		return one.Minor < other.Minor
	}

	if one.Patch != other.Patch {
		return one.Patch < other.Patch
	}

	return rcSortKey(one) < rcSortKey(other)
}

func rcSortKey(t TagVer) int {
	if t.RC < 0 {
		return finalRCSort
	}

	return t.RC
}

// nextPatch is one past the highest final release of rel, 0 when there is none.
func nextPatch(parsed []TagVer) int {
	patch := 0

	for _, t := range parsed {
		if t.RC < 0 && t.Patch >= patch {
			patch = t.Patch + 1
		}
	}

	return patch
}

// NextRc computes the next rc tag on release branch rel: the patch level is one
// past the highest final release, the rc counter one past the highest rc there.
func NextRc(tags []string, rel ReleaseVer) TagVer {
	parsed := TagsFor(tags, rel)
	patch := nextPatch(parsed)

	next := 0

	for _, t := range parsed {
		if t.Patch == patch && t.RC >= next {
			next = t.RC + 1
		}
	}

	return TagVer{rel.Major, rel.Minor, patch, next}
}

// NextFinal computes the next final tag: one patch past the highest release.
func NextFinal(tags []string, rel ReleaseVer) TagVer {
	return TagVer{rel.Major, rel.Minor, nextPatch(TagsFor(tags, rel)), finalRC}
}
