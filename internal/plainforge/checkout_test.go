package plainforge

import (
	"context"
	"errors"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ClearedStack/keelworks/internal/fault"
)

func TestCheckoutAgainstRealGit(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	bareDir := filepath.Join(root, "muster.git")
	workDir := filepath.Join(root, "work")

	runGit := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command(gitBin, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Tester",
			"GIT_AUTHOR_EMAIL=tester@clearedstack.internal",
			"GIT_COMMITTER_NAME=Tester",
			"GIT_COMMITTER_EMAIL=tester@clearedstack.internal",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	runGit(root, "init", "--bare", "--initial-branch=main", bareDir)
	runGit(bareDir, "config", "http.receivepack", "true")
	runGit(root, "clone", bareDir, workDir)
	runGit(workDir, "checkout", "-b", "main")

	// Commit 1: nested directory + regular file + executable script.
	if err := os.MkdirAll(filepath.Join(workDir, "cmd", "muster"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "go.mod"), []byte("module github.com/ClearedStack/muster\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "cmd", "muster", "main.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "build.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(workDir, "add", "go.mod", "cmd/muster/main.go", "build.sh")
	runGit(workDir, "commit", "-m", "First commit")
	commit1 := runGit(workDir, "rev-parse", "HEAD")

	// Annotated tag on commit1.
	runGit(workDir, "tag", "-a", "v0.1.0", "-m", "Release v0.1.0")
	tagObjOID := runGit(workDir, "rev-parse", "refs/tags/v0.1.0")

	// Commit 2 advances main past commit1 so we verify ancestor checkout too.
	if err := os.WriteFile(filepath.Join(workDir, "cmd", "muster", "main.go"), []byte("package main\nfunc main() { println(2) }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(workDir, "add", "cmd/muster/main.go")
	runGit(workDir, "commit", "-m", "Second commit")
	commit2 := runGit(workDir, "rev-parse", "HEAD")

	// Branch with a symlink to verify symlinks are refused with E_NEEDS_SUBTOOL.
	runGit(workDir, "checkout", "-b", "with-symlink")
	if err := os.Symlink("/etc/passwd", filepath.Join(workDir, "escape")); err != nil {
		t.Fatal(err)
	}
	runGit(workDir, "add", "escape")
	runGit(workDir, "commit", "-m", "Add symlink")
	symlinkCommit := runGit(workDir, "rev-parse", "HEAD")

	runGit(workDir, "push", "origin", "main", "with-symlink", "refs/tags/v0.1.0")
	runGit(bareDir, "repack", "-a", "-d")

	srv := httptest.NewServer(&cgi.Handler{
		Path: gitBin,
		Args: []string{"http-backend"},
		Env: []string{
			"GIT_PROJECT_ROOT=" + root,
			"GIT_HTTP_EXPORT_ALL=1",
		},
	})
	defer srv.Close()

	client, err := NewSmartHTTPPinner(srv.URL, srv.Client(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Checkout current main tip (commit2).
	dstTip := filepath.Join(t.TempDir(), "src")
	if err := client.Checkout(context.Background(), "muster", "refs/heads/main", commit2, dstTip); err != nil {
		t.Fatalf("Checkout main tip: %v", err)
	}
	gotMain, err := os.ReadFile(filepath.Join(dstTip, "cmd", "muster", "main.go"))
	if err != nil || !strings.Contains(string(gotMain), "println(2)") {
		t.Fatalf("main.go on commit2 = %q, err=%v", gotMain, err)
	}
	st, err := os.Stat(filepath.Join(dstTip, "build.sh"))
	if err != nil || st.Mode().Perm()&0o111 == 0 {
		t.Fatalf("build.sh executable mode not preserved: %v err=%v", st, err)
	}

	// 2. Checkout earlier commit1 via refs/heads/main (ancestor after fast-forward).
	dstAncestor := filepath.Join(t.TempDir(), "ancestor")
	if err := client.Checkout(context.Background(), "muster", "refs/heads/main", commit1, dstAncestor); err != nil {
		t.Fatalf("Checkout ancestor commit1: %v", err)
	}
	gotOld, err := os.ReadFile(filepath.Join(dstAncestor, "cmd", "muster", "main.go"))
	if err != nil || strings.Contains(string(gotOld), "println(2)") {
		t.Fatalf("main.go on commit1 = %q, err=%v", gotOld, err)
	}

	// 3. Checkout annotated tag both by tag object OID and by peeled commit OID.
	for _, oid := range []string{tagObjOID, commit1} {
		dstTag := filepath.Join(t.TempDir(), "tag-"+oid[:8])
		if err := client.Checkout(context.Background(), "muster", "refs/tags/v0.1.0", oid, dstTag); err != nil {
			t.Fatalf("Checkout annotated tag (%s): %v", oid, err)
		}
		if _, err := os.Stat(filepath.Join(dstTag, "go.mod")); err != nil {
			t.Fatalf("missing go.mod in tag checkout: %v", err)
		}
	}

	// 4. Non-empty destination directory is refused.
	if err := client.Checkout(context.Background(), "muster", "refs/heads/main", commit2, dstTip); err == nil {
		t.Fatal("Checkout accepted non-empty destination directory")
	}

	// 5. Symlinks in repository trees are refused with E_NEEDS_SUBTOOL.
	var f *fault.Fault
	err = client.Checkout(context.Background(), "muster", "refs/heads/with-symlink", symlinkCommit, filepath.Join(t.TempDir(), "sym"))
	if !errors.As(err, &f) || f.Code != fault.NeedsSubtool {
		t.Fatalf("symlink tree: err=%v, want E_NEEDS_SUBTOOL", err)
	}

	// 6. Unreachable commit on nonexistent ref is refused with E_NEEDS_SUBTOOL.
	err = client.Checkout(context.Background(), "muster", "refs/tags/missing", strings.Repeat("a", 40), filepath.Join(t.TempDir(), "missing"))
	if !errors.As(err, &f) || f.Code != fault.NeedsSubtool {
		t.Fatalf("unreachable commit: err=%v, want E_NEEDS_SUBTOOL", err)
	}

	// 7. Stalled server reports E_TIMEOUT.
	stall := make(chan struct{})
	stallSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-stall:
		case <-r.Context().Done():
		}
	}))
	defer stallSrv.Close()
	defer close(stall)

	slowClient, _ := NewSmartHTTPPinner(stallSrv.URL, stallSrv.Client(), 30*time.Millisecond)
	err = slowClient.Checkout(context.Background(), "muster", "refs/heads/main", commit2, filepath.Join(t.TempDir(), "slow"))
	if !errors.As(err, &f) || f.Code != fault.Timeout {
		t.Fatalf("stalled Checkout: err=%v, want E_TIMEOUT", err)
	}
}
