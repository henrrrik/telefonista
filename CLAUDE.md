use red/green tdd

# Build & Test
- `make` — formats and builds
- `go test -v -race ./...` — run tests (matches CI)
- `go vet ./...` — run before committing

# Project
- Single-package Go app (all code in root main package)
- Interfaces (`objectUploader`, `transcriber`) are used for test doubles — keep this pattern
- CI runs on master: vet + test with race detector
