package lazarette

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func generateKeyPair(t *testing.T) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	return priv, pemBytes
}

func makeSignedArtifacts(t *testing.T, priv *ecdsa.PrivateKey, image, imgDigest string) (sigManifestDigest string, sigManifest []byte, payloadDigest string, payload []byte) {
	t.Helper()
	payloadObj := map[string]any{
		"critical": map[string]any{
			"identity": map[string]string{"docker-reference": "lazarette.clearedstack.svc.cluster.local:5000/" + image},
			"image":    map[string]string{"docker-manifest-digest": imgDigest},
			"type":     cosignPayloadType,
		},
		"optional": nil,
	}
	var err error
	payload, err = json.Marshal(payloadObj)
	if err != nil {
		t.Fatal(err)
	}
	payloadDigest = sha256Digest(payload)
	sum := sha256.Sum256(payload)
	sig, err := ecdsa.SignASN1(rand.Reader, priv, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	manifestObj := map[string]any{
		"schemaVersion": 2,
		"mediaType":     OCIManifestMediaType,
		"artifactType":  SimpleSigningMediaType,
		"subject": map[string]any{
			"mediaType": OCIManifestMediaType,
			"digest":    imgDigest,
			"size":      123,
		},
		"layers": []map[string]any{{
			"mediaType": SimpleSigningMediaType,
			"digest":    payloadDigest,
			"size":      len(payload),
			"annotations": map[string]string{
				SignatureAnnotation: base64.StdEncoding.EncodeToString(sig),
			},
		}},
	}
	sigManifest, err = json.Marshal(manifestObj)
	if err != nil {
		t.Fatal(err)
	}
	sigManifestDigest = sha256Digest(sigManifest)
	return sigManifestDigest, sigManifest, payloadDigest, payload
}

func TestParsePublicKey(t *testing.T) {
	_, pemBytes := generateKeyPair(t)
	if _, err := ParsePublicKey(pemBytes); err != nil {
		t.Fatalf("ParsePublicKey valid key: %v", err)
	}
	// Wrong curve (P-384) must be refused.
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der384, _ := x509.MarshalPKIXPublicKey(&p384.PublicKey)
	pem384 := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der384})
	for _, bad := range [][]byte{
		nil,
		[]byte("not pem"),
		append(pemBytes, pemBytes...),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der384}),
		pem384,
	} {
		if _, err := ParsePublicKey(bad); err == nil {
			t.Error("ParsePublicKey accepted an invalid key")
		}
	}
}

func TestVerifyReferrersAndFallbackSigTag(t *testing.T) {
	priv, pemBytes := generateKeyPair(t)
	otherPriv, _ := generateKeyPair(t)
	pub, err := ParsePublicKey(pemBytes)
	if err != nil {
		t.Fatal(err)
	}

	imgDigest := "sha256:" + strings.Repeat("1a", 32)
	otherDigest := "sha256:" + strings.Repeat("2b", 32)
	sigDigest, sigManifest, blobDigest, blob := makeSignedArtifacts(t, priv, "muster", imgDigest)
	wrongKeySigDigest, wrongKeyManifest, wrongKeyBlobDigest, wrongKeyBlob := makeSignedArtifacts(t, otherPriv, "muster", imgDigest)
	wrongSubjectSigDigest, wrongSubjectManifest, wrongSubjectBlobDigest, wrongSubjectBlob := makeSignedArtifacts(t, priv, "muster", otherDigest)

	t.Run("oci-1.1-referrers-valid", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodHead && r.URL.Path == "/v2/muster/manifests/"+imgDigest:
				w.Header().Set("Docker-Content-Digest", imgDigest)
				w.WriteHeader(http.StatusOK)
			case r.Method == http.MethodGet && r.URL.Path == "/v2/muster/referrers/"+imgDigest:
				w.Header().Set("Content-Type", OCIIndexMediaType)
				_, _ = w.Write([]byte(`{"schemaVersion":2,"mediaType":"` + OCIIndexMediaType + `","manifests":[{"mediaType":"` + OCIManifestMediaType + `","digest":"` + sigDigest + `","artifactType":"` + SimpleSigningMediaType + `"}]}`))
			case r.Method == http.MethodGet && r.URL.Path == "/v2/muster/manifests/"+sigDigest:
				_, _ = w.Write(sigManifest)
			case r.Method == http.MethodGet && r.URL.Path == "/v2/muster/blobs/"+blobDigest:
				_, _ = w.Write(blob)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer srv.Close()

		v, err := NewOCIVerifier(srv.URL, srv.Client(), pub, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if got := v.Verify(context.Background(), "muster", imgDigest); got != Verified {
			t.Fatalf("Verify = %v, want Verified", got)
		}
	})

	t.Run("sig-tag-fallback-when-referrers-unimplemented", func(t *testing.T) {
		sigTag := "sha256-" + strings.TrimPrefix(imgDigest, "sha256:") + ".sig"
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodHead && r.URL.Path == "/v2/muster/manifests/"+imgDigest:
				w.WriteHeader(http.StatusOK)
			case r.Method == http.MethodGet && r.URL.Path == "/v2/muster/referrers/"+imgDigest:
				w.WriteHeader(http.StatusUnprocessableEntity) // Lazarette refuses unfamiliar endpoints with 422
			case r.Method == http.MethodGet && r.URL.Path == "/v2/muster/manifests/"+sigTag:
				_, _ = w.Write(sigManifest)
			case r.Method == http.MethodGet && r.URL.Path == "/v2/muster/blobs/"+blobDigest:
				_, _ = w.Write(blob)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer srv.Close()

		v, _ := NewOCIVerifier(srv.URL, srv.Client(), pub, time.Second)
		if got := v.Verify(context.Background(), "muster", imgDigest); got != Verified {
			t.Fatalf("Verify = %v, want Verified", got)
		}
	})

	t.Run("unsigned-cases-are-rejected", func(t *testing.T) {
		cases := []struct {
			name    string
			handler http.HandlerFunc
		}{
			{
				name: "image-missing",
				handler: func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusNotFound)
				},
			},
			{
				name: "no-signature-present",
				handler: func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodHead {
						w.WriteHeader(http.StatusOK)
						return
					}
					w.WriteHeader(http.StatusNotFound)
				},
			},
			{
				name: "signed-by-another-key",
				handler: func(w http.ResponseWriter, r *http.Request) {
					switch {
					case r.Method == http.MethodHead:
						w.WriteHeader(http.StatusOK)
					case strings.Contains(r.URL.Path, "/referrers/"):
						_, _ = w.Write([]byte(`{"manifests":[{"digest":"` + wrongKeySigDigest + `"}]}`))
					case r.URL.Path == "/v2/muster/manifests/"+wrongKeySigDigest:
						_, _ = w.Write(wrongKeyManifest)
					case r.URL.Path == "/v2/muster/blobs/"+wrongKeyBlobDigest:
						_, _ = w.Write(wrongKeyBlob)
					}
				},
			},
			{
				name: "signature-for-different-digest",
				handler: func(w http.ResponseWriter, r *http.Request) {
					switch {
					case r.Method == http.MethodHead:
						w.WriteHeader(http.StatusOK)
					case strings.Contains(r.URL.Path, "/referrers/"):
						_, _ = w.Write([]byte(`{"manifests":[{"digest":"` + wrongSubjectSigDigest + `"}]}`))
					case r.URL.Path == "/v2/muster/manifests/"+wrongSubjectSigDigest:
						_, _ = w.Write(wrongSubjectManifest)
					case r.URL.Path == "/v2/muster/blobs/"+wrongSubjectBlobDigest:
						_, _ = w.Write(wrongSubjectBlob)
					}
				},
			},
			{
				name: "tampered-blob-bytes",
				handler: func(w http.ResponseWriter, r *http.Request) {
					switch {
					case r.Method == http.MethodHead:
						w.WriteHeader(http.StatusOK)
					case strings.Contains(r.URL.Path, "/referrers/"):
						_, _ = w.Write([]byte(`{"manifests":[{"digest":"` + sigDigest + `"}]}`))
					case r.URL.Path == "/v2/muster/manifests/"+sigDigest:
						_, _ = w.Write(sigManifest)
					case r.URL.Path == "/v2/muster/blobs/"+blobDigest:
						_, _ = w.Write(append([]byte(" "), blob...))
					}
				},
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				srv := httptest.NewServer(tc.handler)
				defer srv.Close()
				v, _ := NewOCIVerifier(srv.URL, srv.Client(), pub, time.Second)
				if got := v.Verify(context.Background(), "muster", imgDigest); got != Unsigned {
					t.Fatalf("Verify = %v, want Unsigned", got)
				}
			})
		}
	})

	t.Run("timeout-and-unavailable", func(t *testing.T) {
		stall := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-stall:
			case <-r.Context().Done():
			}
		}))
		defer srv.Close()
		defer close(stall)

		v, _ := NewOCIVerifier(srv.URL, srv.Client(), pub, 30*time.Millisecond)
		if got := v.Verify(context.Background(), "muster", imgDigest); got != TimedOut {
			t.Fatalf("stalled server: got %v, want TimedOut", got)
		}

		srv500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv500.Close()
		v500, _ := NewOCIVerifier(srv500.URL, srv500.Client(), pub, time.Second)
		if got := v500.Verify(context.Background(), "muster", imgDigest); got != Unavailable {
			t.Fatalf("500 server: got %v, want Unavailable", got)
		}
	})
}
