package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"elephant/internal/app"
	"elephant/internal/model"
	"elephant/internal/storage/zova"
	"github.com/google/uuid"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	transport "elephant/internal/transport/mcp"
)

func TestVersionAndCLI(t *testing.T) {
	previous := transport.Version
	transport.Version = "1.2.3-rc.4"
	t.Cleanup(func() { transport.Version = previous })
	var out, logs bytes.Buffer
	if err := run(context.Background(), []string{"--version"}, &out, &logs); err != nil || out.String() != "elephant 1.2.3-rc.4\n" {
		t.Fatal(out.String(), err)
	}
	cwd := t.TempDir()
	cmd := exec.Command("git", "init", "-q", cwd)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(string(b), err)
	}
	t.Setenv("ELEPHANT_DB", filepath.Join(t.TempDir(), "cli.zova"))
	out.Reset()
	if err := run(context.Background(), []string{"--cwd", cwd, "add", "fact", "--title", "Testing", "--body", "Use real repositories"}, &out, &logs); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := run(context.Background(), []string{"--cwd", cwd, "recall"}, &out, &logs); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(out.Bytes()) || !strings.Contains(out.String(), "Testing") {
		t.Fatal(out.String())
	}
}
func TestExportImportBackupCLI(t *testing.T) {
	cwd := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", cwd},
		{"-C", cwd, "remote", "add", "origin", "https://github.com/test/cli-portable.git"},
		{"-C", cwd, "-c", "user.name=T", "-c", "user.email=t@e.com", "commit", "--allow-empty", "-qm", "init"},
	} {
		if b, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatal(string(b), err)
		}
	}
	t.Setenv("ELEPHANT_DB", filepath.Join(t.TempDir(), "cli-portable.zova"))
	ctx := context.Background()
	var out, logs bytes.Buffer
	if err := run(ctx, []string{"--cwd", cwd, "add", "fact", "--title", "Portable", "--body", "Round trip"}, &out, &logs); err != nil {
		t.Fatal(err)
	}
	runExport := func() string {
		t.Helper()
		out.Reset()
		if err := run(ctx, []string{"--cwd", cwd, "export"}, &out, &logs); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	first, second := runExport(), runExport()
	if first != second || !json.Valid([]byte(first)) {
		t.Fatal("export is not deterministic")
	}
	archive := filepath.Join(t.TempDir(), "archive.json")
	if err := os.WriteFile(archive, []byte(first), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := run(ctx, []string{"--cwd", cwd, "import", "--as-remote", "cli", archive}, &out, &logs); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := run(ctx, []string{"--cwd", cwd, "import", "--as-remote", "cli", archive}, &out, &logs); err != nil {
		t.Fatalf("reimport must be idempotent: %v", err)
	}
	backup := filepath.Join(t.TempDir(), "backup.zova")
	out.Reset()
	if err := run(ctx, []string{"backup", "--output", backup}, &out, &logs); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := run(ctx, []string{"--db", backup, "--cwd", cwd, "recall"}, &out, &logs); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Portable") {
		t.Fatalf("restored database lost entries: %s", out.String())
	}
}

func TestStdioProcess(t *testing.T) {
	if os.Getenv("ELEPHANT_TEST_PROCESS") == "1" {
		if err := run(context.Background(), []string{"serve"}, os.Stdout, os.Stderr); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cwd := t.TempDir()
	cmd := exec.Command("git", "init", "-q", cwd)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(string(b), err)
	}
	database := filepath.Join(t.TempDir(), "stdio.zova")
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStdioProcess$")
	command.Env = append(os.Environ(), "ELEPHANT_TEST_PROCESS=1", "ELEPHANT_DB="+database)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	client := sdk.NewClient(&sdk.Implementation{Name: "stdio-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &sdk.CommandTransport{Command: command}, nil)
	if err != nil {
		t.Fatalf("connect: %v %s", err, stderr.String())
	}
	for _, kind := range []string{"fact", "decision", "task"} {
		r, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "add_" + kind, Arguments: map[string]any{"cwd": cwd, "title": "Remember " + kind, "body": "Context for the next agent", "related_files": []string{"main.go"}}})
		if err != nil || r.IsError {
			t.Fatal(r, err)
		}
	}
	r, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "recall_project", Arguments: map[string]any{"cwd": cwd}})
	if err != nil || r.IsError {
		t.Fatal(r, err)
	}
	if err = session.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStdioProcess$")
	restarted.Env = command.Env
	restarted.Stderr = &stderr
	session, err = client.Connect(ctx, &sdk.CommandTransport{Command: restarted}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	r, err = session.CallTool(ctx, &sdk.CallToolParams{Name: "recall_project", Arguments: map[string]any{"cwd": cwd}})
	if err != nil || r.IsError {
		t.Fatal(r, err)
	}
	data, err := json.Marshal(r.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"fact", "decision", "task"} {
		if !bytes.Contains(data, []byte("Remember "+kind)) {
			t.Fatalf("missing persisted %s: %s", kind, data)
		}
	}
}

func TestRemoteCLIRegistry(t *testing.T) {
	t.Setenv("ELEPHANT_DB", filepath.Join(t.TempDir(), "remote.zova"))
	var out, logs bytes.Buffer
	for _, args := range [][]string{{"identity"}, {"remote", "add", "fedora", "--host", "fedora"}, {"remote", "list"}, {"remote", "remove", "fedora"}} {
		out.Reset()
		if err := run(context.Background(), args, &out, &logs); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
}

func TestReceiveProcessAndRemoteRecall(t *testing.T) {
	if os.Getenv("ELEPHANT_RECEIVE_TEST") == "1" {
		if err := run(context.Background(), []string{"receive"}, os.Stdout, os.Stderr); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	database := filepath.Join(t.TempDir(), "receive.zova")
	cwd := t.TempDir()
	for _, args := range [][]string{{"init", "-q", cwd}, {"-C", cwd, "remote", "add", "origin", "https://github.com/test/cli.git"}} {
		if out, err := exec.CommandContext(ctx, "git", args...).CombinedOutput(); err != nil {
			t.Fatal(string(out), err)
		}
	}
	id := func() string { return uuid.Must(uuid.NewV7()).String() }
	now := time.Now().UTC()
	sender := id()
	m := app.Message{ProtocolVersion: 1, MessageID: id(), SenderElephantID: sender, Project: app.ProjectRef{Identity: "github.com/test/cli", Name: "cli"}, Operation: "entry.send", Entry: &model.Entry{ID: id(), Kind: model.Decision, Title: "Remote choice", Body: "Rationale", ActorID: "claude-session", Status: "active", CreatedAt: now, UpdatedAt: now}}
	data, _ := json.Marshal(m)
	for i := 0; i < 2; i++ {
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestReceiveProcessAndRemoteRecall$")
		command.Env = append(os.Environ(), "ELEPHANT_RECEIVE_TEST=1", "ELEPHANT_DB="+database)
		command.Stdin = bytes.NewReader(data)
		var logs bytes.Buffer
		command.Stderr = &logs
		out, err := command.Output()
		if err != nil {
			t.Fatal(err, logs.String())
		}
		var response app.Response
		if err = json.Unmarshal(out, &response); err != nil || response.Error != "" || response.EntryID != m.Entry.ID {
			t.Fatal(string(out), err)
		}
	}
	var out, logs bytes.Buffer
	if err := run(ctx, []string{"--db", database, "--cwd", cwd, "recall", "--remote", sender}, &out, &logs); err != nil {
		t.Fatal(err)
	}
	var received app.RecallResult
	if err := json.Unmarshal(out.Bytes(), &received); err != nil || len(received.Decisions) != 1 || received.Decisions[0].ActorID != "claude-session" {
		t.Fatal(out.String(), err)
	}
	out.Reset()
	if err := run(ctx, []string{"--db", database, "--cwd", cwd, "recall"}, &out, &logs); err != nil {
		t.Fatal(err)
	}
	var local app.RecallResult
	if err := json.Unmarshal(out.Bytes(), &local); err != nil || len(local.Decisions) != 0 {
		t.Fatal(out.String(), err)
	}
}

func TestSearchCLIFilters(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", cwd).CombinedOutput(); err != nil {
		t.Fatal(string(out), err)
	}
	database := filepath.Join(t.TempDir(), "searchcli.zova")
	svc := func() *app.Service {
		db, err := zova.Open(database)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return app.New(db)
	}()
	if _, err := svc.Add(ctx, cwd, model.Fact, app.CreateInput{Title: "Alpha gateway", Body: "cli filters"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Add(ctx, cwd, model.Decision, app.CreateInput{Title: "Beta gateway", Body: "cli filters"}); err != nil {
		t.Fatal(err)
	}
	var out, logs bytes.Buffer
	runOK := func(args []string) string {
		out.Reset()
		if err := run(ctx, args, &out, &logs); err != nil {
			t.Fatalf("%v: %v %s", args, err, logs.String())
		}
		return out.String()
	}
	q := func(extra ...string) []string {
		args := append([]string{"--db", database, "--cwd", cwd, "search", "--limit", "50"}, extra...)
		return append(args, "gateway")
	}
	var all []struct {
		Entry struct {
			ID        string `json:"id"`
			UpdatedAt string `json:"updated_at"`
		} `json:"entry"`
		Match []string `json:"match"`
	}
	if err := json.Unmarshal([]byte(runOK(q())), &all); err != nil || len(all) != 2 {
		t.Fatalf("search: %d %v %s", len(all), err, out.String())
	}
	// Time filters compose with the text query.
	betaStamp := strings.Replace(all[0].Entry.UpdatedAt, "+0000", "Z", 1)
	alphaStamp := strings.Replace(all[1].Entry.UpdatedAt, "+0000", "Z", 1)
	var newer []map[string]any
	if err := json.Unmarshal([]byte(runOK(q("--updated-after", betaStamp))), &newer); err != nil || len(newer) != 1 {
		t.Fatalf("updated-after: %d %v %s", len(newer), err, out.String())
	}
	var older []map[string]any
	if err := json.Unmarshal([]byte(runOK(q("--updated-before", alphaStamp))), &older); err != nil || len(older) != 1 {
		t.Fatalf("updated-before: %d %v %s", len(older), err, out.String())
	}
	// Kind and status filters still compose.
	var onlyFacts []map[string]any
	if err := json.Unmarshal([]byte(runOK(q("--kind", "fact"))), &onlyFacts); err != nil || len(onlyFacts) != 1 {
		t.Fatalf("kind: %d %v %s", len(onlyFacts), err, out.String())
	}
	// Invalid RFC3339 is rejected as invalid input, not a server error.
	out.Reset()
	if err := run(ctx, q("--updated-after", "yesterday"), &out, &logs); err == nil {
		t.Fatal("invalid time accepted")
	}
	// A commit bound that cannot be resolved to a Git commit fails loudly
	// instead of silently matching nothing (this repo has no commits).
	err := run(ctx, q("--commit-start", strings.Repeat("0", 40)), &out, &logs)
	if err == nil {
		t.Fatal("unresolvable commit bound accepted")
	}
	if !strings.Contains(err.Error(), "cannot be resolved") {
		t.Fatalf("unclear error: %v", err)
	}
}

// TestSearchCommitRangeOrderingCLI: reversed and divergent commit ranges are
// clear invalid-input errors at the CLI boundary; forward and equal ranges
// run unchanged.
func TestSearchCommitRangeOrderingCLI(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", cwd).CombinedOutput(); err != nil {
		t.Fatalf("%s %v", out, err)
	}
	commit := func(msg string) string {
		out, err := exec.Command("git", "-C", cwd, "-c", "user.name=T", "-c", "user.email=t@e.com",
			"commit", "--allow-empty", "-qm", msg).CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v", out, err)
		}
		out, err = exec.Command("git", "-C", cwd, "rev-parse", "HEAD").CombinedOutput()
		if err != nil {
			t.Fatal(string(out), err)
		}
		return strings.TrimSpace(string(out))
	}
	c1 := commit("c1")
	c2 := commit("c2")
	// Divergent commit: branch from c1 so neither c2 nor side contains the other.
	out, err := exec.Command("git", "-C", cwd, "rev-parse", "--abbrev-ref", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatal(string(out), err)
	}
	branch := strings.TrimSpace(string(out))
	if out, err := exec.Command("git", "-C", cwd, "checkout", "-q", "-b", "side", c1).CombinedOutput(); err != nil {
		t.Fatalf("%s %v", out, err)
	}
	side := commit("side")
	if out, err := exec.Command("git", "-C", cwd, "checkout", "-q", branch).CombinedOutput(); err != nil {
		t.Fatalf("%s %v", out, err)
	}
	database := filepath.Join(t.TempDir(), "rangecli.zova")
	q := func(flags ...string) []string {
		return append([]string{"--db", database, "--cwd", cwd, "search", "--limit", "50"}, flags...)
	}
	var buf, logs bytes.Buffer
	// Forward and equal ranges run.
	if err := run(ctx, q("--commit-start", c1, "--commit-end", c2), &buf, &logs); err != nil {
		t.Fatalf("forward range: %v", err)
	}
	if err := run(ctx, q("--commit-start", c2, "--commit-end", c2), &buf, &logs); err != nil {
		t.Fatalf("equal range: %v", err)
	}
	// Reversed endpoints are invalid input with a clear message.
	err = run(ctx, q("--commit-start", c2, "--commit-end", c1), &buf, &logs)
	if err == nil {
		t.Fatal("reversed range accepted")
	}
	if !strings.Contains(err.Error(), "reversed") {
		t.Fatalf("unclear reversed error: %v", err)
	}
	// Divergent endpoints are invalid input with a clear message.
	err = run(ctx, q("--commit-start", c2, "--commit-end", side), &buf, &logs)
	if err == nil {
		t.Fatal("divergent range accepted")
	}
	if !strings.Contains(err.Error(), "divergent") {
		t.Fatalf("unclear divergent error: %v", err)
	}
}

func TestLifecycleCLI(t *testing.T) {
	cwd := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", cwd},
		{"-C", cwd, "-c", "user.name=T", "-c", "user.email=t@e.com", "commit", "--allow-empty", "-qm", "init"},
	} {
		if b, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatal(string(b), err)
		}
	}
	t.Setenv("ELEPHANT_DB", filepath.Join(t.TempDir(), "lifecycle.zova"))
	ctx := context.Background()
	call := func(args ...string) app.Record {
		t.Helper()
		var out, logs bytes.Buffer
		if err := run(ctx, append([]string{"--cwd", cwd}, args...), &out, &logs); err != nil {
			t.Fatalf("%v: %v %s", args, err, logs.String())
		}
		var rec app.Record
		if err := json.Unmarshal(out.Bytes(), &rec); err != nil {
			t.Fatalf("%v: %v %s", args, err, out.String())
		}
		return rec
	}
	fails := func(args ...string) {
		t.Helper()
		var out, logs bytes.Buffer
		if err := run(ctx, append([]string{"--cwd", cwd}, args...), &out, &logs); err == nil {
			t.Fatalf("%v succeeded: %s", args, out.String())
		}
	}

	old := call("add", "decision", "--title", "Old", "--body", "Why")
	task := call("add", "task", "--title", "Build", "--body", "Work", "--file", "a.go", "--relation", "implements:"+old.Entry.ID)
	if len(task.Relations) != 2 {
		t.Fatalf("task relations = %+v", task.Relations)
	}
	updated := call("update", "task", task.Entry.ID, "--title", "Build it", "--status", "active", "--file", "b.go")
	if updated.Entry.Title != "Build it" || updated.Entry.Status != "active" || updated.Entry.Body != "Work" {
		t.Fatalf("update = %+v", updated.Entry)
	}
	files := 0
	for _, r := range updated.Relations {
		if r.Type == "modifies" {
			files++
			if !strings.HasSuffix(r.To, ":b.go") {
				t.Fatalf("file links not replaced: %+v", updated.Relations)
			}
		}
	}
	if files != 1 {
		t.Fatalf("file links = %+v", updated.Relations)
	}
	cleared := call("update", "task", task.Entry.ID, "--clear-files", "--target-version", "v2")
	for _, r := range cleared.Relations {
		if r.Type == "modifies" {
			t.Fatalf("--clear-files kept %+v", r)
		}
	}
	if cleared.Entry.TargetVersion == nil || *cleared.Entry.TargetVersion != "v2" {
		t.Fatalf("target version = %v", cleared.Entry.TargetVersion)
	}
	if untouched := call("update", "task", task.Entry.ID, "--body", "More"); untouched.Entry.TargetVersion == nil {
		t.Fatal("update without --target-version cleared it")
	}
	fails("update", "task", task.Entry.ID, "--clear-files", "--file", "c.go")
	fails("update", "fact", task.Entry.ID, "--title", "Wrong kind")
	if done := call("complete", task.Entry.ID); done.Entry.Status != "done" || done.Entry.EndCommit == nil {
		t.Fatalf("complete = %+v", done.Entry)
	}
	other := call("add", "task", "--title", "Maybe", "--body", "Later")
	if cancelled := call("cancel", other.Entry.ID); cancelled.Entry.Status != "cancelled" {
		t.Fatalf("cancel = %+v", cancelled.Entry)
	}
	fact := call("add", "fact", "--title", "F", "--body", "B")
	if retired := call("retire", fact.Entry.ID); retired.Entry.Status != "retired" {
		t.Fatalf("retire = %+v", retired.Entry)
	}
	fails("retire", task.Entry.ID)

	replacement := call("supersede", old.Entry.ID, "--title", "New", "--body", "Better")
	if len(replacement.Relations) != 1 || replacement.Relations[0].Type != "supersedes" {
		t.Fatalf("supersede relations = %+v", replacement.Relations)
	}
	if inspected := call("inspect", old.Entry.ID); inspected.Entry.Status != "superseded" || len(inspected.Incoming) != 2 {
		t.Fatalf("superseded decision = %+v", inspected)
	}
	third := call("add", "decision", "--title", "Third", "--body", "Again", "--supersedes", replacement.Entry.ID)
	if len(third.Relations) != 1 || third.Relations[0].To != "entry:"+replacement.Entry.ID {
		t.Fatalf("add --supersedes relations = %+v", third.Relations)
	}
	fails("add", "task", "--title", "X", "--body", "Y", "--relation", "implements")
}

// Usage mistakes must say what was wrong instead of reporting a storage
// failure, and -h on a subcommand is a successful help request.
func TestCLIUsageErrorsAreInvalidInput(t *testing.T) {
	cwd := t.TempDir()
	if b, err := exec.Command("git", "init", "-q", cwd).CombinedOutput(); err != nil {
		t.Fatal(string(b), err)
	}
	t.Setenv("ELEPHANT_DB", filepath.Join(t.TempDir(), "usage.zova"))
	for _, args := range [][]string{
		{"related", "--bogus", "x"},
		{"import", filepath.Join(t.TempDir(), "missing.json")},
		{"export", "--project"},
		{"recall", "--cwd", ".."},
		{"history", "--bogus"},
		{"checkpoint", "--bogus"},
		{"add", "fact", "--bogus"},
	} {
		var out, logs bytes.Buffer
		err := run(context.Background(), append([]string{"--cwd", cwd}, args...), &out, &logs)
		if !errors.Is(transport.PublicError(err), model.ErrInvalidInput) {
			t.Errorf("%v: got %v, want invalid input", args, transport.PublicError(err))
		}
	}
	for _, args := range [][]string{{"search", "-h"}, {"add", "fact", "-h"}, {"doctor", "-h"}} {
		var out, logs bytes.Buffer
		if err := run(context.Background(), append([]string{"--cwd", cwd}, args...), &out, &logs); !errors.Is(err, flag.ErrHelp) {
			t.Errorf("%v: got %v, want a help request", args, err)
		}
	}
}
