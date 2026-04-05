# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

Telefonista is a simple voicemail service that sends recorded messages to a Slack channel (or user). It uses the 46elks telephony API and stores recordings in S3-compatible object storage. Optionally transcribes voicemails using OpenAI's Whisper API.

# Build & Test
- `go test -v -race ./...` — run tests (matches CI)
- `gofmt -s -w .` — run before committing
- `go vet ./...` — run before committing
- `gocyclo -over 15 .` — run before committing (no functions should exceed 15)

# Project
- Single-package Go app (all code in root main package)
- Interfaces (`objectUploader`, `transcriber`) are used for test doubles — keep this pattern
- CI runs on master: vet + test with race detector

## Workflow

- Use Red/Green TDD
- Create a PR for all changes — do not push directly to master.
- CI runs tests and deploys on merge to master.

