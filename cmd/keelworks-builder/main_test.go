package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClearedStack/keelworks/internal/buildjob"
	"github.com/ClearedStack/keelworks/internal/lazarette"
)

func testKeyFiles(t *testing.T, dir string) (string, *ecdsa.PublicKey) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8DER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8DER})
	keyFile := filepath.Join(dir, "cosign.key")
	if err := os.WriteFile(keyFile, privPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	pub, err := lazarette.ParsePublicKey(pubPEM)
	if err != nil {
		t.Fatal(err)
	}
	return keyFile, pub
}

type mockLazarette struct {
	mu        sync.Mutex
	nextID    int
	uploads   map[string]string
	blobs     map[string][]byte
	manifests map[string]mockManifest
	referrers map[string][]map[string]any
}

type mockManifest struct {
	mediaType string
	raw       []byte
}

func newMockLazarette() *mockLazarette {
	return &mockLazarette{
		uploads:   make(map[string]string),
		blobs:     make(map[string][]byte),
		manifests: make(map[string]mockManifest),
		referrers: make(map[string][]map[string]any),
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (m *mockLazarette) manifestCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.manifests)
}

func (m *mockLazarette) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path, ok := strings.CutPrefix(r.URL.Path, "/v2/")
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	switch {
	case strings.Contains(path, "/blobs/uploads/"):
		repo, id, _ := strings.Cut(path, "/blobs/uploads/")
		switch {
		case r.Method == http.MethodPost && id == "":
			m.nextID++
			uid := fmt.Sprintf("%032x", m.nextID)
			m.uploads[uid] = repo
			w.Header().Set("Location", "/v2/"+repo+"/blobs/uploads/"+uid)
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodPut && id != "":
			if m.uploads[id] != repo {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			delete(m.uploads, id)
			dgst := r.URL.Query().Get("digest")
			data, err := io.ReadAll(r.Body)
			if err != nil || sha256Hex(data) != dgst {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			m.blobs[repo+"@"+dgst] = data
			w.Header().Set("Docker-Content-Digest", dgst)
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}

	case strings.Contains(path, "/blobs/"):
		repo, dgst, _ := strings.Cut(path, "/blobs/")
		data, ok := m.blobs[repo+"@"+dgst]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Docker-Content-Digest", dgst)
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(data)
		}

	case strings.Contains(path, "/referrers/"):
		repo, subj, _ := strings.Cut(path, "/referrers/")
		wantType := r.URL.Query().Get("artifactType")
		var list []map[string]any
		for _, item := range m.referrers[repo+"@"+subj] {
			if wantType != "" && item["artifactType"] != wantType {
				continue
			}
			list = append(list, item)
		}
		if list == nil {
			list = []map[string]any{}
		}
		w.Header().Set("Content-Type", lazarette.OCIIndexMediaType)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schemaVersion": 2,
			"mediaType":     lazarette.OCIIndexMediaType,
			"manifests":     list,
		})

	case strings.Contains(path, "/manifests/"):
		repo, dgst, _ := strings.Cut(path, "/manifests/")
		switch r.Method {
		case http.MethodHead, http.MethodGet:
			st, ok := m.manifests[repo+"@"+dgst]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", st.mediaType)
			w.Header().Set("Docker-Content-Digest", dgst)
			w.WriteHeader(http.StatusOK)
			if r.Method == http.MethodGet {
				_, _ = w.Write(st.raw)
			}
		case http.MethodPut:
			raw, err := io.ReadAll(r.Body)
			if err != nil || sha256Hex(raw) != dgst {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var parsed struct {
				MediaType    string `json:"mediaType"`
				ArtifactType string `json:"artifactType"`
				Subject      *struct {
					MediaType string `json:"mediaType"`
					Digest    string `json:"digest"`
					Size      int64  `json:"size"`
				} `json:"subject"`
			}
			if err := json.Unmarshal(raw, &parsed); err != nil || parsed.MediaType == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if parsed.Subject != nil {
				subj, ok := m.manifests[repo+"@"+parsed.Subject.Digest]
				if !ok || subj.mediaType != parsed.Subject.MediaType || int64(len(subj.raw)) != parsed.Subject.Size {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				m.referrers[repo+"@"+parsed.Subject.Digest] = append(m.referrers[repo+"@"+parsed.Subject.Digest], map[string]any{
					"mediaType":    parsed.MediaType,
					"digest":       dgst,
					"size":         len(raw),
					"artifactType": parsed.ArtifactType,
				})
			}
			m.manifests[repo+"@"+dgst] = mockManifest{mediaType: parsed.MediaType, raw: raw}
			w.Header().Set("Docker-Content-Digest", dgst)
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestBuilderKoEndToEndAndFailureGate(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go is not installed")
	}

	root := t.TempDir()
	bareDir := filepath.Join(root, "muster.git")
	cloneDir := filepath.Join(root, "clone")
	keyFile, pubKey := testKeyFiles(t, root)

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
	runGit(root, "clone", bareDir, cloneDir)
	runGit(cloneDir, "checkout", "-b", "main")

	if err := os.MkdirAll(filepath.Join(cloneDir, "cmd", "muster"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cloneDir, "go.mod"), []byte("module github.com/ClearedStack/muster\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	koYAML := "builds:\n  - id: helper\n    main: ./cmd/helper\n  - id: muster\n    main: ./cmd/muster\n"
	if err := os.WriteFile(filepath.Join(cloneDir, ".ko.yaml"), []byte(koYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cloneDir, "cmd", "muster", "main.go"), []byte("package main\n\nfunc Answer() int { return 42 }\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cloneDir, "cmd", "muster", "main_test.go"), []byte("package main\n\nimport \"testing\"\n\nfunc TestAnswer(t *testing.T) {\n\tif Answer() != 42 { t.Fatal(\"bad\") }\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(cloneDir, "add", ".")
	runGit(cloneDir, "commit", "-m", "Passing service")
	passCommit := runGit(cloneDir, "rev-parse", "HEAD")

	// Second commit on another ref whose tests fail, to verify broken code never reaches ko or Lazarette.
	runGit(cloneDir, "checkout", "-b", "broken")
	if err := os.WriteFile(filepath.Join(cloneDir, "cmd", "muster", "main_test.go"), []byte("package main\n\nimport \"testing\"\n\nfunc TestAnswer(t *testing.T) { t.Fatal(\"golden mismatch\") }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(cloneDir, "add", ".")
	runGit(cloneDir, "commit", "-m", "Broken tests")
	failCommit := runGit(cloneDir, "rev-parse", "HEAD")
	runGit(cloneDir, "tag", "v0.0.1-broken", failCommit)

	runGit(cloneDir, "push", "origin", "main", "refs/tags/v0.0.1-broken")

	pfSrv := httptest.NewServer(&cgi.Handler{
		Path: gitBin,
		Args: []string{"http-backend"},
		Env:  []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"},
	})
	defer pfSrv.Close()

	laz := newMockLazarette()
	var koCalls atomic.Int32
	lazSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_test_ko_push" {
			koCalls.Add(1)
			imgBody := []byte(`{"schemaVersion":2,"mediaType":"` + lazarette.OCIManifestMediaType + `","layers":[]}`)
			dgst := sha256Hex(imgBody)
			laz.mu.Lock()
			laz.manifests["muster@"+dgst] = mockManifest{mediaType: lazarette.OCIManifestMediaType, raw: imgBody}
			laz.mu.Unlock()
			_, _ = w.Write([]byte(dgst))
			return
		}
		laz.ServeHTTP(w, r)
	}))
	defer lazSrv.Close()

	lazURL, _ := url.Parse(lazSrv.URL)

	// Fake ko binary verifies exact arguments and seeds the built image manifest in mock Lazarette.
	fakeKo := filepath.Join(root, "fake-ko")
	koScript := fmt.Sprintf(`#!/bin/sh
set -eu
case "$*" in
  "publish --bare --platform=linux/arm64 --insecure-registry ./cmd/muster") ;;
  *) echo "unexpected ko args: $*" >&2; exit 2 ;;
esac
dgst=$(python3 -c 'import urllib.request; req = urllib.request.urlopen(%q); print(req.read().decode())')
echo "${KO_DOCKER_REPO}@${dgst}"
`, lazSrv.URL+"/_test_ko_push")
	if err := os.WriteFile(fakeKo, []byte(koScript), 0o755); err != nil {
		t.Fatal(err)
	}

	// 1. Failing golden test aborts before calling ko or pushing anything to Lazarette.
	failTerm := filepath.Join(root, "fail-termination-log")
	failEnv := map[string]string{
		buildjob.EnvRepository:    "muster",
		buildjob.EnvApp:           "muster",
		buildjob.EnvBuilder:       "ko",
		buildjob.EnvRef:           "refs/tags/v0.0.1-broken",
		buildjob.EnvCommit:        failCommit,
		buildjob.EnvPlainforgeURL: pfSrv.URL,
		buildjob.EnvImage:         lazURL.Host + "/muster",
		buildjob.EnvCosignKey:     keyFile,
	}
	sharedWork := filepath.Join(root, "work-ko")
	err = run(context.Background(), []string{
		"-work-dir", sharedWork,
		"-tmp-dir", filepath.Join(root, "tmp-fail"),
		"-termination-log", failTerm,
		"-go-bin", goBin,
		"-ko-bin", fakeKo,
	}, func(k string) string { return failEnv[k] })
	if err == nil || !strings.Contains(err.Error(), "tests failed") {
		t.Fatalf("broken test run: err=%v, want tests failed", err)
	}
	if koCalls.Load() != 0 || laz.manifestCount() != 0 {
		t.Fatalf("broken build called ko (%d) or touched Lazarette (%d)", koCalls.Load(), laz.manifestCount())
	}
	if _, statErr := os.Stat(failTerm); !os.IsNotExist(statErr) {
		t.Fatal("broken build wrote termination log")
	}

	// 2. Passing commit cleans work-ko/src, reuses gocache, runs go vet + go test,
	// publishes via ko, signs in Lazarette, writes /dev/termination-log, and
	// passes Keelworks' OCIVerifier.
	passTerm := filepath.Join(root, "pass-termination-log")
	passEnv := map[string]string{
		buildjob.EnvRepository:    "muster",
		buildjob.EnvApp:           "muster",
		buildjob.EnvBuilder:       "ko",
		buildjob.EnvRef:           "refs/heads/main",
		buildjob.EnvCommit:        passCommit,
		buildjob.EnvPlainforgeURL: pfSrv.URL,
		buildjob.EnvImage:         lazURL.Host + "/muster",
		buildjob.EnvCosignKey:     keyFile,
	}
	err = run(context.Background(), []string{
		"-work-dir", sharedWork,
		"-tmp-dir", filepath.Join(root, "tmp-pass"),
		"-termination-log", passTerm,
		"-go-bin", goBin,
		"-ko-bin", fakeKo,
	}, func(k string) string { return passEnv[k] })
	if err != nil {
		t.Fatalf("passing ko run: %v", err)
	}

	termBytes, err := os.ReadFile(passTerm)
	if err != nil {
		t.Fatalf("read termination log: %v", err)
	}
	publishedDigest := strings.TrimSpace(string(termBytes))

	noProxyHTTP := &http.Client{Transport: &http.Transport{Proxy: nil}}
	verifier, err := lazarette.NewOCIVerifier(lazSrv.URL, noProxyHTTP, pubKey, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got := verifier.Verify(context.Background(), "muster", publishedDigest); got != lazarette.Verified {
		t.Fatalf("OCIVerifier after builder run = %v, want Verified", got)
	}
}

func TestBuilderBuildKitEndToEnd(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}

	root := t.TempDir()
	bareDir := filepath.Join(root, "clearedstack.com.git")
	cloneDir := filepath.Join(root, "clone")
	keyFile, pubKey := testKeyFiles(t, root)

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
	runGit(root, "clone", bareDir, cloneDir)
	runGit(cloneDir, "checkout", "-b", "main")

	if err := os.WriteFile(filepath.Join(cloneDir, "package.json"), []byte(`{"name":"clearedstack.com","scripts":{"test":"echo ok"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cloneDir, "Containerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(cloneDir, "add", ".")
	runGit(cloneDir, "commit", "-m", "Web console commit")
	commit := runGit(cloneDir, "rev-parse", "HEAD")
	runGit(cloneDir, "push", "origin", "main")

	pfSrv := httptest.NewServer(&cgi.Handler{
		Path: gitBin,
		Args: []string{"http-backend"},
		Env:  []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"},
	})
	defer pfSrv.Close()

	laz := newMockLazarette()
	lazSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_test_bk_push" {
			idxBody := []byte(`{"schemaVersion":2,"mediaType":"` + lazarette.OCIIndexMediaType + `","manifests":[]}`)
			dgst := sha256Hex(idxBody)
			laz.mu.Lock()
			laz.manifests["conn@"+dgst] = mockManifest{mediaType: lazarette.OCIIndexMediaType, raw: idxBody}
			laz.mu.Unlock()
			_, _ = w.Write([]byte(dgst))
			return
		}
		laz.ServeHTTP(w, r)
	}))
	defer lazSrv.Close()
	lazURL, _ := url.Parse(lazSrv.URL)

	fakeNPM := filepath.Join(root, "fake-npm")
	if err := os.WriteFile(fakeNPM, []byte("#!/bin/sh\n[ \"$1\" = \"test\" ] && [ \"$2\" = \"--offline\" ]\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	fakeBuildctl := filepath.Join(root, "fake-buildctl")
	bkScript := fmt.Sprintf(`#!/bin/sh
set -eu
meta=""
prev=""
has_file=0
for arg in "$@"; do
  if [ "$prev" = "--metadata-file" ]; then meta="$arg"; fi
  if [ "$arg" = "filename=Containerfile" ]; then has_file=1; fi
  prev="$arg"
done
[ "$has_file" = "1" ] && [ -n "$meta" ]
dgst=$(python3 -c 'import urllib.request; req = urllib.request.urlopen(%q); print(req.read().decode())')
printf '{"containerimage.digest":"%%s"}\n' "$dgst" > "$meta"
`, lazSrv.URL+"/_test_bk_push")
	if err := os.WriteFile(fakeBuildctl, []byte(bkScript), 0o755); err != nil {
		t.Fatal(err)
	}

	termFile := filepath.Join(root, "bk-termination-log")
	bkEnv := map[string]string{
		buildjob.EnvRepository:    "clearedstack.com",
		buildjob.EnvApp:           "conn",
		buildjob.EnvBuilder:       "buildkit",
		buildjob.EnvRef:           "refs/heads/main",
		buildjob.EnvCommit:        commit,
		buildjob.EnvPlainforgeURL: pfSrv.URL,
		buildjob.EnvImage:         lazURL.Host + "/conn",
		buildjob.EnvCosignKey:     keyFile,
	}
	if err := run(context.Background(), []string{
		"-work-dir", filepath.Join(root, "work-bk"),
		"-tmp-dir", filepath.Join(root, "tmp-bk"),
		"-termination-log", termFile,
		"-npm-bin", fakeNPM,
		"-buildctl-bin", fakeBuildctl,
	}, func(k string) string { return bkEnv[k] }); err != nil {
		t.Fatalf("buildkit run: %v", err)
	}

	termBytes, err := os.ReadFile(termFile)
	if err != nil {
		t.Fatal(err)
	}
	dgst := strings.TrimSpace(string(termBytes))

	noProxyHTTP := &http.Client{Transport: &http.Transport{Proxy: nil}}
	verifier, _ := lazarette.NewOCIVerifier(lazSrv.URL, noProxyHTTP, pubKey, 2*time.Second)
	if got := verifier.Verify(context.Background(), "conn", dgst); got != lazarette.Verified {
		t.Fatalf("OCIVerifier after BuildKit run = %v, want Verified", got)
	}
}

func TestParseJobEnvAndKoMainValidation(t *testing.T) {
	dir := t.TempDir()
	keyFile, _ := testKeyFiles(t, dir)

	valid := map[string]string{
		buildjob.EnvRepository:    "muster",
		buildjob.EnvApp:           "muster",
		buildjob.EnvBuilder:       "ko",
		buildjob.EnvRef:           "refs/heads/main",
		buildjob.EnvCommit:        strings.Repeat("a", 40),
		buildjob.EnvPlainforgeURL: "http://plainforge.clearedstack.svc.cluster.local:8080",
		buildjob.EnvImage:         "lazarette.clearedstack.svc.cluster.local:5000/muster",
		buildjob.EnvCosignKey:     keyFile,
	}
	if _, err := parseJobEnv(func(k string) string { return valid[k] }); err != nil {
		t.Fatalf("parseJobEnv valid: %v", err)
	}

	mutations := []struct {
		key string
		val string
	}{
		{buildjob.EnvRepository, "logbook"},
		{buildjob.EnvApp, "assay"},
		{buildjob.EnvBuilder, "buildkit"},
		{buildjob.EnvRef, "refs/heads/dev"},
		{buildjob.EnvCommit, "not-a-sha"},
		{buildjob.EnvPlainforgeURL, ""},
		{buildjob.EnvImage, "lazarette.clearedstack.svc.cluster.local:5000/assay"},
		{buildjob.EnvCosignKey, filepath.Join(dir, "missing.key")},
	}
	for _, tc := range mutations {
		cp := make(map[string]string, len(valid))
		for k, v := range valid {
			cp[k] = v
		}
		cp[tc.key] = tc.val
		if _, err := parseJobEnv(func(k string) string { return cp[k] }); err == nil {
			t.Errorf("mutation %s=%q was accepted", tc.key, tc.val)
		}
	}

	// Unsafe main package paths in .ko.yaml must be refused.
	for _, badYAML := range []string{
		"builds:\n  - id: muster\n    main: ../escape\n",
		"builds:\n  - id: muster\n    main: -flag\n",
		"builds:\n  - id: muster\n    main: /abs/path\n",
	} {
		if _, _, err := parseKoMainFromYAML(badYAML, "muster"); err == nil {
			t.Errorf("unsafe .ko.yaml accepted: %q", badYAML)
		}
	}
}
