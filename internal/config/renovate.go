package config

import (
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Renovate is how deps renovate runs Renovate: the image, the docker program,
// where the platform is, where the token comes from, what of rt's environment
// the container sees, and any raw global config to overlay on the file rt
// generates. The rules of each repository stay in its own renovate.json.
type Renovate struct {
	// Image is the Renovate image; a registry mirror prefix goes in the value.
	Image string `yaml:"image"`
	// Docker is the program that runs the image; podman works too.
	Docker string `yaml:"docker"`
	// DockerArgs are extra arguments placed before the image: a network
	// mode, a CA certificate mount, an extra environment variable.
	DockerArgs []string `yaml:"docker_args"`
	// Endpoint is the platform API; empty derives it from the projects'
	// git host (https://<host>/api/v4/ for GitLab).
	Endpoint string `yaml:"endpoint"`
	// TokenEnv names the variable holding the platform token; unset falls
	// back to glab's or gh's own token for the host.
	TokenEnv string `yaml:"token_env"`
	// Update is how far a run may move a dependency: patch (the default),
	// minor or major; --update overrides it for one run.
	Update string `yaml:"update"`
	// Internal is whether the config's own projects move as dependencies
	// of each other: dev (the default) lets Renovate take them to their
	// head on the dev branch alone, where deps freeze has not pinned them;
	// always and never say so for every branch; --internal overrides it.
	Internal string `yaml:"internal"`
	// Group is how the updates are cut into merge requests: all (the
	// default) puts a project's updates in one, separate leaves it to
	// Renovate, one per dependency unless the project's own rules group
	// them; --group overrides it for one run.
	Group string `yaml:"group"`
	// Env is what of rt's environment the container sees: NAME passes a
	// variable through, NAME=value sets one. The projects' .envrc files are
	// not read, since one run spans several projects.
	Env []string `yaml:"env"`
	// CacheDir is mounted as Renovate's cache, so lookups survive a run.
	CacheDir string `yaml:"cache_dir"`
	// Config is raw Renovate global config overlaid on the generated file:
	// prConcurrentLimit, prHourlyLimit, hostRules and whatever else.
	Config map[string]any `yaml:"config"`
	// Hosts adjusts the run on one platform host: a repository on another
	// platform rarely wants the work proxy or the work host rules.
	Hosts map[string]RenovateHost `yaml:"hosts"`
}

// RenovateHost is what the run on one host does differently.
type RenovateHost struct {
	// TokenEnv names the variable this host's token comes from, in place
	// of the block's: the shell's one token cannot serve two platforms.
	TokenEnv string `yaml:"token_env"`
	// Env replaces the env list for this host when it is given, an empty
	// list included: the container then sees none of rt's environment.
	Env *[]string `yaml:"env"`
	// Config is overlaid on the config block key by key.
	Config map[string]any `yaml:"config"`
}

// ForHost is the block as the run on host sees it, the host's own entry
// applied.
func (r *Renovate) ForHost(host string) *Renovate {
	entry, ok := r.Hosts[host]
	if !ok {
		return r
	}

	out := *r
	out.TokenEnv = firstNonEmpty(entry.TokenEnv, r.TokenEnv)

	if entry.Env != nil {
		out.Env = *entry.Env
	}

	out.Config = maps.Clone(r.Config)
	if out.Config == nil {
		out.Config = map[string]any{}
	}

	maps.Copy(out.Config, entry.Config)

	return &out
}

// DefaultRenovateImage is the image deps renovate runs without a config.
const DefaultRenovateImage = "renovate/renovate:44"

// renovateReserved are the generated file's own keys: rt writes them from
// the projects and the flags, so a value in renovate.config would be
// silently overwritten or, worse, win.
//
//nolint:gochecknoglobals // a fixed table
var renovateReserved = append([]string{"platform", "endpoint", "token", "repositories", "dryRun"}, RenovateLevelKeys...)

// RenovateLevelKeys are the keys the update level sets in the global config.
// A repository's own file wins over the global one, so a project setting
// them undoes --update for itself: repo check and --local-config say so.
//
//nolint:gochecknoglobals // a fixed table
var RenovateLevelKeys = []string{"separateMajorMinor", "separateMinorPatch"}

// RenovateUpdates are the update levels, each one including those before it.
//
//nolint:gochecknoglobals // a fixed table
var RenovateUpdates = []string{"patch", "minor", "major"}

// RenovateInternals are the ways the config's own projects move as
// dependencies of each other.
//
//nolint:gochecknoglobals // a fixed table
var RenovateInternals = []string{"dev", "always", "never"}

// RenovateGroups are the ways the updates are cut into merge requests.
//
//nolint:gochecknoglobals // a fixed table
var RenovateGroups = []string{"all", "separate"}

var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// IsEnvName says whether a word is a shell variable name.
func IsEnvName(name string) bool {
	return envNameRe.MatchString(name)
}

func (r *Renovate) applyDefaults() error {
	r.Image = firstNonEmpty(r.Image, DefaultRenovateImage)
	r.Docker = firstNonEmpty(r.Docker, "docker")
	r.TokenEnv = firstNonEmpty(r.TokenEnv, "RENOVATE_TOKEN")

	if !envNameRe.MatchString(r.TokenEnv) {
		return fmt.Errorf("renovate: token_env: %q %w", r.TokenEnv, errBadEnvName)
	}

	r.Update = firstNonEmpty(r.Update, "patch")
	r.CacheDir = expandHome(r.CacheDir)

	// A relative path on docker's -v names a volume, not a directory.
	if r.CacheDir != "" && !filepath.IsAbs(r.CacheDir) {
		abs, err := filepath.Abs(r.CacheDir)
		if err != nil {
			return fmt.Errorf("renovate: cache_dir: %w", err)
		}

		r.CacheDir = abs
	}

	r.Group = firstNonEmpty(r.Group, "all")
	r.Internal = firstNonEmpty(r.Internal, "dev")

	if !slices.Contains(RenovateUpdates, r.Update) {
		return fmt.Errorf("renovate: update: %q %w", r.Update, errBadRenovateUpdate)
	}

	if !slices.Contains(RenovateGroups, r.Group) {
		return fmt.Errorf("renovate: group: %q %w", r.Group, errBadRenovateGroup)
	}

	if !slices.Contains(RenovateInternals, r.Internal) {
		return fmt.Errorf("renovate: internal: %q %w", r.Internal, errBadRenovateInternal)
	}

	if err := validateRenovateBlock("renovate", r.Env, r.Config); err != nil {
		return err
	}

	for host, entry := range r.Hosts {
		var env []string
		if entry.Env != nil {
			env = *entry.Env
		}

		if err := validateRenovateBlock("renovate: hosts: "+host, env, entry.Config); err != nil {
			return err
		}

		if entry.TokenEnv != "" && !envNameRe.MatchString(entry.TokenEnv) {
			return fmt.Errorf("renovate: hosts: %s: token_env: %q %w", host, entry.TokenEnv, errBadEnvName)
		}
	}

	return nil
}

// renovateOwnEnv are the variables deps renovate sets in the container
// itself: a value from the config would replace the platform token, or
// the config file, under its feet.
//
//nolint:gochecknoglobals // a fixed table
var renovateOwnEnv = []string{"RENOVATE_TOKEN", "RENOVATE_CONFIG_FILE", "LOG_FORMAT"}

// validateRenovateBlock checks an env list and a config overlay, the top
// block's or a host's.
func validateRenovateBlock(where string, env []string, cfg map[string]any) error {
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if !envNameRe.MatchString(name) {
			return fmt.Errorf("%s: env: %q %w", where, entry, errBadEnvName)
		}

		if slices.Contains(renovateOwnEnv, name) {
			return fmt.Errorf("%s: env: %s %w", where, name, errRenovateReserved)
		}
	}

	for _, key := range renovateReserved {
		if _, ok := cfg[key]; ok {
			return fmt.Errorf("%s: config: %s %w", where, key, errRenovateReserved)
		}
	}

	// The level's rule goes in front of these, so a value of another shape
	// would be dropped on the way.
	if rules, ok := cfg["packageRules"]; ok {
		if _, isList := rules.([]any); !isList {
			return fmt.Errorf("%s: config: packageRules %w", where, errRenovateRulesNotAList)
		}
	}

	return nil
}

// SplitRemote takes a git url apart into its host and its path, the way a
// platform names a repository: git@host:group/name.git and
// https://host/group/name.git both give host and group/name. A local path
// has no host.
func SplitRemote(url string) (string, string, bool) {
	rest := strings.TrimSpace(url)

	schemed := false

	for _, scheme := range []string{"ssh://", "git://", "https://", "http://"} {
		if after, ok := strings.CutPrefix(rest, scheme); ok {
			rest, schemed = after, true

			break
		}
	}

	// user@ or user:password@ goes; the authority ends at the first / of a
	// url and the first : of the scp form, an @ in the path is the path's.
	authority, _, _ := strings.Cut(rest, "/")
	if !schemed {
		authority, _, _ = strings.Cut(rest, ":")
	}

	if at := strings.LastIndex(authority, "@"); at >= 0 {
		rest = rest[at+1:]
	}

	var host, path string

	if schemed {
		host, path, _ = strings.Cut(rest, "/")

		// An ssh port is the transport's; an http one is part of the
		// platform's address and of its API endpoint.
		if !strings.HasPrefix(url, "http") {
			host, _, _ = strings.Cut(host, ":")
		}
	} else {
		host, path, _ = strings.Cut(rest, ":")
	}

	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")

	if host == "" || path == "" || strings.ContainsAny(host, "/\\") || !strings.Contains(host, ".") {
		return "", "", false
	}

	return host, path, true
}
