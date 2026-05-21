# instant CLI

Zero-friction infrastructure CLI for [instanode.dev](https://instanode.dev).

## Install

```bash
go install github.com/InstaNode-dev/cli@latest
```

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
