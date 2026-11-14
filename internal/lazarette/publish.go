package lazarette

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ClearedStack/keelworks/internal/fault"
	"github.com/ClearedStack/keelworks/internal/registry"
)

// Media types for OCI v1 image configs and gzip-compressed tar layers.
const (
	OCIConfigMediaType    = "application/vnd.oci.image.config.v1+json"
	OCILayerGzipMediaType = "application/vnd.oci.image.layer.v1.tar+gzip"
)

const (
	containerNonRootUID = 65532
	containerNonRootGID = 65532
	maxBaseConfigBytes  = 1 << 20
	maxBaseLayerBytes   = 128 << 20
)

// ImageSpec describes a compiled static Linux Go binary to package and publish
// as a reproducible OCI v1 image in Lazarette.
type ImageSpec struct {
	AppSlug     string
	Binary      []byte
	TargetImage string
	BaseImage   string
	Arch        string
}

type ociImageConfig struct {
	Created      string            `json:"created"`
	Architecture string            `json:"architecture"`
	OS           string            `json:"os"`
	Config       ociExecRuntimeCfg `json:"config"`
	RootFS       ociRootFSConfig   `json:"rootfs"`
}

type ociExecRuntimeCfg struct {
	User       string   `json:"User"`
	Env        []string `json:"Env,omitempty"`
	Entrypoint []string `json:"Entrypoint"`
}

type ociRootFSConfig struct {
	Type    string   `json:"type"`
	DiffIDs []string `json:"diff_ids"`
}

type publishedManifest struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	Config        ociDescriptor   `json:"config"`
	Layers        []ociDescriptor `json:"layers"`
}

// OCIPublisher packages static Linux binaries into deterministic OCI v1 images
// and pushes them by sha256 digest directly to Lazarette over HTTP, requiring
// no external ko or Docker tooling.
type OCIPublisher struct {
	base    *url.URL
	client  *http.Client
	timeout time.Duration
}

// NewOCIPublisher returns an OCIPublisher talking to Lazarette at base.
func NewOCIPublisher(base string, client *http.Client, timeout time.Duration) (*OCIPublisher, error) {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("lazarette URL must be http or https with a host")
	}
	if timeout <= 0 {
		return nil, errors.New("no lazarette publish timeout is configured")
	}
	var c http.Client
	if client != nil {
		c = *client
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &OCIPublisher{base: u, client: &c, timeout: timeout}, nil
}

// PublishBinary packages spec.Binary at /ko-app/<AppSlug> (UID/GID 65532:65532,
// mode 0555, deterministic epoch timestamps), optionally layering atop a
// digest-pinned base image already stored in Lazarette, uploads the layer and
// config blobs, commits the OCI v1 manifest by digest, and returns that
// sha256:<64hex> manifest digest.
func (p *OCIPublisher) PublishBinary(ctx context.Context, spec ImageSpec) (string, error) {
	if !validSlugComponent(spec.AppSlug) || spec.TargetImage == "" || len(spec.Binary) == 0 {
		return "", fault.New(fault.Parse, "OCI image spec is invalid")
	}
	arch := strings.TrimSpace(spec.Arch)
	if arch == "" {
		arch = "arm64"
	}

	callCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	baseLayers, baseDiffIDs, baseEnv, err := p.resolveBaseImage(ctx, callCtx, spec.BaseImage, spec.TargetImage, arch)
	if err != nil {
		return "", err
	}

	rawTar, err := buildDeterministicAppTar(spec.AppSlug, spec.Binary)
	if err != nil {
		return "", err
	}
	appDiffID := sha256Digest(rawTar)

	gzLayer, err := gzipDeterministicBytes(rawTar)
	if err != nil {
		return "", err
	}
	appLayerDigest := sha256Digest(gzLayer)

	cfgDoc := ociImageConfig{
		Created:      "1970-01-01T00:00:00Z",
		Architecture: arch,
		OS:           "linux",
		Config: ociExecRuntimeCfg{
			User:       "65532:65532",
			Env:        baseEnv,
			Entrypoint: []string{"/ko-app/" + spec.AppSlug},
		},
		RootFS: ociRootFSConfig{
			Type:    "layers",
			DiffIDs: append(append([]string(nil), baseDiffIDs...), appDiffID),
		},
	}
	cfgBytes, err := json.Marshal(cfgDoc)
	if err != nil {
		return "", fault.New(fault.Parse, "OCI config could not be encoded")
	}
	cfgDigest := sha256Digest(cfgBytes)

	if err := p.ensureBlob(ctx, callCtx, spec.TargetImage, cfgDigest, cfgBytes); err != nil {
		return "", err
	}
	if err := p.ensureBlob(ctx, callCtx, spec.TargetImage, appLayerDigest, gzLayer); err != nil {
		return "", err
	}

	layers := make([]ociDescriptor, 0, len(baseLayers)+1)
	layers = append(layers, baseLayers...)
	layers = append(layers, ociDescriptor{
		MediaType: OCILayerGzipMediaType,
		Digest:    appLayerDigest,
		Size:      int64(len(gzLayer)),
	})

	manifestDoc := publishedManifest{
		SchemaVersion: 2,
		MediaType:     OCIManifestMediaType,
		Config: ociDescriptor{
			MediaType: OCIConfigMediaType,
			Digest:    cfgDigest,
			Size:      int64(len(cfgBytes)),
		},
		Layers: layers,
	}
	rawManifest, err := json.Marshal(manifestDoc)
	if err != nil {
		return "", fault.New(fault.Parse, "OCI manifest could not be encoded")
	}
	manifestDigest := sha256Digest(rawManifest)

	status, _, err := p.do(callCtx, http.MethodPut, "/v2/"+spec.TargetImage+"/manifests/"+manifestDigest, OCIManifestMediaType, "", rawManifest, 4096)
	if err != nil {
		return "", signerCallErr(ctx, callCtx, "could not push image manifest to Lazarette")
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return "", errors.New("Lazarette refused the built image manifest")
	}
	return manifestDigest, nil
}

func (p *OCIPublisher) resolveBaseImage(parentCtx, callCtx context.Context, baseRef, targetImage, arch string) ([]ociDescriptor, []string, []string, error) {
	baseRef = strings.TrimSpace(baseRef)
	if baseRef == "" || isZeroBootstrapBase(baseRef) {
		return nil, nil, nil, nil
	}
	refPart, baseDigest, hasAt := strings.Cut(baseRef, "@")
	if !hasAt || !registry.ValidDigest(baseDigest) {
		return nil, nil, nil, fault.New(fault.Parse, "base image reference must be pinned by sha256 digest")
	}
	host, baseRepo, hasSlash := strings.Cut(refPart, "/")
	if !hasSlash || host == "" || baseRepo == "" {
		return nil, nil, nil, fault.New(fault.Parse, "base image reference is malformed")
	}
	if host != p.base.Host && host != "lazarette.clearedstack.svc.cluster.local:5000" {
		return nil, nil, nil, fault.New(fault.NeedsSubtool, "base image must reside in internal Lazarette")
	}

	manifestBytes, mediaType, err := p.getManifest(parentCtx, callCtx, baseRepo, baseDigest)
	if err != nil {
		return nil, nil, nil, err
	}
	if mediaType == OCIIndexMediaType {
		childDigest, err := selectPlatformManifest(manifestBytes, arch)
		if err != nil {
			return nil, nil, nil, err
		}
		manifestBytes, mediaType, err = p.getManifest(parentCtx, callCtx, baseRepo, childDigest)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	if mediaType != OCIManifestMediaType {
		return nil, nil, nil, errors.New("base image manifest has an unsupported media type")
	}

	var parsed publishedManifest
	if err := json.Unmarshal(manifestBytes, &parsed); err != nil || parsed.SchemaVersion != 2 || !registry.ValidDigest(parsed.Config.Digest) {
		return nil, nil, nil, errors.New("base image manifest is malformed")
	}

	cfgBytes, err := p.getBlob(parentCtx, callCtx, baseRepo, parsed.Config.Digest, maxBaseConfigBytes)
	if err != nil {
		return nil, nil, nil, err
	}
	var baseCfg ociImageConfig
	if err := json.Unmarshal(cfgBytes, &baseCfg); err != nil {
		return nil, nil, nil, errors.New("base image config is malformed")
	}
	if len(baseCfg.RootFS.DiffIDs) != len(parsed.Layers) {
		return nil, nil, nil, errors.New("base image diff_ids count does not match its layers")
	}

	for _, layer := range parsed.Layers {
		if !registry.ValidDigest(layer.Digest) || layer.Size < 0 || layer.Size > maxBaseLayerBytes {
			return nil, nil, nil, errors.New("base image layer descriptor is invalid")
		}
		status, _, err := p.do(callCtx, http.MethodHead, "/v2/"+targetImage+"/blobs/"+layer.Digest, "", "", nil, 0)
		if err != nil {
			return nil, nil, nil, signerCallErr(parentCtx, callCtx, "could not check base layer in Lazarette")
		}
		if status == http.StatusOK {
			continue
		}
		layerBytes, err := p.getBlob(parentCtx, callCtx, baseRepo, layer.Digest, maxBaseLayerBytes)
		if err != nil {
			return nil, nil, nil, err
		}
		if err := p.ensureBlob(parentCtx, callCtx, targetImage, layer.Digest, layerBytes); err != nil {
			return nil, nil, nil, err
		}
	}

	return parsed.Layers, baseCfg.RootFS.DiffIDs, baseCfg.Config.Env, nil
}

func selectPlatformManifest(indexBytes []byte, wantArch string) (string, error) {
	var idx struct {
		Manifests []struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
			Platform  struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(indexBytes, &idx); err != nil {
		return "", errors.New("base image index is malformed")
	}
	for _, m := range idx.Manifests {
		if m.Platform.OS == "linux" && m.Platform.Architecture == wantArch && registry.ValidDigest(m.Digest) {
			return m.Digest, nil
		}
	}
	return "", errors.New("base image index has no linux/" + wantArch + " manifest")
}

func (p *OCIPublisher) getManifest(parentCtx, callCtx context.Context, repo, dgst string) ([]byte, string, error) {
	accept := OCIManifestMediaType + ", " + OCIIndexMediaType
	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, p.base.String()+"/v2/"+repo+"/manifests/"+dgst, nil)
	if err != nil {
		return nil, "", errors.New("could not prepare base manifest request")
	}
	req.Header.Set("Accept", accept)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, "", signerCallErr(parentCtx, callCtx, "could not fetch base manifest from Lazarette")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", errors.New("base image manifest is not stored in Lazarette")
	}
	const limit = 4 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, "", signerCallErr(parentCtx, callCtx, "could not read base manifest from Lazarette")
	}
	if len(data) > limit || sha256Digest(data) != dgst {
		return nil, "", errors.New("base image manifest did not match its digest")
	}
	var probe struct {
		MediaType string `json:"mediaType"`
	}
	_ = json.Unmarshal(data, &probe)
	return data, probe.MediaType, nil
}

func (p *OCIPublisher) getBlob(parentCtx, callCtx context.Context, repo, dgst string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, p.base.String()+"/v2/"+repo+"/blobs/"+dgst, nil)
	if err != nil {
		return nil, errors.New("could not prepare base blob request")
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, signerCallErr(parentCtx, callCtx, "could not fetch base blob from Lazarette")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("base image blob is missing from Lazarette")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, signerCallErr(parentCtx, callCtx, "could not read base blob from Lazarette")
	}
	if int64(len(data)) > limit || sha256Digest(data) != dgst {
		return nil, errors.New("base image blob did not match its digest")
	}
	return data, nil
}

func (p *OCIPublisher) ensureBlob(parentCtx, callCtx context.Context, image, blobDigest string, data []byte) error {
	status, _, err := p.do(callCtx, http.MethodHead, "/v2/"+image+"/blobs/"+blobDigest, "", "", nil, 0)
	if err != nil {
		return signerCallErr(parentCtx, callCtx, "could not check blob in Lazarette")
	}
	if status == http.StatusOK {
		return nil
	}

	status, hdr, err := p.do(callCtx, http.MethodPost, "/v2/"+image+"/blobs/uploads/", "", "", nil, 4096)
	if err != nil {
		return signerCallErr(parentCtx, callCtx, "could not start blob upload in Lazarette")
	}
	if status == http.StatusCreated {
		return nil
	}
	if status != http.StatusAccepted {
		return errors.New("Lazarette refused to start a blob upload")
	}

	s := &OCISigner{base: p.base}
	putPath, err := s.resolveUploadLocation(hdr.Get("Location"), blobDigest)
	if err != nil {
		return err
	}
	status, _, err = p.do(callCtx, http.MethodPut, putPath, "application/octet-stream", "", data, 4096)
	if err != nil {
		return signerCallErr(parentCtx, callCtx, "could not finish blob upload in Lazarette")
	}
	if status != http.StatusCreated {
		return errors.New("Lazarette refused to commit an image blob")
	}
	return nil
}

func (p *OCIPublisher) do(ctx context.Context, method, path, contentType, accept string, body []byte, limit int64) (int, http.Header, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.base.String()+path, r)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	if limit > 0 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, limit))
	}
	return resp.StatusCode, resp.Header, nil
}

func buildDeterministicAppTar(appSlug string, binary []byte) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	epoch := time.Unix(0, 0).UTC()

	dirHdr := &tar.Header{
		Typeflag: tar.TypeDir,
		Name:     "ko-app/",
		Mode:     0o555,
		Uid:      containerNonRootUID,
		Gid:      containerNonRootGID,
		ModTime:  epoch,
		Format:   tar.FormatUSTAR,
	}
	if err := tw.WriteHeader(dirHdr); err != nil {
		return nil, errors.New("could not write ko-app directory header")
	}

	fileHdr := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     "ko-app/" + appSlug,
		Size:     int64(len(binary)),
		Mode:     0o555,
		Uid:      containerNonRootUID,
		Gid:      containerNonRootGID,
		ModTime:  epoch,
		Format:   tar.FormatUSTAR,
	}
	if err := tw.WriteHeader(fileHdr); err != nil {
		return nil, errors.New("could not write binary tar header")
	}
	if _, err := tw.Write(binary); err != nil {
		return nil, errors.New("could not write binary tar payload")
	}
	if err := tw.Close(); err != nil {
		return nil, errors.New("could not close binary tar layer")
	}
	return buf.Bytes(), nil
}

func gzipDeterministicBytes(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	zw.Name = ""
	zw.Comment = ""
	zw.ModTime = time.Unix(0, 0).UTC()
	zw.OS = 255
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func isZeroBootstrapBase(ref string) bool {
	_, dgst, ok := strings.Cut(ref, "@")
	return ok && dgst == "sha256:"+strings.Repeat("0", 64)
}

func validSlugComponent(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
			return false
		}
	}
	return true
}
