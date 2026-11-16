# Development

Building, testing, and the recorded fixtures. Back to the [README](../README.md).

```sh
make build      # ./bin/whyfail
make install    # ~/go/bin/whyfail
make test       # hermetic: unit tests + recorded Prow fixtures
make lint       # go vet, then golangci-lint when it is installed
make test-live  # real network: four digests (3 recert + cert-manager), health, and savings
make fixtures   # re-record both testdata corpora from live Prow
```

`make test` is hermetic: it serves recorded artifacts through `internal/gcstest` and
never touches the network. `make test-live` is the opposite — it reads the real Prow
bucket and is therefore manual, not part of CI.

## CI

Every push to `main` and every pull request runs these steps in
[`.github/workflows/ci.yml`](../.github/workflows/ci.yml):

1. `gofmt -l .` — fails if anything is unformatted
2. `go vet ./...`
3. `golangci-lint` (pinned version in the workflow)
4. `go mod tidy` followed by `git diff --exit-code go.mod go.sum` — **this is what
   keeps cobra the only direct dependency.** Adding one shows up as a diff, not as a
   silent change.
5. `go test ./... -count=1`
6. `make build`

## Fixtures

`testdata/` holds public artifacts recorded by `scripts/fetch-fixtures.sh`:

- three failing runs from
  [rh-ecosystem-edge/recert#2364](https://github.com/rh-ecosystem-edge/recert/pull/2364)
  — extension-JSON test results, `sippy=not tracked`
- one from
  [openshift/cert-manager-operator#533](https://github.com/openshift/cert-manager-operator/pull/533)
  — a bare `artifacts/junit.xml`, `sippy=tracked`

Bodies are stored exactly as GCS stores them, gzip where the object is gzip-encoded
and identity otherwise, so the sizes in `listing.json.gz` and the on-disk sizes agree
and `Artifacts.TotalBytes` stays truthful.

The hermetic tests serve those files through `internal/gcstest`. The `live` build tag
runs the same assertions against the real bucket, so a Prow layout change surfaces as
a test failure naming the run rather than as a wrong number in a digest.

Changing a fixture means re-running `make fixtures` and committing the result; the
script is the only supported way to produce them.
