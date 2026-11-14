package standin

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ClearedStack/keelworks/internal/buildjob"
	"github.com/ClearedStack/keelworks/internal/registry"
)

func rendered() string {
	target, _ := registry.ByRepository("muster")
	return string(buildjob.Render(buildjob.Spec{
		RunID:         "muster-000000000000",
		Target:        target,
		Ref:           "refs/heads/main",
		Commit:        "1111111111111111111111111111111111111111",
		BuilderImage:  "made-up-image",
		PlainforgeURL: "http://plainforge.clearedstack.svc.cluster.local:8080",
		ImageRegistry: "lazarette.clearedstack.svc.cluster.local:5000",
		CosignSecret:  "keelworks-cosign",
		PriorityClass: "made-up-low-priority",
		Deadline:      time.Minute,
	}))
}

// TestAdmitRefusesEveryBrokenSecurityRule changes one thing at a time in a
// good manifest and expects the stand-in cluster to refuse it.
func TestAdmitRefusesEveryBrokenSecurityRule(t *testing.T) {
	good := rendered()
	if reason := Admit([]byte(good)); reason != "" {
		t.Fatalf("the rendered Job was refused: %s", reason)
	}
	cases := []struct{ old, new, want string }{
		{`"kind": "Job"`, `"kind": "Pod"`, "not a batch/v1 Job"},
		{`"namespace": "clearedstack"`, `"namespace": "default"`, "wrong namespace"},
		{`"activeDeadlineSeconds": 60`, `"activeDeadlineSeconds": 0`, "no deadline"},
		{`"automountServiceAccountToken": false`, `"automountServiceAccountToken": true`, "mounts service account token"},
		{`"priorityClassName": "made-up-low-priority"`, `"priorityClassName": ""`, "no priority class"},
		{`"restartPolicy": "Never"`, `"restartPolicy": "Never", "hostNetwork": true`, "uses the host's namespaces"},
		{`"arm64"`, `"amd64"`, "missing arm64 or cm3588-plus node affinity"},
		{`"cm3588-plus"`, `"other-node"`, "missing arm64 or cm3588-plus node affinity"},
		{"\"emptyDir\": {\n              \"sizeLimit\": \"1Gi\"\n            }", `"hostPath": {"path": "/var/run/docker.sock"}`, "mounts a host path"},
		{`"key": "cosign.key"`, `"key": "other.key"`, "signing-key volume must project cosign.key from a Secret"},
		{"\"runAsNonRoot\": true,\n          \"runAsUser\"", "\"runAsNonRoot\": false,\n          \"runAsUser\"", "pod may run as root"},
		{`"privileged": false`, `"privileged": true`, "privileged container"},
		{`"allowPrivilegeEscalation": false`, `"allowPrivilegeEscalation": true`, "privilege escalation allowed"},
		{`"readOnlyRootFilesystem": true`, `"readOnlyRootFilesystem": false`, "root filesystem is writable"},
		{"\"readOnlyRootFilesystem\": true,\n              \"runAsNonRoot\": true", "\"readOnlyRootFilesystem\": true,\n              \"runAsNonRoot\": false", "container may run as root"},
		{`"capabilities": {`, `"capabilities": {"add": ["SYS_ADMIN"],`, "adds a capability"},
		{`"ALL"`, `"NET_RAW"`, "does not drop every capability"},
		{`"image": "made-up-image"`, `"image": ""`, "no image"},
		{`"readOnly": true`, `"readOnly": false`, "signing-key mount is writable"},
	}
	for _, c := range cases {
		if !strings.Contains(good, c.old) {
			t.Errorf("%q is not in the rendered Job; the test is out of date", c.old)
			continue
		}
		bad := strings.Replace(good, c.old, c.new, 1)
		if got := Admit([]byte(bad)); got != c.want {
			t.Errorf("after %q -> %q: Admit = %q, want %q", c.old, c.new, got, c.want)
		}
		if err := NewCluster(Script{}, nil).Create(context.Background(), []byte(bad)); !errors.Is(err, buildjob.ErrRefused) {
			t.Errorf("after %q -> %q: Create = %v, want ErrRefused", c.old, c.new, err)
		}
	}
}

func TestClusterRefusesAJobThatAlreadyExists(t *testing.T) {
	c := NewCluster(Script{}, nil)
	if err := c.Create(context.Background(), []byte(rendered())); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(context.Background(), []byte(rendered())); !errors.Is(err, buildjob.ErrRefused) {
		t.Fatalf("second Create = %v, want ErrRefused", err)
	}
}

func TestScriptWithAnUnknownOutcomeIsRefused(t *testing.T) {
	if _, err := ParseScript([]byte(`{"outcomes":[{"repository":"muster","commit":"1","outcome":"maybe"}]}`)); err == nil {
		t.Fatal("an unknown outcome was accepted")
	}
	if _, err := ParseScript([]byte(`{"outcomes":[{"repository":"muster","commit":"1","outcome":"pass","digest":"bad"}]}`)); err == nil {
		t.Fatal("a malformed digest was accepted")
	}
	if _, err := ParseScript([]byte(`{`)); err == nil {
		t.Fatal("broken JSON was accepted")
	}
}
