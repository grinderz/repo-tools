package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
	"github.com/grinderz/repo-tools/internal/run"
)

// Third-party dependencies — go.mod requires, pyproject packages, image
// tags, tool pins — are Renovate's job, and each repository keeps its own
// renovate.json for the rules. What rt adds is the run: Renovate in docker
// over the configured projects, the platform token from glab or gh, the
// environment the container needs, and a report in rt's words instead of
// a log stream.

// The dry-run modes Renovate knows, from cheapest to most thorough.
//
//nolint:gochecknoglobals // a fixed table
var renovateDryRunModes = []string{"extract", "lookup", "full"}

// renovateConfigFiles are the places Renovate looks for a repository's
// config, in its own order.
//
//nolint:gochecknoglobals // a fixed table
var renovateConfigFiles = []string{
	"renovate.json", "renovate.json5",
	".github/renovate.json", ".github/renovate.json5",
	".gitlab/renovate.json", ".gitlab/renovate.json5",
	".renovaterc", ".renovaterc.json", ".renovaterc.json5",
}

// Where the generated config and the cache sit inside the container.
const (
	renovateConfigMount = "/config.json"
	renovateNetrcMount  = "/tmp/renovate/netrc" // where ~/.netrc is mounted; NETRC names it to the tools
	renovateCacheMount  = "/tmp/renovate/cache"
)

// The generated config is read by the container's own user, so it is world
// readable — the directory it is in is private, and the container may run
// as another uid without a cache or a netrc; the cache directory is ours.
const (
	renovateConfigPerm = 0o644
	renovateDirPerm    = 0o750
)

type renovateOpts struct {
	DryRun      string // one of renovateDryRunModes, or empty for a real run
	LogFile     string // a copy of the raw JSON log lines
	LocalConfig bool   // each project's config from its working copy, not its branch
	Update      string // how far a dependency may move: patch, minor or major; empty takes the config's
	Group       string // all: one merge request per project; separate: Renovate's own cut; empty takes the config's
	Internal    string // dev, always or never: when the config's own projects move as dependencies; empty: the config's
}

// fill takes the config's choice for every option the flags left empty.
func (o *renovateOpts) fill(cfg *config.Renovate) {
	o.Update = firstNonEmptyString(o.Update, cfg.Update)
	o.Group = firstNonEmptyString(o.Group, cfg.Group)
	o.Internal = firstNonEmptyString(o.Internal, cfg.Internal)
}

func (o *renovateOpts) validate() error {
	if o.DryRun != "" && !slices.Contains(renovateDryRunModes, o.DryRun) {
		return fmt.Errorf("--renovate-dry-run %q %w", o.DryRun, errBadDryRunMode)
	}

	if o.Update != "" && !slices.Contains(config.RenovateUpdates, o.Update) {
		return fmt.Errorf("--update %q %w", o.Update, errBadUpdateLevel)
	}

	if o.Group != "" && !slices.Contains(config.RenovateGroups, o.Group) {
		return fmt.Errorf("--group %q %w", o.Group, errBadGroup)
	}

	if o.Internal != "" && !slices.Contains(config.RenovateInternals, o.Internal) {
		return fmt.Errorf("--internal %q %w", o.Internal, errBadInternal)
	}

	return nil
}

// renovateRepo is one project as Renovate sees it.
type renovateRepo struct {
	Project    *config.Project
	Platform   string // gitlab or github
	Host       string
	Slug       string // group/name, the platform's name for the repository
	Branch     string // the base branch, the project's target branch
	ConfigFile string // the renovate config found on that branch, or in the working copy
	// Config is the file's content, handed to Renovate inside the
	// repository's entry so rt's own rules can follow the file's.
	Config map[string]any
	Local  bool // the file came from the working copy (--local-config)
	// Presets are the shared presets inlined under --local-config, from a
	// working copy or a #ref branch, for the plan.
	Presets []string
}

// baseBranches is what Renovate works against: the branches the
// repository's config names, since they win over rt's, else the target
// branch.
func (repo renovateRepo) baseBranches() string {
	patterns, _ := repo.Config["baseBranchPatterns"].([]any)

	var names []string

	for _, p := range patterns {
		if name, ok := p.(string); ok {
			names = append(names, name)
		}
	}

	if len(names) == 0 {
		return repo.Branch
	}

	return strings.Join(names, ", ")
}

// renovateGroup is one Renovate run: every repository on one platform host.
type renovateGroup struct {
	Platform    string
	Host        string
	Endpoint    string
	Token       string
	TokenSource string
	Repos       []renovateRepo
	// The container environment, made once at planning: the docker flags
	// that name the variables, the values for the process alone, and the
	// plan's account of them, credentials masked.
	EnvFlags []string
	Env      []string
	EnvPlan  string
}

func newRenovateCmd(rctx *run.Ctx, name string) *cobra.Command {
	var opts renovateOpts

	c := &cobra.Command{
		Use:   name + " [project...]",
		Short: "Run Renovate over the projects to open dependency update merge requests",
		Long: "Runs Renovate in docker over the selected projects, one run per platform\n" +
			"host, against each project's target branch. The rules are each\n" +
			"repository's own renovate.json; rt brings the list of repositories, the\n" +
			"platform token (the token_env variable, else glab's or gh's own), the\n" +
			"environment the container needs and a report of the merge requests it\n" +
			"opened. A project with renovate: false or no renovate config on its\n" +
			"target branch is skipped; ci: none only leaves the platform to the git\n" +
			"host (github.com is GitHub, anything else GitLab).\n\n" +
			"--renovate-dry-run runs Renovate's own dry run (extract, lookup or full):\n" +
			"nothing is written to the platform, the report says what would be.\n\n" +
			"--local-config takes each project's renovate config from its working\n" +
			"copy, committed or not, instead of its target branch, so a config can be\n" +
			"tried with --renovate-dry-run full before it is pushed anywhere.\n\n" +
			"--update says how far a dependency may move: patch (the default, and\n" +
			"renovate.update in the config) holds major and minor updates back, minor\n" +
			"holds major back, major takes everything in one update per dependency.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := opts.validate(); err != nil {
				return err
			}

			projects, err := rctx.Select(args)
			if err != nil {
				return err
			}

			opts.fill(&rctx.Cfg.Renovate)

			steps, err := planRenovate(rctx, projects, opts)
			if err != nil {
				return err
			}

			class := run.Destructive
			if opts.DryRun != "" {
				class = run.ReadOnly
			}

			ok, err := rctx.Gate(cmdLabel(cmd), class, steps)
			if err != nil || !ok {
				return err
			}

			if opts.LogFile != "" {
				if err := os.WriteFile(opts.LogFile, nil, renovateConfigPerm); err != nil {
					return fmt.Errorf("log file: %w", err)
				}
			}

			return run.Execute(steps, !rctx.KeepGoing)
		},
	}
	c.Flags().StringVar(&opts.DryRun, "renovate-dry-run", "",
		"run Renovate's own dry run instead of a real one: extract, lookup or full")
	c.Flags().StringVar(&opts.LogFile, "log-file", "", "write Renovate's raw JSON log lines to this file as well")
	c.Flags().StringVar(&opts.Update, "update", "",
		"how far a dependency may move: patch, minor or major (default: renovate.update, patch)")
	c.Flags().StringVar(&opts.Group, "group", "",
		"all: one merge request per project; separate: one per dependency, the project's own groups apart "+
			"(default: renovate.group, all)")
	c.Flags().StringVar(&opts.Internal, "internal", "",
		"when the config's own projects move as dependencies of each other: dev (on the dev branch alone), "+
			"always or never (default: renovate.internal, dev)")
	c.Flags().BoolVar(&opts.LocalConfig, "local-config", false,
		"use each project's renovate config from its working copy, not from its target branch")

	return c
}

// planRenovate sorts the projects into skipped ones and runs: one step per
// skipped project, with the reason, then one step per platform host.
func planRenovate(rctx *run.Ctx, projects []*config.Project, opts renovateOpts) ([]run.Step, error) {
	var (
		steps []run.Step
		repos []renovateRepo
	)

	for _, p := range projects {
		repo, reason, err := renovateRepoFor(rctx, p, opts.LocalConfig)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p.Name, err)
		}

		if reason != "" {
			steps = append(steps, run.Step{Project: p, Skip: true, Warn: reason})

			continue
		}

		repos = append(repos, repo)
	}

	groups := groupRenovate(repos)

	cfg := &rctx.Cfg.Renovate
	if cfg.Endpoint != "" && len(groups) > 1 {
		return nil, fmt.Errorf("%w %d hosts", errRenovateEndpointHosts, len(groups))
	}

	for _, group := range groups {
		group.Endpoint = renovateEndpoint(cfg, group.Platform, group.Host)

		token, source, err := renovateToken(cfg.ForHost(group.Host), group.Platform, group.Host)
		if err != nil {
			return nil, err
		}

		group.Token, group.TokenSource = token, source
	}

	if err := renovateTokenClash(groups); err != nil {
		return nil, err
	}

	for _, group := range groups {
		flags, env, envPlan, err := renovateEnv(cfg.ForHost(group.Host), group.Host, group.Token)
		if err != nil {
			return nil, err
		}

		group.EnvFlags, group.Env, group.EnvPlan = flags, env, envPlan

		steps = append(steps, renovateGroupStep(rctx, group, opts))
	}

	return steps, nil
}

// renovateTokenClash refuses a run that would send one variable's token to
// two hosts: a work credential posted to the other platform. The hosts
// block gives a host a token_env of its own.
func renovateTokenClash(groups []*renovateGroup) error {
	hosts := map[string][]string{} // a $variable source -> the hosts it serves

	for _, group := range groups {
		if strings.HasPrefix(group.TokenSource, "$") {
			hosts[group.TokenSource] = append(hosts[group.TokenSource], group.Host)
		}
	}

	for _, source := range slices.Sorted(maps.Keys(hosts)) {
		if len(hosts[source]) > 1 {
			return fmt.Errorf("%s %w (%s): give each host its own token_env under hosts, or unset it",
				source, errRenovateTokenHosts, strings.Join(hosts[source], ", "))
		}
	}

	return nil
}

// renovateRepoFor sizes a project up for the run: the repository it is on
// the platform, the branch Renovate works against and the config it found
// there — or in the working copy, when local — or the reason it stays out.
func renovateRepoFor(rctx *run.Ctx, p *config.Project, local bool) (renovateRepo, string, error) {
	r := repoOf(p)

	if reason := missingRepo(r); reason != "" {
		return renovateRepo{}, reason, nil
	}

	if !p.RenovateEnabled() {
		return renovateRepo{}, "renovate: false", nil
	}

	host, slug, ok := config.SplitRemote(p.Git)
	if !ok {
		return renovateRepo{}, errNoRemoteHost.Error(), nil
	}

	if err := fetch(rctx, p, r); err != nil {
		return renovateRepo{}, "", fetchFailed(err)
	}

	branch := p.TargetBranch()
	repo := renovateRepo{
		Project: p, Platform: renovatePlatform(p, host),
		Host: host, Slug: slug, Branch: branch,
	}

	// Renovate works on the branch either way; without it the file would
	// be read from the checkout in its name.
	ref := branchRef(r, branch)
	if ref == "" {
		return renovateRepo{}, fmt.Sprintf("branch %s %v", branch, errNoBranch), nil
	}

	if local {
		file, cfg, reason := renovateLocalConfig(r.Dir)
		if reason != "" {
			return renovateRepo{}, reason, nil
		}

		presets, reason := renovateInlinePresets(cfg, projectPresetReader(rctx.Cfg.Projects, host), nil)
		if reason != "" {
			return renovateRepo{}, reason, nil
		}

		// A preset brings keys of its own: the same ones are refused there.
		if key, hint := renovateRefusedKey(cfg); key != "" {
			return renovateRepo{}, fmt.Sprintf("%s with its presets inlined sets %s, %s", file, key, hint), nil
		}

		repo.ConfigFile, repo.Config, repo.Local, repo.Presets = file, cfg, true, presets

		return repo, "", nil
	}

	file := renovateConfigFile(r, ref)
	if file == "" {
		return renovateRepo{}, fmt.Sprintf("no renovate.json on %s (deps renovate needs one, see README)", branch), nil
	}

	body, _, err := r.FileAt(ref, file)
	if err != nil {
		return renovateRepo{}, "", fmt.Errorf("%s: %w", p.Name, err)
	}

	cfg, reason := renovateParseConfig([]byte(body), file, "on "+branch)
	if reason != "" {
		return renovateRepo{}, reason, nil
	}

	repo.ConfigFile, repo.Config = file, cfg

	return repo, "", nil
}

// renovatePlatform is where a project's merge requests go. ci names it
// when it is set; ci: none only says rt watches no pipeline — a tooling
// repository with merge-request pipelines alone — so the host decides then:
// github.com is GitHub, anything else is taken for a GitLab.
func renovatePlatform(p *config.Project, host string) string {
	switch {
	case p.CI != config.CINone:
		return p.CI
	case host == githubHost:
		return config.CIGitHub
	default:
		return config.CIGitLab
	}
}

// renovateConfigFile is the first of Renovate's config file names that
// exists at ref, empty when none does.
func renovateConfigFile(r gitx.Repo, ref string) string {
	for _, path := range renovateConfigFiles {
		if _, found, err := r.FileAt(ref, path); err == nil && found {
			return path
		}
	}

	return ""
}

// renovateLocalConfig is the first of Renovate's config files in a working
// copy, parsed, or the reason there is none to use. JSON5 is Renovate's own
// to read, so only the JSON files can be handed over.
func renovateLocalConfig(dir string) (string, map[string]any, string) {
	for _, path := range renovateConfigFiles {
		data, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil {
			continue
		}

		cfg, reason := renovateParseConfig(data, path, "in the working copy")
		if reason != "" {
			return "", nil, reason
		}

		return path, cfg, ""
	}

	return "", nil, "no renovate.json in the working copy"
}

// renovateParseConfig reads a repository's config the way rt hands it to
// Renovate, or says why it cannot: JSON only (a JSON5 file would need
// Renovate's reader), the schema pointer dropped, no update level key of
// its own, packageRules a list so rt's rule can follow them.
func renovateParseConfig(data []byte, path, where string) (map[string]any, string) {
	if strings.HasSuffix(path, ".json5") {
		return nil, fmt.Sprintf("%s %s is JSON5, rt reads JSON only", path, where)
	}

	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Sprintf("%s %s is not valid JSON: %v", path, where, err)
	}

	delete(cfg, "$schema")

	if key, hint := renovateRefusedKey(cfg); key != "" {
		return nil, fmt.Sprintf("%s %s sets %s, %s", path, where, key, hint)
	}

	if rules, ok := cfg["packageRules"]; ok {
		if _, isList := rules.([]any); !isList {
			return nil, fmt.Sprintf("%s %s: packageRules is not a list", path, where)
		}
	}

	return cfg, ""
}

// renovateOldBaseBranches is the name baseBranchPatterns had before
// Renovate 41, still read by it.
const renovateOldBaseBranches = "baseBranches"

// renovateRefusedKey is the first key a repository's config may not set,
// with why: a level key, which --update owns, or baseBranches — Renovate
// renames the old key on the way in unless the new one is set already,
// which rt's entry does, so the file's branches would be lost.
func renovateRefusedKey(cfg map[string]any) (string, string) {
	if key := renovateLevelKeyIn(cfg); key != "" {
		return key, "which --update owns"
	}

	if _, ok := cfg[renovateOldBaseBranches]; ok {
		return renovateOldBaseBranches, "Renovate's old name: use baseBranchPatterns"
	}

	return "", ""
}

// renovateLevelKeyIn is the first key of the update level a repository
// config sets itself, empty when none: the file's value would win over
// --update for that project.
func renovateLevelKeyIn(cfg map[string]any) string {
	for _, key := range config.RenovateLevelKeys {
		if _, ok := cfg[key]; ok {
			return key
		}
	}

	return ""
}

// groupRenovate puts the repositories of one platform host together, in
// the order they first appeared.
func groupRenovate(repos []renovateRepo) []*renovateGroup {
	var groups []*renovateGroup

	byKey := map[string]*renovateGroup{}

	for _, repo := range repos {
		key := repo.Platform + " " + repo.Host

		group, ok := byKey[key]
		if !ok {
			group = &renovateGroup{Platform: repo.Platform, Host: repo.Host}
			byKey[key] = group
			groups = append(groups, group)
		}

		group.Repos = append(group.Repos, repo)
	}

	return groups
}

// renovateEndpoint is the platform API Renovate talks to: the config's, or
// the host's own — GitLab's v4 API, GitHub's v3 on an enterprise host, and
// nothing for github.com, which Renovate knows.
func renovateEndpoint(cfg *config.Renovate, platform, host string) string {
	if cfg.Endpoint != "" {
		return cfg.Endpoint
	}

	switch {
	case platform == config.CIGitHub && host == githubHost:
		return ""
	case platform == config.CIGitHub:
		return "https://" + host + "/api/v3/"
	default:
		return "https://" + host + "/api/v4/"
	}
}

// renovateToken is the platform token: the token_env variable when it is
// set, else what glab or gh keep for the host. The requests are then
// authored by that token's owner.
func renovateToken(cfg *config.Renovate, platform, host string) (string, string, error) {
	if value := os.Getenv(cfg.TokenEnv); value != "" {
		return value, "$" + cfg.TokenEnv, nil
	}

	var (
		out  string
		err  error
		tool string
	)

	// The long flag names: glab reads -h as --help and would answer a
	// token request with its usage text, exit status 0.
	switch platform {
	case config.CIGitHub:
		tool = "gh"
		out, err = gitx.Repo{}.Stdout("gh", "auth", "token", "--hostname", host)
	default:
		tool = "glab"
		out, err = gitx.Repo{}.Stdout("glab", "config", "get", "token", "--host", host)
	}

	token := strings.TrimSpace(out)

	// A token is one word: anything else is a message, not a credential.
	if err != nil || token == "" || strings.ContainsAny(token, " \t\n") {
		return "", "", fmt.Errorf("%w %s: set $%s or run %s auth login --hostname %s",
			errNoRenovateToken, host, cfg.TokenEnv, tool, host)
	}

	return token, tool + " (" + host + ")", nil
}

// renovateGroupStep is the plan of one run: what runs, where, as whom, with
// what environment, over which repositories.
func renovateGroupStep(rctx *run.Ctx, group *renovateGroup, opts renovateOpts) run.Step {
	cfg := rctx.Cfg.Renovate.ForHost(group.Host)

	mode := "open merge requests"
	if opts.DryRun != "" {
		mode = "dry-run: " + opts.DryRun
	}

	plan := []string{
		fmt.Sprintf("image %s via %s, %s", cfg.Image, cfg.Docker, mode),
		"updates: " + renovateUpdateWording(opts.Update),
		"merge requests: " + renovateGroupWording(opts.Group),
		"internal modules: " + renovateInternalWording(opts.Internal),
	}

	if group.Endpoint != "" {
		plan = append(plan, "endpoint "+group.Endpoint)
	}

	plan = append(plan, "token from "+group.TokenSource)

	switch {
	case group.EnvPlan != "":
		plan = append(plan, "env "+group.EnvPlan)
	case len(rctx.Cfg.Renovate.Env) > 0:
		plan = append(plan, "env none (hosts: "+group.Host+")")
	}

	if cfg.CacheDir != "" {
		plan = append(plan, "cache "+cfg.CacheDir)
	}

	width := 0
	for _, repo := range group.Repos {
		width = max(width, len(repo.Project.Name))
	}

	for _, repo := range group.Repos {
		source := []string{repo.ConfigFile}
		if repo.Local {
			source[0] += " from the working copy"
		}

		for _, preset := range repo.Presets {
			source = append(source, "preset "+preset)
		}

		if renovateInternalOn(opts.Internal, repo) {
			source = append(source, "internal modules too")
		}

		plan = append(plan, fmt.Sprintf("%-*s  %s on %s (%s)",
			width, repo.Project.Name, repo.Slug, planRef(repo.baseBranches()), strings.Join(source, ", ")))
	}

	return run.Step{
		Label: group.Platform + " " + group.Host,
		Plan:  plan,
		Exec:  func() error { return runRenovate(rctx, group, opts) },
	}
}

// githubHost is the one GitHub Renovate knows by name: its platform, and
// the token variable of its own, GITHUB_COM_TOKEN.
const githubHost = "github.com"

// The update levels, as the flag and renovate.update name them.
const (
	updateMinor = "minor"
	updateMajor = "major"
)

// The keys of a package rule rt writes, and the update type of a commit
// moving to its branch's head.
const (
	ruleDescription = "description" // what Renovate shows a rule by in its logs
	ruleEnabled     = "enabled"
	ruleMatchNames  = "matchPackageNames"
	ruleMatchTypes  = "matchUpdateTypes"
	updateDigest    = "digest"
	updateReplace   = "replacement"
)

// The groupings, as the flag and renovate.group name them.
const (
	groupAll      = "all"
	groupSeparate = "separate"
)

// The internal modes, as the flag and renovate.internal name them.
const (
	internalDev    = "dev"
	internalAlways = "always"
	internalNever  = "never"
)

// renovateInternalWording says in the plan when the config's own projects
// move as dependencies.
func renovateInternalWording(internal string) string {
	switch internal {
	case internalAlways:
		return "updated on every branch"
	case internalNever:
		return "held back"
	default:
		return "updated on the dev branch, held back elsewhere"
	}
}

// renovateInternalOn says whether a repository's run moves the config's
// own projects: on its dev branch under dev, where deps freeze has not
// pinned them to release branches.
func renovateInternalOn(internal string, repo renovateRepo) bool {
	switch internal {
	case internalAlways:
		return true
	case internalNever:
		return false
	default:
		// On the dev branch alone: not when the file's own base branches
		// take the run elsewhere too, since the rule is the repository's.
		return repo.Project != nil && repo.baseBranches() == repo.Project.DevBranch
	}
}

// renovateInternalPatterns names the config's projects the way a go.mod
// requires them — host/group/name, and whatever is under it — so a rule can
// match them: the projects of the config alone, not everything on the host.
func renovateInternalPatterns(projects []*config.Project) []string {
	var patterns []string

	for _, p := range projects {
		host, slug, ok := config.SplitRemote(p.Git)
		if !ok {
			continue
		}

		module := host + "/" + slug
		if !slices.Contains(patterns, module) {
			patterns = append(patterns, module, module+"/**")
		}
	}

	return patterns
}

// renovateInternalRules enable or disable the config's own projects as
// dependencies. They go last in a repository's rules: they have to win
// over the level rule, which holds digests back — an internal module on a
// dev branch is a pseudo-version, so moving it to the branch's head is one
// — and over a rule of the file's own, if the shared config disables them.
// Enabled takes two rules: one without an update type, since a rule with
// one is only read once the updates are known and a package the shared
// config disabled would never get that far, and one closing again what
// the level holds back, the digest apart, and the types that are no
// release at all — a tag of an internal module is a version like any
// other, for the level to decide.
func renovateInternalRules(patterns []string, enabled bool, update string) []any {
	if !enabled {
		return []any{map[string]any{
			ruleDescription: "rt deps renovate --internal " + internalNever,
			ruleMatchNames:  patterns,
			ruleEnabled:     false,
		}}
	}

	return []any{
		map[string]any{
			ruleDescription: "rt deps renovate --internal " + internalDev,
			ruleMatchNames:  patterns,
			ruleEnabled:     true,
		},
		map[string]any{
			ruleDescription: "rt deps renovate --internal " + internalDev + ": the level's updates and the digest alone",
			ruleMatchNames:  patterns,
			ruleMatchTypes:  renovateInternalHeld(update),
			ruleEnabled:     false,
		},
	}
}

// renovateNoReleaseTypes are the update types that are no release of the
// dependency: never for an internal module.
//
//nolint:gochecknoglobals // a fixed table
var renovateNoReleaseTypes = []string{"pin", "pinDigest", "rollback", "lockFileMaintenance"}

// renovateInternalHeld are the update types the internal rule closes
// again after enabling the module: what the level holds back, the digest
// apart, and the types that are no release.
func renovateInternalHeld(update string) []string {
	held := make([]string, 0, len(renovateNoReleaseTypes)+len(renovateLevelHeld(update)))

	for _, kind := range renovateLevelHeld(update) {
		if kind != updateDigest {
			held = append(held, kind)
		}
	}

	return append(held, renovateNoReleaseTypes...)
}

// renovateGroupWording says in the plan how the updates are cut.
func renovateGroupWording(group string) string {
	if group == groupSeparate {
		return "one per dependency, the project's own groups apart"
	}

	return "one per project"
}

// renovateGroupRule puts every update of a repository in one group. It
// goes after the repository's own rules — a later rule wins in Renovate —
// so a group of the file's own does not split its packages off.
func renovateGroupRule() map[string]any {
	return map[string]any{
		ruleDescription: "rt deps renovate --group all",
		ruleMatchNames:  []string{"*"},
		"groupName":     "dependencies",
	}
}

// renovateUpdateWording says in the plan what an update level means.
func renovateUpdateWording(update string) string {
	switch update {
	case updateMinor:
		return "minor and patch, major and digest held back"
	case updateMajor:
		return "everything, one update per dependency"
	default:
		return "patch only, major, minor and digest held back"
	}
}

// renovateUpdateConfig is how an update level reads in Renovate's terms: a
// rule that disables the update types above the level, and the separation
// that makes the level its own candidate — a patch is only offered apart
// from the newest minor when separateMinorPatch says so; major means the
// newest release of all, so major and minor are not separated. A digest
// (a git rev moved to the branch's head) and a replacement (a package
// renamed upstream) are no level at all, so only major lets them through.
func renovateUpdateConfig(update string) (map[string]any, []any) {
	held := renovateLevelHeld(update)

	switch update {
	case updateMinor:
		return nil, []any{map[string]any{
			ruleDescription: "rt deps renovate --update minor",
			ruleMatchTypes:  held,
			ruleEnabled:     false,
		}}
	case updateMajor:
		return map[string]any{"separateMajorMinor": false}, nil
	default:
		return map[string]any{"separateMinorPatch": true}, []any{map[string]any{
			ruleDescription: "rt deps renovate --update patch",
			ruleMatchTypes:  held,
			ruleEnabled:     false,
		}}
	}
}

// renovateLevelHeld are the update types a level holds back: none above
// major, and the digest and a replacement below it.
func renovateLevelHeld(update string) []string {
	switch update {
	case updateMinor:
		return []string{updateMajor, updateDigest, updateReplace}
	case updateMajor:
		return nil
	default:
		return []string{updateMajor, updateMinor, updateDigest, updateReplace}
	}
}

// renovateGlobalConfig is the self-hosted config file of one run: the
// platform, the repositories with their base branches and their own config
// files, no onboarding, the dry-run mode, the update level, and the config
// block's own keys over all of it — its packageRules after the level's, so
// they can override it. Each repository's file goes inside its entry, with
// requireConfig: ignored so Renovate does not read it a second time: there
// rt's group rule can come after the file's own rules.
func renovateGlobalConfig(
	cfg *config.Renovate,
	group *renovateGroup,
	opts renovateOpts,
	internal []string,
) ([]byte, error) {
	global := map[string]any{
		"platform":        group.Platform,
		"onboarding":      false,
		"cloneSubmodules": true, // a lock file update needs the path dependencies the submodules are
	}

	if group.Endpoint != "" {
		global["endpoint"] = group.Endpoint
	}

	if opts.DryRun != "" {
		global["dryRun"] = opts.DryRun
	}

	maps.Copy(global, cfg.Config)

	// The tools Renovate runs see only the variables it hands them, so the
	// mounted netrc is named through its config, not the environment.
	if _, mounted := renovateNetrc(cfg); mounted {
		custom, _ := global["customEnvVariables"].(map[string]any)
		custom = maps.Clone(custom)

		if custom == nil {
			custom = map[string]any{}
		}

		if _, set := custom["NETRC"]; !set {
			custom["NETRC"] = renovateNetrcMount
		}

		global["customEnvVariables"] = custom
	}

	levelKeys, levelRules := renovateUpdateConfig(opts.Update)
	maps.Copy(global, levelKeys)

	if own, _ := global["packageRules"].([]any); len(own) > 0 || len(levelRules) > 0 {
		global["packageRules"] = append(levelRules, own...)
	}

	repos := make([]map[string]any, 0, len(group.Repos))
	for _, repo := range group.Repos {
		entry := map[string]any{"baseBranchPatterns": []string{repo.Branch}}

		maps.Copy(entry, repo.Config) // the file's keys over rt's
		entry["requireConfig"] = "ignored"

		own, _ := entry["packageRules"].([]any)
		rules := slices.Clone(own)

		if opts.Group == groupAll {
			rules = append(rules, renovateGroupRule())
		}

		if len(internal) > 0 {
			rules = append(
				rules,
				renovateInternalRules(internal, renovateInternalOn(opts.Internal, repo), opts.Update)...)
		}

		if len(rules) > 0 {
			entry["packageRules"] = rules
		}

		entry["repository"] = repo.Slug
		repos = append(repos, entry)
	}

	global["repositories"] = repos

	out, err := json.MarshalIndent(global, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("renovate config: %w", err)
	}

	return out, nil
}

// renovateFixedFlags is the room for the flag pairs every run has: the
// token, the config file, the log format, and the netrc mount.
const renovateFixedFlags = 8

// renovateNetrc is the netrc file a run mounts for the tools in the
// container, when RENOVATE_HOST_RULES is listed in its env and the file is
// there; the path either way, for the host rules.
func renovateNetrc(cfg *config.Renovate) (string, bool) {
	netrc := netrcPath()
	if netrc == "" || !slices.ContainsFunc(cfg.Env, func(entry string) bool {
		name, _, _ := strings.Cut(entry, "=")

		return name == envHostRules
	}) {
		return netrc, false
	}

	_, err := os.Stat(netrc)

	return netrc, err == nil
}

// renovateEnv is the environment the container gets: the -e flags for the
// docker command line, the values for the docker process's own environment
// (the token and every NAME=value entry — never on the command line), and
// the plan's wording of it, with the passthrough names rt's own environment
// lacks marked. Two of those rt fills itself when the shell has not:
// GITHUB_COM_TOKEN from gh, RENOVATE_HOST_RULES from ~/.netrc, less the
// platform host and github.com. host is the platform host of the run.
func renovateEnv(cfg *config.Renovate, host, token string) ([]string, []string, string, error) {
	flags := make([]string, 0, renovateFixedFlags+2*len(cfg.Env))
	flags = append(flags,
		"-e", "RENOVATE_TOKEN",
		"-e", "RENOVATE_CONFIG_FILE="+renovateConfigMount,
		"-e", "LOG_FORMAT=json",
	)

	env := []string{"RENOVATE_TOKEN=" + token}

	var plan []string

	// Listed, the netrc goes in twice: as hostRules for Renovate's own
	// lookups when the shell has none, and mounted for the tools it runs
	// — go reads it for the module proxy, uv for the package index —
	// named to them through the generated config.
	if netrc, mounted := renovateNetrc(cfg); mounted {
		flags = append(flags, "-v", netrc+":"+renovateNetrcMount+":ro")
		plan = append(plan, "~/.netrc mounted for go and uv")
	}

	for _, entry := range cfg.Env {
		name, value, set := strings.Cut(entry, "=")
		flags = append(flags, "-e", name)

		switch {
		case set && (secretNameRe.MatchString(name) || urlHasUser(value)):
			env = append(env, entry)
			plan = append(plan, name+"=***") // the plan goes to terminals and CI logs
		case set:
			env = append(env, entry)
			plan = append(plan, entry)
		case os.Getenv(name) != "":
			plan = append(plan, name)
		case name == envGitHubToken:
			token := ghToken()
			if token == "" {
				plan = append(plan, name+" (unset)")

				continue
			}

			env = append(env, name+"="+token)
			plan = append(plan, name+" (gh)")
		case name == envHostRules:
			rules, hosts, err := netrcHostRules(netrcPath(), []string{host, githubHost})
			if err != nil {
				return nil, nil, "", fmt.Errorf("renovate: %s: %w", name, err)
			}

			if rules == "" {
				plan = append(plan, name+" (unset)")

				continue
			}

			env = append(env, name+"="+rules)
			plan = append(plan, name+" (~/.netrc: "+strings.Join(hosts, ", ")+")")
		default:
			plan = append(plan, name+" (unset)")
		}
	}

	return flags, env, strings.Join(plan, ", "), nil
}

// urlHasUser says whether a value carries a url with a user, and so a
// password at times: GOPROXY=https://user:secret@proxy.example.com. By
// shape, not parsed: GOPROXY lists urls with | and , between them.
func urlHasUser(value string) bool {
	return urlUserRe.MatchString(value)
}

// urlUserRe is a scheme followed by an authority with a user in it.
var urlUserRe = regexp.MustCompile(`://[^/\s@]+@`)

// secretNameRe names the variables whose value is a credential by the look
// of them, so the plan shows them set, not what they are set to.
var secretNameRe = regexp.MustCompile(
	`(?i)token|secret|password|passwd|credential|auth|api_?key|private_?key|host_?rules`)

// renovateArgs is the docker command line after the program. With a cache
// mounted the container runs as our own uid in group 0: the image's user is
// another uid and could not write to a directory we own, and the Renovate
// images are built for an arbitrary uid as long as the group is 0.
func renovateArgs(cfg *config.Renovate, configPath string, envFlags []string, uid int) []string {
	args := append([]string{"run", "--rm"}, envFlags...)
	args = append(args, "-v", configPath+":"+renovateConfigMount+":ro")

	if cfg.CacheDir != "" {
		args = append(args, "-v", cfg.CacheDir+":"+renovateCacheMount)
	}

	// Our own uid in the container: the image's user could neither write a
	// cache directory we own nor read our netrc (0600).
	if _, mounted := renovateNetrc(cfg); uid >= 0 && (cfg.CacheDir != "" || mounted) {
		args = append(args, "--user", strconv.Itoa(uid)+":0")
	}

	args = append(args, cfg.DockerArgs...)

	return append(args, cfg.Image)
}

// runRenovate runs one group: writes the config, starts the container,
// reads its log into the report.
func runRenovate(rctx *run.Ctx, group *renovateGroup, opts renovateOpts) error {
	cfg := rctx.Cfg.Renovate.ForHost(group.Host)

	dir, err := os.MkdirTemp("", "rt-renovate-*")
	if err != nil {
		return fmt.Errorf("renovate: %w", err)
	}

	defer func() { _ = os.RemoveAll(dir) }()

	global, err := renovateGlobalConfig(cfg, group, opts, renovateInternalPatterns(rctx.Cfg.Projects))
	if err != nil {
		return err
	}

	configPath := filepath.Join(dir, "config.json")

	if err := os.WriteFile(configPath, global, renovateConfigPerm); err != nil {
		return fmt.Errorf("renovate: %w", err)
	}

	if cfg.CacheDir != "" {
		if err := os.MkdirAll(cfg.CacheDir, renovateDirPerm); err != nil {
			return fmt.Errorf("renovate cache: %w", err)
		}
	}

	args := renovateArgs(cfg, configPath, group.EnvFlags, os.Getuid())

	fmt.Printf("    running %s over %d %s\n",
		cfg.Image, len(group.Repos), plural(len(group.Repos), "repository", "repositories"))
	run.Fire(run.EventWait, map[string]string{
		run.EnvMessage: fmt.Sprintf("running renovate over %d repositories on %s", len(group.Repos), group.Host),
	})

	names := make(map[string]string, len(group.Repos))
	for _, repo := range group.Repos {
		names[repo.Slug] = repo.Project.Name
	}

	report := newRenovateReport(group.Platform, names)

	cmd := exec.CommandContext(context.Background(), cfg.Docker, args...) //nolint:gosec // the config says what runs
	cmd.Stderr = os.Stderr

	cmd.Env = append(os.Environ(), group.Env...)

	runErr := streamLines(cmd, opts.LogFile, report.line)

	return report.finish(runErr)
}

// streamLines runs cmd and hands every line of its stdout to onLine as it
// arrives, copying the raw lines to logFile when one is named. The exit
// status comes back once the output is drained.
func streamLines(cmd *exec.Cmd, logFile string, onLine func(string)) error {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("%s: %w", cmd.Path, err)
	}

	var tee io.Writer = io.Discard //nolint:staticcheck // the type is the point: a file replaces it below

	if logFile != "" {
		// Appended: one run is a container per platform host, and the file
		// holds them all; the command truncates it once before the first.
		f, err := os.OpenFile(
			logFile,
			os.O_WRONLY|os.O_CREATE|os.O_APPEND,
			renovateConfigPerm,
		)
		if err != nil {
			return fmt.Errorf("log file: %w", err)
		}

		defer func() { _ = f.Close() }()

		tee = f
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: %w", cmd.Path, err)
	}

	reader := bufio.NewReader(stdout)

	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			_, _ = io.WriteString(tee, line)
			onLine(strings.TrimRight(line, "\r\n"))
		}

		if err != nil {
			break
		}
	}

	if err := cmd.Wait(); err != nil {
		if _, ok := errors.AsType[*exec.ExitError](err); ok {
			return fmt.Errorf("%w", err) // the exit status is the whole message; wrapcheck wants the wrap
		}

		return fmt.Errorf("%s: %w", cmd.Path, err)
	}

	return nil
}

// renovateHosts names the platform hosts of the projects that said
// renovate: true, for repo check's docker and token tests: a config that
// never asked for Renovate is not told what it would need.
func renovateHosts(projects []*config.Project) []string {
	seen := map[string]bool{}

	var hosts []string

	for _, p := range projects {
		if !p.RenovateRequired() {
			continue
		}

		if host, _, ok := config.SplitRemote(p.Git); ok {
			key := renovatePlatform(p, host) + " " + host
			if !seen[key] {
				seen[key] = true
				hosts = append(hosts, key)
			}
		}
	}

	sort.Strings(hosts)

	return hosts
}
