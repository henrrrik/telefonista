# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

Telefonista is a simple voicemail service that sends recorded messages to a Slack channel (or user). It uses the 46elks telephony API and stores recordings in S3-compatible object storage. Optionally transcribes voicemails using OpenAI's Whisper API.

# Build & Test
- `make test` — run tests with the race detector (matches CI)
- `make fmt` — run before committing (gofmt -s + goimports)
- `make lint` — run before committing; golangci-lint with vet, staticcheck, gosec, gocyclo (>10) etc., config in `.golangci.yml`
- `make vulncheck` — govulncheck

# Project
- Single-package Go app (all code in root main package)
- Interfaces (`objectStore`, `transcriber`) are used for test doubles — keep this pattern
- CI runs on PRs and main: golangci-lint, tests with race detector, govulncheck

## Workflow

- Use Red/Green TDD
- Create a PR for all changes — do not push directly to main.
- CI runs tests and deploys on merge to main.

