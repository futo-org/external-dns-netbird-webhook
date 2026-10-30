# AGENTS.md: external-dns-netbird-webhook

Guidance for AI coding agents (and humans) writing Go in this repository.
Where a fact could change (exact versions, task commands, CI steps), this
file points at where to check rather than asserting a specific value, since
restating it here just goes stale.

## Working in this repo: commits and safety

- PR titles follow [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/):
  `<type>[(scope)][!]: <description>` (e.g. `fix(config): reject a negative
  timeout`), which is what drives release-please's version bumps. Individual
  commit messages don't have to follow the format, though matching it is
  fine. Sign off commits: `git commit -s`.
- Never `git commit`, `git push`, or open a PR unless asked to. Ask before
  any destructive or hard-to-reverse action (force-push, `git reset --hard`,
  deleting a branch, rewriting history) instead of defaulting to it.
- Never touch secrets or gitignored files. Check the actual `.gitignore`
  before assuming it already excludes something like `*.key`/`*.crt`/`.env`.
  The NetBird token is passed by env var specifically so it is never
  committed; don't be the exception.
- Don't state a library's API, flags, or defaults from memory: verify
  against `pkg.go.dev`, the source in the module cache, or this project's
  own code. That includes the NetBird SDK and management server and the
  external-dns webhook client and planner, whose behavior this provider
  depends on in detail.
- After a change, run this repo's actual test and lint tasks (see "Build,
  lint, test" below) before calling it done. Don't claim untested code
  works.

## Baseline

- **Idiomatic.** Follow [Effective Go](https://go.dev/doc/effective_go) and
  the [Code Review Comments](https://go.dev/wiki/CodeReviewComments) wiki.
  `gofmt -s` runs on every staged `.go` file via lefthook and again in CI:
  never hand-format, and don't fight it with inline exceptions. Comments
  explain non-obvious constraints only (a hidden invariant, why a workaround
  exists, what would surprise a reader); don't narrate what good naming
  already says, and don't reference the current change or past behavior in
  a comment: that belongs in the PR description and rots as the code moves
  on.
- **Go version.** The language version is whatever `go.mod`'s `go`
  directive says; read it from there rather than assuming one. The
  directive is pinned to the lowest patch release of its minor that the
  dependencies allow (`1.N.0` unless one forces higher);
  `.mise/config.toml`'s `tools.go` is the toolchain that actually builds and
  tests, so the two are expected to differ. Raise the directive only when
  the code or a dependency needs a newer Go version. Write idiomatic Go for
  the directive's version, and run `go fix ./...` (a modernizer runner since
  Go 1.26) after a toolchain bump to pick up mechanical migrations.
- **Idempotent.** The webhook is stateless: every response is re-derived
  from NetBird, so a restart or an extra replica can't affect correctness.
  Reconciliation must converge: applying the same desired state twice makes
  no further writes, and a second external-dns sync with unchanged sources
  plans nothing.
- **DRY and minimal, without premature abstraction.** Three similar call
  sites are fine as-is; don't introduce an interface, options struct, or
  generic helper until a real caller needs the variance it buys. Touch only
  what the task requires, and match the existing style even where you'd do
  it differently. Remove imports, variables, and functions your own change
  orphaned; leave pre-existing dead code alone and mention it instead.
- **Unit tested**, table-driven via `t.Run` subtests, with plain stdlib
  `testing`; don't introduce a test framework. Use `t.Context()` rather than
  `context.Background()` in tests. The `test` task runs with `-race`.
- **`log/slog`**, JSON handler to stdout by default (`LOG_FORMAT=text` for
  local runs), never a third-party logging library. `slog.SetDefault` is
  called once in `main`; use the package-level `slog` functions elsewhere.
- **`github.com/caarlos0/env/v11`** for configuration: one `Config` struct
  in `internal/config`, populated and validated by `Load()`, so invalid
  configuration fails at startup instead of surfacing later as a runtime
  error. Every setting is documented in the README's configuration tables;
  keep them in step. The service is configured entirely by environment
  variables, so don't add a flags dependency.

## Project layout

`cmd/external-dns-netbird-webhook/main.go` is the entrypoint and holds only
wiring: build the logger, load config, construct dependencies, run,
translate the top-level error into an exit code. Everything else lives under
`internal/`:

- `config`: environment configuration and validation.
- `netbird`: the external-dns provider and the NetBird API client.
- `webhook`: the external-dns webhook HTTP protocol.
- `server`: the webhook and health servers, middleware, readiness probe.
- `metrics`, `httpx`: Prometheus metrics and shared HTTP helpers.

## NetBird specifics

- NetBird custom zones hold only A, AAAA, and CNAME records, so external-dns
  runs with `--registry=noop` (no ownership) or `--registry=crd` (ownership
  kept in the cluster; it decides what exists from what it applied and only
  reads NetBird back to copy labels). Both must work. external-dns owns
  whole names and types, so the provider reconciles every record of a name
  and type it mentions, which is also what lets a sync recover after the
  CRD registry forgets a failed update. It never touches a zone outside
  `NETBIRD_ZONES`, and must not make external-dns plan a change it can't
  converge on.
- Talk to NetBird through its REST SDK
  (`github.com/netbirdio/netbird/shared/management/client/rest`). Retries,
  per-attempt timeouts, the response size limit, and API metrics live in
  the `retryingClient` wrapper passed to `rest.WithHttpClient`, because the
  SDK provides none of them.
- The in-memory NetBird API in `internal/netbirdtest`, shared by the unit
  and end-to-end tests, mirrors the management server's record validation
  (zone membership, conflicts, trailing dots). Keep it in step with the
  server, and cover anything that affects convergence with a two-cycle case
  in `TestPlanConverges`, which runs the real external-dns planner.
- The end-to-end tests in `test/e2e` (build tag `e2e`) run the built binary
  and reconcile through external-dns's webhook client and planner with
  `--policy=sync`, under the real noop registry and a stand-in for the CRD
  registry, which needs a Kubernetes API server. Behavior that depends on
  the binary, the HTTP protocol, or several syncs in a row belongs there.

## Errors

Wrap with `fmt.Errorf("<component>: %w", err)` so a caller gets context
without losing the original error for `errors.Is`/`errors.AsType`. Errors
leaving the provider are prefixed `netbird:`; grep a message and you know
where it came from. Define sentinel errors for conditions a caller branches
on, and classify at the boundary (the webhook turns provider errors into
HTTP statuses) rather than scattering classification through business
logic. Never discard an error silently: `_ = someCall()` is only for
genuinely fire-and-forget calls, and say why in a comment when that's not
obvious.

## Context & shutdown

Every function that does I/O takes a `context.Context` as its first
parameter and propagates the one it is given rather than building a fresh
`context.Background()` partway down the call stack. The exceptions are
documented where they occur: `AdjustEndpoints`, whose interface carries no
context, and the readiness probe, which deliberately detaches from the
caller. The root context comes from `signal.NotifyContext`, and the default
handler is re-armed after the first signal so a stuck drain can still be
force-killed. Prefer `golang.org/x/sync/errgroup` over raw `sync.WaitGroup`
plus channels when fanning out goroutines that can fail.

## Build, lint, test (via mise)

Mise is mandatory: it pins the exact Go and tool versions
(`.mise/config.toml`, locked in `.mise/mise.lock`), so running `go build` or
`go test` outside it risks a toolchain mismatch with CI. Run `mise tasks` to
see what's defined; currently `test`, `test-e2e`, `lint`, `lint-fix`, `vet`,
`fmt`, and `vulncheck`.

`lefthook` (`.lefthook.toml`) runs pre-commit: `gofmt -s` on Go files,
`mise fmt` and `mise lock` when the mise config changes, `oxfmt` on JSON,
Markdown, and YAML, and `actionlint` and `zizmor` on workflows. CI
(`.github/workflows/ci.yaml`) runs golangci-lint (including the `e2e` build
tag), the tests plus a `go mod tidy` diff check, the end-to-end tests on pull
requests, govulncheck, and the workflow linters. Lint rules live in
`.golangci.yml`; read it instead of trusting a restated list.

## Containers

The image is a static binary: `CGO_ENABLED=0`, `-trimpath`, and `-ldflags`
stamping `main.Version` and `main.Gitsha`, built `FROM golang:<GO_VERSION>-alpine`
and run `FROM gcr.io/distroless/static:nonroot`. The release workflow passes
the Go version from `.mise/config.toml`. Only drop `CGO_ENABLED=0` if a
dependency genuinely requires cgo, and justify it in a comment. Prometheus
metrics and the health probes are served on `HEALTH_SERVER_ADDR`; the probes
are also mounted on the webhook port.

## Security

`govulncheck ./...` runs in CI as the `Go Vulncheck` job, via
`mise run vulncheck`. Run it locally before cutting a release; a new
advisory that fails the job is an action item (upgrade the module or the Go
toolchain), not noise to suppress.
