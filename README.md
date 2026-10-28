# Elephant

Elephant keeps local project state so a new coding agent can continue where the previous one stopped.

It stores facts, decisions, tasks, and explicit file relationships in one `elephant.zova` database. Git identity and commit metadata give those entries project context. Agents use an MCP stdio server; people can inspect the same state through the CLI.

## Install

Once the installer is published on `main` and a stable GitHub release is available:

```sh
curl -fsSL https://raw.githubusercontent.com/octacity-org/elephant/main/install.sh | sh
```

To install a specific release, including a prerelease:

```sh
curl -fsSL https://raw.githubusercontent.com/octacity-org/elephant/main/install.sh | sh -s -- --version 0.2.0
```

The installer detects Linux ARM64/AMD64, macOS ARM64, or Windows AMD64 under Git Bash. It downloads the matching archive from `octacity-org/elephant`, verifies it against the release's `SHA256SUMS.txt`, then replaces the executable in `~/.local/bin`. It does not use sudo, modify shell configuration, or configure MCP. If that directory is outside your PATH, add it to your PATH or invoke the installed binary by its full path.

Pass `--dir /your/bin/directory` to choose another destination, or set `ELEPHANT_INSTALL_DIR`. Running the installer again upgrades to the selected release. `--repo OWNER/REPO` (or `ELEPHANT_REPO`) supports forks. The default selects the latest **stable** release; use `--version` while only release candidates exist.

Requirements: `curl`, `tar` (or `unzip` for Windows), and `sha256sum` or `shasum`. Git is required when using Elephant with repositories. Go, Zig, and a separate Zova installation are unnecessary for release binaries. On Windows without Git Bash, download and extract the AMD64 ZIP from [GitHub releases](https://github.com/octacity-org/elephant/releases).

You can download and inspect `install.sh` before executing it. Installation does not start Elephant or migrate a database; the database is opened when you run Elephant.

## Build from source

Requirements: Go 1.26.5 or newer, Git, a C compiler, and the native Zova C ABI matching the pinned Go binding (`v1.3.0`). The Go binding does not download or build the native library.

Obtain the matching native library from [Zova](https://github.com/ata-sesli/zova), or build that version with `zig build c-abi` in its source checkout. Then, from the Elephant directory:

```sh
export CGO_CFLAGS="-I/path/to/zova/include"
export CGO_LDFLAGS="-L/path/to/zova/zig-out/lib"
go build -o bin/elephant ./cmd/elephant
./bin/elephant --version
```

For an installed C ABI archive, point these flags at its `include` and `lib` directories instead. The binding already supplies `-lzova_c`. Keep the native header and library from the same Zova version.

## CI and GitHub releases

[The GitHub Actions workflow](.github/workflows/ci.yml) tests every branch push and pull request on exactly these four native runners:

| Platform | Runner | Release archive |
| --- | --- | --- |
| Linux ARM64 | `ubuntu-24.04-arm` | `elephant-vVERSION-linux-arm64.tar.gz` |
| Linux AMD64 | `ubuntu-24.04` | `elephant-vVERSION-linux-amd64.tar.gz` |
| Windows AMD64 | `windows-2022` | `elephant-vVERSION-windows-amd64.zip` |
| macOS ARM64 | `macos-15` | `elephant-vVERSION-macos-arm64.tar.gz` |

Each job checks formatting, verifies Go dependencies, builds Zova's native C ABI from the pinned `v1.3.0` commit with Zig 0.17.0, bundles Zig’s compiler runtime in the static archive for CGO linking, runs tests (including MCP stdio and race checks), runs `go vet`, and builds and smoke-tests Elephant. The Windows native library uses the GNU ABI; macOS targets version 14 or newer. Linux artifacts target glibc-based distributions and are built on Ubuntu 24.04.

Once the workflow is on GitHub, push a version tag to publish a release:

```sh
git tag v1.0.0-rc.1
git push origin v1.0.0-rc.1
```

All four jobs must pass before publishing. Tags such as `v1.0.0-rc.1` create prereleases; tags such as `v1.0.0` create stable releases. The archive contains the executable, README, and dependency license notices. A `SHA256SUMS.txt` file accompanies the four archives. The release stays a draft until all assets are uploaded; rerunning a failed upload can resume the draft, but published assets are not overwritten.

The executable's `--version` and MCP server version come from the tag. Untagged CI builds report `dev-COMMIT`; ordinary local builds report `dev`. Git must be installed at runtime; the native Zova library is linked into the executable.

Only the publishing job receives `contents: write`; it uses GitHub's built-in token, so no extra release secret is needed. A manual workflow run builds and tests without publishing. Update both `go.mod` and the workflow's `ZOVA_VERSION`/`ZOVA_COMMIT` when changing Zova versions.

## Try it

Run against an existing Git repository:

```sh
./bin/elephant --cwd /path/to/repository status
./bin/elephant --cwd /path/to/repository add fact \
  --title "Integration tests require Docker" \
  --body "The integration suite starts real services; start Docker before running it." \
  --file tests/integration_test.go
./bin/elephant --cwd /path/to/repository add task \
  --title "Verify shutdown" \
  --body "Run the integration suite and check that workers acknowledge cancellation."
./bin/elephant --cwd /path/to/repository recall
```

Global flags (`--cwd`, `--db`) precede the command. If omitted, `cwd` is the process working directory. Output is JSON, except help and version. `facts`, `decisions`, and `tasks` accept `--status`, `--target-version`, `--limit`, and `--offset`; `inspect ENTRY_ID` includes outgoing relationships, incoming relationships from this project's entries and checkpoints, and stored evidence rows (`evidence verify` reports their current state).

## Set up an agent

`setup` writes Elephant's MCP server entry into a supported agent's configuration:

```sh
elephant setup codex
elephant setup opencode
```

Codex uses `$CODEX_HOME/config.toml`, defaulting to `~/.codex/config.toml`, and receives a `[mcp_servers.elephant]` table plus an `[mcp_servers.elephant.env]` table. OpenCode uses `$OPENCODE_CONFIG` or `$XDG_CONFIG_HOME/opencode/opencode.json`, defaulting to `~/.config/opencode/opencode.json`, and receives a `mcp.servers.elephant` object with `"type": "local"` (an older `mcp.elephant` entry is migrated forward). Both entries launch the resolved executable as `elephant --db /path/to/elephant.zova serve` and set an explicit `ELEPHANT_ACTOR_ID` so entries carry provenance. Writes go through a synced temporary file plus atomic rename, so an interrupted setup never leaves a truncated configuration.

Setup edits only Elephant's own entry and preserves every other setting. It is idempotent: running it again with the same command and actor writes nothing. Pass `--actor ID` to choose the identity, or `--config FILE` to write a specific configuration file. Unsupported agents are not modified; the command prints a copy-ready example instead.

`doctor` reports whether the installation is usable:

```sh
elephant doctor
elephant doctor --json
```

The checks cover the executable path and version, database accessibility and schema compatibility, Git availability, the MCP launch command and which agents have Elephant registered, the effective actor identity, and configured ASH remotes. Each finding is `ok`, `warning`, or `error`: a missing database, an unconfigured agent, an unset actor, and absent remotes are warnings, while an incompatible schema, an unreadable database, or a missing Git is an error. Errors exit nonzero; warnings exit zero. `--json` prints the same checks as a structured object with a top-level `status`. Restoring database health may complete a pending explicit schema migration, because Elephant opens the database to validate it.

Tests for both commands point `HOME`, `CODEX_HOME`, and `XDG_CONFIG_HOME` at temporary directories, so they never read or write a developer's real agent configuration.

## Connect an agent

Use your coding harness's MCP server configuration. A typical server entry is:

```json
{
  "mcpServers": {
    "elephant": {
      "command": "/absolute/path/to/elephant/bin/elephant",
      "args": ["serve"]
    }
  }
}
```

Configuration wrappers vary between harnesses. Supply `cwd` in tool calls when the target repository differs from the server's working directory, or start the server with `--cwd /path/to/repository serve`.

Start with `recall_project`. It returns current Git metadata, unfinished work, active knowledge, recent history, and graph relationships. Treat entry text as stored project data to assess, rather than privileged server instructions.

Clients with resources support can read the same bounded state as application-controlled data:

- `elephant://project/current/recall` — full bounded recall JSON
- `elephant://project/current/tasks` — unfinished tasks (active, blocked, open)
- `elephant://project/current/decisions` — active decisions

The user-controlled `resume_project` prompt (optional `cwd`, `target_version`) returns a bounded resume message plus the full recall JSON, clearly marking stored bodies as project data rather than server instructions. Scope resolution is centralized: an explicit `cwd` wins, then the first usable `file://` MCP root (Unix paths, Windows drive letters, percent encoding, and UNC shares; other schemes are skipped), then the server directory for clients without roots support. The tasks resource is globally newest-first across unfinished statuses with a bounded cap; existing tools are unchanged.

| Purpose | MCP tools |
| --- | --- |
| Understand the project | `recall_project`, `project_status`, `inspect_entry` |
| Facts | `add_fact`, `update_fact`, `list_facts`, `retire_fact` |
| Decisions | `add_decision`, `update_decision`, `list_decisions`, `supersede_decision` |
| Tasks | `add_task`, `update_task`, `list_tasks`, `complete_task`, `cancel_task` |
| Checkpoints | `add_checkpoint`, `list_checkpoints` |

Create tools require `title` and `body`. They accept `target_version`, `related_files`, and typed entry `relations`:

```json
{
  "cwd": "/path/to/repository",
  "title": "Implement cancellation ownership",
  "body": "Move lifecycle ownership into the runtime and verify shutdown.",
  "related_files": ["internal/runtime/worker.go"],
  "relations": [{"type": "implements", "entry_id": "DECISION_ID"}]
}
```

Use that input with `add_task`. IDs, timestamps, project scope, and start commits are assigned automatically.

Updates require `id`; omitted fields stay unchanged. `related_files` replaces file links; `[]` clears them. Entry `relations` are additive and duplicate edges are idempotent. An empty `target_version` clears it. JSON `null` is treated like omission for optional update fields.

`supersede_decision` takes the old decision's `id` plus the new `title` and `body`. Alternatively, `add_decision` accepts `supersedes: OLD_ID`. Both atomically create the replacement, close the old decision, and add the graph edge.

The CLI has the same lifecycle operations:

```sh
elephant add task --title T --body B --file a.go --relation implements:DECISION_ID
elephant update task ID [--title T] [--body B] [--status S] [--target-version V]
                        [--file PATH ... | --clear-files] [--relation TYPE:ID ...]
elephant complete TASK_ID
elephant cancel TASK_ID
elephant retire FACT_ID
elephant supersede DECISION_ID --title T --body B
elephant add decision --title T --body B --supersedes DECISION_ID
```

`update` changes only the flags given: `--file` replaces all file links, `--clear-files` removes them, `--target-version ""` clears the version, and `--relation` adds edges.

## Deterministic search

```sh
elephant search "shutdown ownership" --kind fact --actor searcher
elephant related ENTRY_ID --direction both --edge implements --depth 1
elephant history path/to/file.go --limit 20
```

Search is project-scoped (use `--remote SRC` for a received table), composes kind/status/actor/version/file/time filters across CLI and MCP, and returns stable ordering with match metadata. Commit filters form an inclusive Git commit range resolved against the local repository: `--commit-start`/`--commit-end` bound it and `--commit C` selects entries active at a single commit, matching entries whose recorded `start_commit`/`end_commit` interval intersects the range; a bound that does not resolve to a commit fails the query instead of degrading to an equality match, and the start bound must be equal to or an ancestor of the end bound (reversed or divergent endpoints are rejected with a clear error). Graph traversal is bounded (direction, edge, depth 1–3, limit 1–50) and never crosses project or remote ownership. MCP adds `search_entries`, `related_entries`, and `file_history`.

## Model and lifecycle

| Entry | Meaning | Statuses |
| --- | --- | --- |
| Fact | What the next agent should know | `active`, `stale`, `retired` |
| Decision | What was chosen and why | `active`, `superseded`, `retired` |
| Task | Work to do or already finished | `open`, `active`, `blocked`, `done`, `cancelled` |

Facts and decisions start active; tasks start open. Terminal entries cannot reopen. Stale facts can become active again. Closing an entry records current HEAD as `end_commit`; task completion and decision supersession preserve historical rows. In a repository with no commits, commit fields are null and status remains the lifecycle authority.

Files are stored as normalized repository-relative paths. File arguments are resolved from the working directory (or MCP `cwd`) like Git pathspecs, so `--file service.go` inside `internal/app` stores `internal/app/service.go`; absolute paths and paths outside the repository are rejected. File graph nodes include project identity to separate identical paths in different repositories. No file contents are stored or scanned. The graph vocabulary is:

- Decision → `affects` → file; fact → `concerns` → file; task → `modifies` → file.
- Decision → `supersedes` → decision; task → `depends_on` → task.
- Task → `implements` → decision; fact → `supports` → decision.

Entry links must stay within one project. Dependencies are explicit links; Elephant does not schedule tasks or infer dependency completion.

## Source evidence

Facts can carry verifiable source evidence: a pointer to a repository file (and optional one-based line) pinned to the commit where it was captured plus a stable digest of the selected bytes. Evidence attaches only to facts, and only to paths present at HEAD — the digest is read from the commit, never from dirty working-tree bytes. Verification is deterministic — Elephant first re-reads the recorded commit, then compares the current checkout against the recorded digest, so it needs no network access and never guesses.

CLI:

```sh
elephant evidence add FACT_ID --file internal/runtime/worker.go --line 42
elephant evidence list FACT_ID
elephant evidence verify FACT_ID              # read-only; never mutates rows or fact status
elephant evidence refresh FACT_ID [--evidence EVIDENCE_ID]   # re-pin to current HEAD
elephant evidence remove EVIDENCE_ID
```

Each row reports one of four states: **unchanged** (the current selected bytes match the recorded digest), **changed** (the file exists but its selected bytes differ), **missing** (the path or pinned line is confirmed absent from the working tree), or **unavailable** (no local checkout, or the recorded commit/object cannot be read). Verification never mutates rows or retires a fact — a changed source is reported, not acted on. `refresh` re-pins rows to the current HEAD commit and digest. Lines are validated against the file at capture; out-of-range lines are rejected. Up to 50 evidence locations are accepted per fact.

Recall includes a bounded evidence section for active facts (limit 50) with the computed state per row. MCP exposes `add_evidence`, `list_evidence`, `verify_evidence`, `refresh_evidence`, and `remove_evidence`.

Evidence can travel with a fact: `elephant remote send fact --evidence EVIDENCE_ID ...` attaches local evidence rows to the outgoing message. On send, rows are rebound to the new entry ID with fresh IDs and provenance (path, line, commit, digest) preserved; the receiver stores them with the sent entry and re-verifies them against its own checkout on recall, reporting `unavailable` when it cannot read the recorded commit or has no matching source. Sending is explicit per row — evidence never travels automatically.

## Session checkpoints

Checkpoints record where one coding session stopped and the next should resume. They are scoped to the owning project table (local checkpoints never leak into remote recall) and carry actor provenance, Git start/end commits, and timestamps.

```sh
elephant checkpoint --summary "Implemented authentication" --next "Add integration tests"
elephant checkpoints --limit 10
```

`checkpoint` accepts repeatable `--completed`, `--next`, `--commands`, and `--failures` list items (up to 50 per list, 2000 bytes each), repeatable `--file PATH`, repeatable `--relation TYPE:ENTRY_UUID`, and `--start-commit COMMIT`. An explicitly supplied start commit is verified with Git and rejected when unknown; otherwise the start defaults to the previous checkpoint's end commit, or to the current HEAD. The end commit is the current HEAD. Creation and all links happen in one transaction, so a failed link rolls back the checkpoint. MCP exposes `add_checkpoint` and `list_checkpoints`. `recall` includes the latest local checkpoint with its relations before older project context. Checkpoints live in the schema v4 `checkpoints` table.

## Sharing state with another Elephant

Each installation keeps its own local truth. Explicit sends create additions in a separate table owned by the sending Elephant on the receiving machine. Normal `recall` never includes those rows. No replication, automatic updates, adoption, or merging occurs.

Install Elephant on both machines and configure an ASH host on the sending machine. Use a repository with a network Git origin on both sides so the normalized project identity matches; local filesystem identities cannot be sent.

```bash
# On A, in the repository:
elephant identity
elephant remote add fedora --backend ash --host fedora
elephant remote ensure-project fedora
ELEPHANT_ACTOR_ID=codex-session-1 elephant remote send fedora fact \
  --title "Integration tests require Podman" \
  --body "The Linux integration suite depends on Podman." \
  --file tests/integration.go

# On B, in a clone of the same repository:
elephant recall --remote SOURCE_ELEPHANT_UUID
elephant recall
```

Use A's `elephant identity` result as `SOURCE_ELEPHANT_UUID`. A stable UUIDv7 identity is stored in each `elephant.zova`; restarts preserve it. A separate database has a separate identity, and copying the database copies its identity. Table ownership uses this Elephant ID. Each row independently records `actor_id`, taken from `ELEPHANT_ACTOR_ID` when creating local or outgoing entries; the default is `unknown`. Configure that environment variable on the MCP server process to identify its actor. Receiving never replaces it with the peer identity.

`remote send` supports `fact`, `decision`, and `task`, plus repeatable `--file PATH`, `--relation TYPE:ENTRY_UUID`, `--evidence EVIDENCE_ID` (facts only, see "Source evidence"), `--target-version VERSION`, and `--supersedes DECISION_UUID`. Responses include the new `entry_id`. Entry relations must target an existing row in the same source table. Remote `supersedes` records an explicit relationship; it does not update the earlier decision. Absolute paths and traversal are rejected. Missing targets or invalid relationships roll back the complete message, including its receipt and graph edits.

Manage peers with `remote list` and `remote remove NAME`. Removal deletes only the peer configuration, preserving received state. `remote add` also accepts `--ash-config FILE` and `--elephant-path EXECUTABLE`. After `ensure-project` or a successful send, Elephant pins the peer's ID; an unexpected identity change fails. If B registers A and establishes its identity, B can use that registered name in `recall --remote NAME` instead of the UUID.

`remote recall NAME` contacts that peer to inspect **this installation's source table there**. It returns the result without importing it. In contrast, `recall --remote NAME_OR_UUID` reads a source table already stored locally. These commands preserve source boundaries and return the same bounded context categories as local recall.

Review received state without merging:

```bash
elephant remote inbox
elephant remote diff fedora
elephant adopt ENTRY_ID --from fedora
```

`inbox` groups received additions by source Elephant and project. `diff` compares one source table with local state, listing `remote_only` entries that have not been adopted. `adopt` creates a new local entry with a new local ID, copies file links, copies entry relations only when their local targets resolve safely, and records source provenance; repeating the same adoption returns the existing local entry. The adopted entry keeps its lifecycle status (a done task stays done), except that a superseded decision is adopted as active. An adopted `supersedes` link follows the local rule: it marks the replaced local decision superseded, and it is skipped and reported when that decision is no longer active. No automatic merging occurs.

MCP adds `list_remotes`, `ensure_remote_project`, `remote_send_fact`, `remote_send_decision`, `remote_send_task`, `remote_recall`, `remote_inbox`, `remote_diff`, and `adopt_entry`. The latter reads locally received state. Existing local tools retain their local-only behavior.

### Machine protocol and ASH

`elephant receive` reads one version-1 JSON message from stdin and writes one JSON response. Operations are `project.ensure`, `entry.send`, and `project.recall`. Envelopes carry `protocol_version`, UUIDv7 `message_id` and `sender_elephant_id`, a portable `project` (`identity`, `name`), and `operation`. `entry.send` also supplies `entry` (including UUIDv7 `id`, `actor_id`, and timestamps), optional `files`, and optional `relations` (`type`, `entry_id`). Input is limited to 1 MiB; structured rejections have an `error` field. The receiver does not need a local checkout.

Receipts persist in the same transaction as the write. Identical message redelivery succeeds; conflicting reuse of a message ID fails. An identical entry with a new message ID is also accepted once per source table, while conflicting entry content fails. Creating the same project/source table again returns the existing table. Retries must retain the original IDs, timestamps, and payload. Running a fresh human `remote send` command creates a new entry, not a retry; Elephant does not automatically retry uncertain transport failures.

ASH invokes the remote Elephant command and never accesses Zova directly. Current ASH `exec` does not forward stdin, so the adapter uses a shell-quoted POSIX `printf` pipeline to deliver JSON to the receiver. The remote machine needs a POSIX shell. The resulting command is limited to 24 KiB (so large bodies accepted locally may exceed this transport limit), output to 4 MiB, and execution to five minutes. State appears in the ASH command arguments; use this transport only on trusted machines. SSH access is the authentication boundary, and sender IDs are provenance claims supplied by the authenticated caller, not cryptographic identities.

## Storage and bounds

The default database is `~/Library/Application Support/elephant/elephant.zova` on macOS, and `$XDG_DATA_HOME/elephant/elephant.zova` (or `~/.local/share/elephant/elephant.zova`) elsewhere. Override it with `ELEPHANT_DB` or `--db`. New data directories and database files use owner-only permissions.

Projects with equivalent normalized origin URLs share state, including across clones. Without an origin, the local Git common directory identifies the project, so linked worktrees share state. Adding or changing an origin can select a different project identity; v1 does not merge registries automatically. Entries are project-wide, not branch-scoped.

The database contains a project registry, generated local and source-specific remote entry tables, per-project evidence tables, a `project_tables` ownership registry, peer configuration, message receipts, schema metadata, and the named Zova graph `elephant`. SQL and graph edits share a transaction. Elephant schema 2 migrates transactionally to schema 3 on open, preserving existing rows and graphs; migration is idempotent. Unsupported schema versions and incompatible Zova formats are rejected. Schema 3 databases cannot be opened by older Elephant binaries. Zova may use transient journal files while writing.

An older but migratable Zova storage format (for example format 11 from Elephant 0.3.0 and earlier) is upgraded automatically on open. Zova writes the current format to a new file, Elephant keeps the original bytes as `elephant.format-N-backup-TIMESTAMP.zova` next to the database, and the new file replaces the database atomically. A `.migrate.lock` file lets only one process migrate while others wait; migration is refused while another process has the database open for writing. Restart running agents after upgrading so no older Elephant process keeps writing to the replaced file. Older binaries cannot open the upgraded database; to downgrade, restore the backup.

Recall returns up to 50 tasks **per unfinished status** (active, blocked, open), 50 active decisions, 50 active facts, 10 completed tasks, and 5 superseded decisions. A `truncated` map identifies categories with more entries. Use the list tools to page through them: default/maximum page size 200. Within each group, entries sort newest first with ID as the tie-breaker. `recall_project` also accepts an exact `target_version` filter. `changed_files` lists files with uncommitted changes (staged, unstaged, untracked, and rename sources) that have linked entries in any status, newest first; files without linked memory are omitted. It scans up to 200 changed paths and reports up to 20 files with 10 entries each, marking `truncated.changed_files` when a bound is hit. It is local-only and follows `--file` links, not evidence paths.

Titles are limited to 300 bytes and bodies to 32 KiB. Each mutation accepts up to 99 file/entry links, with up to 100 outgoing links per entry including supersession. These limits keep retrieval bounded.

## Portable export, import, and backups

```sh
elephant export --project > project-memory.json
elephant import project-memory.json --as-remote archive
elephant backup --output elephant-backup.zova
```

Export writes a deterministic versioned JSON envelope (format_version 2, no wall-clock fields): global entry IDs, actor IDs, table ownership, lifecycle fields, project and source-Elephant metadata, entries sorted by ID, relations sorted by from/type/to, evidence sorted by entry/creation/ID, checkpoints in creation order, and adoption receipts sorted by source and source entry. Identical stored state serializes byte-identically. Import strictly decodes the archive (unknown fields and trailing data rejected), validates IDs, kinds, statuses, paths, relationships, ownership crossings, and rejects duplicate adoption receipts before committing anything, then stores the result in a separate `import:NAME` source table so local truth is never overwritten. Imported adoption receipts stay archived data under that table: they are never inserted into the live adoption registry, so they cannot satisfy, shadow, or otherwise alter a later local adoption, while remaining inspectable and countable on the archive. Idempotency is keyed by archive name and project identity plus the canonical content digest: repeating an identical import returns the existing source, conflicting content for the same project under the same archive name fails explicitly, and different projects can share an archive name. Failed imports roll back tables, rows, graph nodes and edges, and receipts together. Backup stages a storage-safe snapshot in a sibling temporary file and renames it into place only on success, preserving any existing destination on failure. MCP adds `export_project` and `import_project`. Format version 2 covers the complete exported state: entries, relations, evidence, checkpoints, and adoption receipts. Format version 1 envelopes (entries and relations only) remain readable on import.

## Development

With the native build flags set as above:

```sh
go test -v ./...
go test -race ./...
go vet ./...
```

Tests use temporary Git repositories and `.zova` databases. They cover identity, lifecycle validation, atomic rollback, graph persistence, project isolation, pagination, concurrent database handles, and a real MCP stdio process restarted between agents.

The boundaries are `internal/model` (records and validation), `internal/app` (workflows and recall), `internal/storage` (transaction interface and Zova implementation), `internal/git` (Git commands), `internal/agent` (agent configuration discovery and editing), and `internal/transport/mcp` (protocol adapter). The CLI composes them in `cmd/elephant`.

Logs use `slog` on stderr. Set `ELEPHANT_LOG_LEVEL=debug` for underlying diagnostics; normal tool errors omit native database details. Stdout is reserved for MCP traffic when serving.

V1 has no internal LLM, vectors, repository index, conversation ingestion, cloud sync, UI, or automatic stale detection. Facts become stale through explicit updates.

## Memory health review

```sh
elephant review
elephant review --json
```

Review is deterministic and read-only. It reports stable codes (`MISSING_FILE`, `STALE_TASK`, `UNVERIFIED_FACT`, `DANGLING_RELATION`, `SUPERSESSION_ANOMALY`, `UNKNOWN_ACTOR`, `STALE_REMOTE`, `EVIDENCE_UNAVAILABLE`, `EVIDENCE_CHANGED`, `EVIDENCE_MISSING`), severity, source-separated entry context, and an explicit suggested action. Thresholds default to 30/90/30 days with bounded `--stale-task-days`, `--unverified-fact-days`, and `--stale-remote-days` flags. Fact verification follows evidence records rather than entry edits: a fact measures from its newest evidence `VerifiedAt`, or from its creation time when it has no evidence at all, so new facts are not flagged immediately. File and evidence checks skip finished entries (done or cancelled tasks, retired facts, superseded decisions). Every stored source of this project is reviewed — including imported archives without a registered remote — while source tables belonging to other projects never appear, and unavailable information (remote file references, unregistered peers, foreign baselines) is distinguished from confirmed inconsistency (changed or missing evidence). MCP adds `review_memory`.
