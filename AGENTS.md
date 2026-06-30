# Agent Instructions

This repository contains the `github-gitea-sync` Go CLI.

- Keep `cmd/github-gitea-sync` as a thin entrypoint and place reusable behavior under `internal/`.
- Use the Go standard library first. Add dependencies only when they are clearly justified by a phase brief.
- Do not add automated tests, fixtures, mocks, CI workflows, coverage outputs, or test-only helpers for V1 unless the project requirements change.
- Never print configured tokens or other secret values in help, errors, logs, placeholders, or diagnostics.
- Do not perform destructive Gitea actions. Later reconciliation phases must stay conservative unless explicitly authorized.
- Do not add daemons, background workers, schedulers, services, web UI, or interactive prompts unless a later phase explicitly scopes them.
- Keep GitHub and Gitea API behavior limited to the scoped V1 reconciliation paths. Do not add new external integrations or broaden API behavior unless a later implementation brief explicitly scopes that work.
