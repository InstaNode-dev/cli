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

### Targeting an environment

Every `new` verb accepts an optional `--env` flag that the API honors
(default: `development`; CLAUDE.md rule 11):

```bash
instant db new --name app-db --env production
instant cache new --name app-cache --env staging
```

The response prints both the resolved `env` and — when the server downgraded
a request (e.g. anonymous caller asking for `production`) — an
`env_override_reason` line explaining why.

## Multi-service stacks

`instant stack new` is a CLI follow-up — not shipped yet. For multi-service
stacks today, use either the MCP `create_stack` tool (Claude Code, Cursor,
any MCP client) or a direct `POST /stacks/new` call against the API. The
request schema lives at `https://api.instanode.dev/openapi.json`.

Single-service deploys via the CLI are also still a follow-up — `instant
deploy --help` prints the canonical MCP/curl paths.

## Build from source

```bash
go build -o bin/instant .
```
