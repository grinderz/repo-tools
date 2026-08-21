package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/grinderz/repo-tools/internal/config"
	"github.com/grinderz/repo-tools/internal/gitx"
	"github.com/grinderz/repo-tools/internal/run"
)

func newStatusCmd(rctx *run.Ctx, name string) *cobra.Command {
	return &cobra.Command{
		Use:   name + " [project...]",
		Short: "Show branch, dirty state, ahead/behind and submodule pins",
		RunE: func(_ *cobra.Command, args []string) error {
			projects, err := rctx.Select(args)
			if err != nil {
				return err
			}

			for _, p := range projects {
				reportStatus(rctx, p)
			}

			return nil
		},
	}
}

func reportStatus(rctx *run.Ctx, p *config.Project) {
	// The padded ==> header keeps the rows scannable as a table while the
	// project name gets the same marker every other per-project block has.
	header := run.Cyan("==>") + " " + padCell(p.Name, statusNameWidth, run.Bold)
	r := repoOf(p)

	if reason := missingRepo(r); reason != "" {
		fmt.Println(header + reason)

		return
	}

	// Reporting is read-only and meant to be quick, so --fetch is opt-in here.
	if rctx.WantFetchFor(p, false) {
		if reason := fetchOrWarn(rctx, p, r); reason != "" {
			fmt.Println(header + reason)
		}
	}

	branch, err := r.CurrentBranch()
	if err != nil {
		fmt.Printf("%s%v\n", header, err)

		return
	}

	state := "clean"

	if clean, err := r.IsClean(); err == nil && !clean {
		state = "dirty"
	}

	fmt.Printf("%s%-22s %-6s %-10s latest:%s\n",
		header, branch, state, syncState(r, branch), latestReleaseName(r, p))

	reportSubmodules(r)
}

// syncState reports the ahead/behind counts of branch against its upstream.
func syncState(r gitx.Repo, branch string) string {
	if !r.RemoteBranchExists(branch) {
		return "no upstream"
	}

	ahead, behind, err := r.AheadBehind("origin/" + branch)
	if err != nil {
		return "unknown"
	}

	return fmt.Sprintf("+%d/-%d", ahead, behind)
}

func latestReleaseName(r gitx.Repo, p *config.Project) string {
	branches, err := r.RemoteBranches()
	if err != nil {
		return "-"
	}

	name, _, ok := gitx.LatestReleaseBranch(branches, p.ReleaseBranchPrefix)
	if !ok {
		return "-"
	}

	return name
}

func reportSubmodules(r gitx.Repo) {
	paths, err := r.SubmodulePaths()
	if err != nil {
		return
	}

	for _, path := range paths {
		branch, err := r.SubmoduleBranch(path)
		if err != nil || branch == "" {
			branch = "(no branch)"
		}

		fmt.Printf("    %-18s %-22s %s\n", path, branch, submodulePin(r, path))
	}
}

func submodulePin(r gitx.Repo, path string) string {
	out, err := r.Git("submodule", "status", "--", path)
	if err != nil {
		return "-"
	}

	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) == 0 {
		return "-"
	}

	return shorten(strings.TrimLeft(fields[0], "+-U"), shortPinLen)
}
