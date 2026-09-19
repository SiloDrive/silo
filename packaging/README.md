# Packaging

The `.deb` and `.rpm`, the systemd unit, and what an installed Silo looks like.

Everything here is built from the release binaries `.github/workflows/build.yml`
already produces. There is no second compile: `nfpm` takes a finished
`silo` binary and wraps it, so a packaging change cannot change the program.

This directory owns the Debian and RPM side, the service definition, the AUR
recipe in `aur/`, and the Homebrew formula generator in `homebrew/`. The
tap those formulae are served from lives elsewhere, in `dkam/homebrew-silo`;
`install.sh` at the repository root owns the binary-only install.

## What the package installs

| Path | What it is |
|---|---|
| `/usr/bin/silo` | the binary |
| `/usr/lib/systemd/system/silo.service` | the unit, **disabled by default** |
| `/etc/silo/silo.conf` | every setting, commented out at its default |
| `/etc/silo/silo.env` | environment overrides and secrets, mode `0600` |
| `/var/lib/silo` | the data directory, created by systemd at first start |
| `/usr/share/doc/silo/` | `LICENSE.txt`, `README.md`, `NOTICE` |

Both files under `/etc/silo` are `config|noreplace`, so an upgrade never
overwrites an edited one: dpkg prompts, rpm writes the new version alongside as
`.rpmnew`.

The package creates a `silo` system user and group before the files land. It
does **not** remove them on uninstall, and does not touch `/var/lib/silo` —
removing a package must not remove the data it was serving, and recycling the
uid would hand those files to whoever gets the number next.

## Why it is not enabled on install

A fresh Silo has no accounts. It is claimed with a setup token generated on the
host, and `docs/deployment.md` asks that the token be claimed *before* the port
opens. A package that started the service on install would invert that order on
every machine it touched.

So installing prints the three steps instead:

```sh
sudo systemctl enable --now silo
sudo -u silo silo -d /var/lib/silo setup-token
```

`try-restart` on upgrade, not `restart`: an operator who deliberately stopped
the service gets it left stopped.

## The unit

`packaging/systemd/silo.service`, and three parts of it are load-bearing rather
than decorative.

**`TimeoutStopSec=120`.** `SIGTERM` drains the HTTP server for up to 30
seconds, *then* seals any open packs, *then* checkpoints the SQLite WAL
(`fileserver/server.go`, `handleSignals`). Sealing writes files the store must
be able to describe afterwards. systemd's default 90-second `SIGKILL` can land
in the middle of that; 120 leaves room for the drain plus a large seal.

**`Environment=SILO_DATA_DIR=/var/lib/silo`, not `ExecStart=… -d …`.** Silo's
own precedence is flags → environment → `silo.conf` → compiled defaults, and
the unit reproduces it. `-d` is a flag and would beat anything the operator
wrote in `silo.env`, so the data directory is set as an environment variable
that appears *before* `EnvironmentFile=` — systemd applies the two in file
order, so the env file still wins. Overriding it means granting the new path
too, because `ProtectSystem=strict` makes everything outside `StateDirectory=`
read-only:

```sh
sudo systemctl edit silo
```
```ini
[Service]
ReadWritePaths=/srv/silo
```

**`-C /etc/silo/silo.conf` is safe when the file is absent.** The loader stats
the path and falls through to the compiled defaults if it is missing. A file
that exists and cannot be *parsed* is fatal, and the service will not start —
so validate a change against a copy before restarting:

```sh
silo serve -C /tmp/silo.conf.new -d /tmp/throwaway
```

The unit does not ship a logrotate config. Logs go to the journal, and Silo's
`SIGUSR1` rotation only applies to `-l` file logging, which the unit does not
use.

## Homebrew

`homebrew/generate-formula.sh` writes the `Formula/silo.rb` that the
`dkam/homebrew-silo` tap serves. The formula itself lives in the tap; what
lives here is the thing that produces it, because the inputs are here.

```sh
# From a local release directory -- what CI does.
packaging/homebrew/generate-formula.sh v0.5.1 --dist dist

# From a published release -- the by-hand path.
packaging/homebrew/generate-formula.sh v0.5.1 -o ../homebrew-silo/Formula/silo.rb
```

With no `-o` it writes to stdout, so it can be diffed against what the tap
already has without touching it. `SILO_RELEASE_BASE` moves the download host,
the same variable `install.sh` takes.

**Why it generates from `--dist` in CI.** The tap's own generator waits for the
release to be visible and then fetches every `.sha256` back over HTTP. The
`homebrew` job in `build.yml` runs after `release` in the same workflow, where
those sidecars are already on disk — so it reads them locally instead. No race,
no round trip, and the checksums are provably the ones that were published
rather than whatever the URL answers with.

The job needs a `HOMEBREW_TAP_TOKEN` secret with `contents:write` on
`dkam/homebrew-silo`. **Without it the job succeeds and prints the command to
run by hand** — a missing tap token should not turn a good release red. It also
skips the push when the formula is already at that version, so a re-run is
harmless.

### Two things the generated formula fixes

**The test block was asserting something that cannot be true.** The formula in
the tap has:

```ruby
assert_match "v#{version}", shell_output("#{bin}/silo version")
```

`normalizeVersion` in `cmd/silo/main.go` strips the leading `v`, so
`silo version` prints `0.5.1` and never `v0.5.1`. That assertion fails for
every release. The generated formula compares against the bare string:

```ruby
assert_equal version.to_s, shell_output("#{bin}/silo version").strip
```

**The description was inherited from a server Silo no longer is.** It read
"Seafile-compatible server and client in one binary"; that compatibility was
dropped on purpose, and `brew info` was still advertising it. It now reads
"Single-binary file sync server with per-library end-to-end encryption" — 69
characters, no article, no formula name, which is what `brew audit` wants.

### The tap still has its own copy

`dkam/homebrew-silo` has `bin/generate-formula.sh`, which is where this script
came from. Two generators for one formula is one too many, and the tap's copy
carries both bugs above. Replacing it with a line pointing here is the tidy-up;
it is a change to the other repository, so it is not made from this one.

## Building locally

Needs [nfpm](https://nfpm.goreleaser.com/) — `go install
github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.47.0`, the version CI pins.

```sh
mkdir -p packaging/.staged
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o packaging/.staged/silo ./cmd/silo
chmod 0755 packaging/.staged/silo

ARCH=amd64 VERSION=0.0.0-dev nfpm pkg -f packaging/nfpm.yaml -p deb -t /tmp/
ARCH=amd64 VERSION=0.0.0-dev nfpm pkg -f packaging/nfpm.yaml -p rpm -t /tmp/
```

**The binary is staged, not passed in.** nfpm expands `${...}` in most fields
but treats `contents[].src` as a glob and leaves it alone, so a `${BIN}` there
fails with `Glob failed: ${BIN}` rather than with anything that points at the
cause. `packaging/.staged/silo` is the fixed path `nfpm.yaml` names; it is
gitignored, and the caller puts the right architecture there before each run.

`VERSION` must not carry a leading `v`; neither dpkg nor rpm accepts one. CI
strips it from the tag. nfpm also rewrites a prerelease the Debian way, so
`0.0.0-dev` comes out as `0.0.0~dev` in the filename — a plain `0.5.1` is
untouched.

Inspect what you built without installing it:

```sh
dpkg -c /tmp/silo_0.0.0~dev_amd64.deb
rpm -qlp /tmp/silo-0.0.0~dev-1.x86_64.rpm
```

On a machine with no `dpkg`, the deb is an `ar` archive:

```sh
ar x silo_0.0.0~dev_amd64.deb && zstd -dc data.tar.zst | tar -tv
```

## In CI

The `packages` job in `build.yml` runs after `build`, downloads the two Linux
tarballs, and produces four packages (deb and rpm × amd64 and arm64) plus a
`.sha256` for each. It runs on manual dispatch as well as on a tag, so a
packaging mistake surfaces before the tag rather than during the release.

It also installs the `.deb` on the runner and removes it again. That is the
only thing that proves the maintainer scripts actually parse, the paths are
right and `/etc/silo/silo.env` really landed at `0600`. The runner has no
systemd, so every `systemctl` call falls through the `-d /run/systemd/system`
guard — which is worth exercising too, since that is the path a container
install takes.

The `homebrew` job runs after `release`, on tags only, and updates the tap. It
is the one job here that writes to another repository, which is why it is
gated on a secret and why it is the last thing in the file.

## The AUR package

`aur/` holds the three files the `silo-bin` AUR package is made of: `PKGBUILD`,
the `.SRCINFO` that mirrors it, and a systemd unit. Like the deb and the rpm it
is a `-bin` package — it downloads the release tarball and checks it against a
recorded SHA-256 rather than compiling anything, so it cannot produce a binary
that differs from the published one.

Two things about it are easy to get wrong.

**Its unit is a user unit, and the one in `systemd/` is a system unit.** They
are not two copies of the same file and neither should be made to match the
other. The deb and rpm install a service for a machine that exists to run Silo:
a dedicated `silo` account, state under `/var/lib/silo`, config at
`/etc/silo/silo.conf`, and the hardening that goes with running as root long
enough to drop privileges. The AUR unit installs to
`/usr/lib/systemd/user/silo.service` and runs as whoever enabled it, reading
`~/.config/silo/env`, because the Arch user installing this is usually running
Silo for themselves on a machine they already sit in front of. `WantedBy`
differs accordingly — `default.target` rather than `multi-user.target`.

**This is a copy, not the source of truth.** The AUR takes packages by git
push, to `ssh://aur@aur.archlinux.org/silo-bin.git`, and that repository is
what the AUR actually serves. Editing the files here changes nothing on the AUR
until someone copies them across and pushes. They can therefore drift, and the
only thing that catches it is looking.

Publishing a new version means bumping `pkgver`, replacing the three
`sha256sums` with the ones for the new release's tarballs and LICENSE, and
regenerating `.SRCINFO` — the AUR reads metadata from that file, not from the
`PKGBUILD`, so a `PKGBUILD` edit that skips it is invisible:

```sh
cd packaging/aur
updpkgsums                 # rewrites sha256sums from the sources
makepkg --printsrcinfo > .SRCINFO
makepkg -si                # build and install it locally before pushing
```

No `provides`/`conflicts` on `silo` is deliberate and the `PKGBUILD` says why:
that AUR name belongs to LLNL's unrelated scientific data format library, and
claiming it would make the two falsely exclusive.
