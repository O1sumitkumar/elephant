package git

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeRemoteEquivalentForms(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "scp", raw: "git@github.com:octacity-org/solo.git", want: "github.com/octacity-org/solo"},
		{name: "scp without user", raw: "github.com:octacity-org/solo.git", want: "github.com/octacity-org/solo"},
		{name: "https", raw: "https://github.com/octacity-org/solo.git", want: "github.com/octacity-org/solo"},
		{name: "ssh url", raw: "ssh://git@github.com/octacity-org/solo.git", want: "github.com/octacity-org/solo"},
		{name: "trailing slash", raw: "https://github.com/octacity-org/solo.git/", want: "github.com/octacity-org/solo"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeRemote(tt.raw)
			if err != nil {
				t.Fatalf("NormalizeRemote(%q): %v", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("NormalizeRemote(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestInspectSanitizesOriginRemote(t *testing.T) {
	repo := newRepository(t)
	gitOutput(t, repo, "remote", "set-url", "origin", "https://alice:secret@GitHub.com/Org/Repo.git?token=private#fragment")

	metadata, err := Inspect(context.Background(), repo)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if metadata.Remote != "https://GitHub.com/Org/Repo.git" {
		t.Errorf("Remote = %q, want sanitized URL without userinfo, query, or fragment", metadata.Remote)
	}
	if strings.Contains(metadata.Remote, "alice") || strings.Contains(metadata.Remote, "secret") || strings.Contains(metadata.Remote, "private") {
		t.Fatalf("sanitized remote contains credentials: %q", metadata.Remote)
	}
	if metadata.Identity != "github.com/Org/Repo" {
		t.Errorf("Identity = %q, want normalized origin identity", metadata.Identity)
	}

	gitOutput(t, repo, "remote", "set-url", "origin", "ssh://git:secret@GitHub.com/Org/Repo.git?token=private#fragment")
	metadata, err = Inspect(context.Background(), repo)
	if err != nil {
		t.Fatalf("Inspect SSH remote: %v", err)
	}
	if metadata.Remote != "ssh://git@GitHub.com/Org/Repo.git" {
		t.Errorf("SSH Remote = %q, want username retained without password, query, or fragment", metadata.Remote)
	}
}

func TestInspectLocalOriginRemoteForms(t *testing.T) {
	repo := newRepository(t)
	repoRoot := normalizedGitPath(gitOutput(t, repo, "rev-parse", "--show-toplevel"))
	remotePath := filepath.Join(t.TempDir(), "remote.git")
	filePath := filepath.ToSlash(remotePath)
	if isWindowsDrivePath(filePath) {
		filePath = "/" + filePath
	}
	fileRemote := (&url.URL{Scheme: "file", Path: filePath}).String()
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "absolute", raw: remotePath, want: remotePath},
		{name: "relative", raw: "../remote.git", want: filepath.Join(filepath.Dir(repoRoot), "remote.git")},
		{name: "file url", raw: fileRemote, want: remotePath},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gitOutput(t, repo, "remote", "set-url", "origin", tt.raw)
			metadata, err := Inspect(context.Background(), repo)
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			if metadata.Identity != "local:"+tt.want {
				t.Errorf("Identity = %q, want %q", metadata.Identity, "local:"+tt.want)
			}
		})
	}
}

func TestNormalizeLocalRemoteDrivePathIsNotScp(t *testing.T) {
	for _, raw := range []string{`C:/work/repo.git`, `C:\work\repo.git`} {
		t.Run(raw, func(t *testing.T) {
			got, ok, err := normalizeLocalRemote(raw, `C:/workspace`)
			if err != nil {
				t.Fatalf("normalizeLocalRemote(%q): %v", raw, err)
			}
			if !ok {
				t.Fatalf("normalizeLocalRemote(%q) classified a drive path as an SSH scp remote", raw)
			}
			want := filepath.Clean(filepath.FromSlash(raw))
			if got != want {
				t.Errorf("normalizeLocalRemote(%q) = %q, want %q", raw, got, want)
			}
		})
	}
}

func TestNormalizeLocalRemoteWindowsFileURLs(t *testing.T) {
	for _, raw := range []string{"file:///C:/work/repo.git", "file://C:/work/repo.git"} {
		t.Run(raw, func(t *testing.T) {
			got, ok, err := normalizeLocalRemote(raw, `C:/workspace`)
			if err != nil {
				t.Fatalf("normalizeLocalRemote(%q): %v", raw, err)
			}
			if !ok {
				t.Fatalf("normalizeLocalRemote(%q) was not classified as local", raw)
			}
			want := filepath.Clean(filepath.FromSlash("C:/work/repo.git"))
			if got != want {
				t.Errorf("normalizeLocalRemote(%q) = %q, want %q", raw, got, want)
			}
		})
	}
}

func TestRepositoryNameHandlesWindowsLocalIdentity(t *testing.T) {
	if got := repositoryName(`local:C:\Users\example\repo`, `/unused`); got != "repo" {
		t.Errorf("repositoryName() = %q, want repo", got)
	}
	if got := repositoryName(`local:\\server\share\repo`, `/unused`); got != "repo" {
		t.Errorf("repositoryName() UNC = %q, want repo", got)
	}
}

func TestInspectIgnoresGitRepositoryLocationEnvironment(t *testing.T) {
	target := newRepositoryWithoutRemote(t)
	redirect := newRepositoryWithoutRemote(t)
	targetRoot := normalizedGitPath(gitOutput(t, target, "rev-parse", "--show-toplevel"))
	targetHead := gitOutput(t, target, "rev-parse", "HEAD")
	t.Setenv("GIT_DIR", filepath.Join(redirect, ".git"))
	t.Setenv("GIT_WORK_TREE", redirect)
	t.Setenv("GIT_COMMON_DIR", filepath.Join(redirect, ".git"))

	metadata, err := Inspect(context.Background(), target)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if metadata.Root != targetRoot {
		t.Errorf("Root = %q, environment redirected repository commands", metadata.Root)
	}
	if metadata.Head != targetHead {
		t.Errorf("Head = %q, environment redirected repository commands", metadata.Head)
	}
}

func TestNormalizeRemotePreservesPathCaseAndDropsCredentials(t *testing.T) {
	got, err := NormalizeRemote("https://alice:secret@GitHub.com/Org/Repo.GIT/")
	if err != nil {
		t.Fatalf("NormalizeRemote: %v", err)
	}
	if got != "github.com/Org/Repo.GIT" {
		t.Fatalf("NormalizeRemote() = %q, want path case preserved and only a lowercase .git suffix removed", got)
	}
	if strings.Contains(got, "alice") || strings.Contains(got, "secret") {
		t.Fatalf("normalized identity contains credentials: %q", got)
	}
}

func TestInspectNestedDirtyRepository(t *testing.T) {
	repo := newRepository(t)
	commit := gitOutput(t, repo, "rev-parse", "HEAD")
	wantRoot := normalizedGitPath(gitOutput(t, repo, "rev-parse", "--show-toplevel"))
	if err := os.MkdirAll(filepath.Join(repo, "nested", "deeper"), 0o755); err != nil {
		t.Fatalf("mkdir nested cwd: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "nested", "untracked.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("write untracked file: %v", err)
	}

	metadata, err := Inspect(context.Background(), filepath.Join(repo, "nested", "deeper"))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if metadata.Root != wantRoot {
		t.Errorf("Root = %q, want %q", metadata.Root, wantRoot)
	}
	if metadata.Identity != "github.com/Org/Repo" {
		t.Errorf("Identity = %q, want github.com/Org/Repo", metadata.Identity)
	}
	if metadata.Name != "Repo" {
		t.Errorf("Name = %q, want Repo", metadata.Name)
	}
	if metadata.Remote != "git@github.com:Org/Repo.git" {
		t.Errorf("Remote = %q, want configured origin URL", metadata.Remote)
	}
	if metadata.Head != commit {
		t.Errorf("Head = %q, want %q", metadata.Head, commit)
	}
	if metadata.Branch == "" {
		t.Error("Branch is empty for a repository on its initial branch")
	}
	if !metadata.Dirty {
		t.Error("Dirty = false, want true for an untracked file")
	}
	if metadata.Prefix != "nested/deeper/" {
		t.Errorf("Prefix = %q, want nested/deeper/", metadata.Prefix)
	}
	if atRoot, err := Inspect(context.Background(), repo); err != nil || atRoot.Prefix != "" || atRoot.Root != wantRoot {
		t.Errorf("Inspect at root = Prefix %q Root %q, %v; want empty prefix", atRoot.Prefix, atRoot.Root, err)
	}

	encoded, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	for _, field := range []string{`"root"`, `"identity"`, `"name"`, `"remote"`, `"head"`, `"branch"`, `"dirty"`} {
		if !strings.Contains(string(encoded), field) {
			t.Errorf("JSON %s missing from %s", field, encoded)
		}
	}
}

func TestInspectUnbornRepositoryAllowsEmptyHead(t *testing.T) {
	repo := t.TempDir()
	gitOutput(t, repo, "init")
	wantRoot := normalizedGitPath(gitOutput(t, repo, "rev-parse", "--show-toplevel"))
	if err := os.Mkdir(filepath.Join(repo, "nested"), 0o755); err != nil {
		t.Fatalf("mkdir nested cwd: %v", err)
	}

	metadata, err := Inspect(context.Background(), filepath.Join(repo, "nested"))
	if err != nil {
		t.Fatalf("Inspect unborn repository: %v", err)
	}
	if metadata.Head != "" {
		t.Errorf("Head = %q, want empty for unborn repository", metadata.Head)
	}
	if metadata.Remote != "" {
		t.Errorf("Remote = %q, want empty without origin", metadata.Remote)
	}
	wantIdentity := "local:" + wantRoot
	if metadata.Identity != wantIdentity {
		t.Errorf("Identity = %q, want %q", metadata.Identity, wantIdentity)
	}
}

func TestInspectDetachedHeadHasEmptyBranch(t *testing.T) {
	repo := newRepository(t)
	gitOutput(t, repo, "checkout", "--detach", "HEAD")

	metadata, err := Inspect(context.Background(), repo)
	if err != nil {
		t.Fatalf("Inspect detached repository: %v", err)
	}
	if metadata.Branch != "" {
		t.Errorf("Branch = %q, want empty for detached HEAD", metadata.Branch)
	}
}

func TestInspectLocalWorktreesShareIdentity(t *testing.T) {
	mainRepo := newRepositoryWithoutRemote(t)
	worktree := filepath.Join(t.TempDir(), "linked-worktree")
	gitOutput(t, mainRepo, "worktree", "add", "-b", "feature", worktree)
	wantMainRoot := normalizedGitPath(gitOutput(t, mainRepo, "rev-parse", "--show-toplevel"))
	wantWorktreeRoot := normalizedGitPath(gitOutput(t, worktree, "rev-parse", "--show-toplevel"))

	mainMetadata, err := Inspect(context.Background(), mainRepo)
	if err != nil {
		t.Fatalf("Inspect main repository: %v", err)
	}
	worktreeMetadata, err := Inspect(context.Background(), filepath.Join(worktree, "."))
	if err != nil {
		t.Fatalf("Inspect linked worktree: %v", err)
	}

	wantIdentity := "local:" + wantMainRoot
	if mainMetadata.Identity != wantIdentity {
		t.Errorf("main Identity = %q, want %q", mainMetadata.Identity, wantIdentity)
	}
	if worktreeMetadata.Identity != wantIdentity {
		t.Errorf("worktree Identity = %q, want %q", worktreeMetadata.Identity, wantIdentity)
	}
	if worktreeMetadata.Root != wantWorktreeRoot {
		t.Errorf("worktree Root = %q, want %q", worktreeMetadata.Root, wantWorktreeRoot)
	}
}

func TestInspectSeparateGitDirIdentityUsesCommonDir(t *testing.T) {
	repo := t.TempDir()
	commonDir := filepath.Join(t.TempDir(), "git-metadata")
	gitOutput(t, repo, "init", "--separate-git-dir", commonDir)
	wantCommonDir := normalizedGitPath(gitOutput(t, repo, "rev-parse", "--path-format=absolute", "--git-common-dir"))

	metadata, err := Inspect(context.Background(), repo)
	if err != nil {
		t.Fatalf("Inspect separate-git-dir repository: %v", err)
	}
	if metadata.Identity != "local:"+wantCommonDir {
		t.Errorf("Identity = %q, want local identity based on common Git directory %q", metadata.Identity, wantCommonDir)
	}
}

func TestInspectNonRepository(t *testing.T) {
	_, err := Inspect(context.Background(), t.TempDir())
	if !errors.Is(err, ErrNotGitRepository) {
		t.Fatalf("Inspect non-repository error = %v, want ErrNotGitRepository", err)
	}
}

func newRepository(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	gitOutput(t, repo, "init", "-b", "main")
	gitOutput(t, repo, "config", "user.email", "test@example.com")
	gitOutput(t, repo, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	gitOutput(t, repo, "add", "README.md")
	gitOutput(t, repo, "commit", "-m", "initial")
	gitOutput(t, repo, "remote", "add", "origin", "git@github.com:Org/Repo.git")
	return repo
}

func newRepositoryWithoutRemote(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	gitOutput(t, repo, "init", "-b", "main")
	gitOutput(t, repo, "config", "user.email", "test@example.com")
	gitOutput(t, repo, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	gitOutput(t, repo, "add", "README.md")
	gitOutput(t, repo, "commit", "-m", "initial")
	return repo
}

func gitOutput(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = cwd
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func normalizedGitPath(path string) string {
	return filepath.Clean(filepath.FromSlash(path))
}

func TestInspectChangedFilesAreRootRelativeAndIncludeRenameSources(t *testing.T) {
	repo := newRepository(t)
	if err := os.MkdirAll(filepath.Join(repo, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitOutput(t, repo, "mv", "README.md", "docs/read me.md")
	if err := os.MkdirAll(filepath.Join(repo, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "nested", "new file.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	metadata, err := Inspect(context.Background(), filepath.Join(repo, "nested"))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	want := []string{"README.md", "docs/read me.md", "nested/new file.txt"}
	if strings.Join(metadata.ChangedFiles, "|") != strings.Join(want, "|") {
		t.Fatalf("ChangedFiles = %q, want %q", metadata.ChangedFiles, want)
	}
	if !metadata.Dirty {
		t.Fatal("Dirty = false with changed files")
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "new file.txt") {
		t.Fatalf("changed files leaked into status JSON: %s", encoded)
	}
	clean, err := Inspect(context.Background(), newRepository(t))
	if err != nil {
		t.Fatal(err)
	}
	if clean.Dirty || len(clean.ChangedFiles) != 0 {
		t.Fatalf("clean repository reported changes: %+v", clean)
	}
}
