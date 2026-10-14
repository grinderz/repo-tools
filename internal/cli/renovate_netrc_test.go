package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grinderz/repo-tools/internal/config"
)

func TestParseNetrc(t *testing.T) {
	t.Parallel()

	data := `machine one.example.com login alice password s3cret
machine two.example.com
  login bob
  password hunter2
  account ignored
default login anon password anon
macdef init
  echo this is a macro
  machine not.a.host login x password y

machine three.example.com login carol
machine one.example.com login again password twice
`

	got := parseNetrc(data)
	want := []netrcEntry{
		{Machine: "one.example.com", Login: "alice", Password: "s3cret"},
		{Machine: "two.example.com", Login: "bob", Password: "hunter2"},
		{Machine: "three.example.com", Login: "carol"},
		{Machine: "one.example.com", Login: "again", Password: "twice"},
	}

	if len(got) != len(want) {
		t.Fatalf("entries = %+v", got)
	}

	for pos := range want {
		if got[pos] != want[pos] {
			t.Errorf("entry %d = %+v, want %+v", pos, got[pos], want[pos])
		}
	}
}

// The rules take the complete entries, skip the hosts rt covers otherwise
// and the duplicates, and a missing file is simply no rules.
func TestNetrcHostRules(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "netrc")

	data := "machine git.example.com login me password glpat-x\n" +
		"machine github.com login me password ghp-x\n" +
		"machine pypi.example.com login alice password s3cret\n" +
		"machine pypi.example.com login bob password other\n" +
		"machine half.example.com login nobody\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	rules, hosts, err := netrcHostRules(path, []string{"git.example.com", "github.com"})
	if err != nil {
		t.Fatal(err)
	}

	if rules != `[{"matchHost":"pypi.example.com","password":"s3cret","username":"alice"}]` {
		t.Errorf("rules = %s", rules)
	}

	if strings.Join(hosts, ",") != "pypi.example.com" {
		t.Errorf("hosts = %v", hosts)
	}

	if rules, hosts, err := netrcHostRules(filepath.Join(dir, "missing"), nil); err != nil || rules != "" ||
		hosts != nil {
		t.Errorf("a missing file: %q %v %v", rules, hosts, err)
	}

	if rules, _, err := netrcHostRules("", nil); err != nil || rules != "" {
		t.Errorf("no path: %q %v", rules, err)
	}
}

// GITHUB_COM_TOKEN and RENOVATE_HOST_RULES listed but unset come from gh
// and ~/.netrc; a value in the environment wins over both.
func TestRenovateEnvFallbacks(t *testing.T) {
	dir := t.TempDir()

	gh := "#!/bin/sh\n[ \"$1 $2 $3 $4\" = \"auth token --hostname github.com\" ] || exit 1\necho ghp-from-gh\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(gh), 0o755); err != nil {
		t.Fatal(err)
	}

	netrc := filepath.Join(dir, "netrc")
	if err := os.WriteFile(netrc, []byte("machine pypi.example.com login alice password s3cret\n"+
		"machine git.example.com login me password glpat-x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("NETRC", netrc)
	t.Setenv("GITHUB_COM_TOKEN", "")
	t.Setenv("RENOVATE_HOST_RULES", "")

	cfg := &config.Renovate{Env: []string{"GITHUB_COM_TOKEN", "RENOVATE_HOST_RULES"}}

	flags, env, plan, err := renovateEnv(cfg, "git.example.com", "tok")
	if err != nil {
		t.Fatal(err)
	}

	if joined := strings.Join(flags, " "); !strings.Contains(joined, "-v "+netrc+":/tmp/renovate/netrc:ro") {
		t.Errorf("the netrc should be mounted for the tools: %q", flags)
	}

	// The tools get the file's name through Renovate's config, with a
	// customEnvVariables of the user's own kept.
	own := &config.Renovate{
		Env:    cfg.Env,
		Config: map[string]any{"customEnvVariables": map[string]any{"CGO_ENABLED": "0"}},
	}

	global, err := renovateGlobalConfig(own, &renovateGroup{Platform: "gitlab", Host: "git.example.com"},
		renovateOpts{Update: "patch", Group: "all"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(global), `"NETRC": "/tmp/renovate/netrc"`) ||
		!strings.Contains(string(global), `"CGO_ENABLED": "0"`) {
		t.Errorf("config:\n%s", global)
	}

	if custom, _ := own.Config["customEnvVariables"].(map[string]any); len(custom) != 1 {
		t.Error("the config's own map must not be changed in place")
	}

	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "GITHUB_COM_TOKEN=ghp-from-gh") ||
		!strings.Contains(
			joined,
			`RENOVATE_HOST_RULES=[{"matchHost":"pypi.example.com","password":"s3cret","username":"alice"}]`,
		) {
		t.Errorf("env = %q", env)
	}

	if plan != "~/.netrc mounted for go and uv, GITHUB_COM_TOKEN (gh), RENOVATE_HOST_RULES (~/.netrc: pypi.example.com)" {
		t.Errorf("plan = %q", plan)
	}

	t.Setenv("GITHUB_COM_TOKEN", "ghp-mine")
	t.Setenv("RENOVATE_HOST_RULES", "[]")

	_, env, plan, err = renovateEnv(cfg, "git.example.com", "tok")
	if err != nil {
		t.Fatal(err)
	}

	if len(env) != 1 || plan != "~/.netrc mounted for go and uv, GITHUB_COM_TOKEN, RENOVATE_HOST_RULES" {
		t.Errorf("the environment must win, the netrc still goes to the tools: %q %q", env, plan)
	}

	t.Setenv("GITHUB_COM_TOKEN", "")
	t.Setenv("RENOVATE_HOST_RULES", "")
	t.Setenv("NETRC", filepath.Join(dir, "none"))
	t.Setenv("PATH", dir+"/nowhere")

	flags, env, plan, err = renovateEnv(cfg, "git.example.com", "tok")
	if err != nil {
		t.Fatal(err)
	}

	if len(env) != 1 || plan != "GITHUB_COM_TOKEN (unset), RENOVATE_HOST_RULES (unset)" ||
		strings.Contains(strings.Join(flags, " "), "netrc") {
		t.Errorf("nothing to fall back on: %q %q %q", env, plan, flags)
	}
}

// A host rules value set in the config is a credential: masked in the
// plan, passed in the environment; the netrc still goes to the tools.
func TestRenovateEnvHostRulesValue(t *testing.T) {
	netrc := filepath.Join(t.TempDir(), "netrc")
	if err := os.WriteFile(netrc, []byte("machine x login u password p\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("NETRC", netrc)

	valued := &config.Renovate{Env: []string{
		`RENOVATE_HOST_RULES=[{"matchHost":"x","password":"p"}]`,
		"GOPROXY=https://user:s3cret@proxy.example.com|direct", // a password by its shape, not its name
		"NPM_CONFIG__AUTH=dXNlcjpwYXNz",                        // and by its name
	}}

	flags, env, plan, err := renovateEnv(valued, "git.example.com", "tok")
	if err != nil {
		t.Fatal(err)
	}

	if plan != "~/.netrc mounted for go and uv, RENOVATE_HOST_RULES=***, GOPROXY=***, NPM_CONFIG__AUTH=***" ||
		!strings.Contains(strings.Join(env, "\n"), "GOPROXY=https://user:s3cret@proxy.example.com|direct") ||
		!strings.Contains(strings.Join(env, "\n"), `"password":"p"`) ||
		!strings.Contains(strings.Join(flags, " "), "-v "+netrc+":/tmp/renovate/netrc:ro") {
		t.Errorf("a host rules value: %q %q %q", plan, env, flags)
	}
}
