# Contributing to the InstaNode CLI

## Filing issues

Bugs and feature requests welcome at https://github.com/InstaNode-dev/cli/issues.

## Workflow

```
git clone https://github.com/InstaNode-dev/cli
cd cli
go build ./...
go vet ./...
go test ./... -short -p 1
```

All three must pass before opening a PR.

## Local testing

```
go run ./cmd/instanode --help
go run ./cmd/instanode up
```

Set `INSTANODE_API_URL=http://localhost:8080` to point at a local api instance.

## Style

- Follow existing patterns. Help strings are user-visible — keep them tight.
- New flags get a one-line `--help` description and a test that exercises the success + error path.
- Errors returned to the user should include a one-line agent_action hint matching the api's structured error envelope (see api/internal/handlers/helpers.go for the registry).

## PR checklist

- `go build ./...` green
- `go vet ./...` green
- `go test ./... -short -p 1` green
- Help-text changes mirrored in README if user-visible
- New command added: include a unit test + a README example

## License

MIT. By contributing, you agree your contributions are licensed under the same.
