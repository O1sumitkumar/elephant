package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"elephant/internal/app"
	"elephant/internal/model"
	"elephant/internal/storage/zova"
	transport "elephant/internal/transport/mcp"
	"elephant/internal/transport/receive"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	// A subcommand's -h has already printed its usage.
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, "elephant:", transport.PublicError(err))
		os.Exit(1)
	}
}

const usage = `Elephant — local continuity for coding agents

Usage: elephant [--cwd DIR] [--db FILE.zova] COMMAND

  serve                 Run the MCP stdio server (also the default command)
  setup AGENT           Register the MCP server with codex or opencode
  doctor [--json]       Diagnose executable, database, Git, MCP, actor, remotes
  recall                Show saved project context and unfinished work
  identity              Show this installation’s stable Elephant ID
  remote                Manage peers, ensure projects, send entries, or fetch state
  receive               Process one machine JSON message from stdin
  status                Show project identity and Git metadata
  facts|decisions|tasks  List entries (--status, --target-version, --limit, --offset)
  inspect ID            Show an entry, its outgoing and incoming relationships, and evidence
  adopt ENTRY_ID --from NAME_OR_UUID  Promote a received entry into local truth
  evidence add ENTRY_ID --file PATH [--line N]
  evidence list|verify ENTRY_ID
  evidence refresh ENTRY_ID [--evidence EVIDENCE_ID]
  evidence remove EVIDENCE_ID
  checkpoint --summary TEXT [--completed TEXT ...] [--next TEXT ...]
             [--commands TEXT ...] [--failures TEXT ...]
             [--file PATH ...] [--relation TYPE:ID ...] [--start-commit COMMIT]
  checkpoints [--limit N] [--offset N]  List session checkpoints
  export [--project]    Write versioned project JSON to stdout
  import FILE [--as-remote NAME]  Load an export into a separate source table
  backup --output FILE.zova  Write a consistent database snapshot
  search [QUERY] [--kind KIND] [--status STATUS] [--actor ACTOR]
         [--target-version V] [--commit C] [--commit-start C] [--commit-end C]
         [--updated-after RFC3339] [--updated-before RFC3339] [--file PATH] [--remote SRC]
  related ENTRY_ID [--direction both|outgoing|incoming] [--edge TYPE] [--depth N]
  history PATH [--limit N]
  review [--json] [--stale-task-days N] [--unverified-fact-days N]
         [--stale-remote-days N]  Deterministic read-only memory health review
  add fact|decision|task --title TEXT --body TEXT [--target-version TEXT] [--file PATH ...]
                        [--relation TYPE:ENTRY_UUID ...] [--supersedes DECISION_ID]
  update fact|decision|task ID [--title TEXT] [--body TEXT] [--status STATUS]
         [--target-version TEXT] [--file PATH ... | --clear-files] [--relation TYPE:ENTRY_UUID ...]
  complete|cancel TASK_ID  Close a task
  retire FACT_ID        Retire a fact
  supersede DECISION_ID --title TEXT --body TEXT [add flags]  Replace an active decision
  remote send fact --title TEXT --body TEXT [--evidence EVIDENCE_ID ...]
                   [--file PATH ...] [--relation TYPE:ENTRY_UUID ...]
  --version             Print version

Use recall --remote NAME_OR_UUID for a locally stored remote source table.
remote inbox groups received additions by source; remote diff NAME compares
one source table with local state; adopt creates a new local entry with
provenance and is idempotent.
setup accepts --actor ID and --config FILE; unsupported agents print an example.
doctor prints text by default and structured JSON with --json.
recall includes a bounded evidence section for active facts; verification
reports unchanged, changed, missing, or unavailable and never retires a fact.
'remote send --evidence' carries a local evidence row with the sent fact;
the receiver re-verifies it against its own checkout on recall.
recall includes the latest checkpoint before older project context, and
changed_files lists uncommitted files that have linked entries.
Export output is deterministic for unchanged state; import validates fully
before committing and never overwrites local truth.
Review is read-only and deterministic; thresholds default to 30/90/30 days.
ELEPHANT_ACTOR_ID identifies the actor creating rows (default: unknown).
ELEPHANT_DB overrides the default database path.
ELEPHANT_LOG_LEVEL accepts debug, info, warn, or error. Logs go to stderr.
`

func defaultDB() (string, error) {
	if p := os.Getenv("ELEPHANT_DB"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "elephant", "elephant.zova"), nil
	}
	root := os.Getenv("XDG_DATA_HOME")
	if root == "" {
		root = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(root, "elephant", "elephant.zova"), nil
}

type fileFlags []string

func (f *fileFlags) String() string     { return fmt.Sprint([]string(*f)) }
func (f *fileFlags) Set(s string) error { *f = append(*f, s); return nil }

// parseFlags parses subcommand flags. Failures are invalid input; -h keeps
// flag.ErrHelp so main exits 0 after the flag set printed its usage.
func parseFlags(f *flag.FlagSet, args []string) error {
	if err := f.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", model.ErrInvalidInput, err)
	}
	return nil
}

func readImportFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", model.ErrInvalidInput, err)
	}
	if len(data) > 32<<20 {
		return nil, fmt.Errorf("%w: import file too large", model.ErrInvalidInput)
	}
	return data, nil
}
func run(ctx context.Context, args []string, out, logs io.Writer) error {
	fs := flag.NewFlagSet("elephant", flag.ContinueOnError)
	fs.SetOutput(logs)
	cwd := fs.String("cwd", "", "repository working directory")
	dbPath := fs.String("db", "", "Zova database path")
	version := fs.Bool("version", false, "print version")
	fs.Usage = func() { fmt.Fprint(out, usage) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("%w: %v", model.ErrInvalidInput, err)
	}
	if *version {
		fmt.Fprintln(out, "elephant", transport.Version)
		return nil
	}
	var level slog.Level
	if raw := os.Getenv("ELEPHANT_LOG_LEVEL"); raw != "" {
		if err := level.UnmarshalText([]byte(raw)); err != nil {
			return fmt.Errorf("%w: ELEPHANT_LOG_LEVEL must be debug, info, warn, or error", model.ErrInvalidInput)
		}
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: level})))
	if *cwd == "" {
		var err error
		*cwd, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	if *dbPath == "" {
		var err error
		*dbPath, err = defaultDB()
		if err != nil {
			return err
		}
	}
	command := "serve"
	rest := fs.Args()
	if len(rest) > 0 {
		command = rest[0]
		rest = rest[1:]
	}
	switch command {
	case "setup":
		return runSetup(ctx, out, logs, rest, *dbPath)
	case "doctor":
		return runDoctor(ctx, out, logs, rest, *dbPath)
	}
	db, err := zova.Open(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	service := app.New(db)
	var result any
	switch command {
	case "receive":
		if len(rest) != 0 {
			return model.ErrInvalidInput
		}
		return receive.Handle(ctx, service, os.Stdin, out)
	case "identity":
		if len(rest) != 0 {
			return model.ErrInvalidInput
		}
		result, err = service.Identity(ctx)
	case "remote":
		result, err = runRemote(ctx, service, *cwd, rest, logs)
	case "serve":
		if len(rest) != 0 {
			return fmt.Errorf("%w: serve takes no arguments", model.ErrInvalidInput)
		}
		return transport.New(service, *cwd).Run(ctx, &sdk.StdioTransport{})
	case "recall":
		f := flag.NewFlagSet("recall", flag.ContinueOnError)
		f.SetOutput(logs)
		source := f.String("remote", "", "source Elephant UUID or registered name")
		if err = parseFlags(f, rest); err != nil {
			return err
		}
		if f.NArg() != 0 {
			return model.ErrInvalidInput
		}
		if *source == "" {
			result, err = service.Recall(ctx, *cwd, "")
		} else {
			result, err = service.RecallRemote(ctx, *cwd, *source)
		}
	case "evidence":
		result, err = runEvidence(ctx, service, *cwd, rest, logs)
	case "status":
		if len(rest) != 0 {
			return model.ErrInvalidInput
		}
		result, err = service.Status(ctx, *cwd)
	case "facts", "decisions", "tasks":
		list := flag.NewFlagSet(command, flag.ContinueOnError)
		list.SetOutput(logs)
		status := list.String("status", "", "status filter")
		target := list.String("target-version", "", "version filter")
		limit := list.Int("limit", 200, "maximum entries (1–200)")
		offset := list.Int("offset", 0, "pagination offset")
		if err = parseFlags(list, rest); err != nil {
			return err
		}
		if list.NArg() != 0 {
			return model.ErrInvalidInput
		}
		kinds := map[string]model.Kind{"facts": model.Fact, "decisions": model.Decision, "tasks": model.Task}
		result, err = service.List(ctx, *cwd, model.Filter{Kind: kinds[command], Status: *status, TargetVersion: *target, Limit: *limit, Offset: *offset})
	case "inspect":
		if len(rest) != 1 {
			return fmt.Errorf("%w: inspect requires one entry ID", model.ErrInvalidInput)
		}
		result, err = service.Get(ctx, *cwd, rest[0])
	case "checkpoint":
		result, err = runCheckpoint(ctx, service, *cwd, rest, logs)
	case "checkpoints":
		result, err = runCheckpoints(ctx, service, *cwd, rest, logs)
	case "add":
		result, err = runAdd(ctx, service, *cwd, rest, logs)
	case "update":
		result, err = runUpdate(ctx, service, *cwd, rest, logs)
	case "supersede":
		result, err = runSupersede(ctx, service, *cwd, rest, logs)
	case "complete", "cancel", "retire":
		result, err = runClose(ctx, service, *cwd, command, rest)
	case "adopt":
		adopt := flag.NewFlagSet("adopt", flag.ContinueOnError)
		adopt.SetOutput(logs)
		from := adopt.String("from", "", "remote name or source Elephant UUID")
		if err = parseFlags(adopt, rest); err != nil {
			return err
		}
		if adopt.NArg() != 1 || *from == "" {
			return fmt.Errorf("%w: adopt requires ENTRY_ID --from NAME_OR_UUID", model.ErrInvalidInput)
		}
		result, err = service.Adopt(ctx, *cwd, adopt.Arg(0), *from)
	case "export":
		f := flag.NewFlagSet("export", flag.ContinueOnError)
		f.SetOutput(logs)
		project := f.String("project", "", "reserved scope selector (current project)")
		if err = parseFlags(f, rest); err != nil {
			return err
		}
		if f.NArg() != 0 || (*project != "" && *project != "current") {
			return fmt.Errorf("%w: export takes no positional arguments", model.ErrInvalidInput)
		}
		result, err = service.Export(ctx, *cwd)
	case "import":
		f := flag.NewFlagSet("import", flag.ContinueOnError)
		f.SetOutput(logs)
		asRemote := f.String("as-remote", "archive", "archive source name")
		if err = parseFlags(f, rest); err != nil {
			return err
		}
		if f.NArg() != 1 {
			return fmt.Errorf("%w: import requires one FILE argument", model.ErrInvalidInput)
		}
		data, readErr := readImportFile(f.Arg(0))
		if readErr != nil {
			return readErr
		}
		result, err = service.Import(ctx, data, *asRemote)
	case "backup":
		f := flag.NewFlagSet("backup", flag.ContinueOnError)
		f.SetOutput(logs)
		output := f.String("output", "", "destination .zova file")
		if err = parseFlags(f, rest); err != nil {
			return err
		}
		if f.NArg() != 0 || *output == "" {
			return fmt.Errorf("%w: backup requires --output FILE.zova", model.ErrInvalidInput)
		}
		if filepath.Ext(*output) != ".zova" {
			return fmt.Errorf("%w: backup destination must end in .zova", model.ErrInvalidInput)
		}
		result, err = map[string]string{"backup": *output}, service.Backup(ctx, *output)
	case "search":
		f := flag.NewFlagSet("search", flag.ContinueOnError)
		f.SetOutput(logs)
		kind := f.String("kind", "", "fact, decision, or task")
		status := f.String("status", "", "status filter")
		actor := f.String("actor", "", "actor ID filter")
		version := f.String("target-version", "", "version filter")
		commit := f.String("commit", "", "entries active at this commit (single-commit range)")
		commitStart := f.String("commit-start", "", "inclusive range start; must precede --commit-end")
		commitEnd := f.String("commit-end", "", "inclusive range end; must follow --commit-start")
		updatedAfter := f.String("updated-after", "", "updated_at >= RFC3339 time (e.g. 2026-09-01T00:00:00Z)")
		updatedBefore := f.String("updated-before", "", "updated_at <= RFC3339 time")
		file := f.String("file", "", "file filter, relative to the working directory")
		source := f.String("remote", "", "source Elephant UUID or registered name")
		limit := f.Int("limit", 20, "maximum results (1-50)")
		offset := f.Int("offset", 0, "pagination offset")
		if err = parseFlags(f, rest); err != nil {
			return err
		}
		if f.NArg() > 1 {
			return fmt.Errorf("%w: search takes at most one query argument", model.ErrInvalidInput)
		}
		var after, before *time.Time
		if *updatedAfter != "" {
			t, err := time.Parse(time.RFC3339, *updatedAfter)
			if err != nil {
				return fmt.Errorf("%w: --updated-after requires RFC3339: %v", model.ErrInvalidInput, err)
			}
			after = &t
		}
		if *updatedBefore != "" {
			t, err := time.Parse(time.RFC3339, *updatedBefore)
			if err != nil {
				return fmt.Errorf("%w: --updated-before requires RFC3339: %v", model.ErrInvalidInput, err)
			}
			before = &t
		}
		query := ""
		if f.NArg() == 1 {
			query = f.Arg(0)
		}
		result, err = service.Search(ctx, *cwd, app.SearchFilters{Query: query, Kind: model.Kind(*kind), Status: *status, Actor: *actor, TargetVersion: *version, Commit: *commit, CommitStart: *commitStart, CommitEnd: *commitEnd, File: *file, Source: *source, UpdatedAfter: after, UpdatedBefore: before, Limit: *limit, Offset: *offset})
	case "related":
		f := flag.NewFlagSet("related", flag.ContinueOnError)
		f.SetOutput(logs)
		direction := f.String("direction", "both", "outgoing, incoming, or both")
		edge := f.String("edge", "", "edge type filter")
		depth := f.Int("depth", 1, "traversal depth (1-3)")
		limit := f.Int("limit", 20, "maximum results (1-50)")
		if err = parseFlags(f, rest); err != nil {
			return err
		}
		if f.NArg() != 1 {
			return fmt.Errorf("%w: related requires one ENTRY_ID", model.ErrInvalidInput)
		}
		result, err = service.Related(ctx, *cwd, f.Arg(0), *direction, *edge, *depth, *limit)
	case "history":
		f := flag.NewFlagSet("history", flag.ContinueOnError)
		f.SetOutput(logs)
		limit := f.Int("limit", 20, "maximum results (1-50)")
		if err = parseFlags(f, rest); err != nil {
			return err
		}
		if f.NArg() != 1 {
			return fmt.Errorf("%w: history requires one file path", model.ErrInvalidInput)
		}
		result, err = service.History(ctx, *cwd, f.Arg(0), *limit)
	case "review":
		f := flag.NewFlagSet("review", flag.ContinueOnError)
		f.SetOutput(logs)
		asJSON := f.Bool("json", false, "machine-readable JSON output")
		staleTask := f.Int("stale-task-days", 30, "days before an open task is stale (1-3650)")
		unverifiedFact := f.Int("unverified-fact-days", 90, "days before a fact needs verification (1-3650)")
		staleRemote := f.Int("stale-remote-days", 30, "days before a remote is stale (1-3650)")
		if err = parseFlags(f, rest); err != nil {
			return err
		}
		if f.NArg() != 0 {
			return fmt.Errorf("%w: review takes no positional arguments", model.ErrInvalidInput)
		}
		findings, reviewErr := service.Review(ctx, *cwd, &app.ReviewOptions{StaleTaskDays: *staleTask, UnverifiedFactDays: *unverifiedFact, StaleRemoteDays: *staleRemote})
		if reviewErr != nil {
			return reviewErr
		}
		if !*asJSON {
			printReviewHuman(out, findings)
			return nil
		}
		result = findings
	default:
		return fmt.Errorf("%w: unknown command %q; use --help", model.ErrInvalidInput, command)
	}
	if err != nil {
		return err
	}
	return writeJSON(out, result)
}
