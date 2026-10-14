package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/grinderz/repo-tools/internal/gitx"
)

// Two names in renovate.env Renovate itself defines, and rt can fill when
// the shell has not: GITHUB_COM_TOKEN from gh's login, RENOVATE_HOST_RULES
// from ~/.netrc. A value in the environment always wins.
const (
	envGitHubToken = "GITHUB_COM_TOKEN" //nolint:gosec // the variable's name, not a credential
	envHostRules   = "RENOVATE_HOST_RULES"
)

// The netrc keywords that take a value.
const (
	netrcMachine  = "machine"
	netrcLogin    = "login"
	netrcPassword = "password"
	netrcAccount  = "account"
)

// ghToken is gh's token for github.com, empty when gh is not there or not
// logged in: the lookups of releases and github.com modules are
// rate-limited without one, which is a warning, not a stop.
func ghToken() string {
	out, err := gitx.Repo{}.Stdout("gh", "auth", "token", "--hostname", githubHost)
	token := strings.TrimSpace(out)

	if err != nil || token == "" || strings.ContainsAny(token, " \t\n") {
		return ""
	}

	return token
}

// netrcPath is the file the netrc rules come from: $NETRC when set, as
// curl and python read it, else ~/.netrc.
func netrcPath() string {
	if path := os.Getenv("NETRC"); path != "" {
		return path
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	return filepath.Join(home, ".netrc")
}

// netrcEntry is one machine entry of a netrc file.
type netrcEntry struct {
	Machine  string
	Login    string
	Password string
}

// parseNetrc reads the machine entries of a netrc file, in order: the
// keywords machine, login, password and account, default (skipped, it
// names no host) and macdef (skipped with its lines, to the blank one).
// Words are split on whitespace as go and git read the file; a quoted
// value with a space in it, which curl allows, is not understood.
func parseNetrc(data string) []netrcEntry {
	var (
		entries []netrcEntry
		current *netrcEntry
	)

	tokens := netrcTokens(data)

	for pos := 0; pos < len(tokens); pos++ {
		value := ""
		if pos+1 < len(tokens) {
			value = tokens[pos+1]
		}

		switch tokens[pos] {
		case netrcMachine:
			entries = append(entries, netrcEntry{Machine: value})
			current = &entries[len(entries)-1]
			pos++
		case "default":
			current = nil
		case netrcLogin, netrcPassword, netrcAccount:
			if current != nil {
				current.set(tokens[pos], value)
			}

			pos++
		}
	}

	return entries
}

// netrcTokens is the file as words, a macdef and the lines of its body
// left out.
func netrcTokens(data string) []string {
	var tokens []string

	inMacro := false

	for line := range strings.SplitSeq(data, "\n") {
		if inMacro {
			inMacro = strings.TrimSpace(line) != ""

			continue
		}

		fields := strings.Fields(line)
		if pos := slices.Index(fields, "macdef"); pos >= 0 {
			fields, inMacro = fields[:pos], true
		}

		tokens = append(tokens, fields...)
	}

	return tokens
}

func (entry *netrcEntry) set(key, value string) {
	switch key {
	case netrcLogin:
		entry.Login = value
	case netrcPassword:
		entry.Password = value
	}
}

// netrcHostRules is Renovate's hostRules JSON for the netrc entries with a
// login and a password, except the hosts in skip (the platform, whose
// token rt already passes, and github.com, whose token is its own
// variable), and the hosts it covers for the plan. Nothing in the file
// means no rules and no error.
func netrcHostRules(path string, skip []string) (string, []string, error) {
	if path == "" {
		return "", nil, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil, nil
		}

		return "", nil, fmt.Errorf("%s: %w", path, err)
	}

	var (
		rules []map[string]string
		hosts []string
	)

	for _, entry := range parseNetrc(string(data)) {
		if entry.Login == "" || entry.Password == "" || slices.Contains(skip, entry.Machine) ||
			slices.Contains(hosts, entry.Machine) {
			continue
		}

		rules = append(rules, map[string]string{
			"matchHost": entry.Machine,
			"username":  entry.Login,
			"password":  entry.Password,
		})
		hosts = append(hosts, entry.Machine)
	}

	if len(rules) == 0 {
		return "", nil, nil
	}

	out, err := json.Marshal(rules)
	if err != nil {
		return "", nil, fmt.Errorf("%s: %w", path, err)
	}

	return string(out), hosts, nil
}
