package cli

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
	"github.com/grinderz/repo-tools/internal/run"
)

func newCheckCmd(rctx *run.Ctx, name string) *cobra.Command {
	return &cobra.Command{
		Use:   name + " [project...]",
		Short: "Validate the config against the working copies",
		RunE: func(_ *cobra.Command, args []string) error {
			projects, err := rctx.Select(args)
			if err != nil {
				return err
			}

			problems := 0

			for _, p := range projects {
				issues := checkProject(rctx, p)

				report(p.Name, issues)

				if len(issues) > 0 {
					problems++
				}
			}

			// The hooks and the renovate run are the config's, not a
			// project's: checked once.
			if issues := checkHooks(rctx.Cfg); len(issues) > 0 {
				report("hooks", issues)

				problems++
			}

			if issues := checkRenovate(rctx, projects); len(issues) > 0 {
				report("renovate", issues)

				problems++
			}

			if problems > 0 {
				return fmt.Errorf("%d %w", problems, errWithIssues)
			}

			return nil
		},
	}
}

func checkProject(rctx *run.Ctx, p *config.Project) []string {
	r := repoOf(p)
	if reason := missingRepo(r); reason != "" {
		return []string{reason}
	}

	var issues []string

	// Validation is read-only and meant to be quick, so --fetch is opt-in here.
	if rctx.WantFetchFor(p, false) {
		if reason := fetchOrWarn(rctx, p, r); reason != "" {
			issues = append(issues, reason)
		}
	}

	issues = append(issues, checkRemote(r, p)...)
	issues = append(issues, checkReleaseBranch(r, p)...)
	issues = append(issues, checkWorktree(r)...)
	issues = append(issues, checkSubmodules(rctx, r, p)...)
	issues = append(issues, checkDeps(rctx, p)...)
	issues = append(issues, checkToolchain(rctx, p)...)
	issues = append(issues, checkRenovateConfig(rctx, r, p)...)
	issues = append(issues, checkCI(p)...)
	issues = append(issues, mrCheck(rctx, p)...)
	issues = append(issues, checkChangelog(rctx, r, p)...)

	return issues
}

// checkChangelog reports a changelog setup that would fail at generation time.
func checkChangelog(rctx *run.Ctx, r gitx.Repo, p *config.Project) []string {
	if !p.ChangelogEnabled() {
		return nil
	}

	var issues []string

	branch, err := changelogBranch(r, p, "")
	if err != nil {
		return []string{err.Error()}
	}

	cmds := changelogCmds(rctx, p, branch)
	if len(cmds) == 0 {
		issues = append(issues, "changelog is enabled but no cmds.changelog are set")
	}

	issues = append(issues, checkPrograms("changelog", cmds)...)

	// A git-cliff config usually lives in a tooling submodule, which is only
	// present once that submodule is checked out.
	cliffConfig := changelogEnv(rctx, p, branch)["GIT_CLIFF_CONFIG"]
	if cliffConfig == "" {
		return issues
	}

	if _, err := os.Stat(filepath.Join(r.Dir, cliffConfig)); err != nil {
		issues = append(issues, fmt.Sprintf(
			"GIT_CLIFF_CONFIG %s is missing, changelog update checks submodules out before generating",
			cliffConfig,
		))
	}

	return issues
}

func report(name string, issues []string) {
	// The padded ==> header keeps the verdict column aligned while the project
	// name gets the same marker every other per-project block has.
	header := run.Arrow() + " " + padCell(name, statusNameWidth, run.Bold)

	if len(issues) == 0 {
		fmt.Println(header + run.Green("ok"))

		return
	}

	fmt.Println(header + run.Yellow(fmt.Sprintf("%d issue(s)", len(issues))))

	for _, i := range issues {
		fmt.Printf("    - %s\n", i)
	}
}

func checkRemote(r gitx.Repo, p *config.Project) []string {
	var issues []string

	url, err := r.Git("remote", "get-url", "origin")

	switch {
	case err != nil:
		issues = append(issues, "no origin remote")
	case url != p.Git:
		issues = append(issues, fmt.Sprintf("origin is %s, config says %s", url, p.Git))
	}

	if !r.RemoteBranchExists(p.DevBranch) {
		issues = append(issues, fmt.Sprintf("origin/%s (dev_branch) does not exist", p.DevBranch))
	}

	// Renovate takes an internal module to the head of its repository's
	// default branch on the platform, which deps renovate --internal dev,
	// and --pkg naming a module, assume is the dev branch. The clone
	// records origin's default branch as origin/HEAD; a clone without one
	// is not told.
	if head, err := r.Git("symbolic-ref", "-q", "refs/remotes/origin/HEAD"); err == nil {
		if branch := strings.TrimPrefix(head, "refs/remotes/origin/"); branch != p.DevBranch {
			issues = append(issues, fmt.Sprintf(
				"origin's default branch is %s, dev_branch is %s: renovate's internal updates would follow %s",
				branch, p.DevBranch, branch))
		}
	}

	// "release" is resolved per project, and the release branch may not exist
	// yet; the pending-branch check above already covers that case.
	if cb := p.ChangelogBranch; cb != "" && cb != config.ChangelogBranchRelease && !r.RemoteBranchExists(cb) {
		issues = append(issues, fmt.Sprintf("origin/%s (changelog_branch) does not exist", cb))
	}

	return issues
}

func checkWorktree(r gitx.Repo) []string {
	var issues []string

	if clean, err := r.IsClean(); err == nil && !clean {
		issues = append(issues, "working tree is dirty")
	}

	branch, err := r.CurrentBranch()
	if err != nil {
		return append(issues, fmt.Sprintf("cannot read current branch: %v", err))
	}

	if !r.RemoteBranchExists(branch) {
		return issues
	}

	ahead, behind, err := r.AheadBehind("origin/" + branch)
	if err != nil {
		return issues
	}

	if ahead > 0 {
		issues = append(issues, fmt.Sprintf("%s is %d commit(s) ahead of origin", branch, ahead))
	}

	if behind > 0 {
		issues = append(issues, fmt.Sprintf("%s is %d commit(s) behind origin", branch, behind))
	}

	return issues
}

// checkReleaseBranch reports a release branch the config names but nobody has
// created yet. Every project gets one — release branch does not look at
// deps_freeze — and a library others freeze to needs its branch most of all, so
// this is reported for the whole config rather than only where a freeze runs.
func checkReleaseBranch(r gitx.Repo, p *config.Project) []string {
	_, pending := freezeRef(r, p)
	if pending == "" {
		return nil
	}

	return []string{pending}
}

func checkSubmodules(rctx *run.Ctx, r gitx.Repo, p *config.Project) []string {
	var issues []string

	// The freeze these settings describe runs on the release branch, so that is
	// the .gitmodules to judge them against — the checked-out branch may carry a
	// different set of submodules entirely.
	ref, pending := freezeRef(r, p)

	// A config that names a release branch nobody has created yet is the normal
	// state before release branch runs; there is simply nothing to compare the
	// submodule settings against, and checkReleaseBranch has already said so.
	if pending != "" {
		return nil
	}

	actual, err := r.SubmodulePathsAt(ref)
	if err != nil {
		return append(issues, fmt.Sprintf("cannot read .gitmodules: %v", err))
	}

	if ref != "" {
		issues = append(issues, prefixed(ref, checkSubmoduleSet(rctx, r, p, actual, ref))...)

		return issues
	}

	return checkSubmoduleSet(rctx, r, p, actual, ref)
}

// freezeRef is the revision deps freeze would work on. It returns an empty ref
// when there is no release branch at all — or no release, release: false —
// and the working tree has to do, and the pending-branch complaint when the
// config names one that does not exist yet.
func freezeRef(r gitx.Repo, p *config.Project) (string, string) {
	if !p.ReleaseEnabled() {
		return "", ""
	}

	branch, _, err := latestRelease(r, p, "")
	if err != nil {
		return "", ""
	}

	return releaseRef(r, branch)
}

// prefixed names the revision an issue was found on, so a complaint about a
// branch nobody has checked out is not mistaken for one about the working copy.
func prefixed(ref string, issues []string) []string {
	out := make([]string, 0, len(issues))
	for _, i := range issues {
		out = append(out, fmt.Sprintf("on %s: %s", ref, i))
	}

	return out
}

func checkSubmoduleSet(
	rctx *run.Ctx,
	r gitx.Repo,
	p *config.Project,
	actual []string,
	ref string,
) []string {
	var issues []string

	have := make(map[string]bool, len(actual))
	for _, a := range actual {
		have[a] = true
	}

	submodules, err := freezeList(rctx, r, p, ref)
	if err != nil {
		return append(issues, fmt.Sprintf("cannot read .gitmodules: %v", err))
	}

	configured := make(map[string]bool, len(submodules))

	for _, sm := range submodules {
		configured[sm.Path] = true

		if !have[sm.Path] {
			issues = append(issues, fmt.Sprintf("submodule %s is in the config but not in .gitmodules", sm.Path))

			continue
		}

		if sm.FreezeTo != freezeToRelease && sm.FreezeTo != config.FreezeToProject {
			continue
		}

		if _, err := resolveFreezeBranch(rctx, r, p, sm, ref); err != nil {
			issues = append(issues, fmt.Sprintf("submodule %s: freeze_to %s unresolvable: %v",
				sm.Path, sm.FreezeTo, err))
		}
	}

	for _, a := range actual {
		if !configured[a] && p.DepsFreezeEnabled() {
			issues = append(issues, fmt.Sprintf("submodule %s is in .gitmodules but not configured for freeze", a))
		}
	}

	return issues
}

// checkCI reports a pipeline watch that cannot run: ci names the system and
// the watch goes through its CLI, so that program has to be reachable — and
// finding out at the first push after a release tag is the worst time.
func checkCI(p *config.Project) []string {
	programs := map[string]string{config.CIGitLab: "glab", config.CIGitHub: "gh"}

	program, ok := programs[p.CI]
	if !ok {
		return nil
	}

	if _, err := exec.LookPath(program); err != nil {
		return []string{fmt.Sprintf("ci (%s): %s is not in PATH", p.CI, program)}
	}

	return nil
}

// checkHooks reports hook commands whose program is not on PATH — a notifier
// that is missing would only be found out as a warning in the middle of a
// release, when nobody is watching.
//
//nolint:prealloc // three events, and usually no issue at all
func checkHooks(cfg *config.Config) []string {
	var issues []string

	for _, event := range []string{config.HookAsk, config.HookWait, config.HookDone} {
		issues = append(issues, checkPrograms("hooks."+event, cfg.Hooks[event])...)
	}

	return issues
}

// checkDeps reports dependency commands that cannot run here: the config says
// which commands a project needs, so the only thing worth verifying is that the
// program each one starts with is actually reachable.
func checkDeps(rctx *run.Ctx, p *config.Project) []string {
	return checkPrograms(fmt.Sprintf("deps (%s)", p.Deps), depsCmds(rctx, p, p.DevBranch))
}

// checkToolchain reports toolchain commands and checks whose program is not
// on PATH, the way checkDeps does for the deps commands.
func checkToolchain(rctx *run.Ctx, p *config.Project) []string {
	branch := p.TargetBranch()
	vars := toolchainVars(rctx, p, branch)

	templates := append(rctx.Cfg.ToolchainCommands(p), rctx.Cfg.ToolchainCheckCommands(p)...)
	cmds := make([]string, 0, len(templates))

	for _, cmd := range templates {
		cmds = append(cmds, expand(cmd, vars))
	}

	return checkPrograms("toolchain", cmds)
}

// checkRenovateConfig reports a renovate config that is not even JSON —
// Renovate would open an error issue for that, a worse place to learn it —
// and a missing one where the project said renovate: true. Without that
// word a project with no config is simply not one deps renovate looks
// after, which is no finding. A shared preset of a configured project has
// to read the branch this config has that project on, renovate: false or
// not: deps freeze pins it either way, whoever runs Renovate reads it.
func checkRenovateConfig(rctx *run.Ctx, r gitx.Repo, p *config.Project) []string {
	branch := p.TargetBranch()

	// A branch nobody has created yet is checkReleaseBranch's finding; the
	// working tree would be read in its name here.
	ref := branchRef(r, branch)
	if ref == "" {
		return nil
	}

	file := renovateConfigFile(r, ref)
	if file == "" {
		if !p.RenovateRequired() {
			return nil
		}

		return []string{"renovate: true but no renovate.json on " + branch}
	}

	body, found, err := r.FileAt(ref, file)
	if err != nil || !found {
		return nil
	}

	// Read as deps renovate reads it, so what skips the project there is a
	// finding here: not JSON, JSON5, a level key of its own, odd rules.
	if _, reason := renovateParseConfig([]byte(body), file, "on "+branch); reason != "" {
		if !p.RenovateEnabled() {
			return nil
		}

		return []string{"renovate: " + reason}
	}

	return checkPresetRefs(rctx, p, file, branch, body)
}

// checkRenovate reports a renovate run that could not start: the docker
// program missing, no token for a host, a cache directory that cannot be
// made. Only when some project is eligible at all.
func checkRenovate(rctx *run.Ctx, projects []*config.Project) []string {
	cfg := &rctx.Cfg.Renovate

	// A hosts entry is a typo whether or not any project asked for Renovate.
	issues := checkRenovateHosts(cfg, rctx.Cfg.Projects)

	hosts := renovateHosts(projects)
	if len(hosts) == 0 {
		return issues
	}

	if _, err := exec.LookPath(cfg.Docker); err != nil {
		issues = append(issues, cfg.Docker+" is not in PATH")
	}

	for _, key := range hosts {
		platform, host, _ := strings.Cut(key, " ")

		if _, _, err := renovateToken(cfg.ForHost(host), platform, host); err != nil {
			issues = append(issues, err.Error())
		}
	}

	if issue := checkCacheDir(cfg.CacheDir); issue != "" {
		issues = append(issues, issue)
	}

	return issues
}

// checkCacheDir says whether the run could make or use cache_dir, without
// making it: a check is read-only. A missing directory is fine when its
// nearest existing parent is a directory we can write to.
func checkCacheDir(dir string) string {
	if dir == "" {
		return ""
	}

	for probe := dir; ; probe = filepath.Dir(probe) {
		info, err := os.Stat(probe)

		switch {
		case err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR):
			return fmt.Sprintf("cache_dir: %v", err) // a parent that cannot be read is not one to build under
		case err == nil && !info.IsDir():
			return fmt.Sprintf("cache_dir: %s is not a directory", probe)
		case err == nil:
			f, err := os.CreateTemp(probe, ".rt-check-*")
			if err != nil {
				return fmt.Sprintf("cache_dir: %s is not writable", probe)
			}

			_ = f.Close()
			_ = os.Remove(f.Name())

			return ""
		case filepath.Dir(probe) == probe:
			return fmt.Sprintf("cache_dir: %v", err)
		}
	}
}

// checkRenovateHosts reports a hosts entry no project is on: a typo there
// would leave the run it meant to adjust as it is, without a word.
func checkRenovateHosts(cfg *config.Renovate, projects []*config.Project) []string {
	known := map[string]bool{}

	for _, p := range projects {
		if host, _, ok := config.SplitRemote(p.Git); ok {
			known[host] = true
		}
	}

	var issues []string

	for _, host := range slices.Sorted(maps.Keys(cfg.Hosts)) {
		if !known[host] {
			issues = append(issues, "hosts: "+host+" is no project's git host")
		}
	}

	return issues
}

// checkPrograms reports the commands whose program is not on PATH. A missing
// generator is otherwise found only at run time, and a pipeline can even hide
// it: git-cliff | sed exits through sed and writes an empty file.
func checkPrograms(label string, cmds []string) []string {
	var issues []string

	for _, cmd := range cmds {
		for _, program := range pipelinePrograms(cmd) {
			if _, err := exec.LookPath(program); err != nil {
				issues = append(issues, fmt.Sprintf("%s: %s is not in PATH", label, program))
			}
		}
	}

	return issues
}

// pipelinePrograms names the programs a shell command runs: the first word of
// every stage of a pipeline or a list (|, &&, || and ;), skipping anything
// that is shell syntax rather than a program.
func pipelinePrograms(cmd string) []string {
	var (
		programs []string
		inCase   bool
	)

	stages := shellStages(cmd)

	for pos, stage := range stages {
		fields := strings.Fields(stage)

		// Inside case … esac the program follows the pattern, a word that
		// ends in ), and the pattern may be any word at all: py) as well
		// as *). a|b) is cut at the | into stages, so a one-word stage
		// before one that opens with a pattern is the pattern's other half.
		if len(fields) > 0 && fields[0] == "case" {
			inCase = true
		}

		if inCase {
			if len(fields) == 1 && opensWithCasePattern(stages[pos+1:]) {
				continue
			}

			fields = afterCasePattern(fields)
		}

		if len(fields) > 0 && fields[0] == "esac" {
			inCase = false
		}

		if program := stageProgram(fields); program != "" && !slices.Contains(programs, program) {
			programs = append(programs, program) // once: a program is on PATH or not
		}
	}

	return programs
}

// stageProgram is the program a stage's words run, "" when they run none:
// shell syntax, a builtin, a redirection or an expansion.
func stageProgram(fields []string) string {
	for len(fields) > 0 && (shellKeyword(fields[0]) || shellAssignment(fields[0])) {
		fields = fields[1:] // GOFLAGS=-mod=mod go mod tidy runs go
	}

	if len(fields) == 0 || shellBuiltin(fields[0]) {
		return ""
	}

	// (cd sub && make) is two stages, the parens are the subshell's
	program := strings.TrimRight(strings.TrimLeft(fields[0], "("), ")")

	if shellBuiltin(program) || strings.ContainsAny(program, "=<>&$(`\"'") {
		return "" // a redirection or an expansion, not a program name
	}

	return program
}

// afterCasePattern is what follows the last case pattern of a stage, the
// stage itself when it has none.
func afterCasePattern(fields []string) []string {
	cut := -1

	for i, word := range fields {
		if casePattern(word) {
			cut = i
		}
	}

	return fields[cut+1:]
}

// casePattern says whether a word closes a case pattern: *), py), but not
// the ) of a subshell.
func casePattern(word string) bool {
	return strings.HasSuffix(word, ")") && !strings.HasPrefix(word, "(")
}

// opensWithCasePattern says whether the next stage that has any words
// starts with a case pattern.
func opensWithCasePattern(stages []string) bool {
	for _, stage := range stages {
		if fields := strings.Fields(stage); len(fields) > 0 {
			return casePattern(fields[0])
		}
	}

	return false
}

// shellAssignment reports a NAME=value word: in front of a command it sets
// the command's environment, it is not the command.
func shellAssignment(word string) bool {
	name, _, ok := strings.Cut(word, "=")

	return ok && config.IsEnvName(name)
}

// shellStages splits a command at the |, &&, || and ; that stand outside
// quotes (single, double or backticks), so a perl or sed script that carries
// those characters stays whole.
func shellStages(cmd string) []string {
	var (
		stages []string
		quote  byte
	)

	start := 0

	for pos := 0; pos < len(cmd); pos++ {
		c := cmd[pos]

		switch {
		case c == '\\' && quote != '\'':
			pos++ // the next character is escaped
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"' || c == '`':
			quote = c
		case c == '|' || c == ';' || c == '&' && pos+1 < len(cmd) && cmd[pos+1] == '&':
			stages = append(stages, cmd[start:pos])

			if (c == '|' || c == '&') && pos+1 < len(cmd) && cmd[pos+1] == c {
				pos++
			}

			start = pos + 1
		}
	}

	return append(stages, cmd[start:])
}

// shellKeyword reports a word that opens or continues a compound command, the
// program that follows it is the one that runs.
func shellKeyword(word string) bool {
	switch word {
	case "if", "then", "else", "elif", "while", "until", "do", "!", "{", "time", "exec":
		return true
	}

	return false
}

// shellBuiltin reports a word whose stage runs no program: a keyword that
// closes a compound command, one whose next word is a name or a pattern (for
// f in …, case x in …), or a builtin with no binary worth looking for.
func shellBuiltin(word string) bool {
	switch word {
	case "fi", "done", "esac", "}", "in", "for", "case", "select", "function",
		"[", "[[", "test", ":", "cd", "exit", "return", "export", "local", "read", "shift", "trap",
		"set", "unset", "source", ".", "eval":
		return true
	}

	return false
}
