# github-gitea-sync

`github-gitea-sync` is a local Go CLI that reconciles same-owner, same-name GitHub repositories into Gitea pull mirrors. It runs once, reports what it found, optionally creates missing pull mirrors, and exits.

V1 is intentionally conservative:

- Mirrors Git repository data only.
- Matches GitHub and Gitea repositories by the same owner name and repository name.
- Skips GitHub forks.
- Does not delete, archive, rename, overwrite, convert, repair, force-sync, or mutate settings on existing Gitea repositories.
- Does not create missing Gitea users or organizations.
- Does not mirror issues, pull requests, releases, wiki pages, packages, projects, branch protection, collaborators, teams, secrets, settings, or non-Git content.
- Does not provide a daemon, scheduler, background worker, database, web UI, SSH runtime path, Kubernetes runtime path, or interactive prompt.
- Does not include automated tests, fixtures, mocks, CI workflows, or coverage artifacts for V1.

## Build

From this repository:

```sh
go build -o bin/github-gitea-sync ./cmd/github-gitea-sync
```

Then run the binary from `./bin/github-gitea-sync`.

## Configuration

Reconciliation commands read configuration only from environment variables.

| Variable | Required | Default | Purpose |
| --- | --- | --- | --- |
| `GGS_GITHUB_TOKEN` | Yes | None | GitHub API token used to inventory repositories. For private mirror creation, the token may be sent to Gitea as the migration credential for the one create request. |
| `GGS_GITEA_TOKEN` | Yes | None | Gitea API token used to inspect owners/repositories and create safe pull mirrors during `sync`. |
| `GGS_GITEA_BASE_URL` | Yes | None | Absolute Gitea base URL. The CLI appends `/api/v1` when needed. |
| `GGS_GITHUB_ACCOUNTS` | Yes | None | Comma-separated GitHub users or organizations to reconcile. Empty entries are invalid. |
| `GGS_INCLUDE_PRIVATE` | No | `true` | Include private GitHub repositories in inventory. |
| `GGS_INCLUDE_ARCHIVED` | No | `true` | Include archived GitHub repositories in inventory. |
| `GGS_MIRROR_INTERVAL` | No | `8h` | Positive Go duration sent to Gitea as the pull mirror interval for created mirrors. |
| `GGS_STATE_PATH` | No | `~/.config/github-gitea-sync/state.json` | Local JSON state path for GitHub inventory metadata and conditional request data. |
| `GGS_LOG_FORMAT` | No | `text` | Currently only `text` is accepted. |

Example setup with placeholders:

```sh
export GGS_GITHUB_TOKEN='<github-token>'
export GGS_GITEA_TOKEN='<gitea-token>'
export GGS_GITEA_BASE_URL='https://gitea.example.com'
export GGS_GITHUB_ACCOUNTS='<owner>'
export GGS_STATE_PATH='/tmp/ggs-phase005-state.json'
```

Do not put real token values in committed docs, scripts, shell snippets, logs, or workflow artifacts.

## Commands

Running without a subcommand prints help and exits successfully:

```sh
./bin/github-gitea-sync
```

Available commands:

| Command | Behavior |
| --- | --- |
| `help` | Prints help. Extra arguments are a usage error. |
| `version` | Prints the binary name and version string. Extra arguments are a usage error. |
| `plan` | Reads GitHub and Gitea state and reports intended mirror actions. It does not create or mutate Gitea repositories. |
| `verify` | Reads GitHub and Gitea state and reports mirror health, source drift, missing sources, and warnings. It does not create or mutate Gitea repositories. |
| `sync` | Reads state and creates only entries reported as safe `CREATE_MIRROR` actions. It creates Gitea pull mirrors through `/repos/migrate`, then re-fetches the repository before reporting `CREATED_MIRROR`. |

`plan`, `verify`, and `sync` accept `--json`:

```sh
./bin/github-gitea-sync plan --json
```

No other flags or positional arguments are supported.

## Output

Text output is the default. It includes:

- `command`
- `started_at`
- `finished_at`
- `gitea_base_url` when configured
- a summary count line
- a status count line
- per-result rows

JSON output is emitted as one object with:

- `command`
- `started_at`
- `finished_at`
- `gitea_base_url`
- `summary`
- `results`

Result entries may include owner/repository names, GitHub and Gitea identities, private/archived flags, expected and observed source URLs, a message, and a redacted error string. Output must not include configured token values or credential-bearing clone URLs.

## Status Taxonomy

| Status | Severity | Meaning |
| --- | --- | --- |
| `OK_MIRRORED` | `ok` | Existing Gitea pull mirror source matches the GitHub repository. |
| `CREATE_MIRROR` | `action` | The expected Gitea repository is missing and is safe for `sync` to create as a pull mirror. |
| `CREATED_MIRROR` | `action` | `sync` created the pull mirror and confirmed the repository exists afterward. |
| `SKIPPED_FORK` | `skipped` | GitHub repository is a fork and is out of V1 scope. |
| `BLOCKED_MISSING_GITEA_OWNER` | `blocked` | The same-name Gitea user or organization does not exist. |
| `BLOCKED_OWNER_TYPE_MISMATCH` | `blocked` | GitHub owner type and Gitea owner type do not match. |
| `BLOCKED_EXISTING_NON_MIRROR` | `blocked` | The destination Gitea repository exists but is not a pull mirror. |
| `BLOCKED_MIRROR_SOURCE_MISMATCH` | `blocked` | The destination is a pull mirror, but its source does not match the GitHub repository. |
| `WARN_MIRROR_SOURCE_UNVERIFIED` | `warning` | The destination is a mirror, but the source could not be proven from Gitea data. |
| `WARN_DUPLICATE_NAME` | `warning` | A repository name appears under multiple configured Gitea owners. This warning does not block by itself. |
| `REPORT_GITHUB_SOURCE_MISSING` | `warning` | `verify` or `sync` found a Gitea repository with no visible same-owner GitHub source in the configured inventory. |
| `ERROR_CONFIG` | `error` | Required or optional configuration is invalid. |
| `ERROR_GITHUB_ACCESS` | `error` | GitHub owner or repository inventory failed. |
| `ERROR_RATE_LIMITED` | `error` | GitHub returned primary or secondary rate limiting. |
| `ERROR_GITEA_ACCESS` | `error` | Gitea owner, repository, or mirror creation access failed. |
| `ERROR_INTERNAL` | `error` | Local state, output, or post-create confirmation failed internally. |

## Exit Codes

| Exit code | When |
| --- | --- |
| `0` | Help, version, and reconciliation reports with no `error` or `blocked` entries. Warnings and actions alone do not force a non-zero exit. |
| `1` | Reconciliation reports with at least one `error` or `blocked` entry, plus internal output/runtime failures. |
| `2` | Usage errors, such as an unknown command, unsupported flag, or unexpected argument. |
| `3` | Configuration load errors reported as `ERROR_CONFIG`. |

## Local State

The CLI stores local JSON state at `GGS_STATE_PATH` or `~/.config/github-gitea-sync/state.json`. Missing state files are treated as empty state. Corrupt JSON or unsupported state versions fail clearly.

State version `1` may contain:

- GitHub owner identity metadata.
- Repository IDs, names, owner names, full names, private/archived/fork flags, and safe URLs.
- Skipped fork snapshots.
- ETags and conditional request metadata.
- Cached repository page snapshots.
- Last successful inventory timestamps.

State is written through a temporary file followed by rename. Before writing, the CLI rejects configured secrets and credential-bearing URLs. For validation or one-off runs, prefer a temporary path:

```sh
export GGS_STATE_PATH='/tmp/ggs-phase005-state.json'
```

`plan` and `verify` may update local state after successful inventory, but they do not mutate Gitea repositories.

## Security And Token Handling

- Use tokens with only the access needed for the selected GitHub and Gitea owners.
- Configure tokens through environment variables only.
- Never commit real token values or credential-bearing clone URLs.
- Do not pass tokenized clone URLs as source URLs.
- The CLI redacts configured secrets from errors and sanitizes credential-bearing URLs before output or state writes.
- Private mirror creation may pass the GitHub token to Gitea for that create request, but the CLI must not print it or persist it in state.
- README examples use placeholders only.

## GitHub Behavior

GitHub inventory uses the REST API and:

- Detects each configured owner through `/users/{owner}`.
- Lists organization repositories through `/orgs/{owner}/repos`.
- Lists user repositories by merging public `/users/{owner}/repos` results with authenticated `/user/repos` results.
- Requests `per_page=100`.
- Excludes forks from mirror creation and reports them as `SKIPPED_FORK`.
- Includes private and archived repositories by default.
- Stores ETags, conditional request metadata, and cached page snapshots in local state.
- Reports primary and secondary rate limits as `ERROR_RATE_LIMITED`.

## Gitea Behavior

The target Gitea contract is Gitea 1.26.4. The local OpenAPI file `../gitea-swagger.v1.json` is the source contract for V1 inspection of mirror verification and creation shapes.

Gitea inventory and creation use the REST API and:

- Validates each same-name Gitea owner as an organization or user.
- Lists owner repositories through owner-type-specific endpoints.
- Reads repository mirror fields, including `mirror`, `mirror_interval`, and `original_url`.
- Creates pull mirrors through `/repos/migrate` only for `CREATE_MIRROR` entries during `sync`.
- Sends `mirror: true`, the configured `mirror_interval`, `service: github`, and disables non-Git migration options.
- Does not call destructive endpoints or `mirror-sync`.

## Validation Notes

Credential-free validation:

```sh
go build -o bin/github-gitea-sync ./cmd/github-gitea-sync
go vet ./...
./bin/github-gitea-sync
./bin/github-gitea-sync help
./bin/github-gitea-sync version
./bin/github-gitea-sync plan
```

The last command should fail with `ERROR_CONFIG` when required environment variables are unset.

Credential-backed validation should use a temporary `GGS_STATE_PATH` and carefully selected owners. Run `plan`, `verify`, `plan --json`, and `verify --json` first and confirm they are read-only with respect to Gitea repositories. Run `sync` only against a deliberately selected GitHub repository whose same-owner/same-name Gitea repository is confirmed missing and whose Gitea owner already exists.
