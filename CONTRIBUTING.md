# Contributing

Thanks for helping improve Roast. Bug reports and focused pull requests are
welcome. For large features, new dependencies, inference protocols, or changes
to artifact formats, open an issue before writing the implementation so the
contract can be agreed on first.

## Development

Roast requires Go 1.27 or newer. Clone the repository, then run:

```sh
go test ./...
go vet ./...
go build ./cmd/roast
```

Format Go changes with `gofmt`. Tests use local HTTP servers and temporary Git
repositories; they must not require real model credentials or send repository
content to an inference provider.

Before opening a pull request, also check that `go mod tidy` leaves `go.mod`
and `go.sum` unchanged. CI runs vulnerability, Git-history secret, and
dependency-license checks. New runtime dependencies must use a license
compatible with Roast's MIT/Apache-2.0 dual license and should be justified in
the pull request.

Keep changes narrow and include regression tests for behavior changes. Update
the README whenever a public flag, environment variable, configuration field,
artifact, data destination, or exit-code behavior changes.

## Security

Do not report vulnerabilities in a public issue. Follow [SECURITY.md](SECURITY.md)
instead.
