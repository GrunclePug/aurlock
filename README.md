# aurlock

A minimal AUR helper that never installs an AUR package at a commit you haven't approved.

## Overview

AUR packages get taken over, and the next update quietly changes what the PKGBUILD downloads or runs. aurlock pins every AUR package to the git commit you approved. Each update shows only the diff since that approval, and a fixed set of red flags makes risky changes harder to wave through. It fetches, checks, builds and installs AUR packages itself, and ships as a single static binary.

## Features

* **Approvals**: an AUR package installs only at a commit you approved. The approvals file is plain text and can be synced between machines.
* **Diff since approval**: an update shows only what changed since the approved commit, never the whole package again.
* **Red flags**: fixed rules over the PKGBUILD text, `.SRCINFO`, git history and AUR metadata. Any flag means approving takes typing the package name.
* **Nothing runs before approval**: until a commit is approved, aurlock reads `.SRCINFO` and the raw PKGBUILD text only. It never sources the PKGBUILD or runs makepkg.
* **Sandboxed builds**: makepkg runs in bubblewrap with the system read-only and `$HOME` hidden. Sources download in one step, and the build itself runs offline unless you grant it network.
* **Second opinion**: `[c]` asks Claude Code, without tools, to explain the flags. It's advice only and never approves.
* **arcus compatible**: handles the yay-style calls arcus makes, so it can replace yay.
* **Pure Go**: standard library only, built with `CGO_ENABLED=0` into one static binary.

## Installation

### Requirements

* Go 1.27.3 or later and Make, to build.
* At runtime: git, pacman, makepkg (base-devel), bubblewrap and sudo. gpg for packages with signed sources. Claude Code (`claude`) for the optional second opinion.

### Build

```sh
git clone https://github.com/grunclepug/aurlock.git
cd aurlock
make build
sudo make install   # optional, to /usr/local/bin
```

## Usage

```sh
aurlock install solidcraft-bin   # review, approve, build and install
aurlock upgrade                  # the same, for everything installed with aurlock
aurlock outdated                 # list pending updates; exit 1 if any
aurlock list                     # approvals, what's installed here, and what isn't tracked
aurlock forget solidcraft-bin    # drop an approval (doesn't uninstall)
aurlock -version
```

| Command | What it does |
|---------|--------------|
| `install <pkg>...` | Looks the packages up, fetches their git repositories, checks them, prompts, builds and installs. |
| `upgrade` | Like `install`, for every package aurlock installed here whose AUR commit or approval moved on. |
| `outdated` | Lists installed packages whose AUR commit differs from the installed one, and whether that commit is approved yet. Exit 1 if any, 0 if none, 2 on error. Untracked AUR packages are mentioned on stderr but don't count. |
| `list` | Prints the approvals, the packages installed here, and foreign packages aurlock didn't install, with their AUR version if the AUR has them. |
| `forget <pkg>...` | Drops approvals, by package base or package name. The packages stay installed. |

A clean update:

```
solidcraft-bin 0.25.1-1 → 0.26.0-1   no flags
approve and install? [y/N/c/v/d]
```

A flagged one:

```
solidcraft-bin 0.25.1-1 → 0.26.0-1   2 flags
  ! where: new source host example.org (PKGBUILD:14)
  ! what:  curl in build() (PKGBUILD:31)
type "solidcraft-bin" to approve, or [c]laude / [v]iew / [d]iff / [n]o:
```

| Key | Action |
|-----|--------|
| `y` | Approve and install. Only offered when there are no flags. |
| the package name | Approve and install a flagged package. |
| `v` | Show the PKGBUILD and install scripts in `$PAGER`. |
| `d` | Show `git diff` since the approved commit, through git's pager. Updates only. |
| `c` | Ask Claude Code for a second opinion, then return to the prompt. |
| `g` | Grant the build network access. This is a flag, so it then takes the name typed. Offered when the package had a grant before or its last offline build failed on the network. |
| `n`, Enter, anything else | Skip the package. |

A package that's already approved at the AUR's current commit installs without a prompt, for example after another machine approved it. Without a terminal, aurlock fails instead of prompting. Ctrl-C stops at the next safe point and never kills pacman mid-transaction; a second Ctrl-C quits at once.

aurlock upgrades only AUR packages and builds them against what's installed, so upgrade the system first: `sudo pacman -Syu && aurlock upgrade`.

### Configuration

| Variable | Flag | Default | Description |
|----------|------|---------|-------------|
| `XDG_CONFIG_HOME` | | `~/.config` | Holds `aurlock/approved`. |
| `XDG_STATE_HOME` | | `~/.local/state` | Holds `aurlock/installed` and network hints. |
| `XDG_CACHE_HOME` | | `~/.cache` | Holds `aurlock/`: git mirrors, builds, sources, packages and the keyring. |
| `PAGER` | | `less` | Pager for `[v]`. |
| `AURLOCK_AI` | | `claude -p --tools "" ...` | Replaces the second-opinion command. It runs through `sh` and gets the prompt on stdin. It must run without tools. |
| `LANG`, `TERM`, `MAKEFLAGS` | | | Passed into the build sandbox. Nothing else from the environment is. |
| | `--noconfirm` | off | With `-S`: install approved packages without prompts, and fail any package that needs review. It never approves anything. |
| | `--needed` | off | With `-S`: skip packages installed at the approved commit. |
| | `--removemake` | off | With `-S`: remove the make dependencies installed for the build. |

### Moving from another AUR helper

aurlock only upgrades packages it installed. `aurlock list` shows the foreign packages it doesn't track. Adopt the AUR ones with `aurlock install <pkg>...`: that reviews the package at the AUR's current commit and rebuilds it, since nothing says which commit the installed build came from. aurlock never adopts packages automatically, because a locally built package can share an AUR package's name.

## Files

| Path | What | Shared |
|------|------|--------|
| `~/.config/aurlock/approved` | Approvals, one line per package base. | Yes, if you sync it. |
| `~/.local/state/aurlock/installed` | Which approved commit each installed package came from. | No |
| `~/.local/state/aurlock/needs-network/` | Packages whose last offline build looked like a network failure. | No |
| `~/.cache/aurlock/repos/<pkgbase>.git` | Bare mirror of the package's AUR git repository. | No |
| `~/.cache/aurlock/build/`, `src/`, `pkg/` | Build directories, downloaded sources and built packages, one directory per package base. | No |
| `~/.cache/aurlock/gnupg/` | Public keys for packages with signed sources. | No |

The approvals file is plain text, one record per line, `#` comments allowed:

```
# pkgbase  commit  version  maintainer  approved  network
solidcraft-bin 3f2a...c91e 0.25.1-1 FrogSnot 2026-10-29 offline
```

* `maintainer` is the AUR maintainer at approval time (`-` if orphaned). The next review compares against it.
* `network` is `offline` or `online`. Approving a new commit always resets it to `offline` unless you grant network again.
* aurlock writes it atomically into the directory of its real path, so a symlink into a synced folder stays a symlink.
* If a Syncthing conflict copy (`approved.sync-conflict-*`) sits next to it, aurlock warns on every run until you merge it by hand.

## Red flags

Any flag means approving takes typing the package name. On a first install the rules look at the whole package. On an update they look at what changed since the approved commit.

* **Who**: the AUR maintainer changed since the approval; a commit author whose name and email appear nowhere earlier in the history; the package is orphaned; it was first submitted under 30 days ago; the approved commit is missing from the history or isn't an ancestor of the current one.
* **Where**: a new source host, or an existing source moved to another host; a `SKIP` checksum on a non-VCS source; a checksum changed while `pkgver` stayed the same; a URL host in the PKGBUILD that isn't among `.SRCINFO`'s sources; `.SRCINFO` unchanged while the PKGBUILD's sources or checksums changed; VCS sources (`git+` and friends), whose upstream code isn't pinned.
* **What**: an install script added or changed (or present, on a first install); an `install=` in the PKGBUILD that `.SRCINFO` doesn't list; `curl`, `wget`, `npm`, `npx`, `bun`, `pnpm`, `yarn`, `pip`, `cargo install`, `go install`, `git clone`, piping into a shell, `base64`, `eval`, `/dev/tcp`, `nc`, `ncat` or `socat` in the PKGBUILD, an install script or a local shell script source; repository files the build doesn't need; a network grant.

Top-level assignments count as data, so `depends=(curl)` isn't a flag, but a command substitution inside one is.

## Builds

Every build runs in bubblewrap, and only after its commit is approved:

1. The build directory gets the reviewed files byte for byte, not a git checkout, so the repository's `.gitattributes` can't change what gets built.
2. Dependencies are installed first with `sudo pacman -S --needed --asdeps`, since makepkg can't use sudo inside the sandbox. An AUR package that depends on another AUR package that isn't installed is refused: install that one first.
3. **Sources, with network**: `makepkg --verifysource` downloads and verifies the sources. If the package lists `validpgpkeys`, aurlock first fetches those exact fingerprints from keyserver.ubuntu.com into its own keyring. Your `~/.gnupg` never enters the sandbox.
4. **Build, offline**: `makepkg --holdver` runs with no network unless the approval grants it.
5. The built packages you asked for are installed with `sudo pacman -U`.

The sandbox mounts `/usr`, `/etc` and the pacman database read-only, with a fresh `/dev`, `/proc` and `/tmp`. It can write only the package's build, source and output directories. `$HOME` isn't mounted, so a build can't read SSH keys, browser profiles or tokens. The environment is cleared apart from a few variables, and nested user namespaces are disabled.

If an offline build fails with what looks like a network error, aurlock says so. The next `aurlock install` of that package offers to grant network for that commit.

Known limitation: bubblewrap maps only your own user into the sandbox. A PKGBUILD that changes ownership to another user (`chown root:root` in `package()`, or `install -o`) fails to build.

## Second opinion

`[c]` runs `claude -p --tools "" --safe-mode --strict-mcp-config --no-session-persistence` in a fresh empty directory, with the prompt on stdin. Claude Code gets no tools, no MCP servers, hooks or other customizations, and keeps no session. The prompt has the package's name, versions, maintainer, age and flags, plus the diff (or the whole PKGBUILD and install scripts on a first install). It marks the package content as untrusted data, and asks for why each flag fired, a malicious likelihood and a recommendation in about ten lines. The answer is advice: approving still takes `y` or the typed name.

Set `AURLOCK_AI` to use another command. It must not have tools either.

## arcus

aurlock understands the calls arcus makes to its AUR helper:

```sh
aurlock --removemake -S --needed [--noconfirm] <pkg>...
aurlock -Rns [--noconfirm] <pkg>...
aurlock -D --asdeps <pkg>...
```

`-S` works like `install`. `-R...` and `-D...` run `sudo pacman` with the same arguments, and `-R` also drops removed packages from the install log. Any other pacman-style flag is an error.

## Development

```sh
make run ARGS="list"   # run as version dev
make test              # tests, against a fake AUR and fake system commands
make lint              # go vet and staticcheck
make build             # bin/aurlock
make clean
```

## Contributing

Fork the repository and open a pull request.

## License

MIT. See [LICENSE](LICENSE).

## Author

Chad Humphries | [Website](https://grunclepug.com/) | [GitHub](https://github.com/GrunclePug) | [More projects](https://github.com/GrunclePug?tab=repositories)
