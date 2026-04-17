# instant CLI

Zero-friction infrastructure CLI for [instanode.dev](https://instanode.dev).

## Install

```bash
go install github.com/instant-dev/cli@latest
```

## Usage

```bash
instant db new                 # Provision a Postgres database
instant cache new              # Provision a Redis cache
instant nosql new              # Provision a MongoDB document store
instant queue new              # Provision a NATS JetStream queue
instant resources              # List your provisioned resources (requires login)
instant status                 # Show locally tracked resources
instant login                  # Log in to your instanode.dev account
instant whoami                 # Show current account
```

## Build from source

```bash
go build -o bin/instant .
```
