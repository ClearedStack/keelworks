package plainforge

import (
	"context"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUpdateAppYAML(t *testing.T) {
	oldDigest := "sha256:" + strings.Repeat("4e", 32)
	newDigest := "sha256:" + strings.Repeat("9b", 32)
	src := "# Made-up app manifest\nname: muster\nnamespace: clearedstack\nimage: muster\ndigest: " + oldDigest + " # pinned\nreplicas: 1\nport: 8080\nmemory: 48Mi\n"

	got, changed, err := UpdateAppYAML([]byte(src), newDigest)
	if err != nil || !changed {
		t.Fatalf("UpdateAppYAML: changed=%v err=%v", changed, err)
	}
	want := strings.Replace(src, oldDigest, newDigest, 1)
	if string(got) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}

	// Updating to the same digest is a no-op.
	same, changed, err := UpdateAppYAML(got, newDigest)
	if err != nil || changed || string(same) != want {
		t.Fatalf("idempotent UpdateAppYAML: changed=%v err=%v", changed, err)
	}

	// Malformed or ambiguous app.yaml files must be refused rather than guessed.
	badFiles := []string{
		"",
		"name: muster\nimage: muster\n",
		"digest: sha256:not-hex\n",
		"digest: " + oldDigest + "\ndigest: " + oldDigest + "\n",
		"digest: " + oldDigest + "\r\n",
	}
	for i, bad := range badFiles {
		if _, _, err := UpdateAppYAML([]byte(bad), newDigest); err == nil {
			t.Errorf("case %d: broken app.yaml was accepted", i)
		}
	}
}

func TestSmartHTTPPinnerAgainstRealGit(t *testing.T) {
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
		return string(out)
	}

	runGit(root, "init", "--bare", "--initial-branch=main", bareDir)
	runGit(bareDir, "config", "http.receivepack", "true")
	runGit(root, "clone", bareDir, workDir)
	runGit(workDir, "checkout", "-b", "main")

	initialDigest := "sha256:" + strings.Repeat("11", 32)
	initialManifest := "name: muster\nnamespace: clearedstack\nimage: muster\ndigest: " + initialDigest + "\nreplicas: 1\nport: 8080\nmemory: 48Mi\n"
	if err := os.WriteFile(filepath.Join(workDir, "app.yaml"), []byte(initialManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "README.md"), []byte("# muster\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(workDir, "add", "app.yaml", "README.md")
	runGit(workDir, "commit", "-m", "Initial manifest")
	runGit(workDir, "push", "origin", "main")

	// Repack with deltas so unpackObjects exercises delta resolution as well.
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

	pinner, err := NewSmartHTTPPinner(srv.URL, srv.Client(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	pinner.now = func() time.Time { return time.Date(2026, 11, 14, 1, 0, 0, 0, time.UTC) }

	nextDigest := "sha256:" + strings.Repeat("22", 32)
	if st := pinner.Pin(context.Background(), "muster", "muster", nextDigest); st != Pinned {
		t.Fatalf("Pin = %v, want Pinned", st)
	}

	// Verify repository integrity with git fsck, the commit message, and app.yaml.
	runGit(bareDir, "fsck", "--full", "--strict")
	msg := strings.TrimSpace(runGit(bareDir, "log", "-1", "--format=%B", "refs/heads/main"))
	wantMsg := strings.TrimSpace(CommitMessage("muster", nextDigest))
	if msg != wantMsg || !strings.Contains(msg, SkipMarker) {
		t.Fatalf("commit message = %q, want %q", msg, wantMsg)
	}
	blob := runGit(bareDir, "show", "refs/heads/main:app.yaml")
	if !strings.Contains(blob, "digest: "+nextDigest+"\n") {
		t.Fatalf("app.yaml after pin:\n%s", blob)
	}
	readme := runGit(bareDir, "show", "refs/heads/main:README.md")
	if readme != "# muster\n" {
		t.Fatalf("README.md was altered: %q", readme)
	}

	// Pinning the same digest a second time creates no new commit.
	headBefore := strings.TrimSpace(runGit(bareDir, "rev-parse", "refs/heads/main"))
	if st := pinner.Pin(context.Background(), "muster", "muster", nextDigest); st != Pinned {
		t.Fatalf("second Pin = %v, want Pinned", st)
	}
	headAfter := strings.TrimSpace(runGit(bareDir, "rev-parse", "refs/heads/main"))
	if headBefore != headAfter {
		t.Fatalf("second Pin created a redundant commit: %s -> %s", headBefore, headAfter)
	}

	// Missing repository returns BadManifest.
	if st := pinner.Pin(context.Background(), "no-such-repo", "muster", nextDigest); st != BadManifest {
		t.Fatalf("missing repo: got %v, want BadManifest", st)
	}
}

func TestSmartHTTPPinnerTimeoutAndUnavailable(t *testing.T) {
	digest := "sha256:" + strings.Repeat("33", 32)
	stall := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-stall:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(stall)

	p, _ := NewSmartHTTPPinner(srv.URL, srv.Client(), 30*time.Millisecond)
	if st := p.Pin(context.Background(), "muster", "muster", digest); st != TimedOut {
		t.Fatalf("stalled server: got %v, want TimedOut", st)
	}

	srv500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv500.Close()
	p500, _ := NewSmartHTTPPinner(srv500.URL, srv500.Client(), time.Second)
	if st := p500.Pin(context.Background(), "muster", "muster", digest); st != Unavailable {
		t.Fatalf("500 server: got %v, want Unavailable", st)
	}
}
