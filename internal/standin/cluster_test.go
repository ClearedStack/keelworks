package standin

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ClearedStack/keelworks/internal/testjob"
)

func rendered() string {
	return string(testjob.Render(testjob.Spec{
		RunID: "muster-000000000000", Repository: "muster", Ref: "refs/heads/main",
		Commit: "1111111111111111111111111111111111111111", Image: "made-up-image",
		PriorityClass: "made-up-low-priority", Deadline: time.Minute,
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
		{`"priorityClassName": "made-up-low-priority"`, `"priorityClassName": ""`, "no priority class"},
		{`"restartPolicy": "Never"`, `"restartPolicy": "Never", "hostNetwork": true`, "uses the host's namespaces"},
		{`"restartPolicy": "Never"`, `"restartPolicy": "Never", "volumes": [{"name": "sock", "hostPath": {"path": "/var/run/docker.sock"}}]`, "mounts a volume"},
		{"\"runAsNonRoot\": true,\n          \"seccompProfile\"", "\"runAsNonRoot\": false,\n          \"seccompProfile\"", "pod may run as root"},
		{`"privileged": false`, `"privileged": true`, "privileged container"},
		{`"allowPrivilegeEscalation": false`, `"allowPrivilegeEscalation": true`, "privilege escalation allowed"},
		{"\"privileged\": false,\n              \"runAsNonRoot\": true", "\"privileged\": false,\n              \"runAsNonRoot\": false", "container may run as root"},
		{`"capabilities": {`, `"capabilities": {"add": ["SYS_ADMIN"],`, "adds a capability"},
		{`"ALL"`, `"NET_RAW"`, "does not drop every capability"},
		{`"image": "made-up-image"`, `"image": ""`, "no image"},
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
		if err := NewCluster(Script{}).Create(context.Background(), []byte(bad)); !errors.Is(err, testjob.ErrRefused) {
			t.Errorf("after %q -> %q: Create = %v, want ErrRefused", c.old, c.new, err)
		}
	}
}

func TestClusterRefusesAJobThatAlreadyExists(t *testing.T) {
	c := NewCluster(Script{})
	if err := c.Create(context.Background(), []byte(rendered())); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(context.Background(), []byte(rendered())); !errors.Is(err, testjob.ErrRefused) {
		t.Fatalf("second Create = %v, want ErrRefused", err)
	}
}

func TestScriptWithAnUnknownOutcomeIsRefused(t *testing.T) {
	if _, err := ParseScript([]byte(`{"outcomes":[{"repository":"muster","commit":"1","outcome":"maybe"}]}`)); err == nil {
		t.Fatal("an unknown outcome was accepted")
	}
	if _, err := ParseScript([]byte(`{`)); err == nil {
		t.Fatal("broken JSON was accepted")
	}
}
