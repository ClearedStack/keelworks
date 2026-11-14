package buildjob

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClearedStack/keelworks/internal/fault"
	"github.com/ClearedStack/keelworks/internal/lazarette"
	"github.com/ClearedStack/keelworks/internal/plainforge"
	"github.com/ClearedStack/keelworks/internal/registry"
)

func sampleSpec() Spec {
	target, _ := registry.ByRepository("muster")
	return Spec{
		RunID:         "muster-674f2946f186",
		Target:        target,
		Ref:           "refs/heads/main",
		Commit:        "1111111111111111111111111111111111111111",
		BuilderImage:  "lazarette.clearedstack.svc.cluster.local:5000/keelworks-ko@sha256:" + strings.Repeat("aa", 32),
		PlainforgeURL: "http://plainforge.clearedstack.svc.cluster.local:8080",
		ImageRegistry: "lazarette.clearedstack.svc.cluster.local:5000",
		CosignSecret:  "keelworks-cosign",
		PriorityClass: "cs-build-low",
		Deadline:      10 * time.Minute,
	}
}

func TestRenderIsDeterministicAndHardened(t *testing.T) {
	s := sampleSpec()
	a := Render(s)
	b := Render(s)
	if !bytes.Equal(a, b) {
		t.Fatal("Render returned different bytes for identical Specs")
	}
	var raw map[string]any
	if err := json.Unmarshal(a, &raw); err != nil {
		t.Fatalf("rendered manifest is not valid JSON: %v", err)
	}
	text := string(a)
	for _, mustContain := range []string{
		`"readOnlyRootFilesystem": true`,
		`"runAsNonRoot": true`,
		`"allowPrivilegeEscalation": false`,
		`"kubernetes.io/arch"`,
		`"arm64"`,
		`"kubernetes.io/hostname"`,
		`"cm3588-plus"`,
		`"secretName": "keelworks-cosign"`,
		`"key": "cosign.key"`,
	} {
		if !strings.Contains(text, mustContain) {
			t.Errorf("manifest missing %s:\n%s", mustContain, text)
		}
	}
	if strings.Contains(text, "docker.sock") {
		t.Fatal("manifest references docker.sock")
	}
}

type stubVerifier struct {
	status lazarette.Status
	calls  int
}

func (s *stubVerifier) Verify(_ context.Context, _, _ string) lazarette.Status {
	s.calls++
	return s.status
}

type stubPinner struct {
	status plainforge.Status
	calls  int
}

func (s *stubPinner) Pin(_ context.Context, _, _, _ string) plainforge.Status {
	s.calls++
	return s.status
}

type stubCluster struct {
	createErr error
	waitOut   Outcome
	waitErr   error
	deleted   int
}

func (s *stubCluster) Create(ctx context.Context, _ []byte) error {
	if s.createErr != nil {
		return s.createErr
	}
	return ctx.Err()
}

func (s *stubCluster) Wait(ctx context.Context, _, _ string) (Outcome, error) {
	if s.waitErr != nil {
		return Outcome{}, s.waitErr
	}
	if ctx.Err() != nil {
		return Outcome{}, ctx.Err()
	}
	return s.waitOut, nil
}

func (s *stubCluster) Delete(_ context.Context, _, _ string) error {
	s.deleted++
	return nil
}

func TestRunPipelineFailureModes(t *testing.T) {
	digest := "sha256:" + strings.Repeat("5c", 32)
	spec := sampleSpec()

	t.Run("golden-test-failure-never-verifies-or-pins", func(t *testing.T) {
		cl := &stubCluster{waitOut: Outcome{Status: TestFailed}}
		ver := &stubVerifier{status: lazarette.Verified}
		pin := &stubPinner{status: plainforge.Pinned}
		res := Run(context.Background(), cl, ver, pin, spec)
		if res.State != StateFailed || res.Reason != ReasonTestFailed || res.Digest != "" {
			t.Fatalf("got %+v", res)
		}
		if ver.calls != 0 || pin.calls != 0 || cl.deleted != 1 {
			t.Fatalf("ver=%d pin=%d deleted=%d", ver.calls, pin.calls, cl.deleted)
		}
	})

	t.Run("unsigned-in-lazarette-never-pins", func(t *testing.T) {
		cl := &stubCluster{waitOut: Outcome{Status: Succeeded, Digest: digest}}
		ver := &stubVerifier{status: lazarette.Unsigned}
		pin := &stubPinner{status: plainforge.Pinned}
		res := Run(context.Background(), cl, ver, pin, spec)
		if res.State != StateFailed || res.Reason != ReasonSignFailed || res.Digest != "" {
			t.Fatalf("got %+v", res)
		}
		if ver.calls != 1 || pin.calls != 0 || cl.deleted != 1 {
			t.Fatalf("ver=%d pin=%d deleted=%d", ver.calls, pin.calls, cl.deleted)
		}
	})

	t.Run("lazarette-timeout-reports-E_TIMEOUT", func(t *testing.T) {
		cl := &stubCluster{waitOut: Outcome{Status: Succeeded, Digest: digest}}
		ver := &stubVerifier{status: lazarette.TimedOut}
		pin := &stubPinner{status: plainforge.Pinned}
		res := Run(context.Background(), cl, ver, pin, spec)
		if res.State != StateTimedOut || res.Code != fault.Timeout || pin.calls != 0 {
			t.Fatalf("got %+v, pin.calls=%d", res, pin.calls)
		}
	})

	t.Run("plainforge-bad-manifest-reports-E_PARSE", func(t *testing.T) {
		cl := &stubCluster{waitOut: Outcome{Status: Succeeded, Digest: digest}}
		ver := &stubVerifier{status: lazarette.Verified}
		pin := &stubPinner{status: plainforge.BadManifest}
		res := Run(context.Background(), cl, ver, pin, spec)
		if res.State != StateFailed || res.Code != fault.Parse || res.Reason != ReasonPinFailed || res.Digest != "" {
			t.Fatalf("got %+v", res)
		}
	})

	t.Run("plainforge-timeout-reports-E_TIMEOUT", func(t *testing.T) {
		cl := &stubCluster{waitOut: Outcome{Status: Succeeded, Digest: digest}}
		ver := &stubVerifier{status: lazarette.Verified}
		pin := &stubPinner{status: plainforge.TimedOut}
		res := Run(context.Background(), cl, ver, pin, spec)
		if res.State != StateTimedOut || res.Code != fault.Timeout {
			t.Fatalf("got %+v", res)
		}
	})

	t.Run("signed-and-pinned-passes", func(t *testing.T) {
		cl := &stubCluster{waitOut: Outcome{Status: Succeeded, Digest: digest}}
		ver := &stubVerifier{status: lazarette.Verified}
		pin := &stubPinner{status: plainforge.Pinned}
		res := Run(context.Background(), cl, ver, pin, spec)
		if res.State != StatePassed || res.Digest != digest || res.Code != "" || res.Reason != "" {
			t.Fatalf("got %+v", res)
		}
	})
}

func TestAPIClusterOverTLS(t *testing.T) {
	digest := "sha256:" + strings.Repeat("7f", 32)
	spec := sampleSpec()
	jobName := spec.Name()

	var polls atomic.Int32
	var deleted atomic.Int32
	mode := "succeed"

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sa-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/apis/batch/v1/namespaces/clearedstack/jobs":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"kind":"Job"}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/apis/batch/v1/namespaces/clearedstack/jobs/"+jobName:
			if r.URL.Query().Get("propagationPolicy") == "Background" {
				deleted.Add(1)
			}
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/apis/batch/v1/namespaces/clearedstack/jobs/"+jobName:
			if polls.Add(1) == 1 {
				_, _ = w.Write([]byte(`{"status":{"conditions":[]}}`))
				return
			}
			switch mode {
			case "succeed", "empty-digest":
				_, _ = w.Write([]byte(`{"status":{"conditions":[{"type":"Complete","status":"True"}]}}`))
			case "deadline":
				_, _ = w.Write([]byte(`{"status":{"conditions":[{"type":"Failed","status":"True","reason":"DeadlineExceeded"}]}}`))
			default:
				_, _ = w.Write([]byte(`{"status":{"conditions":[{"type":"Failed","status":"True","reason":"BackoffLimitExceeded"}]}}`))
			}
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/clearedstack/pods":
			switch mode {
			case "succeed":
				_, _ = w.Write([]byte(`{"items":[{"status":{"containerStatuses":[{"state":{"terminated":{"exitCode":0,"message":"` + digest + `\n"}}}]}}]}`))
			case "empty-digest":
				_, _ = w.Write([]byte(`{"items":[{"status":{"containerStatuses":[{"state":{"terminated":{"exitCode":0,"message":""}}}]}}]}`))
			case "sign-failed":
				_, _ = w.Write([]byte(`{"items":[{"status":{"containerStatuses":[{"state":{"terminated":{"exitCode":1,"message":"sign-failed\n"}}}]}}]}`))
			case "evicted":
				_, _ = w.Write([]byte(`{"items":[{"status":{"reason":"Evicted"}}]}`))
			default:
				_, _ = w.Write([]byte(`{"items":[{"status":{"containerStatuses":[{"state":{"terminated":{"exitCode":1,"message":"test-failed\n"}}}]}}]}`))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	caPath := filepath.Join(t.TempDir(), "ca.crt")
	cert := srv.Certificate()
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: x509CertificateBytes(cert)})
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	tlsClient, err := TLSClient(caPath)
	if err != nil {
		t.Fatal(err)
	}
	api, err := NewAPICluster(srv.URL, tlsClient, func() (string, error) { return "sa-token", nil })
	if err != nil {
		t.Fatal(err)
	}
	api.pollInterval = 5 * time.Millisecond

	cases := []struct {
		mode       string
		wantStatus Status
		wantDigest string
	}{
		{"succeed", Succeeded, digest},
		{"empty-digest", SignFailed, ""},
		{"sign-failed", SignFailed, ""},
		{"test-failed", TestFailed, ""},
		{"deadline", DeadlineExceeded, ""},
		{"evicted", Preempted, ""},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			mode = tc.mode
			polls.Store(0)
			if err := api.Create(context.Background(), Render(spec)); err != nil {
				t.Fatalf("Create: %v", err)
			}
			out, err := api.Wait(context.Background(), Namespace, jobName)
			if err != nil {
				t.Fatalf("Wait: %v", err)
			}
			if out.Status != tc.wantStatus || out.Digest != tc.wantDigest {
				t.Fatalf("got %+v, want status=%v digest=%q", out, tc.wantStatus, tc.wantDigest)
			}
			if err := api.Delete(context.Background(), Namespace, jobName); err != nil {
				t.Fatalf("Delete: %v", err)
			}
		})
	}
	if int(deleted.Load()) != len(cases) {
		t.Fatalf("deleted with Background propagation %d times, want %d", deleted.Load(), len(cases))
	}
}

func x509CertificateBytes(c *x509.Certificate) []byte { return c.Raw }
