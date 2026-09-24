# repo-tools

Batch workflows over a configured set of git repositories that use submodules.
Today that is the release flow — changelog generation, release branches,
dependency freezing, release candidate tags and cherry-picks from the dev
branch into the release branch — and the command groups leave room for more.

## Build

```sh
make build          # -> artifacts/rt
make install        # -> $GOBIN/rt
```

Or without a checkout:

```sh
go install github.com/grinderz/repo-tools/cmd/rt@latest
```

`rt help --all` prints the help of every command at once — eighteen screens
otherwise, and the only practical way to grep for a flag. `make help` lists
every target. The other useful ones are `make test` (gotestsum, race
detector), `make test.repeat` (twice in one process, shuffled),
`make test.coverage` (`scripts/check-coverage.sh` fails the run below
`GO_TEST_COVERAGE_THRESHOLD`),
`make test.docker` / `make test.docker.coverage` (the same in the golang
image), `make lint` (go vet, golangci-lint, shellcheck and the pre-commit
hooks),
`make lint.fix` and `make go.format`.

## Configuration

Copy `config.example.yaml` to `config.yaml` and fill in the real projects;
`config.yaml` and `config-*.yaml` are gitignored, since they carry the real
project list and remote URLs. The config is looked up in this order:
`--config/-c`, `$REPO_TOOLS_CONFIG`, `./config.yaml`,
`~/.config/repo-tools/config.yaml`.

The checkout ships an `.envrc` that points `$REPO_TOOLS_CONFIG` at its
`config.yaml`, so `rt` finds it from any directory once
[direnv](https://direnv.net) has allowed it. A release config is selected for
one command with `-c`, for one shell with the environment variable, or for the
whole checkout by putting the export in `.envrc.local` (gitignored).

```yaml
projects_dir: ~/src/projects
rc_tag_message: "release candidate {version}"
confirm: always               # always | destructive | never
diff: true                    # show the diff and ask before every commit
color: true                   # unset means: colour when on a terminal
pager: "less -R"              # reviews on a terminal; "" prints instead
diff_lines: 200               # cap when printing without a pager; 0 for all
direnv: true                  # run project commands through direnv when .envrc exists

cmds:                         # the shell commands the projects run
  changelog:                  # in order, see changelog update below
    - "git-cliff --repository ./ | sed 's/\r$//' > CHANGELOG-cliff.md"
    - "{rt} changelog gen --header '# CHANGELOG of {project}' > CHANGELOG-git.md"
  deps:                       # dependency commands, keyed by deps kind
    uv:
      - "uv sync"
    go:
      - "make deps.update.internal"
  deps_check:                 # freshness checks for rt deps check
    uv:
      - "uv lock --check"
    go:
      - "go mod tidy -diff"
changelog_env:
  GIT_CLIFF_CONFIG: ".dev-include/config/cliff.toml"
  GIT_CLIFF__CHANGELOG__HEADER: "# Changelog of {project}\n\n"

deps_pins:                    # internal refs that are not submodules
  go:
    file: Makefile
    var: GO_DEPS_UPDATE_INTERNAL

disable:                      # commands this config must not run
  - "deps freeze"

merge_request:                # the MRs mr: always opens, see below
  branch: "rt/{command}/{branch}/{date}"
  title: "{message}"
  description: ""             # body template; empty leaves it to template, or empty
  template: ""                # a repository MR template to use as the body, e.g. Default
  assignees: []               # usernames; mr_assignees on a project overrides
  reviewers: []               # same, mr_reviewers
  wait: dependents            # wait for the merge: dependents | always | none
  wait_minutes: 60
  settle_seconds: 0           # and this much longer after the merge

hooks:                        # shell commands at rt's own events, see below
  ask:
    - "notify-send rt \"$RT_MESSAGE\""
  done:
    - "notify-send rt \"$RT_COMMAND: $RT_STATUS\""

flows:                        # named command sequences for rt flow <name>
  release:
    - "repo sync"
    - "release branch"
    - "repo check"
    - "deps freeze"
    - "release rc"
    - "changelog update"
  bump:                       # a step may carry flags, as on the command line
    - "repo sync --checkout"
    - "deps submodules --mr"

defaults:                     # per-project settings, see below
  dev_branch: develop
  release_branch_prefix: release-
  deps: none                  # a key of cmds.deps, or none
  ci: none                    # watch pipelines after pushes: gitlab | github | none
  mr: none                    # commits as merge requests: always | none | unset (only under --mr)
  changelog: true
  deps_freeze: true
  release_tags: true          # rc and final tags; on unless set to false

projects:
  - name: pylib               # order matters, see below
    git: git@git.example.com:group/pylib.git
    deps: uv                  # overrides defaults.deps
    changelog_branch: ""      # empty means dev_branch
    submodules:
      - path: .dev-include
        freeze_to: develop    # project | release | an explicit branch name
    project_dir: ""           # wins over the global projects_dir
```

Message templates contain a colon, so they have to be quoted — an unquoted
`changelog_commit_message: ci/AB-0000: update changelog` is a mapping to YAML,
not a string. The error says so.

**Placeholders.** Not every template takes every one:

| Template | `{project}` | `{branch}` | `{task}` | `{product_version}` | `{version}` | `{commits}` | `{rt}` |
|---|:-:|:-:|:-:|:-:|:-:|:-:|:-:|
| `rc_tag_message`, `release_tag_message` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | |
| `changelog_commit_message` | ✓ | ✓ | ✓ | ✓ | | | |
| `freeze_commit_message` | ✓ | ✓ | ✓ | ✓ | | | |
| `submodules_commit_message` | ✓ | ✓ | ✓ | ✓ | | | |
| `rebase_pin_commit_message` | ✓ | ✓ | ✓ | ✓ | | | |
| `merge_request.branch`, `.title`, `.description` | ✓ | ✓ | ✓ | ✓ | | | |
| `notes_header` | | | | ✓ | | | |
| `notes_section_title` | ✓ | ✓ | ✓ | ✓ | | | |
| `cmds.changelog`, `changelog_env` | ✓ | ✓ | ✓ | ✓ | | | ✓ |
| `repo exec` command | ✓ | ✓ | ✓ | ✓ | | | ✓ |

The merge request templates also take `{command}` (the committing command,
dashes for spaces: `deps-submodules`), `{date}` (today, `YYYY-MM-DD`) and,
the title and the description, `{message}` (the commit subject).
`{branch}` is the branch being committed to or tagged. `{version}` is the
computed tag itself — `1.26.0-rc.2`, not the `X.Y` of the branch — which is why
it exists only where a tag is being made; a commit is made on a branch and has
no version. `{rt}` is the path of the running binary, so a changelog command can
call back into it without `rt` being on `PATH`. `{task}` is the ticket id
parsed out of the branch name — `feat/AB-123` renders it as `AB-123` — so a
commit message template like `"build/{task}: pin rebased submodules"` carries
the real ticket. The pattern is an uppercase id (`AB-123`), which is what
keeps `release-1.4` from reading as a ticket; a branch without one renders
`{task}` empty. An unknown placeholder is left
in the text as written rather than blanked out.

**Product version.** `product_version` names the release the whole set of
projects belongs to — a calendar version like `2026.06`, independent of the
per-project tags:

```yaml
product_version: "2026.06"
rc_tag_message: "release candidate {version} of {product_version}"
```

gives `release candidate 6.8.0-rc.0 of 2026.06`. It pairs with a per-release
config: the file, the release branches in it and this string describe one
release together. Leaving it out is fine; a template that then mentions
`{product_version}` is a config error rather than a tag pushed with the
placeholder still in it.

**A command that fails must look failed.** The project commands run with
`set -o pipefail` where the shell supports it: `git-cliff … | sed … > file` ends
in `sed`, so without it a missing generator exits 0 and leaves an empty file
behind, committed as though it were a changelog. `rt repo check` looks the other
way at the same problem — it reports the programs of `cmds.deps` and
`cmds.changelog` that are not on `PATH`, every stage of a pipeline included.

**Dependency commands.** Nothing about a language is built in. `cmds.deps` maps
a kind name to the commands to run, and a project picks one with `deps:`; the
kinds are whatever the config defines, so `cargo` or `npm` needs no code change.
A project can replace the list outright with its own `cmds.deps`, and
`deps: none` (the built-in default) runs nothing. A `deps:` value that no
`cmds.deps` entry defines is a config error, not a silent no-op. The commands
run in the project directory, in order, with the same `{project}` and `{branch}`
placeholders; `rt deps freeze` and `rt deps submodules` run them after moving
the submodule pins, and `--no-deps` skips them for one run.

**Not every internal dependency is a submodule.** A go project consumes its
libraries through `go.mod`, and the refs they are resolved from live in a make
variable — `make deps.update.internal` walks `GO_DEPS_UPDATE_INTERNAL`, a list
of `module@ref`. `deps_pins` says where such a list is, keyed by deps kind the
same way `cmds.deps` is, and `rt deps freeze` moves every ref whose module is
another project in this config onto that project's branch before running the
deps commands. Without that the commands would resolve dev heads and write
their pseudo-versions into `go.mod`, which is the opposite of a freeze. The
branch chosen is the same one `freeze_to: project` picks — `release_branch` when
the config names one, the dev branch otherwise — so a dev config pins dev
branches and a per-release config pins release branches. A module no project in
the config provides stops the command: the variable lists internal dependencies,
so an unknown one means a missing project, and freezing the rest would ship a
release with that dependency still following its dev branch. A file or variable
that is not there is reported rather than an error — an old release branch can
predate the convention.

**A config can forbid commands.** `disable` lists command names under `rt`, and
one of them refuses to run with that config loaded — `deps freeze is disabled in
config.yaml`. A group name disables everything under it, and an entry that names
no command is a config error rather than a prohibition that silently never
applies. The everyday dev config uses it to keep release-only work out: a freeze
run there would pin dependencies to dev branches on a release branch.

**A config can name whole routines.** `flows` maps a name to a sequence of
command names, and `rt flow <name> [project...]` runs them in order over the
same projects — the release walkthrough below as one command. The sequence is
printed first, then every command behaves exactly as if typed by hand: it
plans, asks and executes on its own, with the same flags (`--dry-run` shows
every plan and executes nothing). A step may carry a command's own flags —
`"deps submodules --mr"`, `"repo sync --checkout"` — which reach it the way
the shell would pass them; an unknown flag is a config error at startup. The first failure and the first declined
plan stop the flow — the commands after a refused release branch would act on
the release that was just refused. Every step is resolved before any runs —
and the whole `flows:` block is resolved on every rt start, like `disable` —
so a typo, a group name or a nested flow is a config error found today, not on
release day; a `disable`d command stops the flow when it would actually run.
A few commands are refused as flow steps outright, with the reason in the
error: `git cherry-pick` (needs one project and a choice of commits),
`git rebase` (would force-push whatever branch each project has checked
out), `repo exec` (the command after `--` cannot be written in a step) and
`changelog gen` (prints one repository's document, works outside the
config). `rt flow` without a name lists the flows the config defines, steps
and all, marking any step the disable list would block.

**A protected dev branch takes its commits as merge requests.** `mr` sits
next to `ci`, per project or in `defaults`: with `mr: always` (or `--mr` for
one run, for the projects that leave `mr` unset) the committing commands —
`deps submodules`, `deps freeze`, `changelog update` — put the commit on a
branch of their own instead of pushing to the target branch, push it, open
the request into the target through `glab` or `gh` (so the project needs
`ci: gitlab` or `ci: github`), and watch the pipeline the request starts.
`mr: none` is final: a project whose dev branch takes pushes, or whose CI
runs no request pipelines, stays out even under `--mr`. The branch comes from
the `merge_request.branch` template, `rt/{command}/{branch}/{date}` by
default — `rt/deps-submodules/develop/2026-09-23` — so two commands run on
the same day get branches of their own; the title from `merge_request.title`,
the commit subject by default; `merge_request.description` is the body.
The web form fills an empty body from the repository's template and the API
does not, so a body left empty stays empty unless `merge_request.template`
names one — `Default` reads `.gitlab/merge_request_templates/Default.md`
(`.github/PULL_REQUEST_TEMPLATE/Default.md` for github) from the target
branch, expands the placeholders in it and sends it as the body; `repo check`
reports a named template the branch lacks. Off by default, since a template
usually mentions people. And
`merge_request.assignees` / `.reviewers` name who gets it (`mr_assignees` /
`mr_reviewers` on a project replace the lists). A rerun on the same day
rewrites the branch with one
fresh commit and updates the request already open for it rather than opening
a second one. The diff review shows the request with the message and asks
once, `Commit, push and open MR into develop?`; a decline leaves the changes
staged as usual. Afterwards the working copy is back on the target branch,
submodules on its pins, and the local branch is what `repo prune` deletes once
the request is merged. `--no-mr` pushes directly for one run where the config
says `always`; `repo check` reports a project that has the mode on but no CLI
to open a request with.

A library's request is not the end of its step: the projects after it in the
run pull its dev branch, and their bump only sees the change once the request
is merged. So after the pipeline the command waits for the merge — `waiting
for <url> to be merged (needed by api, worker)` — and goes on to the next
project when it lands; the operator merges in the browser, rt notices.
`merge_request.wait` says when: `dependents` (the default) only when a project
still to come in the run depends on this one — carries its url in
`.gitmodules` on the target branch, or lists its module in the `deps_pins`
variable — `always`, or `none`. A request closed without a merge fails the
step, and so does `wait_minutes` (60 by default) running out.
`settle_seconds` adds a pause after the merge for what the merge sets in
motion — a package build, a registry — to land before the next project pulls
it. The plan says all of it: `then wait for the MR to be merged (needed by
api) and 30s more`.

**A run can call the operator back.** A run is mostly waiting — for an
answer, for a pipeline, for a merge request — and `hooks` maps rt's own
events to shell commands, so a desktop notification says when the terminal
needs a look: `ask` fires before every question (`Proceed?`, `Commit and
push?`, the commit picker, a retry), `wait` when there is something outside
to wait for (a pipeline, once it is running; a merge request, when the wait
for the merge starts), `done` once when the invoked command is over — a
flow counts as one. The hook runs through `sh` in rt's own directory, with
the event in `RT_EVENT`, the invoked command in `RT_COMMAND`, the project a
step is working on in `RT_PROJECT` / `RT_PROJECT_DIR`, the question, the
thing waited for or the error in `RT_MESSAGE`, `RT_STATUS` (`ok`, `failed`,
`declined`) for `done` and `RT_URL` for `wait`. A hook is a courtesy, not a
gate: its failure is a warning, nothing reads its output, and a dry run
fires none. `repo check` verifies the hooks' programs are on `PATH`.

**A push can be watched to the end of its pipeline.** A project with
`ci: gitlab` or `ci: github` gets the pipeline of every pushed commit and tag
polled through the system's own CLI — `glab` or `gh`, which also carry the
authentication, so rt holds no tokens. The step ends when the pipeline does: a
failure fails the step (and stops a flow), success and skipped are reported,
a manual gate is named and left to the operator. No pipeline appearing within
two minutes is a warning, not an error — and a `[skip ci]` commit, like the
changelog one, is recognized and not watched at all. `ci_retries: N` gives a
failed pipeline that many more chances before the failure is final (one by
default, `0` makes a failure final at once) — the
failed jobs are restarted, the way the retry button works, and the watch
follows the rerun with a fresh deadline. The watch is opt-in per project,
`--no-ci` skips it for one run, and `ci_poll_seconds` / `ci_wait_minutes`
tune the polling (10 s / 30 min by default). `repo check` verifies the CLI is
on PATH wherever `ci` is set.

**Submodules follow their own project.** A submodule that is itself one of the
configured projects needs no entry: leave `submodules:` out and every such
submodule is frozen to the branch its project works on in this config — its
`release_branch` when the config names one, its dev branch otherwise. So
`release_branch: release-1.26` on `pylib` is what `api` freezes `pylib` to,
written once instead of in every consumer. The same value can be given
explicitly as `freeze_to: project`.

Matching is by remote, not by path or by `.gitmodules` section name: the same
library sits at `.dev-include` in one project and `vendor/dev-include` in
another, and the section name is local to each file. The remote is compared
after normalising the ways git writes the same one — `git@host:group/lib.git`,
`https://host/group/lib`, with or without the `.git` suffix — so a config that
spells it one way still recognises the other.

An explicit `submodules:` list still wins and is keyed by path, since a path is
what identifies a submodule inside one project. Submodules that belong to no
configured project are left alone, and `derive_submodules: false` turns the
whole thing off.

**One config per release.** A project may name the release branch it works
with:

```yaml
  - name: api
    release_branch: release-6.8      # need not exist yet
    submodules:
      - path: pylib
        freeze_to: release-1.26      # the release branch of that project
```

Every command then takes that branch instead of the highest one on origin:
`rt release branch` creates exactly it, `rt deps freeze`, `rt release rc`,
`rt release tag` and `rt git cherry-pick` work on it. `--release-branch` still
wins over the config, and `--version`/`--major` still win in `release branch`.
That makes a second config a whole release plan — `config.yaml` for everyday
dev-branch work, `config-2026.06.yaml` with the branches this release will get,
used through `-c` or `$REPO_TOOLS_CONFIG`. Until those branches exist,
`rt repo check` reports `origin/release-6.8 does not exist yet, run release
branch to create it` rather than judging the settings against another branch.

**Changelog on the release branch.** `changelog_branch: release` means "this
project's release branch" — whatever `release_branch` names, or the highest one
on origin. Set once under `defaults`, it moves `rt changelog update` off the dev
branch for a whole release config without repeating the branch name per project,
and it cannot drift from `release_branch` because it is the same value:

```yaml
defaults:
  changelog_branch: release
```

Any other value is a branch name as before, empty still means the dev branch,
and `--branch` still wins over all of it.

**What a tag contains.** A tag message can list the commits it covers, which
is what makes an rc tag readable months later:

```yaml
rc_tag_message: |
  release candidate {version} of {product_version}

  {commit_count} commit(s) since the previous tag:
  {commits}
```

`{commits}` is one line per commit, `<short hash> <subject>`, oldest first;
`{commit_hashes}` is the hashes alone and `{commit_count}` their number. The
range is everything since the previous tag of the same release line — so an rc
lists what came in since the rc before it. The first tag on a freshly cut branch
has no such tag, and the branch carries nothing the dev branch does not, so it
counts from the previous release line instead: that is what the release
contains. Only a project with no tags at all falls back to what the release
branch has and the dev branch does not, i.e. the cherry-picks. Merges are left
out. History is only read when the template asks
for it, and the placeholders are all available in both tag messages.

**Defaults.** `dev_branch`, `release_branch_prefix`, `deps`, `changelog_branch`,
`changelog`, `deps_freeze` and `release_tags` can be set once under `defaults`
instead of being repeated in every project. Precedence is project field, then
`defaults`, then the built-in value: `develop`, `release-`, `none`, both
changelog/freeze toggles off, and `release_tags` on. An explicit
`changelog: false` in a project overrides `defaults.changelog: true` — the
fields distinguish "unset" from "set to false".

**Warnings before a tag.** Two states make a tag questionable rather than
impossible, and both are reported in the plan and again at the confirmation
prompt. A final tag whose release branch has moved past its last release
candidate (`release-1.6 has moved 2 commit(s) since 1.6.4-rc.1, which is what
was tested`), or one with no candidate at all, means releasing something that
was never built as an rc. Submodules whose `.gitmodules` branch on the release
branch is not what `freeze_to` asks for (`submodules differ from freeze_to
(pylib tracks develop, config says release-1.26)`) mean the tag pins something
other than what the config describes — run `rt deps freeze` first.

**Projects that are not tagged.** `release_tags: false` takes a project out of
`rt release rc` and `rt release tag` while leaving everything else in place: it
still gets a release branch, a frozen set of submodules and cherry-picks. That
fits a library consumed by branch rather than by version, which would otherwise
collect tags nobody reads.

## Rules that apply to every command

These flags exist on every command:

```sh
rt -c ~/work/release.yaml repo status   # a config other than the looked-up one
rt release rc --dry-run                 # print the plan, execute nothing
rt release rc -y                        # answer every question with yes (CI)
rt release rc --confirm                 # ask even when the config says never
rt changelog update --no-diff           # commit without reviewing the diff
rt repo check --fetch                   # refresh remote refs first
rt release rc --no-fetch                # trust the refs already on disk
rt deps freeze --skip compose,docs      # exclude projects
rt release rc --keep-going              # do not stop at the first failed project
rt repo status --color                  # force colour through a pipe
rt changelog update --no-color          # plain output
```

**The first failure stops the run.** Projects usually depend on the one before
them — a library that failed to freeze has no business being tagged, and the
service that consumes it should not be committed against a half-updated pin. A
failed project prints its error and the batch ends with a non-zero status;
`--keep-going` restores the older behaviour of carrying on and listing the
failures at the end. Whatever a failed run left in a working tree is named on
the spot, together with the commands to inspect or discard it.

**Project environments.** A repository that keeps its `GOPROXY`, `GOPRIVATE` or
package index URLs in an `.envrc` needs them when its own commands run, or
`make deps.update.internal` reaches for the public proxy and fails on a private
module. Where a project has an `.envrc` and `direnv` is installed, `deps freeze`,
`deps submodules` and `changelog update` run their commands through
`direnv exec`; the plan says `env from .envrc via direnv` when they do. An
`.envrc` that direnv has not been allowed to load stops the project with
`run: direnv allow <dir>` rather than running the command with the wrong
environment. `direnv: false` in the config turns the whole thing off.

**Colour.** Diffs come coloured from git, and warnings, errors and headers get
a little of their own. It is on when a terminal is attached and off when the
output is a pipe or `NO_COLOR` is set; `color: true|false` in the config and
`--color` / `--no-color` override that in the usual order.

**Projects run strictly in config order.** The tool never reorders them, so put
libraries before the projects that consume them as submodules.

**Project selection.** Positional arguments filter by name; with none given,
every project in the config runs. `--skip name` excludes. A name that is not in
the config is an error raised before anything executes. The execution order
stays the config order even if the arguments are given in another one.

```sh
rt changelog update              # every project
rt changelog update api worker   # only these two
rt release rc --skip compose     # everything except this one
```

**Confirmation.** Commands that commit, push or tag ask first. `confirm` in the
config picks the policy (`always` by default, `destructive` to ask only before
those commands, or `never` for CI);
`-y/--yes` skips the question, `--confirm` forces it even when the config says
never. The plan for every project is printed before the question, which is the
same output `--dry-run` produces.

**Review before it leaves the machine.** Every outward step is shown first and
confirmed:

| Step | What is shown |
|---|---|
| commit | the staged diff, then the commit message, then the question |
| tag | the message in full, the commit it will point at, and any warning |
| release branch | the commit it would be cut from, and how much has landed since the last release |
| cherry-pick | the patch of the picks, right before the push |
| `repo clean` | everything that is about to be thrown away |

The commit message comes after the diff on purpose: above a regenerated
changelog it would have scrolled away by the time anyone answers. A tag is
reviewed because it cannot be amended once pushed, a release branch because
that single commit decides what the release ships, and a cherry-pick at push
time because an auto-resolved submodule pin is not visible any earlier.

A submodule pin move is rendered as the list of submodule commits it crosses,
with git's `(rewind)` marker when the pin goes backwards, instead of the two
raw hashes git prints by default — that is what makes a frozen pin or an
auto-resolved conflict reviewable at all. Diffs are coloured by git itself.

The output speaks one visual language, the one a package manager taught
everyone's eyes: `::` (bold cyan) opens a phase — a plan header, a flow step, a
question; `==>` (bold green) names the project being worked on; `WARNING:` is
yellow and `ERROR:` red, always with the colon; hints and side notes (like the
`config:` line) are dim. One meaning per shape, the same shape in every
command. While a pipeline is watched, the running line carries its progress —
`gitlab pipeline #123 running (3/7 jobs): <url>` — a new line whenever the
count moves, so the wait has a shape even in a scrollback.

Prompts only accept answers given after the question is on screen: keystrokes
typed while a long diff or a pipeline watch was running are discarded, so a
stray Enter cannot decline a release step nobody looked at. And only `y`/`yes`,
`n`/`no` or plain Enter for the default count as answers — anything else sends
the question back instead of being read as a no. When a fetch or a
push fails on a terminal, rt prints everything git said — the `remote:` hook
message, the `! [rejected]` detail — right after the attempt, then asks
before trying again: `git push failed — touch the key if it was waiting.
Retry? [Y/n]`. An ssh key on a hardware token wants a touch that is easy to
miss, and plain Enter retries as many times as needed. Branch pushes carry
`--no-follow-tags`: the push means that branch and nothing else, so a global
`push.followTags` cannot drag a stale tag along and sink it — tags travel
only with `release rc` and `release tag`. Unattended runs retry three times on
their own with a pause, and the failure is reported with git's actual
`fatal:` line, not just the exit status. For the mutating commands a fetch
that still fails after the retries is an error that stops the run, not a
skipped project: a skip would let a flow sail past its tagging step on a
hiccup nobody saw. Only the read-only reports — `repo status`, `repo check`,
`git cherry-pick --list` — name the failure and move on.

On a terminal the body goes through a pager, so a regenerated changelog can be
read in full and scrolled back; the question comes after the pager exits. It is
`$PAGER` or `less -R`, and `pager:` in the config overrides that — an empty
string turns paging off. The default carries no `-F` and no `-X` on purpose: a
short diff opens too instead of flashing past into the prompt, and the pager
takes the alternate screen, so quitting brings the terminal back to the plan. A single-line body is printed rather than paged: a
one-line tag message has nothing to scroll, and a pager for it is one more
screen to dismiss. Without a terminal there is nothing to scroll at all, so the
text is printed and cut after `diff_lines` lines (200 by default, `0` for all
of it) with the command to see the rest.

Every question stands on its own: a blank line, a bold `::` mark and the
question in bold, with the default dimmed — after a long diff it has to be
findable without scrolling back.

Colour is used for three things, and each keeps its colour everywhere: a ref —
branch, tag, revision — is cyan, a commit or tag message green, a commit hash
yellow the way git prints one. `==>` is bold green and project names bold,
warnings yellow, errors red, hints dimmed, and diffs are coloured by git
itself. A tag message also gets its subject in bold; all of it is display only,
the text pushed with the tag is the plain one.

It is on by default; `diff: false` turns it off, and `--diff` / `--no-diff`
override the config for one run. Declining keeps the work locally (staged
changes, or picked commits on the local branch), prints how to inspect and
discard it, and reports the project as not completed. Under `--yes` or
`confirm: never` everything is still printed but nothing is asked, so CI stays
unattended.

**Fetching.** Commands that act on origin refresh remote refs before planning,
so the plan is not built on stale data; the read-only `rt repo status` and
`rt repo check` do not, to stay quick. `--fetch` and `--no-fetch` override
whichever default a command has.

A single project can be kept off the network with `fetch: false`, which suits a
repository behind another credential — a second hardware key, say — that would
otherwise interrupt every batch for a touch. Its commands then work with the
refs already on disk; an explicit `--fetch` still overrides it, and everything
that needs a fresh ref says so instead of guessing.

**Inputs are named before the work.** Every run prints one line to stderr
saying which of the four candidate config files was used, how many projects it
holds, and the switches that quietly change behaviour:

```
config: /home/me/src/projects/config.yaml (7 projects), confirm=destructive, fetch=off, dry-run
```

It goes to stderr, so `rt git cherry-pick --list` and `rt changelog gen` stay
pipeable.

**Local work is never pushed by accident.** Before committing, a command
fast-forwards the branch to origin and refuses to continue when it still has
local commits that origin does not — otherwise the push at the end would carry
that unrelated work along. Push or reset it first.

## Commands

Commands are grouped by what they act on:

| Command | What it does |
|---|---|
| `rt repo status` | branch, dirty state, ahead/behind, submodule pins |
| `rt repo check` | validates the config against the working copies and the release branch |
| `rt repo sync` | clones missing projects, fetches, fast-forwards the dev branch; `--checkout` switches to it |
| `rt repo clean` | discards uncommitted changes and restores submodule pins |
| `rt repo report` | prints a markdown table of the projects, columns from the config |
| `rt repo prune` | deletes local branches that are safe to lose: gone upstreams, stale release leftovers |
| `rt repo exec` | runs one shell command in every project, through direnv |
| `rt changelog update` | runs `cmds.changelog`, commits and pushes if files changed |
| `rt changelog gen` | prints the plain git-log changelog of one repository |
| `rt release branch` | creates `release-X.Y` from the dev branch; an existing branch is a warning, a local leftover of a failed push is reused |
| `rt release rc` | tags `X.Y.Z-rc.N` on the release branch and pushes; a head already tagged is a skip |
| `rt release tag` | tags the final `X.Y.Z` and pushes |
| `rt release status` | one line per project: branch, freeze state, pins drift, rc/final tags, head, compare counters, pipeline |
| `rt release notes` | prints markdown release notes: per project, what its latest tag added |
| `rt deps freeze` | pins submodules to release branches, updates deps, commits and pushes |
| `rt deps submodules` | moves the configured submodules to the head of the branch they already track |
| `rt deps status` | shows how far submodule pins and module pins have drifted from their branches |
| `rt deps check` | runs the kind's freshness checks (`go mod tidy -diff`, `uv lock --check`) |
| `rt git cherry-pick` | moves commits from the dev branch into the release branch, chosen by hash, by ticket, or from a list |
| `rt git compare` | shows how the dev and release branches differ: still to pick, release-only, already in both |
| `rt git rebase` | rebases a branch onto its target, auto-resolving conflicting submodule pins |
| `rt ci status` | shows the pipeline state of each project's head, or of any `--ref` |
| `rt ci watch` | attaches the usual pipeline watch to a revision already on origin |
| `rt flow <name>` | runs the command sequence the config defines under that name; without a name, lists the flows |

Release branches are never merged back into the dev branch. Fixes land in the
dev branch and travel to the release branch through `rt git cherry-pick`.

### A release, start to finish

```sh
rt repo sync                       # clone what is missing, fast-forward dev
rt repo check                      # config against the working copies

rt release branch                  # cut release-X.Y from dev
rt deps freeze                     # pin submodules, update deps, commit, push
rt deps check                      # dependency files still tidy after the freeze?
rt release rc                      # tag X.Y.0-rc.1

rt git cherry-pick api --task AB-3151   # a fix arrives in dev
rt release rc                           # tag X.Y.0-rc.2

rt release tag                     # tag the final X.Y.0

rt changelog update                # regenerate, commit, push on the dev branch
```

Every step takes the same project filters, so a single project moves alone:
`rt deps freeze api`. Prefix any of them with `--dry-run` to see the plan first.

The whole routine can live in the config as a flow and run as one command per
project: `rt flow release api`.

### repo

```sh
rt repo status                  # branch, dirty state, ahead/behind, pins
rt repo status api worker       # only these projects
rt repo check                   # config vs disk, cmds.deps programs on PATH
rt repo sync                    # clone missing, fetch, fast-forward dev
rt repo sync --no-pull          # fetch only, leave the dev branch where it is
rt repo sync --checkout         # and put every clean working copy on the dev branch
rt repo clean api               # throw away uncommitted changes in one project
rt repo clean --untracked       # and delete files git does not track
rt repo prune                   # delete local branches that are safe to lose
rt repo exec -- git gc          # one shell command in every project
```

`rt repo sync` fast-forwards the dev branch without moving HEAD: when another
branch is checked out, the ref is updated and the working copy stays where it
is; when the dev branch is the one checked out, the submodules follow the pins
the fast-forward brought, so a merged bump does not leave the tree dirty. `--checkout` switches it to the dev branch as well, with the submodules
on the pins that branch records, so a release's worth of repositories comes
back to `develop` in one go. The plan is the check before the switch: a
project with uncommitted changes, untracked files included, is skipped and
the changes are named (`working tree is dirty: app.py, notes.md; commit or
run repo clean first`), and a branch left behind with commits origin does
not have is pointed out too. Nothing is stashed or carried across.

`rt repo prune` deletes, per project, the local branches nothing would miss:
branches whose upstream is gone from origin but whose commits some remote
branch still carries, and local release branches sitting at or behind their
origin counterpart with no commits of their own — the leftovers failed pushes
and finished releases accumulate. The current branch and the dev branch are
never touched, and a branch with commits nobody else has is kept and named
(`inspect it first`), not deleted. It fetches with `--prune` first and shows
the usual plan before anything goes.

`rt repo exec [project...] -- <command...>` runs the command after `--` in
each project's directory, through direnv where there is an `.envrc` — the
project's own environment, the way its deps and changelog commands run.
`{project}`, `{branch}` (the target branch), `{product_version}` and `{rt}`
expand per project first, so
`rt repo exec -- echo "{project} works on {branch}"` and calling back into
rt via `{rt}` both work. The command is arbitrary, so it plans and confirms
like any destructive step.

```sh
rt repo report                  # markdown table: one row per project
rt repo report api worker       # only these projects
```

`rt repo report` prints a paste-ready markdown table, one row per project.
The columns come from the config's `report` list — a header and a value
template each — so the dev config reports commits and a release config
reports tags with the same command. Templates take `{project}`, `{branch}`,
`{product_version}`, `{commit}` (short head of the target branch),
`{subject}` (its commit subject) and `{tag}` (the highest tag of the branch's
release line, or of the whole repository when the branch is not a release
one); a fact that is not there — no clone, no branch, no tag yet — renders as
`-`, and a placeholder can chain fallbacks: `{tag|branch}` renders the first
fact that exists, so an untagged line reports its branch instead of a dash.
Without a `report` list the table is project | branch | commit.

`rt repo clean` resets the working tree to HEAD and puts every submodule back
on its recorded pin. That is what clears the leftovers of a run that stopped
half way — the state every committing command refuses to start on. Tracked
files only, unless `--untracked` also deletes what git does not know about;
untracked files count as dirty too, so a run blocked by them needs that flag.
What would go is listed in the plan, shown as a diff and confirmed before
anything is thrown away, since none of it can be recovered afterwards.

### Versions

`rt release branch` defaults to a minor bump of the highest existing release
branch (`--version X.Y` or `--major` override it). `rt release rc` and
`rt release tag` derive the next tag from the tags already on the release
branch: the patch level is one past the highest final release, and the rc
counter is one past the highest rc of that patch.

```sh
rt release branch                          # minor bump: release-1.4 -> release-1.5
rt release branch --major                  # release-1.4 -> release-2.0
rt release branch --version 3.0            # exactly this one
rt release rc                              # next rc on the highest release branch
rt release rc --release-branch release-1.4 # tag an older release branch
rt release tag                             # the final X.Y.Z
rt release tag --tag 1.4.7                 # an explicit tag instead
```

`--release-branch` and `--tag` work on both `rt release rc` and
`rt release tag`.

### release status

```sh
rt release status                          # the whole release at a glance
rt release status api worker               # only these projects
rt release status --release-branch release-1.4
```

The dashboard for "what is left": one line per project with the release
branch (or `pending` before `rt release branch` ran), whether the freeze on
that branch matches the config (`ok` / `stale(n)`, with the stale submodules
listed under the row), whether every pin sits at the head of its branch
(`pins`: `ok` / `drift(n)` — `rt deps status` folded into one cell, drifted
pins listed under the row), the line's highest rc and final tag, the head state
(`tagged`, or `+N commits` since the last tag — what the next rc would ship),
`rt git compare`'s counters as `dev/rel/both` — commits only in the dev
branch (pending cherry-picks, yellow while there are any), only in the
release branch, and in both — and the head pipeline's state via glab/gh. A
failed pipeline's URL gets a detail line. Fetches first; a project whose
fetch fails is skipped rather than reported from stale refs. `--no-ci`
blanks the pipeline column.

### ci

```sh
rt ci status                     # pipeline of every project's head
rt ci status api                 # one project
rt ci status api --ref 1.4.0     # a tag's pipeline
rt ci watch api                  # follow the head pipeline to the end
rt ci watch api --ref release-1.4
```

The push commands watch their own pipelines; these two look without pushing.
`ci status` asks once and prints one line per project — state, label,
progress, and the URL when something needs attention. `ci watch` attaches the
usual watch — poll interval, job progress, `ci_retries` included — to a
revision already on origin: for picking a watch back up after an aborted run,
a manual push, or before the final tag. The default revision is the project's
target branch head; `--ref` takes any branch on origin or any tag. Projects
with `ci: none` are reported as not configured and skipped. A failed pipeline
fails `ci watch` (after the retries); `ci status` only reports and always
exits 0.

### release notes

```sh
rt release notes                           # the whole release as one document
rt release notes api worker                # only these projects
rt release notes --release-branch release-1.4
```

Prints the release as one markdown document, ready to paste into an
announcement: a heading from `product_version`, then one section per project
with the highest tag of its release line and the commits that tag added since
the line's previous tag. The first tag of a line counts from the previous
release line — which is the release — and a repository's very first tag from
what the release branch has over the dev branch, i.e. the cherry-picks.
States like a missing clone or an untagged line become italic notes rather
than errors, so the document always renders whole. Read-only; `--fetch` is
opt-in like the other paste-ready reports.

The headings are templates: `notes_header` shapes the document's first line
(default `# Release {product_version}`, or `# Release notes` without a
product version) and `notes_section_title` each section's heading (default
`## {project} {tag}`; `{tag}` is the tag the section covers). A section that
carries a note instead of a tag keeps the plain project heading.

### changelog

Every command in `cmds.changelog` runs in order in the project directory, with
`changelog_env` in the environment, and each one redirects to the file it
generates — the same two-generator setup the GitLab job uses (git-cliff plus a
plain git-log document). Whatever the generators changed is then committed and
pushed in one commit. A project can replace the list with its own
`cmds.changelog` and override single environment keys with `changelog_env`;
project keys are merged over the global map, which is how a repository that
keeps its own `cliff.toml` is expressed in one line.

Before generating, `git submodule update --init` runs so that a
`GIT_CLIFF_CONFIG` living in a tooling submodule is actually on disk; set
`changelog_init_submodules: false` to skip it.

The default commit message carries `[skip ci]`, which keeps the changelog commit
out of the next changelog and off the pipeline.

`rt changelog gen` is the built-in replacement for the shared `changelog-gen.sh`
script: it prints commits grouped by commit date, newest day first, and needs
neither bash nor a checked out tooling submodule. It reads the history in one
`git log` instead of one per day, which is the difference between 4.5s and 0.03s
on a repository with 800 distinct commit dates.

Its output matches that script's except for one deliberate fix. The script took
its date list from `%cd`, the date in the commit's own timezone, then re-queried
each day with `--since/--until`, which git reads in the local timezone. A commit
made near midnight in another timezone therefore landed under a neighbouring
date — or disappeared from the document entirely when that neighbour had no
commits of its own. Grouping straight by `%cd` files every commit exactly once,
under the date its author saw, and gives the same result no matter which machine
or timezone runs it. Expect a small first-run diff where such commits reappear.

It also works standalone, on any repository, without a config:

```sh
rt changelog gen                                  # the repository in $PWD
rt changelog gen --repo ~/src/api                 # somewhere else
rt changelog gen --header '# CHANGELOG of api'    # a different first line
rt changelog gen --since 2026-01-01 --until 2026-06-30 > CHANGELOG-h1.md
```

`rt changelog update` is the batch form and publishes; it takes `--branch` when
the changelog has to be generated somewhere other than `changelog_branch`:

```sh
rt changelog update                        # every project, on its own branch
rt changelog update api --branch release-1.5
```

### deps freeze

```sh
rt deps freeze                                    # the highest release branch
rt deps freeze api --release-branch release-1.4   # an older one, one project
rt deps freeze --no-deps                          # move the pins, run no deps commands
```

On the release branch it rewrites the submodule branches in `.gitmodules`,
fetches inside each submodule and runs `git submodule update --remote` — the
fetch matters because git only fetches there on demand, so a branch created
after the submodule was cloned would otherwise be missing. Then it moves the
module refs of `deps_pins`, runs the project's `cmds.deps` and commits and
pushes whatever changed. Like `deps submodules`, the plan reads `.gitmodules` on
the release branch rather than on whatever is checked out; a configured
submodule that branch does not have is reported and skipped instead of failing
the project mid-run. `freeze_to: release` resolves to the highest release branch
of the submodule's own remote; when the submodule is itself a configured project
with a local clone, that is answered locally instead of over the network.

### deps submodules

Moves the project's configured submodules to the head of the branch
`.gitmodules` already has them tracking, runs the project's `cmds.deps` so a
lock file follows the new pin, then commits and pushes. It is the everyday
counterpart of `deps freeze`: freeze decides *which* branch a submodule
follows and is a release act, this one only follows it further and works on
the dev branch. The scope is the same as freeze's — the `submodules` the
project lists, or without a list every submodule another configured project
provides — so a repository the project merely carries, a dashboard bundle or
a vendored service that tracks a branch of its own, is never moved.

```sh
rt deps submodules                          # every project, on its dev branch
rt deps submodules api worker               # only these
rt deps submodules --branch release-1.5     # somewhere other than the dev branch
rt deps submodules --submodule pylib        # only this submodule path
rt deps submodules --no-deps                # move the pins, run no deps commands
rt deps submodules --no-commit              # update the working tree, commit nothing
rt deps submodules --allow-dirty            # finish what a --no-commit run left
rt deps submodules --mr                     # commit to a branch and open a merge request
```

`--no-commit` leaves its work in the tree, which the next ordinary run refuses
to start on. `--allow-dirty` is the way to finish it: the run begins on the
dirty tree, prints what was already there, and commits it along with its own
changes. It is only accepted where the diff review can still be answered — not
under `--no-diff`, `--yes` or `confirm: never` — since that review is what
keeps unrelated work out of the commit. `rt deps freeze` takes the same flag.

The submodule list and the branch each one tracks are read from `.gitmodules`
as it is on the target branch (`origin/<branch>` when there is one), not from
whatever is checked out — otherwise running against a release branch from a
develop checkout would move a frozen pin back onto the dev branch.

Each submodule is fetched before `git submodule update --remote`, so a commit
pushed after the submodule was cloned is picked up. `--remote` is not
recursive, so afterwards the submodule's own submodules are brought to the
pins its new commit records (`submodule update --init --recursive` inside) —
left behind they read as "modified content", dirt inside the submodule that no
commit of the parent can absorb. A submodule with no `branch`
in `.gitmodules` is left alone and reported, since `--remote` would otherwise
follow whatever the remote's default branch is — run `rt deps freeze` to give it
one. A project whose pins are already current says so and commits nothing.

### deps status

```sh
rt deps status                    # every project, against its target branch
rt deps status api worker         # only these
```

The read-only answer to "is a `deps submodules` or `deps freeze` run due".
For every configured submodule that tracks a branch: the recorded pin against the head
of that branch, inside the submodule's own clone — `ok`, `behind N
commit(s)`, or `diverged` when the pin is not on the branch at all. For every
`module@ref` of the deps kind's pin variable: whether the ref is the branch
this config would freeze it to, and how far the version `go.mod` actually
resolved is behind that branch's head in the provider project's clone — a
pseudo-version is compared by the commit baked into it, a released version
through its tag.

Everything is read at origin's view of the target branch, so it reports what
is committed, not what a working tree happens to contain. Fetches first —
the submodules and the providers too, each provider once per run.

### deps check

```sh
rt deps check                     # every project
rt deps check api                 # one project
```

Runs the deps kind's freshness checks from `cmds.deps_check` in each
project's working copy, through direnv where there is an `.envrc` — the same
way `deps freeze` runs the real commands. A check that exits non-zero marks
the project stale and its output says why; the run then fails naming how many
projects need attention. The commands are the config's, nothing about a
language is built in:

```yaml
cmds:
  deps_check:
    uv:
      - "uv lock --check"         # uv.lock still matches pyproject.toml?
    go:
      - "go mod tidy -diff"       # go.mod/go.sum still tidy? (Go >= 1.23)
```

A project can replace its kind's list with its own `cmds.deps_check`; a kind
with no checks is skipped. Keys must be `cmds.deps` kinds, anything else is a
config error.

### git cherry-pick

```sh
rt git cherry-pick --list                        # candidates, every project
rt git cherry-pick api                           # choose from a numbered list
rt git cherry-pick api abc123 def456             # take these commits
rt git cherry-pick api --task AB-3151,AB-3595    # take whole tickets
rt git cherry-pick api --grep '^fix/'            # take what matches a pattern
rt git cherry-pick api --since 2026-08-01        # scope the list by date
rt git cherry-pick api --since 2026-08-01 --until 2026-08-15
rt git cherry-pick api --release-branch release-1.4   # target an older branch
```

**Candidates.** The dev-branch commits the release branch does not have yet,
oldest first, which is the order they have to be picked in. Commits whose change
is already there are dropped, both by patch equivalence and by the
`(cherry picked from commit …)` trailers that `-x` leaves behind. `--list`
prints them for every project without picking anything, and takes the same
filters as a real run.

**Choosing.** With commit hashes on the command line, those are the commits.
With `--task` or `--grep`, everything they match is taken — so pulling three
tickets that span different commit types is one command:

```sh
rt git cherry-pick api --task AB-3151,AB-3595,AB-1335
```

`--task` matches ticket ids mentioned in the subject, `--grep` a regexp; both
are repeatable, case insensitive, and combine as "any of these". A ticket id
matches as a whole word, so `--task AB-315` does not drag in `AB-3151`.

`--since` and `--until` take anything git understands (`2026-08-01`,
`"2 weeks ago"`) and filter on commit date. They only scope the list rather than
choose from it, because a date is far too blunt to be a selection.

With neither hashes nor a subject filter, the candidates are printed numbered
and the picker takes numbers, ranges and `all`: `1 3`, `2-4`, `1 4-6`, `all`; an
empty answer, `q` or `none` aborts. Whatever is chosen is applied oldest first,
no matter how it was typed. Asking needs someone to answer, so this form is
refused under `--yes` or `confirm: never` — pass hashes or a subject filter
there instead.

**Nothing to do.** A run that selects nothing says which case it is: everything
selected is already in the release branch, or nothing was selected at all.
Explicit hashes are checked the same way before anything is touched — a commit
the release branch already carries is dropped with a warning naming why (picked
from it before, the commit itself is an ancestor, or an equivalent patch is
there). Should git still end up with an empty pick mid-run, that is a warning
and a skip too.

**Running.** Picks use `git cherry-pick -x`; the push happens after the review
described above. Conflicts are handled as follows:

- **submodule pin** — the develop pin is ignored and the submodule is re-pinned
  to the head of the branch it tracks in the release branch's `.gitmodules`,
  which is where `rt deps freeze` left it.
- **`.gitmodules` itself** — the release version wins, so a develop commit that
  retargets a submodule cannot unfreeze the release.
- **a submodule with no `branch` in `.gitmodules`** — the run stops, because
  `--remote` would otherwise follow the remote's default branch. Run
  `rt deps freeze` first.
- **a submodule whose tracked branch is missing on its remote** — reported as
  `origin/<branch> does not exist` after a fetch inside the submodule, instead
  of a raw git error.
- **anything else** — the run stops with the cherry-pick left in progress and
  prints the repository to resolve it in. Other projects are not touched.

### git compare

```sh
rt git compare                            # every project
rt git compare api worker                 # only these
rt git compare --task AB-3151             # one ticket's fate across the repos
rt git compare --since 2026-08-01         # only recent commits
rt git compare --release-branch release-1.4
```

The overview to read before a cherry-pick session. For each project the
commits made since the dev and release branches diverged are sorted into three
groups, oldest first:

- **only in the dev branch** — the cherry-pick candidates, exactly what
  `rt git cherry-pick --list` would offer;
- **only in the release branch** — the release's own commits: dependency
  freezes, version bumps, direct hotfixes;
- **in both** — already carried over, shown once from the dev side. A commit
  brought over by `cherry-pick -x` names its release copy
  (`picked as ff00aa1…`), even when conflict resolution changed the patch; a
  change applied to both branches independently says `equivalent patch`.

It fetches first so the picture is origin's, not the clone's; a project whose
fetch fails is skipped with a warning rather than compared against stale refs.
Common history from before the branch point stays out entirely. The filters
are the cherry-pick ones and narrow every group, so `--task AB-3151` answers
"has this ticket reached the release yet?" across all projects at once.

### git rebase

```sh
rt git rebase api                          # rebase what api has checked out
rt git rebase api --branch feat/AB-3151    # a named branch
rt git rebase api --onto main              # onto something other than the target branch
rt git rebase api --no-push                # keep the result local
rt git rebase api --submodules             # rebase & push the submodule branches first
```

Rebases `--branch` (default: whatever the project has checked out) onto
`--onto` (default: the project's target branch) and resolves conflicting
submodule pointers automatically — the local twin of the CI submodule-rebase
job, for the rebase an MR asks for after the target branch moved a pin.

A conflicted submodule is pinned to the freshly fetched head of its matching
branch: the branch named like the branch being rebased when the submodule
has one (cross-repo feature work), otherwise the branch it tracks in
`.gitmodules`. Re-fetching means a submodule branch that was itself rebased
is picked up at its current state, not at the stale commit the superproject
still points to.

Safety rules, same as the job's: the pin coming from the rebase target must
be reachable from the head being pinned — otherwise the submodule branch is
not rebased onto its own target yet, and resolving here would drop commits
(`rebase the submodule branch first`); when falling back to the tracked
branch, the pin from the rebased commit must be reachable too. A commit that
becomes empty after resolving is skipped with a warning. Release branches
are refused outright: fixes reach them through `rt git cherry-pick`, never
by rebasing.

What the automation refuses to resolve — a conflict outside a submodule, a
pin the chosen branch has not absorbed — aborts the rebase by default,
leaving the branch exactly where it was. On a terminal there is a choice:
answer yes to `Keep the rebase in progress for manual resolution?` and
finish the conflict by hand with every submodule pin resolved so far kept —
the commands to continue are printed.

Before the push the rebased commits are listed and, when origin already has
the branch, `git range-diff` shows how the rebase changed the patches —
resolved pins included — through the usual pager. The push itself is
`--force-with-lease`, after a confirmation; `--no-push` keeps the result
local and prints the push command instead.

`--submodules` does the cross-repo half too: before the parent, every
submodule branch named like the one being rebased is itself rebased onto the
branch that submodule tracks in `.gitmodules` on the target side —
recursively, deepest first — and force-pushed after its own confirmation.
The parent rebase then picks the fresh heads up through the usual pin
resolution, and a pin no conflict refreshed is re-pinned and committed
(`rebase_pin_commit_message`, default
`chore(deps): pin rebased submodule branches`), so the branch never points
at commits the force-push just orphaned. The submodule pushes happen even
under `--no-push`: a local-only submodule rebase would leave the parent
pinning commits origin has never seen. A rerun skips a submodule branch
already on top and already pushed.

## Tests and linting

```sh
make test
make lint
```

The cherry-pick tests build throwaway repositories with a real submodule and
exercise the conflict resolution end to end. They run in parallel under the
race detector; the few that swap `os.Stdout` or the colour switch stay serial
and say so. `make lint` is `go vet`, golangci-lint 2.13.2 with
`.golangci.yml` — every linter is on except a documented handful, and the
gci/gofmt/gofumpt/goimports formatters are enforced (`make go.format` applies
them, `make lint.fmt` only reports) — and the pre-commit hooks of
`.pre-commit-config.yaml`, gitleaks among them (`make lint.pre-commit`, needs
`uvx`). Errors wrap a sentinel at the fixed part of their sentence, so the
text stays readable and `errors.Is` still works.
