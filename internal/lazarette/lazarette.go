// Package lazarette checks with Lazarette that a built image and its cosign
// signature are both stored before Keelworks writes the digest to app.yaml.
package lazarette

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ClearedStack/keelworks/internal/registry"
)

// Media types and annotation keys of OCI and cosign SimpleSigning artifacts.
const (
	OCIManifestMediaType   = "application/vnd.oci.image.manifest.v1+json"
	OCIIndexMediaType      = "application/vnd.oci.image.index.v1+json"
	SimpleSigningMediaType = "application/vnd.dev.cosign.simplesigning.v1+json"
	SignatureAnnotation    = "dev.cosignproject.cosign/signature"
	cosignPayloadType      = "cosign container image signature"
)

const (
	maxManifestBytes = 64 << 10
	maxPayloadBytes  = 64 << 10
	maxReferrers     = 16
	maxLayers        = 8
)

// Status is what Keelworks concluded about an image digest in Lazarette.
type Status int

const (
	// Verified: the image manifest is stored and carries a valid cosign
	// signature under the configured public key.
	Verified Status = iota + 1
	// Unsigned: the image is missing, has no signature artifact, or has no
	// valid signature for that digest under the configured public key.
	Unsigned
	// TimedOut: Lazarette did not answer within the verification timeout.
	TimedOut
	// Unavailable: Lazarette could not be reached or returned a server fault.
	Unavailable
)

// Verifier checks whether an image digest is stored and signed in Lazarette.
type Verifier interface {
	Verify(ctx context.Context, image, digest string) Status
}

// ParsePublicKey parses an ECDSA P-256 public key in PKIX PEM form.
func ParsePublicKey(pemData []byte) (*ecdsa.PublicKey, error) {
	block, rest := pem.Decode(pemData)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 || block.Type != "PUBLIC KEY" {
		return nil, errors.New("cosign public key must be a single PEM PUBLIC KEY block")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, errors.New("cosign public key is not valid PKIX")
	}
	pub, ok := parsed.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return nil, errors.New("cosign public key must be ECDSA P-256")
	}
	return pub, nil
}

// KeySource returns the trusted ECDSA P-256 public key for signature checks.
type KeySource func() (*ecdsa.PublicKey, error)

// FilePublicKey returns a KeySource that re-reads and parses path on each call
// so rotating a mounted Kubernetes Secret (cosign.pub) takes effect without
// restarting Keelworks.
func FilePublicKey(path string) KeySource {
	return func() (*ecdsa.PublicKey, error) {
		pemData, err := os.ReadFile(path)
		if err != nil {
			return nil, errors.New("cosign public key file could not be read")
		}
		return ParsePublicKey(pemData)
	}
}

// OCIVerifier checks Lazarette over cluster-private HTTP using the OCI
// Distribution v1.1 API (with fallback to cosign .sig tags) and verifies
// ECDSA P-256 signatures in memory.
type OCIVerifier struct {
	base      string
	client    *http.Client
	keySource KeySource
	timeout   time.Duration
}

// NewOCIVerifier returns an OCIVerifier pinned to a fixed in-memory public key.
func NewOCIVerifier(base string, client *http.Client, pub *ecdsa.PublicKey, timeout time.Duration) (*OCIVerifier, error) {
	if pub == nil {
		return nil, errors.New("no cosign public key is configured")
	}
	return NewDynamicOCIVerifier(base, client, func() (*ecdsa.PublicKey, error) {
		return pub, nil
	}, timeout)
}

// NewDynamicOCIVerifier returns an OCIVerifier that resolves the trusted
// public key from keySource on each verification call.
func NewDynamicOCIVerifier(base string, client *http.Client, keySource KeySource, timeout time.Duration) (*OCIVerifier, error) {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("lazarette URL must be http or https with a host")
	}
	if keySource == nil {
		return nil, errors.New("no cosign public key source is configured")
	}
	if timeout <= 0 {
		return nil, errors.New("no lazarette timeout is configured")
	}
	var c http.Client
	if client != nil {
		c = *client
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &OCIVerifier{base: u.String(), client: &c, keySource: keySource, timeout: timeout}, nil
}

// Verify checks that image@digest exists in Lazarette and has a valid cosign
// signature for digest.
func (v *OCIVerifier) Verify(ctx context.Context, image, digest string) Status {
	if image == "" || !registry.ValidDigest(digest) {
		return Unsigned
	}
	pub, err := v.keySource()
	if err != nil || pub == nil {
		return Unavailable
	}
	callCtx, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()

	// 1. Check that the image manifest itself is present by digest.
	status, hdr, _, err := v.request(callCtx, http.MethodHead, "/v2/"+image+"/manifests/"+digest, OCIManifestMediaType+", "+OCIIndexMediaType, 0)
	if err != nil {
		return classifyErr(ctx, callCtx)
	}
	switch status {
	case http.StatusOK:
		if got := hdr.Get("Docker-Content-Digest"); got != "" && got != digest {
			return Unsigned
		}
	case http.StatusNotFound, http.StatusBadRequest, http.StatusUnprocessableEntity:
		return Unsigned
	default:
		return Unavailable
	}

	// 2. Locate signature manifests via OCI 1.1 referrers, falling back to the
	// classic sha256-<hex>.sig tag when referrers are not served.
	manifests, st := v.signatureManifests(callCtx, image, digest)
	if st != Verified {
		return st
	}
	for _, rawManifest := range manifests {
		switch v.verifyManifest(ctx, callCtx, pub, image, digest, rawManifest) {
		case Verified:
			return Verified
		case TimedOut:
			return TimedOut
		case Unavailable:
			return Unavailable
		}
	}
	return Unsigned
}

type ociIndex struct {
	Manifests []struct {
		MediaType    string `json:"mediaType"`
		Digest       string `json:"digest"`
		ArtifactType string `json:"artifactType"`
	} `json:"manifests"`
}

type ociManifest struct {
	SchemaVersion int `json:"schemaVersion"`
	Layers        []struct {
		MediaType   string            `json:"mediaType"`
		Digest      string            `json:"digest"`
		Annotations map[string]string `json:"annotations"`
	} `json:"layers"`
}

type simpleSigning struct {
	Critical struct {
		Image struct {
			DockerManifestDigest string `json:"docker-manifest-digest"`
		} `json:"image"`
		Type string `json:"type"`
	} `json:"critical"`
}

func (v *OCIVerifier) signatureManifests(callCtx context.Context, image, digest string) ([][]byte, Status) {
	refPath := "/v2/" + image + "/referrers/" + digest + "?artifactType=" + url.QueryEscape(SimpleSigningMediaType)
	status, _, body, err := v.request(callCtx, http.MethodGet, refPath, OCIIndexMediaType, maxManifestBytes)
	if err != nil {
		return nil, classifyErr(context.Background(), callCtx)
	}
	if status == http.StatusOK {
		var idx ociIndex
		if json.Unmarshal(body, &idx) != nil || len(idx.Manifests) > maxReferrers {
			return nil, Unsigned
		}
		var out [][]byte
		for _, desc := range idx.Manifests {
			if !registry.ValidDigest(desc.Digest) {
				continue
			}
			if desc.ArtifactType != "" && desc.ArtifactType != SimpleSigningMediaType {
				continue
			}
			st, _, mbody, err := v.request(callCtx, http.MethodGet, "/v2/"+image+"/manifests/"+desc.Digest, OCIManifestMediaType, maxManifestBytes)
			if err != nil {
				return nil, classifyErr(context.Background(), callCtx)
			}
			if st == http.StatusOK && sha256Digest(mbody) == desc.Digest {
				out = append(out, mbody)
			} else if st >= 500 {
				return nil, Unavailable
			}
		}
		if len(out) > 0 {
			return out, Verified
		}
		return nil, Unsigned
	}
	if status != http.StatusNotFound && status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return nil, Unavailable
	}

	// Fallback: sha256-<hex>.sig tag.
	sigTag := "sha256-" + strings.TrimPrefix(digest, "sha256:") + ".sig"
	st, _, mbody, err := v.request(callCtx, http.MethodGet, "/v2/"+image+"/manifests/"+sigTag, OCIManifestMediaType, maxManifestBytes)
	if err != nil {
		return nil, classifyErr(context.Background(), callCtx)
	}
	switch st {
	case http.StatusOK:
		return [][]byte{mbody}, Verified
	case http.StatusNotFound, http.StatusBadRequest, http.StatusUnprocessableEntity:
		return nil, Unsigned
	default:
		return nil, Unavailable
	}
}

func (v *OCIVerifier) verifyManifest(parentCtx, callCtx context.Context, pub *ecdsa.PublicKey, image, digest string, rawManifest []byte) Status {
	var m ociManifest
	if json.Unmarshal(rawManifest, &m) != nil || m.SchemaVersion != 2 || len(m.Layers) == 0 || len(m.Layers) > maxLayers {
		return Unsigned
	}
	for _, layer := range m.Layers {
		if layer.MediaType != SimpleSigningMediaType || !registry.ValidDigest(layer.Digest) {
			continue
		}
		sigB64 := layer.Annotations[SignatureAnnotation]
		if sigB64 == "" {
			continue
		}
		sigBytes, err := base64.StdEncoding.DecodeString(sigB64)
		if err != nil || len(sigBytes) == 0 {
			continue
		}
		st, _, payload, err := v.request(callCtx, http.MethodGet, "/v2/"+image+"/blobs/"+layer.Digest, SimpleSigningMediaType, maxPayloadBytes)
		if err != nil {
			return classifyErr(parentCtx, callCtx)
		}
		if st >= 500 {
			return Unavailable
		}
		if st != http.StatusOK || sha256Digest(payload) != layer.Digest {
			continue
		}
		sum := sha256.Sum256(payload)
		if !ecdsa.VerifyASN1(pub, sum[:], sigBytes) {
			continue
		}
		var ss simpleSigning
		if json.Unmarshal(payload, &ss) != nil {
			continue
		}
		if ss.Critical.Type == cosignPayloadType && ss.Critical.Image.DockerManifestDigest == digest {
			return Verified
		}
	}
	return Unsigned
}

func (v *OCIVerifier) request(ctx context.Context, method, path, accept string, limit int64) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, v.base+path, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	if method == http.MethodHead {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
		return resp.StatusCode, resp.Header, nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return 0, nil, nil, err
	}
	if int64(len(data)) > limit {
		return http.StatusBadRequest, resp.Header, nil, nil
	}
	return resp.StatusCode, resp.Header, data, nil
}

func classifyErr(parentCtx, callCtx context.Context) Status {
	if parentCtx.Err() == nil && errors.Is(callCtx.Err(), context.DeadlineExceeded) {
		return TimedOut
	}
	return Unavailable
}

func sha256Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
