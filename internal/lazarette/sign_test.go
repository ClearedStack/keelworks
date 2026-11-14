package lazarette

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClearedStack/keelworks/internal/fault"
)

func generatePrivatePEMs(t *testing.T) (*ecdsa.PrivateKey, []byte, []byte, []byte) {
	t.Helper()
	priv, pubPEM := generateKeyPair(t)
	pkcs8DER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	sec1DER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8PEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8DER})
	sec1PEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: sec1DER})
	return priv, pkcs8PEM, sec1PEM, pubPEM
}

func TestParsePrivateKey(t *testing.T) {
	_, pkcs8PEM, sec1PEM, pubPEM := generatePrivatePEMs(t)

	if _, err := ParsePrivateKey(pkcs8PEM); err != nil {
		t.Fatalf("ParsePrivateKey PKCS#8: %v", err)
	}
	if _, err := ParsePrivateKey(sec1PEM); err != nil {
		t.Fatalf("ParsePrivateKey SEC 1: %v", err)
	}

	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p384DER, _ := x509.MarshalPKCS8PrivateKey(p384)
	p384PEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: p384DER})

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsaDER, _ := x509.MarshalPKCS8PrivateKey(rsaKey)
	rsaPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: rsaDER})

	for _, bad := range [][]byte{
		nil,
		[]byte("not pem"),
		pubPEM,
		append(pkcs8PEM, pkcs8PEM...),
		p384PEM,
		rsaPEM,
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("corrupt")}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte("corrupt")}),
	} {
		if _, err := ParsePrivateKey(bad); err == nil {
			t.Error("ParsePrivateKey accepted an invalid key")
		}
	}
}

// strictOCIServer enforces Lazarette's OCI v1.1 manifest, blob, subject, and
// referrers rules in memory so OCISigner and OCIVerifier are tested end to end.
type strictOCIServer struct {
	mu        sync.Mutex
	nextID    int
	uploads   map[string]string
	blobs     map[string][]byte
	manifests map[string]storedManifest
	referrers map[string][]referrerDesc
}

type storedManifest struct {
	mediaType string
	raw       []byte
}

type referrerDesc struct {
	MediaType    string `json:"mediaType"`
	Digest       string `json:"digest"`
	Size         int64  `json:"size"`
	ArtifactType string `json:"artifactType,omitempty"`
}

func newStrictOCIServer() *strictOCIServer {
	return &strictOCIServer{
		uploads:   make(map[string]string),
		blobs:     make(map[string][]byte),
		manifests: make(map[string]storedManifest),
		referrers: make(map[string][]referrerDesc),
	}
}

func (s *strictOCIServer) seedManifest(repo, mediaType string, raw []byte) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	dgst := sha256Digest(raw)
	s.manifests[repo+"@"+dgst] = storedManifest{mediaType: mediaType, raw: append([]byte(nil), raw...)}
	return dgst
}

func (s *strictOCIServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path, ok := strings.CutPrefix(r.URL.Path, "/v2/")
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case strings.Contains(path, "/blobs/uploads/"):
		repo, id, _ := strings.Cut(path, "/blobs/uploads/")
		switch {
		case r.Method == http.MethodPost && id == "":
			s.nextID++
			uid := fmt.Sprintf("%032x", s.nextID)
			s.uploads[uid] = repo
			w.Header().Set("Location", "/v2/"+repo+"/blobs/uploads/"+uid)
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodPut && id != "":
			if s.uploads[id] != repo {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			delete(s.uploads, id)
			dgst := r.URL.Query().Get("digest")
			data, err := io.ReadAll(r.Body)
			if err != nil || sha256Digest(data) != dgst {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			s.blobs[repo+"@"+dgst] = data
			w.Header().Set("Docker-Content-Digest", dgst)
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}

	case strings.Contains(path, "/blobs/"):
		repo, dgst, _ := strings.Cut(path, "/blobs/")
		data, ok := s.blobs[repo+"@"+dgst]
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
		repo, subjDigest, _ := strings.Cut(path, "/referrers/")
		wantArtifact := r.URL.Query().Get("artifactType")
		var list []referrerDesc
		for _, entry := range s.referrers[repo+"@"+subjDigest] {
			if wantArtifact != "" && entry.ArtifactType != wantArtifact {
				continue
			}
			list = append(list, entry)
		}
		if list == nil {
			list = []referrerDesc{}
		}
		w.Header().Set("Content-Type", OCIIndexMediaType)
		if wantArtifact != "" {
			w.Header().Set("OCI-Filters-Applied", "artifactType")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schemaVersion": 2,
			"mediaType":     OCIIndexMediaType,
			"manifests":     list,
		})

	case strings.Contains(path, "/manifests/"):
		repo, dgst, _ := strings.Cut(path, "/manifests/")
		switch r.Method {
		case http.MethodHead, http.MethodGet:
			m, ok := s.manifests[repo+"@"+dgst]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", m.mediaType)
			w.Header().Set("Docker-Content-Digest", dgst)
			w.WriteHeader(http.StatusOK)
			if r.Method == http.MethodGet {
				_, _ = w.Write(m.raw)
			}
		case http.MethodPut:
			raw, err := io.ReadAll(r.Body)
			if err != nil || sha256Digest(raw) != dgst {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var doc referrerManifest
			if err := json.Unmarshal(raw, &doc); err != nil || doc.SchemaVersion != 2 || doc.MediaType != OCIManifestMediaType {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			// Enforce Lazarette's blob-before-manifest rule and exact size check.
			for _, desc := range append([]ociDescriptor{doc.Config}, doc.Layers...) {
				b, ok := s.blobs[repo+"@"+desc.Digest]
				if !ok || int64(len(b)) != desc.Size {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
			}
			// Enforce Lazarette's subject manifest existence, mediaType, and size check.
			if doc.Subject.Digest != "" {
				subj, ok := s.manifests[repo+"@"+doc.Subject.Digest]
				if !ok || subj.mediaType != doc.Subject.MediaType || int64(len(subj.raw)) != doc.Subject.Size {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				artifactType := doc.ArtifactType
				if artifactType == "" {
					artifactType = doc.Config.MediaType
				}
				s.referrers[repo+"@"+doc.Subject.Digest] = append(s.referrers[repo+"@"+doc.Subject.Digest], referrerDesc{
					MediaType:    doc.MediaType,
					Digest:       dgst,
					Size:         int64(len(raw)),
					ArtifactType: artifactType,
				})
				w.Header().Set("OCI-Subject", doc.Subject.Digest)
			}
			s.manifests[repo+"@"+dgst] = storedManifest{mediaType: doc.MediaType, raw: raw}
			w.Header().Set("Docker-Content-Digest", dgst)
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestOCISignerRoundTripWithOCIVerifier(t *testing.T) {
	priv, _, _, pubPEM := generatePrivatePEMs(t)
	otherPriv, _, _, _ := generatePrivatePEMs(t)
	pub, err := ParsePublicKey(pubPEM)
	if err != nil {
		t.Fatal(err)
	}

	oci := newStrictOCIServer()
	srv := httptest.NewServer(oci)
	defer srv.Close()

	signer, err := NewOCISigner(srv.URL, srv.Client(), priv, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewOCIVerifier(srv.URL, srv.Client(), pub, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	// 1. OCI image manifest subject -> SignAndPush -> OCIVerifier returns Verified.
	manifestDigest := oci.seedManifest("muster", OCIManifestMediaType, []byte(`{"schemaVersion":2,"mediaType":"`+OCIManifestMediaType+`","layers":[]}`))
	if got := verifier.Verify(context.Background(), "muster", manifestDigest); got != Unsigned {
		t.Fatalf("before signing: Verify = %v, want Unsigned", got)
	}
	refDigest, err := signer.SignAndPush(context.Background(), "lazarette.clearedstack.svc.cluster.local:5000/muster", "muster", manifestDigest)
	if err != nil || !strings.HasPrefix(refDigest, "sha256:") {
		t.Fatalf("SignAndPush manifest: refDigest=%q err=%v", refDigest, err)
	}
	if got := verifier.Verify(context.Background(), "muster", manifestDigest); got != Verified {
		t.Fatalf("after signing: Verify = %v, want Verified", got)
	}

	// 2. OCI image index subject (multi-platform / ko index) also signs and verifies cleanly.
	indexDigest := oci.seedManifest("conn", OCIIndexMediaType, []byte(`{"schemaVersion":2,"mediaType":"`+OCIIndexMediaType+`","manifests":[]}`))
	if _, err := signer.SignAndPush(context.Background(), "lazarette.clearedstack.svc.cluster.local:5000/conn", "conn", indexDigest); err != nil {
		t.Fatalf("SignAndPush index: %v", err)
	}
	if got := verifier.Verify(context.Background(), "conn", indexDigest); got != Verified {
		t.Fatalf("after signing index: Verify = %v, want Verified", got)
	}

	// 3. Signing with an untrusted key must not pass verification.
	untrustedSigner, _ := NewOCISigner(srv.URL, srv.Client(), otherPriv, 2*time.Second)
	untrustedDigest := oci.seedManifest("assay", OCIManifestMediaType, []byte(`{"schemaVersion":2,"mediaType":"`+OCIManifestMediaType+`","layers":[]}`))
	if _, err := untrustedSigner.SignAndPush(context.Background(), "lazarette.clearedstack.svc.cluster.local:5000/assay", "assay", untrustedDigest); err != nil {
		t.Fatalf("untrusted SignAndPush: %v", err)
	}
	if got := verifier.Verify(context.Background(), "assay", untrustedDigest); got != Unsigned {
		t.Fatalf("untrusted signature: Verify = %v, want Unsigned", got)
	}

	// 4. Missing subject manifest is refused before uploading any blobs.
	missingDigest := "sha256:" + strings.Repeat("9c", 32)
	if _, err := signer.SignAndPush(context.Background(), "lazarette.clearedstack.svc.cluster.local:5000/muster", "muster", missingDigest); err == nil {
		t.Fatal("SignAndPush succeeded for missing subject manifest")
	}
}

func TestOCISignerRejectsOffHostRedirectAndTimesOut(t *testing.T) {
	priv, _, _, _ := generatePrivatePEMs(t)
	imgRaw := []byte(`{"schemaVersion":2,"mediaType":"` + OCIManifestMediaType + `","layers":[]}`)
	imgDigest := sha256Digest(imgRaw)

	// Off-host upload Location must be refused so key-signed payloads never leave Lazarette.
	offHostSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/manifests/"):
			w.Header().Set("Content-Type", OCIManifestMediaType)
			_, _ = w.Write(imgRaw)
		case r.Method == http.MethodHead && strings.Contains(r.URL.Path, "/blobs/"):
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/blobs/uploads/"):
			w.Header().Set("Location", "https://evil.example/upload")
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer offHostSrv.Close()

	signer, _ := NewOCISigner(offHostSrv.URL, offHostSrv.Client(), priv, time.Second)
	if _, err := signer.SignAndPush(context.Background(), "lazarette:5000/muster", "muster", imgDigest); err == nil {
		t.Fatal("SignAndPush accepted off-host upload Location")
	}

	// Stalled registry reports E_TIMEOUT.
	stall := make(chan struct{})
	stallSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-stall:
		case <-r.Context().Done():
		}
	}))
	defer stallSrv.Close()
	defer close(stall)

	slowSigner, _ := NewOCISigner(stallSrv.URL, stallSrv.Client(), priv, 30*time.Millisecond)
	_, err := slowSigner.SignAndPush(context.Background(), "lazarette:5000/muster", "muster", imgDigest)
	var f *fault.Fault
	if !errors.As(err, &f) || f.Code != fault.Timeout {
		t.Fatalf("stalled SignAndPush: err=%v, want E_TIMEOUT", err)
	}
}
