package lazarette

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ClearedStack/keelworks/internal/fault"
)

func TestOCIPublisherStandaloneDeterministicAndSigned(t *testing.T) {
	priv, _, _, pubPEM := generatePrivatePEMs(t)
	pub, err := ParsePublicKey(pubPEM)
	if err != nil {
		t.Fatal(err)
	}

	oci := newStrictOCIServer()
	srv := httptest.NewServer(oci)
	defer srv.Close()

	pub1, err := NewOCIPublisher(srv.URL, srv.Client(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	pub2, err := NewOCIPublisher(srv.URL, srv.Client(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	binaryPayload := []byte("\x7fELF-fake-arm64-static-muster-binary")
	spec := ImageSpec{
		AppSlug:     "muster",
		Binary:      binaryPayload,
		TargetImage: "muster",
		BaseImage:   "lazarette.clearedstack.svc.cluster.local:5000/static@sha256:" + strings.Repeat("0", 64),
		Arch:        "arm64",
	}

	dgst1, err := pub1.PublishBinary(context.Background(), spec)
	if err != nil {
		t.Fatalf("first PublishBinary: %v", err)
	}
	dgst2, err := pub2.PublishBinary(context.Background(), spec)
	if err != nil {
		t.Fatalf("second PublishBinary: %v", err)
	}
	if dgst1 != dgst2 {
		t.Fatalf("non-deterministic image digests: %s vs %s", dgst1, dgst2)
	}

	// Inspect stored manifest, config, and tar.gz layer in Lazarette.
	oci.mu.Lock()
	storedM, ok := oci.manifests["muster@"+dgst1]
	oci.mu.Unlock()
	if !ok {
		t.Fatalf("manifest %s not stored in Lazarette", dgst1)
	}

	var manifest publishedManifest
	if err := json.Unmarshal(storedM.raw, &manifest); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	if manifest.SchemaVersion != 2 || manifest.MediaType != OCIManifestMediaType || len(manifest.Layers) != 1 {
		t.Fatalf("unexpected manifest structure: %+v", manifest)
	}
	if manifest.Config.MediaType != OCIConfigMediaType || manifest.Layers[0].MediaType != OCILayerGzipMediaType {
		t.Fatalf("unexpected media types: config=%s layer=%s", manifest.Config.MediaType, manifest.Layers[0].MediaType)
	}

	oci.mu.Lock()
	cfgRaw := oci.blobs["muster@"+manifest.Config.Digest]
	layerGz := oci.blobs["muster@"+manifest.Layers[0].Digest]
	oci.mu.Unlock()

	var cfg ociImageConfig
	if err := json.Unmarshal(cfgRaw, &cfg); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	if cfg.Created != "1970-01-01T00:00:00Z" || cfg.OS != "linux" || cfg.Architecture != "arm64" {
		t.Fatalf("unexpected image platform/created: %+v", cfg)
	}
	if cfg.Config.User != "65532:65532" || len(cfg.Config.Entrypoint) != 1 || cfg.Config.Entrypoint[0] != "/ko-app/muster" {
		t.Fatalf("unexpected runtime config: %+v", cfg.Config)
	}
	if cfg.RootFS.Type != "layers" || len(cfg.RootFS.DiffIDs) != 1 {
		t.Fatalf("unexpected rootfs: %+v", cfg.RootFS)
	}

	zr, err := gzip.NewReader(bytes.NewReader(layerGz))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	rawTar, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip layer: %v", err)
	}
	_ = zr.Close()
	if sha256Digest(rawTar) != cfg.RootFS.DiffIDs[0] {
		t.Fatalf("layer diff_id mismatch: got %s, config has %s", sha256Digest(rawTar), cfg.RootFS.DiffIDs[0])
	}

	tr := tar.NewReader(bytes.NewReader(rawTar))
	hdrDir, err := tr.Next()
	if err != nil || hdrDir.Name != "ko-app/" || hdrDir.Typeflag != tar.TypeDir || hdrDir.Uid != 65532 || hdrDir.Gid != 65532 {
		t.Fatalf("unexpected ko-app dir entry: hdr=%+v err=%v", hdrDir, err)
	}
	hdrBin, err := tr.Next()
	if err != nil || hdrBin.Name != "ko-app/muster" || hdrBin.Mode != 0o555 || hdrBin.Uid != 65532 || hdrBin.Gid != 65532 {
		t.Fatalf("unexpected binary tar entry: hdr=%+v err=%v", hdrBin, err)
	}
	gotBin, err := io.ReadAll(tr)
	if err != nil || !bytes.Equal(gotBin, binaryPayload) {
		t.Fatalf("binary payload mismatch: err=%v", err)
	}

	// Sign with OCISigner and verify with OCIVerifier end to end.
	signer, err := NewOCISigner(srv.URL, srv.Client(), priv, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := signer.SignAndPush(context.Background(), "lazarette.clearedstack.svc.cluster.local:5000/muster", "muster", dgst1); err != nil {
		t.Fatalf("SignAndPush: %v", err)
	}
	verifier, err := NewOCIVerifier(srv.URL, srv.Client(), pub, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got := verifier.Verify(context.Background(), "muster", dgst1); got != Verified {
		t.Fatalf("Verify published+signed image = %v, want Verified", got)
	}
}

func TestOCIPublisherBaseImageIndexAndPolicyRefusal(t *testing.T) {
	oci := newStrictOCIServer()
	srv := httptest.NewServer(oci)
	defer srv.Close()

	// Seed a base static image in Lazarette under repository "static", wrapped in an OCI image index.
	baseRawTar, err := gzipDeterministicBytes([]byte("fake-ca-certs-layer"))
	if err != nil {
		t.Fatal(err)
	}
	baseLayerDigest := sha256Digest(baseRawTar)
	baseDiffID := sha256Digest([]byte("fake-ca-certs-layer-uncompressed"))
	baseCfg, _ := json.Marshal(ociImageConfig{
		Created:      "1970-01-01T00:00:00Z",
		Architecture: "arm64",
		OS:           "linux",
		Config: ociExecRuntimeCfg{
			User: "65532:65532",
			Env:  []string{"SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt"},
		},
		RootFS: ociRootFSConfig{
			Type:    "layers",
			DiffIDs: []string{baseDiffID},
		},
	})
	baseCfgDigest := sha256Digest(baseCfg)

	oci.mu.Lock()
	oci.blobs["static@"+baseLayerDigest] = baseRawTar
	oci.blobs["static@"+baseCfgDigest] = baseCfg
	oci.mu.Unlock()

	baseManifestRaw, _ := json.Marshal(publishedManifest{
		SchemaVersion: 2,
		MediaType:     OCIManifestMediaType,
		Config: ociDescriptor{
			MediaType: OCIConfigMediaType,
			Digest:    baseCfgDigest,
			Size:      int64(len(baseCfg)),
		},
		Layers: []ociDescriptor{{
			MediaType: OCILayerGzipMediaType,
			Digest:    baseLayerDigest,
			Size:      int64(len(baseRawTar)),
		}},
	})
	baseManifestDigest := oci.seedManifest("static", OCIManifestMediaType, baseManifestRaw)

	indexRaw, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     OCIIndexMediaType,
		"manifests": []map[string]any{
			{
				"mediaType": OCIManifestMediaType,
				"digest":    "sha256:" + strings.Repeat("aa", 32),
				"size":      123,
				"platform":  map[string]string{"os": "linux", "architecture": "amd64"},
			},
			{
				"mediaType": OCIManifestMediaType,
				"digest":    baseManifestDigest,
				"size":      len(baseManifestRaw),
				"platform":  map[string]string{"os": "linux", "architecture": "arm64"},
			},
		},
	})
	indexDigest := oci.seedManifest("static", OCIIndexMediaType, indexRaw)

	publisher, err := NewOCIPublisher(srv.URL, srv.Client(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	dgst, err := publisher.PublishBinary(context.Background(), ImageSpec{
		AppSlug:     "assay",
		Binary:      []byte("assay-arm64-binary"),
		TargetImage: "assay",
		BaseImage:   "lazarette.clearedstack.svc.cluster.local:5000/static@" + indexDigest,
		Arch:        "arm64",
	})
	if err != nil {
		t.Fatalf("PublishBinary layered atop index: %v", err)
	}

	oci.mu.Lock()
	assayManifestRaw := oci.manifests["assay@"+dgst].raw
	oci.mu.Unlock()
	var assayManifest publishedManifest
	if err := json.Unmarshal(assayManifestRaw, &assayManifest); err != nil {
		t.Fatal(err)
	}
	if len(assayManifest.Layers) != 2 || assayManifest.Layers[0].Digest != baseLayerDigest {
		t.Fatalf("expected 2 layers starting with base layer %s, got %+v", baseLayerDigest, assayManifest.Layers)
	}

	oci.mu.Lock()
	assayCfgRaw := oci.blobs["assay@"+assayManifest.Config.Digest]
	oci.mu.Unlock()
	var assayCfg ociImageConfig
	if err := json.Unmarshal(assayCfgRaw, &assayCfg); err != nil {
		t.Fatal(err)
	}
	if len(assayCfg.RootFS.DiffIDs) != 2 || assayCfg.RootFS.DiffIDs[0] != baseDiffID {
		t.Fatalf("expected inherited base diff_id, got %+v", assayCfg.RootFS)
	}
	if len(assayCfg.Config.Env) != 1 || assayCfg.Config.Env[0] != "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt" {
		t.Fatalf("expected inherited base env, got %+v", assayCfg.Config.Env)
	}

	// External base image registries must be refused with E_NEEDS_SUBTOOL.
	_, err = publisher.PublishBinary(context.Background(), ImageSpec{
		AppSlug:     "assay",
		Binary:      []byte("assay-arm64-binary"),
		TargetImage: "assay",
		BaseImage:   "cgr.dev/chainguard/static@sha256:" + strings.Repeat("11", 32),
	})
	var f *fault.Fault
	if !errors.As(err, &f) || f.Code != fault.NeedsSubtool {
		t.Fatalf("external base image: err=%v, want E_NEEDS_SUBTOOL", err)
	}

	// Unpinned base image tag and unsafe slugs must fail with E_PARSE.
	for _, bad := range []ImageSpec{
		{AppSlug: "assay", Binary: []byte("x"), TargetImage: "assay", BaseImage: "lazarette.clearedstack.svc.cluster.local:5000/static:latest"},
		{AppSlug: "../escape", Binary: []byte("x"), TargetImage: "assay"},
		{AppSlug: "assay", Binary: nil, TargetImage: "assay"},
	} {
		_, err = publisher.PublishBinary(context.Background(), bad)
		if !errors.As(err, &f) || f.Code != fault.Parse {
			t.Fatalf("bad ImageSpec %+v: err=%v, want E_PARSE", bad, err)
		}
	}
}

func TestDynamicOCIVerifierLiveKeyRotation(t *testing.T) {
	privA, _, _, pubPEMA := generatePrivatePEMs(t)
	privB, _, _, pubPEMB := generatePrivatePEMs(t)

	oci := newStrictOCIServer()
	srv := httptest.NewServer(oci)
	defer srv.Close()

	dir := t.TempDir()
	pubPath := filepath.Join(dir, "cosign.pub")
	if err := os.WriteFile(pubPath, pubPEMA, 0o600); err != nil {
		t.Fatal(err)
	}

	verifier, err := NewDynamicOCIVerifier(srv.URL, srv.Client(), FilePublicKey(pubPath), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	dgstA := oci.seedManifest("muster", OCIManifestMediaType, []byte(`{"schemaVersion":2,"mediaType":"`+OCIManifestMediaType+`","layers":[]}`))
	dgstB := oci.seedManifest("assay", OCIManifestMediaType, []byte(`{"schemaVersion":2,"mediaType":"`+OCIManifestMediaType+`","layers":[]}`))

	signerA, _ := NewOCISigner(srv.URL, srv.Client(), privA, 2*time.Second)
	signerB, _ := NewOCISigner(srv.URL, srv.Client(), privB, 2*time.Second)
	if _, err := signerA.SignAndPush(context.Background(), "lazarette.clearedstack.svc.cluster.local:5000/muster", "muster", dgstA); err != nil {
		t.Fatal(err)
	}
	if _, err := signerB.SignAndPush(context.Background(), "lazarette.clearedstack.svc.cluster.local:5000/assay", "assay", dgstB); err != nil {
		t.Fatal(err)
	}

	// Under key A: image A passes, image B is rejected as Unsigned.
	if got := verifier.Verify(context.Background(), "muster", dgstA); got != Verified {
		t.Fatalf("before rotation key A image: Verify = %v, want Verified", got)
	}
	if got := verifier.Verify(context.Background(), "assay", dgstB); got != Unsigned {
		t.Fatalf("before rotation key B image: Verify = %v, want Unsigned", got)
	}

	// Atomically rotate cosign.pub on disk (matching kubelet Secret projection swap) without restarting OCIVerifier.
	staged := filepath.Join(dir, "cosign.pub.next")
	if err := os.WriteFile(staged, pubPEMB, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staged, pubPath); err != nil {
		t.Fatal(err)
	}

	if got := verifier.Verify(context.Background(), "assay", dgstB); got != Verified {
		t.Fatalf("after rotation key B image: Verify = %v, want Verified", got)
	}
	if got := verifier.Verify(context.Background(), "muster", dgstA); got != Unsigned {
		t.Fatalf("after rotation old key A image: Verify = %v, want Unsigned", got)
	}

	// Unreadable key file reports Unavailable cleanly.
	if err := os.Remove(pubPath); err != nil {
		t.Fatal(err)
	}
	if got := verifier.Verify(context.Background(), "assay", dgstB); got != Unavailable {
		t.Fatalf("missing key file: Verify = %v, want Unavailable", got)
	}
}
