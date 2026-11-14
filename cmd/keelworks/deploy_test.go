package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ClearedStack/keelworks/internal/plainforge"
	"github.com/ClearedStack/keelworks/internal/registry"
)

func TestDeployManifestsAndRepoHygiene(t *testing.T) {
	root := filepath.Clean("../..")

	// 1. Verify app.yaml is valid and updatable by Keelworks' own pinner logic.
	appYAML, err := os.ReadFile(filepath.Join(root, "app.yaml"))
	if err != nil {
		t.Fatalf("read app.yaml: %v", err)
	}
	nextDigest := "sha256:" + strings.Repeat("ab", 32)
	updated, changed, err := plainforge.UpdateAppYAML(appYAML, nextDigest)
	if err != nil || !changed || !strings.Contains(string(updated), "digest: "+nextDigest+"\n") {
		t.Fatalf("app.yaml failed UpdateAppYAML: changed=%v err=%v", changed, err)
	}

	// 2. Verify deploy/ manifests satisfy all cluster security, affinity and pinning rules.
	deployRaw, err := os.ReadFile(filepath.Join(root, "deploy", "deployment.yaml"))
	if err != nil {
		t.Fatalf("read deploy/deployment.yaml: %v", err)
	}
	deployText := string(deployRaw)
	for _, want := range []string{
		"namespace: clearedstack",
		"runAsNonRoot: true",
		"readOnlyRootFilesystem: true",
		"allowPrivilegeEscalation: false",
		`drop: ["ALL"]`,
		"kubernetes.io/arch",
		`values: ["arm64"]`,
		"kubernetes.io/hostname",
		`values: ["cm3588-plus"]`,
		"memory: 16Mi",
		"memory: 32Mi",
	} {
		if !strings.Contains(deployText, want) {
			t.Errorf("deploy/deployment.yaml missing %q", want)
		}
	}

	// Every image reference in deploy/deployment.yaml and .ko.yaml must point to
	// Lazarette and be pinned by a valid sha256 digest (never :latest).
	koRaw, err := os.ReadFile(filepath.Join(root, ".ko.yaml"))
	if err != nil {
		t.Fatalf("read .ko.yaml: %v", err)
	}
	for _, fileContent := range []string{deployText, string(koRaw)} {
		if strings.Contains(fileContent, ":latest") {
			t.Error("manifest contains mutable :latest tag")
		}
		for _, line := range strings.Split(fileContent, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "#") {
				continue
			}
			for _, prefix := range []string{"image: ", "defaultBaseImage: ", "- -ko-image=", "- -buildkit-image="} {
				if val, ok := strings.CutPrefix(trimmed, prefix); ok {
					if !strings.HasPrefix(val, "lazarette.clearedstack.svc.cluster.local:5000/") {
						t.Errorf("image %q does not pull from internal Lazarette", val)
					}
					_, digest, hasAt := strings.Cut(val, "@")
					if !hasAt || !registry.ValidDigest(digest) {
						t.Errorf("image %q is not pinned by a valid sha256 digest", val)
					}
				}
			}
		}
	}

	netpolRaw, err := os.ReadFile(filepath.Join(root, "deploy", "networkpolicy.yaml"))
	if err != nil {
		t.Fatalf("read deploy/networkpolicy.yaml: %v", err)
	}
	netpolText := string(netpolRaw)
	for _, want := range []string{
		"name: default-deny",
		"name: keelworks",
		"name: keelworks-build",
		"app.kubernetes.io/name: plainforge",
		"app.kubernetes.io/name: lazarette",
	} {
		if !strings.Contains(netpolText, want) {
			t.Errorf("deploy/networkpolicy.yaml missing %q", want)
		}
	}
	for _, forbidden := range []string{"logbook", "5432", "cs-workers"} {
		for _, line := range strings.Split(netpolText, "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "#") && strings.Contains(line, forbidden) {
				t.Errorf("deploy/networkpolicy.yaml unexpectedly opens %q: %s", forbidden, line)
			}
		}
	}

	// 3. Walk the repository to enforce: no Dockerfile, no Secret manifests,
	// no PEM private keys, and no em dashes anywhere.
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.EqualFold(d.Name(), "Dockerfile") || strings.HasPrefix(d.Name(), "Dockerfile.") {
			t.Errorf("forbidden Dockerfile present: %s", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(data)
		if strings.ContainsRune(text, '\u2014') {
			t.Errorf("em dash (U+2014) found in %s", path)
		}
		if strings.Contains(text, "PRIVATE"+" KEY-----") {
			t.Errorf("PEM private key material found in %s", path)
		}
		if strings.HasPrefix(path, filepath.Join(root, "deploy")) && strings.Contains(text, "\nkind: Secret\n") {
			t.Errorf("Kubernetes Secret checked into deploy/: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
