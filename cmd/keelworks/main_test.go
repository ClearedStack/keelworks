package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ClearedStack/keelworks/internal/caller"
	"github.com/ClearedStack/keelworks/internal/controller"
	"github.com/ClearedStack/keelworks/internal/lazarette"
	"github.com/ClearedStack/keelworks/internal/plainforge"
)

func directHTTPClient() *http.Client {
	return &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{Proxy: nil},
	}
}

func TestRunRealClusterEndToEnd(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	tmp := t.TempDir()

	// 1. Set up a real bare Git repository served over smart HTTP as Plainforge.
	bareDir := filepath.Join(tmp, "muster.git")
	workDir := filepath.Join(tmp, "work")
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
	runGit(tmp, "init", "--bare", "--initial-branch=main", bareDir)
	runGit(bareDir, "config", "http.receivepack", "true")
	runGit(tmp, "clone", bareDir, workDir)
	runGit(workDir, "checkout", "-b", "main")
	initialDigest := "sha256:" + strings.Repeat("10", 32)
	if err := os.WriteFile(filepath.Join(workDir, "app.yaml"), []byte("name: muster\nnamespace: clearedstack\nimage: muster\ndigest: "+initialDigest+"\nreplicas: 1\nport: 8080\nmemory: 48Mi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(workDir, "add", "app.yaml")
	runGit(workDir, "commit", "-m", "Seed app.yaml")
	runGit(workDir, "push", "origin", "main")
	commitHash := strings.TrimSpace(runGit(bareDir, "rev-parse", "refs/heads/main"))

	forgeSrv := httptest.NewServer(&cgi.Handler{
		Path: gitBin,
		Args: []string{"http-backend"},
		Env:  []string{"GIT_PROJECT_ROOT=" + tmp, "GIT_HTTP_EXPORT_ALL=1"},
	})
	defer forgeSrv.Close()

	// 2. Generate an ephemeral ECDSA P-256 cosign keypair and serve Lazarette OCI + referrers.
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pubPath := filepath.Join(tmp, "cosign.pub")
	if err := os.WriteFile(pubPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), 0o600); err != nil {
		t.Fatal(err)
	}

	builtDigest := "sha256:" + strings.Repeat("4c", 32)
	payloadBytes, _ := json.Marshal(map[string]any{
		"critical": map[string]any{
			"identity": map[string]string{"docker-reference": "lazarette.clearedstack.svc.cluster.local:5000/muster"},
			"image":    map[string]string{"docker-manifest-digest": builtDigest},
			"type":     "cosign container image signature",
		},
	})
	payloadSum := sha256.Sum256(payloadBytes)
	payloadDigest := "sha256:" + hex.EncodeToString(payloadSum[:])
	sigASN1, err := ecdsa.SignASN1(rand.Reader, priv, payloadSum[:])
	if err != nil {
		t.Fatal(err)
	}
	sigManifestBytes, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     lazarette.OCIManifestMediaType,
		"artifactType":  lazarette.SimpleSigningMediaType,
		"layers": []map[string]any{{
			"mediaType": lazarette.SimpleSigningMediaType,
			"digest":    payloadDigest,
			"size":      len(payloadBytes),
			"annotations": map[string]string{
				lazarette.SignatureAnnotation: base64.StdEncoding.EncodeToString(sigASN1),
			},
		}},
	})
	sigSum := sha256.Sum256(sigManifestBytes)
	sigManifestDigest := "sha256:" + hex.EncodeToString(sigSum[:])

	lazSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && r.URL.Path == "/v2/muster/manifests/"+builtDigest:
			w.Header().Set("Docker-Content-Digest", builtDigest)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/v2/muster/referrers/"+builtDigest:
			_, _ = w.Write([]byte(`{"schemaVersion":2,"mediaType":"` + lazarette.OCIIndexMediaType + `","manifests":[{"mediaType":"` + lazarette.OCIManifestMediaType + `","digest":"` + sigManifestDigest + `","artifactType":"` + lazarette.SimpleSigningMediaType + `"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v2/muster/manifests/"+sigManifestDigest:
			_, _ = w.Write(sigManifestBytes)
		case r.Method == http.MethodGet && r.URL.Path == "/v2/muster/blobs/"+payloadDigest:
			_, _ = w.Write(payloadBytes)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer lazSrv.Close()

	// 3. Set up a TLS Kubernetes API server handling TokenReview, Jobs, and Pods.
	kubeSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer keelworks-sa-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/apis/authentication.k8s.io/v1/tokenreviews":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"apiVersion":"authentication.k8s.io/v1","kind":"TokenReview","status":{"authenticated":true,"user":{"username":"` + caller.Plainforge + `"},"audiences":["` + caller.Audience + `"]}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/apis/batch/v1/namespaces/clearedstack/jobs":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"kind":"Job"}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/apis/batch/v1/namespaces/clearedstack/jobs/"):
			_, _ = w.Write([]byte(`{"status":{"conditions":[{"type":"Complete","status":"True"}]}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/clearedstack/pods":
			_, _ = w.Write([]byte(`{"items":[{"status":{"containerStatuses":[{"state":{"terminated":{"exitCode":0,"message":"` + builtDigest + `\n"}}}]}}]}`))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/apis/batch/v1/namespaces/clearedstack/jobs/"):
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer kubeSrv.Close()

	caFile := filepath.Join(tmp, "ca.crt")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: kubeSrv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(tmp, "token")
	if err := os.WriteFile(tokenFile, []byte("keelworks-sa-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 4. Launch keelworks in real-cluster mode and deliver a push webhook.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{
			"-addr", "127.0.0.1:0",
			"-ko-image", "lazarette.clearedstack.svc.cluster.local:5000/keelworks-ko@sha256:" + strings.Repeat("aa", 32),
			"-buildkit-image", "lazarette.clearedstack.svc.cluster.local:5000/keelworks-buildkit@sha256:" + strings.Repeat("bb", 32),
			"-plainforge-url", forgeSrv.URL,
			"-lazarette-url", lazSrv.URL,
			"-cosign-pub-file", pubPath,
			"-priority-class", "cs-build-low",
			"-job-timeout", "1m",
			"-token-review-timeout", "5s",
			"-kube-url", kubeSrv.URL,
			"-kube-ca-file", caFile,
			"-kube-token-file", tokenFile,
		}, ready)
	}()

	listenAddr := <-ready
	hc := directHTTPClient()
	webhookBody := `{"repository":"muster","ref":"refs/heads/main","before":"0000000000000000000000000000000000000000","after":"` + commitHash + `"}`
	req, err := http.NewRequest(http.MethodPost, "http://"+listenAddr+"/webhook", bytes.NewBufferString(webhookBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer plainforge-projected-token")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	ansBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /webhook: status %d, body %s", resp.StatusCode, ansBytes)
	}
	var accepted struct {
		Run string `json:"run"`
	}
	_ = json.Unmarshal(ansBytes, &accepted)

	// Poll GET /runs/<id> until finished.
	var report controller.Report
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		rResp, err := hc.Get("http://" + listenAddr + "/runs/" + accepted.Run)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rResp.Body)
		rResp.Body.Close()
		_ = json.Unmarshal(b, &report)
		if report.State != controller.StateRunning && report.State != "" {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if report.State != "passed" || report.Digest != builtDigest {
		t.Fatalf("final report = %+v", report)
	}

	// Confirm app.yaml in Plainforge was updated with [skip-ci].
	msg := strings.TrimSpace(runGit(bareDir, "log", "-1", "--format=%B", "refs/heads/main"))
	if !strings.Contains(msg, plainforge.SkipMarker) {
		t.Fatalf("commit message missing [skip-ci]: %q", msg)
	}
	updatedManifest := runGit(bareDir, "show", "refs/heads/main:app.yaml")
	if !strings.Contains(updatedManifest, "digest: "+builtDigest+"\n") {
		t.Fatalf("app.yaml not updated:\n%s", updatedManifest)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run returned error on shutdown: %v", err)
	}
}
