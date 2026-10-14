package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/run"
)

func TestRenovateEndpoint(t *testing.T) {
	t.Parallel()

	cfg := &config.Renovate{}

	cases := []struct{ platform, host, want string }{
		{config.CIGitLab, "git.example.com", "https://git.example.com/api/v4/"},
		{config.CIGitHub, "github.com", ""},
		{config.CIGitHub, "ghe.example.com", "https://ghe.example.com/api/v3/"},
	}

	for _, tc := range cases {
		if got := renovateEndpoint(cfg, tc.platform, tc.host); got != tc.want {
			t.Errorf("%s %s: %q, want %q", tc.platform, tc.host, got, tc.want)
		}
	}

	cfg.Endpoint = "https://gl.internal/api/v4/"
	if got := renovateEndpoint(cfg, config.CIGitLab, "git.example.com"); got != cfg.Endpoint {
		t.Errorf("the configured endpoint wins: %q", got)
	}
}

// The generated config names the platform, the repositories with their base
// branches and no onboarding; the dry-run mode only when asked; the config
// block's own keys on top.
func TestRenovateGlobalConfig(t *testing.T) {
	t.Parallel()

	cfg := &config.Renovate{Config: map[string]any{"prConcurrentLimit": 5}}
	group := &renovateGroup{
		Platform: "gitlab", Host: "git.example.com", Endpoint: "https://git.example.com/api/v4/",
		Repos: []renovateRepo{
			{Slug: "group/api", Branch: "develop"},
			{Slug: "group/worker", Branch: "release-1.2"},
		},
	}

	out, err := renovateGlobalConfig(cfg, group, renovateOpts{DryRun: "full", Update: "patch", Group: "separate"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	want := `{
  "cloneSubmodules": true,
  "dryRun": "full",
  "endpoint": "https://git.example.com/api/v4/",
  "onboarding": false,
  "packageRules": [
    {
      "description": "rt deps renovate --update patch",
      "enabled": false,
      "matchUpdateTypes": [
        "major",
        "minor",
        "digest",
        "replacement"
      ]
    }
  ],
  "platform": "gitlab",
  "prConcurrentLimit": 5,
  "repositories": [
    {
      "baseBranchPatterns": [
        "develop"
      ],
      "repository": "group/api",
      "requireConfig": "ignored"
    },
    {
      "baseBranchPatterns": [
        "release-1.2"
      ],
      "repository": "group/worker",
      "requireConfig": "ignored"
    }
  ],
  "separateMinorPatch": true
}`
	if string(out) != want {
		t.Errorf("config:\n%s\nwant:\n%s", out, want)
	}

	plain, err := renovateGlobalConfig(&config.Renovate{}, group, renovateOpts{Update: "major", Group: "separate"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(plain, &parsed); err != nil {
		t.Fatal(err)
	}

	if _, ok := parsed["dryRun"]; ok {
		t.Error("a real run must not carry dryRun")
	}

	if parsed["separateMajorMinor"] != false || parsed["packageRules"] != nil {
		t.Errorf("major: one update per dependency, no rule: %v", parsed)
	}

	// The config's own rules come after the level's, so they can override it.
	own := &config.Renovate{
		Config: map[string]any{"packageRules": []any{map[string]any{"matchPackageNames": []any{"x"}}}},
	}

	minor, err := renovateGlobalConfig(own, group, renovateOpts{Update: "minor", Group: "separate"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := json.Unmarshal(minor, &parsed); err != nil {
		t.Fatal(err)
	}

	rules, _ := parsed["packageRules"].([]any)
	if len(rules) != 2 {
		t.Fatalf("minor rules = %v", rules)
	}

	level, _ := rules[0].(map[string]any)
	own2, _ := rules[1].(map[string]any)

	if level["description"] != "rt deps renovate --update minor" || own2["matchPackageNames"] == nil {
		t.Errorf("minor rules = %v", rules)
	}

	bad1 := renovateOpts{Update: "patchy"}

	if bad1.validate() == nil {
		t.Error("an unknown level must be refused")
	}

	bad2 := renovateOpts{Group: "some"}

	if bad2.validate() == nil {
		t.Error("an unknown grouping must be refused")
	}

	bad3 := renovateOpts{Internal: "sometimes"}

	if bad3.validate() == nil {
		t.Error("an unknown internal mode must be refused")
	}
}

// internalRulesAre says whether a repository's rules are the group rule
// and then the internal rules of the wanted state: enabled is two rules,
// the package on and every type but the digest off; disabled is one.
func internalRulesAre(rules []map[string]any, enabled bool, patterns int) bool {
	wantRules := 2
	if enabled {
		wantRules = 3
	}

	if len(rules) != wantRules || rules[0]["groupName"] != "dependencies" {
		return false
	}

	names, _ := rules[1]["matchPackageNames"].([]any)
	if rules[1]["enabled"] != enabled || len(names) != patterns {
		return false
	}

	if !enabled {
		return true
	}

	// patch: major, minor and replacement held back again, and the four
	// types that are no release
	types, _ := rules[2]["matchUpdateTypes"].([]any)

	return rules[2]["enabled"] == false && len(types) == 7
}

// The internal rule closes again what the level holds back, the digest
// apart, and the types that are no release, so a tagged internal module
// moves as the level says.
func TestRenovateInternalHeld(t *testing.T) {
	t.Parallel()

	for update, want := range map[string]string{
		"patch": "major,minor,replacement,pin,pinDigest,rollback,lockFileMaintenance",
		"minor": "major,replacement,pin,pinDigest,rollback,lockFileMaintenance",
		"major": "pin,pinDigest,rollback,lockFileMaintenance",
	} {
		if got := strings.Join(renovateInternalHeld(update), ","); got != want {
			t.Errorf("%s: %s, want %s", update, got, want)
		}
	}
}

// The config's own projects, as dependencies: named by their remotes, and
// enabled by the last rule of a repository on its dev branch under dev, on
// every branch under always, disabled under never — after the group rule.
func TestRenovateInternalRule(t *testing.T) {
	t.Parallel()

	projects := []*config.Project{
		{Name: "core", Git: "git@git.example.com:group/core.git"},
		{Name: "libs", Git: "https://github.com/someone/libs.git"},
		{Name: "local", Git: "/srv/git/local.git"},
		{Name: "core-again", Git: "https://git.example.com/group/core"},
	}

	patterns := renovateInternalPatterns(projects)
	if strings.Join(patterns, " ") != "git.example.com/group/core git.example.com/group/core/** "+
		"github.com/someone/libs github.com/someone/libs/**" {
		t.Errorf("patterns = %v", patterns)
	}

	api := &config.Project{Name: "api", DevBranch: "develop"}
	group := &renovateGroup{
		Platform: "gitlab", Host: "git.example.com",
		Repos: []renovateRepo{
			{Project: api, Slug: "group/api", Branch: "develop"},
			{Project: api, Slug: "group/api", Branch: "release-1.2"},
		},
	}

	type entry struct {
		Branches     []string         `json:"baseBranchPatterns"` //nolint:tagliatelle // Renovate's key
		PackageRules []map[string]any `json:"packageRules"`
	}

	rulesOf := func(internal string) []entry {
		t.Helper()

		out, err := renovateGlobalConfig(
			&config.Renovate{},
			group,
			renovateOpts{Update: "patch", Group: "all", Internal: internal},
			patterns,
		)
		if err != nil {
			t.Fatal(err)
		}

		var parsed struct {
			Repositories []entry `json:"repositories"`
		}

		if err := json.Unmarshal(out, &parsed); err != nil {
			t.Fatal(err)
		}

		return parsed.Repositories
	}

	for _, tc := range []struct {
		mode         string
		dev, release bool
	}{
		{"dev", true, false},
		{"always", true, true},
		{"never", false, false},
	} {
		repos := rulesOf(tc.mode)

		for pos, want := range []bool{tc.dev, tc.release} {
			if !internalRulesAre(repos[pos].PackageRules, want, len(patterns)) {
				t.Errorf("%s on %s: rules = %v", tc.mode, repos[pos].Branches, repos[pos].PackageRules)
			}
		}
	}

	if renovateInternalWording("dev") != "updated on the dev branch, held back elsewhere" ||
		renovateInternalWording("never") != "held back" {
		t.Error("wording")
	}

	// Without patterns — a config whose projects have no host — no rule.
	out, err := renovateGlobalConfig(&config.Renovate{}, group, renovateOpts{Update: "patch", Group: "separate"}, nil)
	if err != nil || strings.Contains(string(out), "--internal") {
		t.Errorf("no patterns, no rule: %v\n%s", err, out)
	}
}

// --group all puts rt's group rule after the repository's own rules, in
// its entry, so a group of the file's own does not win over it; separate
// adds nothing.
func TestRenovateGroupRule(t *testing.T) {
	t.Parallel()

	ownRule := map[string]any{"matchPackageNames": []any{"otel-*"}, "groupName": "otel"}
	group := &renovateGroup{
		Platform: "gitlab", Host: "git.example.com",
		Repos: []renovateRepo{
			{Slug: "group/api", Branch: "develop", Config: map[string]any{"packageRules": []any{ownRule}}},
			{Slug: "group/worker", Branch: "develop"},
		},
	}

	out, err := renovateGlobalConfig(&config.Renovate{}, group, renovateOpts{Update: "patch", Group: "all"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	var parsed struct {
		Repositories []struct {
			Repository   string           `json:"repository"`
			PackageRules []map[string]any `json:"packageRules"`
		} `json:"repositories"`
	}

	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}

	api, worker := parsed.Repositories[0], parsed.Repositories[1]
	if len(api.PackageRules) != 2 || api.PackageRules[0]["groupName"] != "otel" ||
		api.PackageRules[1]["groupName"] != "dependencies" {
		t.Errorf("api rules = %v", api.PackageRules)
	}

	if len(worker.PackageRules) != 1 || worker.PackageRules[0]["groupName"] != "dependencies" {
		t.Errorf("worker rules = %v", worker.PackageRules)
	}

	if rules, _ := group.Repos[0].Config["packageRules"].([]any); len(rules) != 1 {
		t.Error("the file's own rules must not be changed in place")
	}

	out, err = renovateGlobalConfig(&config.Renovate{}, group, renovateOpts{Update: "patch", Group: "separate"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(out), "rt deps renovate --group all") {
		t.Errorf("separate adds no rule:\n%s", out)
	}

	if renovateGroupWording("all") != "one per project" ||
		renovateGroupWording("separate") != "one per dependency, the project's own groups apart" {
		t.Error("wording")
	}
}

// The token and every NAME=value entry go to the docker process's
// environment, never to its command line, where ps would show them.
func TestRenovateArgsAndEnv(t *testing.T) {
	t.Setenv("GOPROXY", "https://proxy.example.com")
	t.Setenv("GONOSUMDB", "")

	cfg := &config.Renovate{
		Image: "renovate/renovate:44", CacheDir: "/cache",
		Env:        []string{"GOPROXY", "GONOSUMDB", "GOFLAGS=-mod=mod", "GITHUB_COM_TOKEN=ghp-secret"},
		DockerArgs: []string{"--network", "host"},
	}

	flags, env, plan, err := renovateEnv(cfg, "git.example.com", "glpat-secret")
	if err != nil {
		t.Fatal(err)
	}

	args := strings.Join(renovateArgs(cfg, "/tmp/x/config.json", flags, 1000), " ")

	want := "run --rm -e RENOVATE_TOKEN -e RENOVATE_CONFIG_FILE=/config.json -e LOG_FORMAT=json " +
		"-e GOPROXY -e GONOSUMDB -e GOFLAGS -e GITHUB_COM_TOKEN -v /tmp/x/config.json:/config.json:ro " +
		"-v /cache:/tmp/renovate/cache --user 1000:0 --network host renovate/renovate:44"
	if args != want {
		t.Errorf("args:\n%s\nwant:\n%s", args, want)
	}

	if strings.Contains(args, "glpat-secret") || strings.Contains(args, "-mod=mod") {
		t.Error("values must not be on the command line")
	}

	// Without a cache the image's own user runs, unless a netrc is mounted,
	// which that user could not read.
	noCache := &config.Renovate{Image: "renovate/renovate:44"}
	if args := strings.Join(renovateArgs(noCache, "/tmp/x/config.json", nil, 1000), " "); strings.Contains(
		args,
		"--user",
	) {
		t.Errorf("no cache, no netrc: %s", args)
	}

	t.Setenv("NETRC", filepath.Join(t.TempDir(), "netrc"))

	if err := os.WriteFile(os.Getenv("NETRC"), []byte("machine x login u password p\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	withNetrc := &config.Renovate{Image: "renovate/renovate:44", Env: []string{"RENOVATE_HOST_RULES"}}
	if args := strings.Join(renovateArgs(withNetrc, "/tmp/x/config.json", nil, 1000), " "); !strings.Contains(
		args,
		"--user 1000:0",
	) {
		t.Errorf("a mounted netrc needs our uid: %s", args)
	}

	if strings.Contains(plan, "ghp-secret") {
		t.Error("a credential set in the config must not show in the plan")
	}

	joined := strings.Join(env, " ")
	if !strings.Contains(joined, "RENOVATE_TOKEN=glpat-secret") || !strings.Contains(joined, "GOFLAGS=-mod=mod") {
		t.Errorf("env = %q", env)
	}

	if !strings.Contains(joined, "GITHUB_COM_TOKEN=ghp-secret") {
		t.Errorf("the masked value still has to reach the container: %q", env)
	}

	if plan != "GOPROXY, GONOSUMDB (unset), GOFLAGS=-mod=mod, GITHUB_COM_TOKEN=***" {
		t.Errorf("plan = %q", plan)
	}
}

// renovateFixture is mrFixture with a host in the project's git url, so it
// is a repository on a platform; renovate.json is put on develop when asked.
func renovateFixture(t *testing.T, withConfig bool) (fixture, *run.Ctx, *config.Project) {
	t.Helper()

	f, rctx, p := mrFixture(t)
	p.Git = "git@git.example.com:group/parent.git"

	if withConfig {
		writeFile(t, f.parent, "renovate.json", `{"extends":["config:recommended"]}`+"\n")
		commit(t, f.parent, "parent: add renovate config")
		git(t, f.parent, "push", "--quiet", "origin", "develop")
		git(t, f.parent, "fetch", "--quiet", "origin")
	}

	return f, rctx, p
}

// A project stays out with a reason the plan can show; one with a config on
// its target branch is in, with the file named.
//
//nolint:paralleltest // fakeGlab sets PATH
func TestRenovateRepoForSkipReasons(t *testing.T) {
	fakeGlab(t)

	_, rctx, p := renovateFixture(t, false)

	if _, reason, _ := renovateRepoFor(rctx, p, false); !strings.Contains(reason, "no renovate.json on develop") {
		t.Errorf("no config: %q", reason)
	}

	off := false
	p.Renovate = &off

	if _, reason, _ := renovateRepoFor(rctx, p, false); reason != "renovate: false" {
		t.Errorf("opted out: %q", reason)
	}

	p.Renovate = nil
	p.ReleaseBranch = "release-9.9"

	for _, local := range []bool{false, true} {
		if _, reason, _ := renovateRepoFor(rctx, p, local); reason !=
			"branch release-9.9 exists neither locally nor on origin" {
			t.Errorf("a branch nobody has, local=%v: %q", local, reason)
		}
	}

	p.ReleaseBranch = ""

	_, rctx, p = renovateFixture(t, true)

	repo, reason, err := renovateRepoFor(rctx, p, false)
	if err != nil || reason != "" {
		t.Fatalf("a project with a config should be in: %q %v", reason, err)
	}

	if repo.Host != "git.example.com" || repo.Slug != "group/parent" || repo.Branch != "develop" ||
		repo.ConfigFile != "renovate.json" || repo.Platform != "gitlab" || repo.Local {
		t.Errorf("repo = %+v", repo)
	}

	// The file on the branch is read, as the working copy's is.
	if extends, _ := repo.Config["extends"].([]any); len(extends) != 1 || extends[0] != "config:recommended" {
		t.Errorf("the branch's file should be read: %v", repo.Config)
	}

	// ci: none only says no pipeline is watched: Renovate still runs, on
	// the platform the host names.
	p.CI = config.CINone

	if repo, reason, _ := renovateRepoFor(rctx, p, false); reason != "" || repo.Platform != "gitlab" {
		t.Errorf("ci none should still be in, on gitlab: %q %+v", reason, repo)
	}

	if got := renovatePlatform(&config.Project{CI: config.CINone}, "github.com"); got != "github" {
		t.Errorf("github.com with ci none = %q", got)
	}

	p.CI = config.CIGitLab

	p.Git = "/srv/git/parent.git"

	if _, reason, _ := renovateRepoFor(rctx, p, false); reason != "git url has no host" {
		t.Errorf("local url: %q", reason)
	}
}

// --local-config reads the working copy, committed or not: a project with
// no config on its branch is in, and the file's keys reach the repository's
// entry with the branch's file left unread.
//
//nolint:paralleltest // fakeGlab sets PATH
func TestRenovateLocalConfig(t *testing.T) {
	fakeGlab(t)

	f, rctx, p := renovateFixture(t, false)

	if _, reason, _ := renovateRepoFor(rctx, p, true); reason != "no renovate.json in the working copy" {
		t.Errorf("no file: %q", reason)
	}

	writeFile(t, f.parent, "renovate.json", "{not json")

	if _, reason, _ := renovateRepoFor(rctx, p, true); !strings.Contains(reason, "is not valid JSON") {
		t.Errorf("broken file: %q", reason)
	}

	writeFile(t, f.parent, "renovate.json", `{"separateMinorPatch": false}`)

	if _, reason, _ := renovateRepoFor(rctx, p, true); !strings.Contains(
		reason,
		"sets separateMinorPatch, which --update owns",
	) {
		t.Errorf("a level key in the file: %q", reason)
	}

	writeFile(t, f.parent, "renovate.json", `{"baseBranches": ["develop"]}`)

	if _, reason, _ := renovateRepoFor(rctx, p, true); !strings.Contains(
		reason,
		"sets baseBranches, Renovate's old name: use baseBranchPatterns",
	) {
		t.Errorf("the old key in the file: %q", reason)
	}

	writeFile(t, f.parent, "renovate.json",
		`{"$schema":"https://docs.renovatebot.com/renovate-schema.json","labels":["dependencies"]}`)

	repo, reason, err := renovateRepoFor(rctx, p, true)
	if err != nil || reason != "" || repo.ConfigFile != "renovate.json" {
		t.Fatalf("an uncommitted config should be used: %q %v %+v", reason, err, repo)
	}

	group := &renovateGroup{Platform: "gitlab", Repos: []renovateRepo{repo}}

	out, err := renovateGlobalConfig(
		&config.Renovate{},
		group,
		renovateOpts{DryRun: "full", Update: "major", Group: "separate"},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	want := `"repositories": [
    {
      "baseBranchPatterns": [
        "develop"
      ],
      "labels": [
        "dependencies"
      ],
      "repository": "group/parent",
      "requireConfig": "ignored"
    }
  ]`
	if !strings.Contains(string(out), want) || strings.Contains(string(out), "$schema") {
		t.Errorf("config:\n%s", out)
	}

	if plan := strings.Join(renovateGroupStep(rctx, group, renovateOpts{}).Plan, "\n"); !strings.Contains(plan,
		"group/parent on develop (renovate.json from the working copy, internal modules too)") {
		t.Errorf("plan:\n%s", plan)
	}

	// The file's own base branches win over rt's, so the plan shows them.
	repo.Config["baseBranchPatterns"] = []any{"main", "release-*"}
	group.Repos[0] = repo

	if plan := strings.Join(renovateGroupStep(rctx, group, renovateOpts{}).Plan, "\n"); !strings.Contains(plan,
		"group/parent on main, release-* (renovate.json from the working copy)") {
		t.Errorf("plan with the file's branches:\n%s", plan)
	}
}

// A hosts entry shows in the plan of that host's run alone, and a hosts
// entry no project is on is a typo repo check names.
func TestRenovateHostsEntry(t *testing.T) {
	t.Parallel()

	rctx := testCtx()

	empty := []string{}
	rctx.Cfg.Renovate = config.Renovate{
		Image: "renovate/renovate:44", Docker: "docker",
		Env: []string{"GOPROXY=https://proxy.example.com"},
		Hosts: map[string]config.RenovateHost{
			"github.com": {Env: &empty},
			"gitub.com":  {Env: &empty},
		},
	}

	repo := renovateRepo{Project: &config.Project{Name: "libs"}, Slug: "owner/libs", Branch: "master"}

	for host, want := range map[string]string{
		"github.com":      "env none (hosts: github.com)",
		"git.example.com": "env GOPROXY=https://proxy.example.com",
	} {
		group := &renovateGroup{Platform: "github", Host: host, Repos: []renovateRepo{repo}}

		_, _, envPlan, err := renovateEnv(rctx.Cfg.Renovate.ForHost(host), host, "")
		if err != nil {
			t.Fatal(err)
		}

		group.EnvPlan = envPlan

		if plan := strings.Join(renovateGroupStep(rctx, group, renovateOpts{}).Plan, "\n"); !strings.Contains(
			plan,
			want,
		) {
			t.Errorf("%s plan:\n%s\nwant %q", host, plan, want)
		}
	}

	projects := []*config.Project{{Name: "libs", Git: "git@github.com:owner/libs.git"}}
	if issues := checkRenovateHosts(&rctx.Cfg.Renovate, projects); len(issues) != 1 ||
		issues[0] != "hosts: gitub.com is no project's git host" {
		t.Errorf("issues = %v", issues)
	}
}

func TestGroupRenovateByPlatformAndHost(t *testing.T) {
	t.Parallel()

	onGitLab := &config.Project{Name: "a", CI: config.CIGitLab}
	onGitHub := &config.Project{Name: "b", CI: config.CIGitHub}

	groups := groupRenovate([]renovateRepo{
		{Project: onGitLab, Platform: "gitlab", Host: "git.example.com", Slug: "g/a"},
		{Project: onGitHub, Platform: "github", Host: "github.com", Slug: "o/b"},
		{Project: onGitLab, Platform: "gitlab", Host: "git.example.com", Slug: "g/c"},
	})

	if len(groups) != 2 || groups[0].Platform != "gitlab" || len(groups[0].Repos) != 2 ||
		groups[1].Host != "github.com" {
		t.Errorf("groups = %+v", groups)
	}
}

// The token comes from the configured variable, else from glab's own store
// for the host; neither is an error that names both ways out.
func TestRenovateToken(t *testing.T) {
	fakeGlab(t)

	cfg := &config.Renovate{TokenEnv: "RT_TEST_RENOVATE_TOKEN"}

	t.Setenv("RT_TEST_RENOVATE_TOKEN", "from-env")

	if token, source, err := renovateToken(cfg, config.CIGitLab, "git.example.com"); err != nil ||
		token != "from-env" || source != "$RT_TEST_RENOVATE_TOKEN" {
		t.Errorf("env should win: %q %q %v", token, source, err)
	}

	t.Setenv("RT_TEST_RENOVATE_TOKEN", "")

	if token, source, err := renovateToken(cfg, config.CIGitLab, "git.example.com"); err != nil ||
		token != "glpat-from-glab" || source != "glab (git.example.com)" {
		t.Errorf("glab fallback: %q %q %v", token, source, err)
	}

	t.Setenv("PATH", t.TempDir())

	if _, _, err := renovateToken(cfg, config.CIGitLab, "git.example.com"); err == nil ||
		!strings.Contains(err.Error(), "no renovate token for git.example.com") {
		t.Errorf("no token anywhere must be an error: %v", err)
	}
}

// One variable's token serves one host: with two hosts in the run it is
// refused, a token_env of the host's own is not.
func TestRenovateTokenClash(t *testing.T) {
	t.Parallel()

	groups := []*renovateGroup{
		{Host: "git.example.com", TokenSource: "$RENOVATE_TOKEN"},
		{Host: "github.com", TokenSource: "$RENOVATE_TOKEN"},
	}

	if err := renovateTokenClash(groups); err == nil || err.Error() !=
		"$RENOVATE_TOKEN is set and would go to every host of the run (git.example.com, github.com): "+
			"give each host its own token_env under hosts, or unset it" {
		t.Errorf("err = %v", err)
	}

	groups[1].TokenSource = "$GITHUB_TOKEN"

	if err := renovateTokenClash(groups); err != nil {
		t.Errorf("two variables: %v", err)
	}

	groups[1].TokenSource = "gh (github.com)"

	if err := renovateTokenClash(groups); err != nil {
		t.Errorf("a tool's token: %v", err)
	}
}

// fakeDocker puts a docker on PATH that records its arguments and the token
// it got, keeps a copy of the config it was given, prints canned log lines
// and exits with $FAKE_DOCKER_EXIT.
func fakeDocker(t *testing.T, lines ...string) string {
	t.Helper()

	dir := t.TempDir()
	log := filepath.Join(dir, "docker.log")
	canned := filepath.Join(dir, "lines.jsonl")

	if err := os.WriteFile(canned, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	script := `#!/bin/sh
echo "args: $*" >> "$FAKE_DOCKER_LOG"
echo "token: $RENOVATE_TOKEN" >> "$FAKE_DOCKER_LOG"
for a in "$@"; do
  case "$a" in *:/config.json:ro) cp "${a%:/config.json:ro}" "$FAKE_DOCKER_LOG.config" ;; esac
done
cat "` + canned + `"
exit "${FAKE_DOCKER_EXIT:-0}"
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("FAKE_DOCKER_LOG", log)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return log
}

// The whole run with a fake docker: the plan, the container's arguments and
// environment, the config it read, and the report of what it did.
func TestDepsRenovateRunsDockerAndReports(t *testing.T) {
	fakeGlab(t)

	log := fakeDocker(t,
		`{"level":30,"msg":"Repository started","repository":"group/parent"}`,
		`{"level":30,"msg":"PR created","repository":"group/parent","pr":7,"prTitle":"update thing to v2"}`,
		`{"level":40,"msg":"something odd","repository":"group/parent"}`,
	)

	_, rctx, p := renovateFixture(t, true)
	rctx.Cfg.Renovate = config.Renovate{Image: "renovate/renovate:44", Docker: "docker", TokenEnv: "RT_TEST_NO_TOKEN"}

	var (
		steps []run.Step
		err   error
	)

	logFile := filepath.Join(t.TempDir(), "renovate.jsonl")

	out := captureOutput(t, func() {
		steps, err = planRenovate(rctx, []*config.Project{p}, renovateOpts{DryRun: "full", LogFile: logFile})
		if err == nil {
			err = run.Execute(steps, true)
		}
	})
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}

	plan := strings.Join(steps[0].Plan, "\n")
	wantPlan := []string{
		"dry-run: full",
		"token from glab (git.example.com)",
		"group/parent on develop (renovate.json, internal modules too)",
	}

	for _, want := range wantPlan {
		if !strings.Contains(plan, want) {
			t.Errorf("plan lacks %q:\n%s", want, plan)
		}
	}

	for _, want := range []string{
		"==> gitlab git.example.com", "==> parent", "MR !7 created: update thing to v2",
		"WARNING: something odd", "1 MR created, 0 updated, 1 warning(s), 0 error(s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	calls, _ := os.ReadFile(log)
	if !strings.Contains(string(calls), "token: glpat-from-glab") ||
		strings.Contains(string(calls), "args: run --rm -e RENOVATE_TOKEN=") {
		t.Errorf("the token should come through the environment only:\n%s", calls)
	}

	config, _ := os.ReadFile(log + ".config")
	if !strings.Contains(string(config), `"repository": "group/parent"`) ||
		!strings.Contains(string(config), `"dryRun": "full"`) {
		t.Errorf("config given to the container:\n%s", config)
	}

	// A non-zero exit fails the step.
	t.Setenv("FAKE_DOCKER_EXIT", "1")

	captureOutput(t, func() { err = run.Execute(steps, true) })

	if err == nil || !strings.Contains(err.Error(), "stopped at gitlab git.example.com") {
		t.Errorf("exit 1 must fail the step: %v", err)
	}

	// The log file collects every host's run: the steps append, the
	// command truncates once before them.
	raw, _ := os.ReadFile(logFile)
	if got := strings.Count(string(raw), "\n"); got != 6 || strings.Count(string(raw), `"PR created"`) != 2 {
		t.Errorf("log file should hold both runs' lines, has %d:\n%s", got, raw)
	}
}
