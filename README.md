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
every target. The other useful ones are `make test`,
`make test.coverage`, `make lint` (go vet plus golangci-lint), `make lint.fix`
and `make format`.

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

changelog_cmds:               # run in order, see changelog update below
  - "git-cliff --repository ./ | sed 's/\r$//' > CHANGELOG-cliff.md"
  - "{rt} changelog gen --header '# CHANGELOG of {project}' > CHANGELOG-git.md"
changelog_env:
  GIT_CLIFF_CONFIG: ".dev-include/config/cliff.toml"
  GIT_CLIFF__CHANGELOG__HEADER: "# Changelog of {project}\n\n"

deps_cmds:                    # dependency commands, keyed by deps kind
  uv:
    - "uv sync"
  go:
    - "make deps.update.internal"

deps_pins:                    # internal refs that are not submodules
  go:
    file: Makefile
    var: GO_DEPS_UPDATE_INTERNAL

disable:                      # commands this config must not run
  - "deps freeze"

flows:                        # named command sequences for rt flow <name>
  release:
    - "repo sync"
    - "release branch"
    - "repo check"
    - "deps freeze"
    - "release rc"
    - "changelog update"

defaults:                     # per-project settings, see below
  dev_branch: develop
  release_branch_prefix: release-
  deps: none                  # a key of deps_cmds, or none
  ci: none                    # watch pipelines after pushes: gitlab | github | none
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

| Template | `{project}` | `{branch}` | `{product_version}` | `{version}` | `{commits}` | `{rt}` |
|---|:-:|:-:|:-:|:-:|:-:|:-:|
| `rc_tag_message`, `release_tag_message` | ✓ | ✓ | ✓ | ✓ | ✓ | |
| `changelog_commit_message` | ✓ | ✓ | ✓ | | | |
| `freeze_commit_message` | ✓ | ✓ | ✓ | | | |
| `submodules_commit_message` | ✓ | ✓ | ✓ | | | |
| `changelog_cmds`, `changelog_env` | ✓ | ✓ | ✓ | | | ✓ |

`{branch}` is the branch being committed to or tagged. `{version}` is the
computed tag itself — `1.26.0-rc.2`, not the `X.Y` of the branch — which is why
it exists only where a tag is being made; a commit is made on a branch and has
no version. `{rt}` is the path of the running binary, so a changelog command can
call back into it without `rt` being on `PATH`. An unknown placeholder is left
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
way at the same problem — it reports the programs of `deps_cmds` and
`changelog_cmds` that are not on `PATH`, every stage of a pipeline included.

**Dependency commands.** Nothing about a language is built in. `deps_cmds` maps
a kind name to the commands to run, and a project picks one with `deps:`; the
kinds are whatever the config defines, so `cargo` or `npm` needs no code change.
A project can replace the list outright with its own `deps_cmds`, and
`deps: none` (the built-in default) runs nothing. A `deps:` value that no
`deps_cmds` entry defines is a config error, not a silent no-op. The commands
run in the project directory, in order, with the same `{project}` and `{branch}`
placeholders; `rt deps freeze` and `rt deps submodules` run them after moving
the submodule pins, and `--no-deps` skips them for one run.

**Not every internal dependency is a submodule.** A go project consumes its
libraries through `go.mod`, and the refs they are resolved from live in a make
variable — `make deps.update.internal` walks `GO_DEPS_UPDATE_INTERNAL`, a list
of `module@ref`. `deps_pins` says where such a list is, keyed by deps kind the
same way `deps_cmds` is, and `rt deps freeze` moves every ref whose module is
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
every plan and executes nothing). The first failure and the first declined
plan stop the flow — the commands after a refused release branch would act on
the release that was just refused. Every step is resolved before any runs —
and the whole `flows:` block is resolved on every rt start, like `disable` —
so a typo, a group name or a nested flow is a config error found today, not on
release day; a `disable`d command stops the flow when it would actually run.

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
everyone's eyes: `::` (bold cyan) opens a phase — a plan header, a flow step,
a question; `==>` names the project being worked on; `WARNING:` is yellow and
`ERROR:` red, always with the colon; hints and side notes (like the `config:`
line) are dim. One meaning per shape, the same shape in every command. While
a pipeline is watched, the running line carries its progress —
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
yellow the way git prints one. `==>` and project names are bold, warnings
yellow, errors red, hints dimmed, and diffs are coloured by git itself. A tag
message also gets its subject in bold; all of it is display only, the text
pushed with the tag is the plain one.

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
| `rt repo sync` | clones missing projects, fetches, fast-forwards the dev branch |
| `rt repo clean` | discards uncommitted changes and restores submodule pins |
| `rt repo report` | prints a markdown table of the projects, columns from the config |
| `rt changelog update` | runs `changelog_cmds`, commits and pushes if files changed |
| `rt changelog gen` | prints the plain git-log changelog of one repository |
| `rt release branch` | creates `release-X.Y` from the dev branch; an existing branch is a warning, a local leftover of a failed push is reused |
| `rt release rc` | tags `X.Y.Z-rc.N` on the release branch and pushes; a head already tagged is a skip |
| `rt release tag` | tags the final `X.Y.Z` and pushes |
| `rt deps freeze` | pins submodules to release branches, updates deps, commits and pushes |
| `rt deps submodules` | moves every submodule to the head of the branch it already tracks |
| `rt git cherry-pick` | moves commits from the dev branch into the release branch, chosen by hash, by ticket, or from a list |
| `rt flow <name>` | runs the command sequence the config defines under that name |

Release branches are never merged back into the dev branch. Fixes land in the
dev branch and travel to the release branch through `rt git cherry-pick`.

### A release, start to finish

```sh
rt repo sync                       # clone what is missing, fast-forward dev
rt repo check                      # config against the working copies

rt release branch                  # cut release-X.Y from dev
rt deps freeze                     # pin submodules, update deps, commit, push
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
rt repo check                   # config vs disk, deps_cmds programs on PATH
rt repo sync                    # clone missing, fetch, fast-forward dev
rt repo sync --no-pull          # fetch only, leave the dev branch where it is
rt repo clean api               # throw away uncommitted changes in one project
rt repo clean --untracked       # and delete files git does not track
```

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

### changelog

Every command in `changelog_cmds` runs in order in the project directory, with
`changelog_env` in the environment, and each one redirects to the file it
generates — the same two-generator setup the GitLab job uses (git-cliff plus a
plain git-log document). Whatever the generators changed is then committed and
pushed in one commit. A project can replace the list with its own
`changelog_cmds` and override single environment keys with `changelog_env`;
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
rt deps freeze --no-deps                          # move the pins, run no deps_cmds
```

On the release branch it rewrites the submodule branches in `.gitmodules`,
fetches inside each submodule and runs `git submodule update --remote` — the
fetch matters because git only fetches there on demand, so a branch created
after the submodule was cloned would otherwise be missing. Then it moves the
module refs of `deps_pins`, runs the project's `deps_cmds` and commits and
pushes whatever changed. Like `deps submodules`, the plan reads `.gitmodules` on
the release branch rather than on whatever is checked out; a configured
submodule that branch does not have is reported and skipped instead of failing
the project mid-run. `freeze_to: release` resolves to the highest release branch
of the submodule's own remote; when the submodule is itself a configured project
with a local clone, that is answered locally instead of over the network.

### deps submodules

Moves every submodule to the head of the branch `.gitmodules` already has it
tracking, runs the project's `deps_cmds` so a lock file follows the new pin,
then commits and pushes. It is the everyday counterpart of `deps freeze`: freeze
decides *which* branch a submodule follows and is a release act, this one only
follows it further and works on the dev branch.

```sh
rt deps submodules                          # every project, on its dev branch
rt deps submodules api worker               # only these
rt deps submodules --branch release-1.5     # somewhere other than the dev branch
rt deps submodules --submodule pylib        # only this submodule path
rt deps submodules --no-deps                # move the pins, run no deps_cmds
rt deps submodules --no-commit              # update the working tree, commit nothing
rt deps submodules --allow-dirty            # finish what a --no-commit run left
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

## Tests and linting

```sh
make test
make lint
```

The cherry-pick tests build throwaway repositories with a real submodule and
exercise the conflict resolution end to end. Linting runs golangci-lint 2.12.2
with `.golangci.yml`: every linter is on except a documented handful, and the
gci/gofmt/gofumpt/goimports formatters are enforced (`make format` applies
them, `make lint.fmt` only reports).
