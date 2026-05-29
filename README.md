# instant CLI

Zero-friction infrastructure CLI for [instanode.dev](https://instanode.dev).

## Install

Pre-built binaries for darwin / linux × amd64 / arm64 (the curl-pipe-sh
script auto-detects your platform):

```bash
curl -sSfL https://instanode.dev/install.sh | sh
```

The installer downloads the latest release archive from
[GitHub Releases](https://github.com/InstaNode-dev/cli/releases), verifies
its SHA-256 against the signed `checksums.txt`, and drops the binary at
`/usr/local/bin/instant`. Set `INSTANT_INSTALL_DIR=$HOME/.local/bin` to
avoid sudo; set `INSTANT_VERSION=v0.2.0` to pin a specific release.

Or, with a Go toolchain already installed:

```bash
go install github.com/InstaNode-dev/cli@latest
```

Windows users: download the `.zip` from the
[releases page](https://github.com/InstaNode-dev/cli/releases) and add
`instant.exe` to your `PATH`.

## Usage

Every provisioning command requires a `--name` flag. The name must be 1–64
characters and match `^[A-Za-z0-9][A-Za-z0-9 _-]*$`; omitting it is rejected
both locally and by the API (HTTP 400).

```bash
instant db new --name app-db          # Provision a Postgres database
instant cache new --name app-cache    # Provision a Redis cache
instant nosql new --name app-docs     # Provision a MongoDB document store
instant queue new --name app-jobs     # Provision a NATS JetStream queue
instant resources                     # List your provisioned resources (requires login)
instant status                        # Show locally tracked resources
instant login                         # Log in to your instanode.dev account
instant whoami                        # Show current account
```

## Build from source

```bash
go build -o bin/instant .
```
