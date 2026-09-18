package wstools

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benl1006/Autonomous-CI-Platform/orchestrator/internal/config"
	"github.com/benl1006/Autonomous-CI-Platform/orchestrator/internal/types"
)

type stubGitClient struct {
	initErr error
	pushSHA string
	pushErr error
	inited  string
}

func (s *stubGitClient) InitRepo(ctx context.Context, path string, pr types.PullRequest) error {
	s.inited = path
	return s.initErr
}

func (s *stubGitClient) AddAllCommitPush(commitMsg, wsPath, branch string) (string, error) {
	return s.pushSHA, s.pushErr
}

func TestInsertTests_WritesFilesAndRejectsEscapes(t *testing.T) {
	dir := t.TempDir()
	if err := InsertTests(dir, []types.ChangedFile{{
		Path:     "foo_test.go",
		Contents: []byte("package foo"),
	}}); err != nil {
		t.Fatalf("InsertTests: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "foo_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "package foo" {
		t.Errorf("tests file = %q", got)
	}

	if err := InsertTests(dir, []types.ChangedFile{{
		Path:     "../escape_test.go",
		Contents: []byte("package bad"),
	}}); err == nil || !strings.Contains(err.Error(), "escapes workspace root") {
		t.Fatalf("expected path escape error, got %v", err)
	}
}

func TestReadChangedFiles(t *testing.T) {
	dir := t.TempDir()
	want := "package main"
	nestedDir := filepath.Join(dir, "pkg", "internal")
	if err := os.MkdirAll(nestedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nestedDir, "sample.go"), []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ReadFiles(dir, []string{"pkg/internal/sample.go"})
	if err != nil {
		t.Fatalf("ReadChangedFiles: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(ReadChangedFiles) = %d, want 1", len(got))
	}
	if got[0].Path != "pkg/internal/sample.go" {
		t.Fatalf("Path = %q, want %q", got[0].Path, "pkg/internal/sample.go")
	}
	if string(got[0].Contents) != want {
		t.Fatalf("Contents = %q, want %q", string(got[0].Contents), want)
	}

	if _, err := ReadFiles(dir, []string{"../escape.go"}); err == nil || !strings.Contains(err.Error(), "escapes workspace root") {
		t.Fatalf("expected escape-path error, got %v", err)
	}
}

func TestListAllFilePaths_ReturnsRegularFilesRelativeToWorkspace(t *testing.T) {
	dir := t.TempDir()
	nestedDir := filepath.Join(dir, "pkg", "internal")
	if err := os.MkdirAll(nestedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for path, contents := range map[string]string{
		"README.md":              "read me",
		"pkg/internal/sample.go": "package internal",
	} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(path)), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	paths, err := ListAllFilePaths(dir)
	if err != nil {
		t.Fatalf("ListAllFilePaths: %v", err)
	}
	if len(paths) != 2 {
		t.Fatalf("file count = %d, want 2", len(paths))
	}
	if !containsPath(paths, "README.md") || !containsPath(paths, filepath.FromSlash("pkg/internal/sample.go")) {
		t.Fatalf("paths = %#v, want workspace-relative files", paths)
	}
}

func containsPath(paths []string, want string) bool {
	for _, path := range paths {
		if path == want {
			return true
		}
	}
	return false
}

func TestGetChangedFilePaths_UsesGitHubAPI(t *testing.T) {
	prevToken, prevTransport := config.GithubToken, http.DefaultTransport
	t.Cleanup(func() {
		config.GithubToken = prevToken
		http.DefaultTransport = prevTransport
	})
	config.GithubToken = "gh-test-token"

	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			t.Fatalf("method = %q, want %q", r.Method, http.MethodGet)
		}
		if !strings.Contains(r.URL.Path, "/repos/octo/proj/pulls/42/files") {
			t.Fatalf("unexpected URL path: %s", r.URL.String())
		}
		if got := r.Header.Get("Authorization"); got == "" {
			t.Fatal("expected auth header on GitHub request")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(`[
				{"filename":"src/app.go"},
				{"filename":"tests/app_test.go"}
			]`)),
		}, nil
	})

	got, err := GetChangedFilePaths(context.Background(), "octo", "proj", 42)
	if err != nil {
		t.Fatalf("GetChangedFilePaths: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(GetChangedFilePaths) = %d, want 2", len(got))
	}
	if got[0] != "src/app.go" || got[1] != "tests/app_test.go" {
		t.Fatalf("GetChangedFilePaths = %#v, want [src/app.go tests/app_test.go]", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestInitWorkspace_Success(t *testing.T) {
	prev := config.WsDir
	t.Cleanup(func() { config.WsDir = prev })
	config.WsDir = t.TempDir()

	cli := &stubGitClient{}
	path, cleanup, err := InitWorkspace(context.Background(), types.PullRequest{HeadSHA: "abc"}, cli)
	if err != nil {
		t.Fatalf("InitWorkspace: %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })
	if path == "" || cli.inited != path {
		t.Errorf("path=%q inited=%q", path, cli.inited)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("workspace missing: %v", err)
	}
}

func TestInitWorkspace_InitFailureCleansUp(t *testing.T) {
	prev := config.WsDir
	t.Cleanup(func() { config.WsDir = prev })
	config.WsDir = t.TempDir()

	cli := &stubGitClient{initErr: errors.New("clone failed")}
	path, cleanup, err := InitWorkspace(context.Background(), types.PullRequest{HeadSHA: "abc"}, cli)
	if err == nil {
		t.Fatal("expected error")
	}
	if cleanup != nil {
		t.Fatal("cleanup should be nil after failed init")
	}
	if path != "" {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Errorf("workspace %s should have been removed", path)
		}
	}
}

func TestClearWorkspaces(t *testing.T) {
	prev := config.WsDir
	t.Cleanup(func() { config.WsDir = prev })
	dir := t.TempDir()
	config.WsDir = filepath.Join(dir, "workspaces")
	if err := os.MkdirAll(filepath.Join(config.WsDir, "leftover"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ClearWorkspaces(); err != nil {
		t.Fatalf("ClearWorkspaces: %v", err)
	}
	info, err := os.Stat(config.WsDir)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatal("expected recreated directory")
	}
	entries, err := os.ReadDir(config.WsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("expected empty dir, got %v", entries)
	}
}
