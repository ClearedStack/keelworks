package lazarette

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ClearedStack/keelworks/internal/fault"
	"github.com/ClearedStack/keelworks/internal/registry"
)

// EmptyConfigMediaType is the OCI v1.1 empty config descriptor media type used
// on artifact referrer manifests.
const EmptyConfigMediaType = "application/vnd.oci.empty.v1+json"

var emptyConfigBytes = []byte("{}")

// ParsePrivateKey parses an ECDSA P-256 private key in PKCS#8 ("PRIVATE KEY")
// or SEC 1 ("EC PRIVATE KEY") PEM form.
func ParsePrivateKey(pemData []byte) (*ecdsa.PrivateKey, error) {
	block, rest := pem.Decode(pemData)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("cosign private key must be a single PEM block")
	}
	var priv *ecdsa.PrivateKey
	switch block.Type {
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("cosign private key is not valid PKCS#8")
		}
		k, ok := parsed.(*ecdsa.PrivateKey)
		if !ok {
			return nil, errors.New("cosign private key must be ECDSA P-256")
		}
		priv = k
	case "EC PRIVATE KEY":
		k, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("cosign private key is not valid SEC 1 EC")
		}
		priv = k
	default:
		return nil, errors.New("cosign private key has an unfamiliar PEM type")
	}
	if priv.Curve != elliptic.P256() {
		return nil, errors.New("cosign private key must be ECDSA P-256")
	}
	return priv, nil
}

type signPayloadDoc struct {
	Critical signCritical `json:"critical"`
	Optional any          `json:"optional"`
}

type signCritical struct {
	Identity signIdentity `json:"identity"`
	Image    signImage    `json:"image"`
	Type     string       `json:"type"`
}

type signIdentity struct {
	DockerReference string `json:"docker-reference"`
}

type signImage struct {
	DockerManifestDigest string `json:"docker-manifest-digest"`
}

// SignPayload builds a canonical cosign SimpleSigning payload for dockerRef at
// digest and signs its SHA-256 hash with priv, returning the payload bytes and
// base64-encoded ASN.1 ECDSA signature.
func SignPayload(priv *ecdsa.PrivateKey, dockerRef, digest string) ([]byte, string, error) {
	if priv == nil || priv.Curve != elliptic.P256() {
		return nil, "", fault.New(fault.Parse, "cosign signing key is invalid")
	}
	if dockerRef == "" || !registry.ValidDigest(digest) {
		return nil, "", fault.New(fault.Parse, "image reference or digest is invalid")
	}
	doc := signPayloadDoc{
		Critical: signCritical{
			Identity: signIdentity{DockerReference: dockerRef},
			Image:    signImage{DockerManifestDigest: digest},
			Type:     cosignPayloadType,
		},
		Optional: nil,
	}
	payload, err := json.Marshal(doc)
	if err != nil {
		return nil, "", fault.New(fault.Parse, "signature payload could not be encoded")
	}
	sum := sha256.Sum256(payload)
	sig, err := ecdsa.SignASN1(rand.Reader, priv, sum[:])
	if err != nil {
		return nil, "", fault.New(fault.Parse, "signature could not be computed")
	}
	return payload, base64.StdEncoding.EncodeToString(sig), nil
}

// OCISigner signs stored image manifests in Lazarette and publishes an OCI v1.1
// SimpleSigning referrer manifest by sha256 digest.
type OCISigner struct {
	base    *url.URL
	client  *http.Client
	priv    *ecdsa.PrivateKey
	timeout time.Duration
}

// NewOCISigner returns an OCISigner talking to Lazarette at base (for example
// "http://lazarette.clearedstack.svc.cluster.local:5000").
func NewOCISigner(base string, client *http.Client, priv *ecdsa.PrivateKey, timeout time.Duration) (*OCISigner, error) {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("lazarette URL must be http or https with a host")
	}
	if priv == nil || priv.Curve != elliptic.P256() {
		return nil, errors.New("no valid cosign private key is configured")
	}
	if timeout <= 0 {
		return nil, errors.New("no lazarette timeout is configured")
	}
	var c http.Client
	if client != nil {
		c = *client
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &OCISigner{base: u, client: &c, priv: priv, timeout: timeout}, nil
}

type ociDescriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type referrerManifest struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	ArtifactType  string          `json:"artifactType"`
	Config        ociDescriptor   `json:"config"`
	Layers        []ociDescriptor `json:"layers"`
	Subject       ociDescriptor   `json:"subject"`
}

// SignAndPush fetches image@digest from Lazarette to confirm its exact size and
// media type, signs digest with the configured cosign private key, uploads the
// SimpleSigning blobs, and commits an OCI v1.1 referrer manifest by digest. It
// returns the sha256 digest of the referrer manifest.
func (s *OCISigner) SignAndPush(ctx context.Context, dockerRef, image, digest string) (string, error) {
	if dockerRef == "" || image == "" || !registry.ValidDigest(digest) {
		return "", fault.New(fault.Parse, "image reference or digest is invalid")
	}
	callCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	subjMediaType, subjSize, err := s.fetchSubjectMeta(ctx, callCtx, image, digest)
	if err != nil {
		return "", err
	}

	payload, sigB64, err := SignPayload(s.priv, dockerRef, digest)
	if err != nil {
		return "", err
	}

	cfgDigest := sha256Digest(emptyConfigBytes)
	if err := s.ensureBlob(ctx, callCtx, image, cfgDigest, emptyConfigBytes); err != nil {
		return "", err
	}
	payloadDigest := sha256Digest(payload)
	if err := s.ensureBlob(ctx, callCtx, image, payloadDigest, payload); err != nil {
		return "", err
	}

	manifestDoc := referrerManifest{
		SchemaVersion: 2,
		MediaType:     OCIManifestMediaType,
		ArtifactType:  SimpleSigningMediaType,
		Config: ociDescriptor{
			MediaType: EmptyConfigMediaType,
			Digest:    cfgDigest,
			Size:      int64(len(emptyConfigBytes)),
		},
		Layers: []ociDescriptor{{
			MediaType: SimpleSigningMediaType,
			Digest:    payloadDigest,
			Size:      int64(len(payload)),
			Annotations: map[string]string{
				SignatureAnnotation: sigB64,
			},
		}},
		Subject: ociDescriptor{
			MediaType: subjMediaType,
			Digest:    digest,
			Size:      subjSize,
		},
	}
	rawManifest, err := json.Marshal(manifestDoc)
	if err != nil {
		return "", fault.New(fault.Parse, "referrer manifest could not be encoded")
	}
	refDigest := sha256Digest(rawManifest)

	status, _, err := s.do(callCtx, http.MethodPut, "/v2/"+image+"/manifests/"+refDigest, OCIManifestMediaType, rawManifest, 4096)
	if err != nil {
		return "", signerCallErr(ctx, callCtx, "could not push signature manifest to Lazarette")
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return "", errors.New("Lazarette refused the signature manifest")
	}
	return refDigest, nil
}

func (s *OCISigner) fetchSubjectMeta(parentCtx, callCtx context.Context, image, digest string) (string, int64, error) {
	accept := OCIManifestMediaType + ", " + OCIIndexMediaType
	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, s.base.String()+"/v2/"+image+"/manifests/"+digest, nil)
	if err != nil {
		return "", 0, errors.New("could not prepare Lazarette manifest request")
	}
	req.Header.Set("Accept", accept)
	resp, err := s.client.Do(req)
	if err != nil {
		return "", 0, signerCallErr(parentCtx, callCtx, "could not fetch image manifest from Lazarette")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, errors.New("built image manifest is not stored in Lazarette")
	}
	const maxSubjectManifest = 4 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSubjectManifest+1))
	if err != nil {
		return "", 0, signerCallErr(parentCtx, callCtx, "could not read image manifest from Lazarette")
	}
	if len(body) > maxSubjectManifest || sha256Digest(body) != digest {
		return "", 0, errors.New("stored image manifest did not match its digest")
	}

	var probe struct {
		MediaType string `json:"mediaType"`
	}
	mediaType := ""
	if json.Unmarshal(body, &probe) == nil {
		mediaType = probe.MediaType
	}
	if mediaType == "" {
		if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err == nil {
			mediaType = mt
		}
	}
	if mediaType != OCIManifestMediaType && mediaType != OCIIndexMediaType {
		return "", 0, errors.New("stored image manifest has an unfamiliar media type")
	}
	return mediaType, int64(len(body)), nil
}

func (s *OCISigner) ensureBlob(parentCtx, callCtx context.Context, image, blobDigest string, data []byte) error {
	status, _, err := s.do(callCtx, http.MethodHead, "/v2/"+image+"/blobs/"+blobDigest, "", nil, 0)
	if err != nil {
		return signerCallErr(parentCtx, callCtx, "could not check blob in Lazarette")
	}
	if status == http.StatusOK {
		return nil
	}

	status, hdr, err := s.do(callCtx, http.MethodPost, "/v2/"+image+"/blobs/uploads/", "", nil, 4096)
	if err != nil {
		return signerCallErr(parentCtx, callCtx, "could not start blob upload in Lazarette")
	}
	if status == http.StatusCreated {
		return nil
	}
	if status != http.StatusAccepted {
		return errors.New("Lazarette refused to start a blob upload")
	}

	putPath, err := s.resolveUploadLocation(hdr.Get("Location"), blobDigest)
	if err != nil {
		return err
	}
	status, _, err = s.do(callCtx, http.MethodPut, putPath, "application/octet-stream", data, 4096)
	if err != nil {
		return signerCallErr(parentCtx, callCtx, "could not finish blob upload in Lazarette")
	}
	if status != http.StatusCreated {
		return errors.New("Lazarette refused to commit a signature blob")
	}
	return nil
}

func (s *OCISigner) resolveUploadLocation(loc, blobDigest string) (string, error) {
	if loc == "" {
		return "", errors.New("Lazarette returned no upload Location header")
	}
	u, err := url.Parse(loc)
	if err != nil {
		return "", errors.New("Lazarette returned a malformed upload Location")
	}
	if u.IsAbs() {
		if u.Scheme != s.base.Scheme || u.Host != s.base.Host {
			return "", errors.New("Lazarette upload Location points to another host")
		}
	}
	q := u.Query()
	q.Set("digest", blobDigest)
	u.RawQuery = q.Encode()
	path := u.Path
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	return path, nil
}

func (s *OCISigner) do(ctx context.Context, method, path, contentType string, body []byte, limit int64) (int, http.Header, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.base.String()+path, r)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	if limit > 0 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, limit))
	}
	return resp.StatusCode, resp.Header, nil
}

func signerCallErr(parentCtx, callCtx context.Context, msg string) error {
	if parentCtx.Err() == nil && errors.Is(callCtx.Err(), context.DeadlineExceeded) {
		return fault.New(fault.Timeout, "Lazarette signing timed out")
	}
	return errors.New(msg)
}
