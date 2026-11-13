package controller

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/clearedstack/keelworks/internal/push"
	"github.com/clearedstack/keelworks/internal/standin"
	"github.com/clearedstack/keelworks/internal/testjob"
)

const (
	passing   = "1111111111111111111111111111111111111111"
	stalling  = "4444444444444444444444444444444444444444"
	preempted = "5555555555555555555555555555555555555555"
)

func setup(t *testing.T, cfg Config) (*Controller, *standin.Cluster, standin.Plainforge) {
	t.Helper()
	cluster := standin.NewCluster(script(t))
	ctl, err := New(cfg, cluster)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ctl.Close)
	return ctl, cluster, standin.Plainforge{Keelworks: ctl}
}

func wait(t *testing.T, ctl *Controller, id string) Report {
	t.Helper()
	select {
	case <-ctl.Done(id):
	case <-time.After(30 * time.Second):
		t.Fatal("the run did not end")
	}
	report, _ := ctl.Report(id)
	return report
}

func runID(repository, ref, commit string) string {
	return push.Event{Repository: repository, Ref: ref, Commit: commit}.RunID()
}

func TestSecondDeliveryStartsNothing(t *testing.T) {
	ctl, cluster, forge := setup(t, testConfig())
	id := runID("muster", "refs/heads/main", passing)

	status, body := forge.Push("muster", "refs/heads/main", passing, "Tidy the roll call")
	if status != http.StatusAccepted {
		t.Fatalf("first delivery: status %d, body %s", status, body)
	}
	wait(t, ctl, id)
	status, body = forge.Push("muster", "refs/heads/main", passing, "Tidy the roll call")
	want := `{"status":"known","run":"` + id + `"}` + "\n"
	if status != http.StatusOK || string(body) != want {
		t.Fatalf("second delivery: status %d, body %s, want 200 %s", status, body, want)
	}
	if n := len(cluster.Created()); n != 1 {
		t.Fatalf("%d Jobs were created, want 1", n)
	}
}

func TestInterruptedRunIsRetriedOnRedelivery(t *testing.T) {
	ctl, cluster, forge := setup(t, testConfig())
	id := runID("plainforge", "refs/heads/main", preempted)
	for i := 1; i <= 2; i++ {
		status, _ := forge.Push("plainforge", "refs/heads/main", preempted, "Sort the branch list")
		if status != http.StatusAccepted {
			t.Fatalf("delivery %d: status %d, want 202", i, status)
		}
		if r := wait(t, ctl, id); r.State != testjob.StateInterrupted || r.Reason != testjob.ReasonPreempted {
			t.Fatalf("delivery %d: report %+v", i, r)
		}
	}
	if n := len(cluster.Created()); n != 2 {
		t.Fatalf("%d Jobs were created, want 2", n)
	}
}

func TestBodyOverTheLimitIsRefused(t *testing.T) {
	_, cluster, forge := setup(t, testConfig())
	body := standin.Notification("muster", "refs/heads/main", passing, strings.Repeat("a", push.MaxBody))
	status, answer := forge.Deliver(body)
	want := `{"status":"refused","code":"E_PARSE","detail":"body is too large"}` + "\n"
	if status != http.StatusBadRequest || string(answer) != want {
		t.Fatalf("status %d, body %s", status, answer)
	}
	if len(cluster.Created()) != 0 {
		t.Fatal("a Job was created")
	}
}

func TestTableFullRefusesAndFinishedRunsMakeRoom(t *testing.T) {
	cfg := testConfig()
	cfg.MaxRuns = 1
	ctl, _, forge := setup(t, cfg)

	if status, _ := forge.Push("shipkeeper", "refs/heads/main", stalling, "Add a slow test"); status != http.StatusAccepted {
		t.Fatalf("status %d, want 202", status)
	}
	status, body := forge.Push("muster", "refs/heads/main", passing, "Tidy the roll call")
	want := `{"status":"refused","detail":"too many runs in progress"}` + "\n"
	if status != http.StatusServiceUnavailable || string(body) != want {
		t.Fatalf("status %d, body %s", status, body)
	}

	// A second controller whose only run has finished gives its place away.
	ctl2, _, forge2 := setup(t, cfg)
	first := runID("muster", "refs/heads/main", passing)
	forge2.Push("muster", "refs/heads/main", passing, "Tidy the roll call")
	wait(t, ctl2, first)
	if status, _ := forge2.Push("muster", "refs/tags/v1", passing, "Tag it"); status != http.StatusAccepted {
		t.Fatalf("status %d, want 202", status)
	}
	if _, ok := ctl2.Report(first); ok {
		t.Fatal("the oldest finished run was not forgotten")
	}
	_ = ctl
}

func TestCloseInterruptsARunningJobAndLeavesNoneBehind(t *testing.T) {
	ctl, cluster, forge := setup(t, testConfig())
	id := runID("shipkeeper", "refs/heads/main", stalling)
	forge.Push("shipkeeper", "refs/heads/main", stalling, "Add a slow test")
	if r, _ := ctl.Report(id); r.State != StateRunning {
		t.Fatalf("state %q, want running", r.State)
	}
	ctl.Close()
	if r, _ := ctl.Report(id); r.State != testjob.StateInterrupted || r.Reason != testjob.ReasonShutdown {
		t.Fatalf("report %+v", r)
	}
	if n := cluster.Live(); n != 0 {
		t.Fatalf("%d Jobs were left behind", n)
	}
	if status, _ := forge.Push("muster", "refs/heads/main", passing, "After close"); status != http.StatusServiceUnavailable {
		t.Fatalf("status %d after Close, want 503", status)
	}
}

func TestOtherPathsAndMethods(t *testing.T) {
	ctl, _, _ := setup(t, testConfig())
	cases := []struct {
		method, path string
		status       int
		body         string
	}{
		{"GET", "/healthz", 200, `{"status":"ok"}`},
		{"POST", "/healthz", 405, `{"status":"refused","detail":"method not allowed"}`},
		{"GET", "/webhook", 405, `{"status":"refused","detail":"method not allowed"}`},
		{"POST", "/runs/muster-000000000000", 405, `{"status":"refused","detail":"method not allowed"}`},
		{"GET", "/runs/muster-000000000000", 404, `{"status":"refused","detail":"no such run"}`},
		{"GET", "/", 404, `{"status":"refused","detail":"no such path"}`},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		ctl.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, bytes.NewReader(nil)))
		if rec.Code != c.status || rec.Body.String() != c.body+"\n" {
			t.Errorf("%s %s: status %d, body %s", c.method, c.path, rec.Code, rec.Body)
		}
	}
}

func TestConfigWithAnythingMissingIsRefused(t *testing.T) {
	broken := []func(*Config){
		func(c *Config) { c.TestImage = "" },
		func(c *Config) { c.PriorityClass = "" },
		func(c *Config) { c.JobTimeout = 0 },
		func(c *Config) { c.MaxRuns = 0 },
	}
	for i, breakIt := range broken {
		cfg := testConfig()
		breakIt(&cfg)
		if _, err := New(cfg, standin.NewCluster(standin.Script{})); err == nil {
			t.Errorf("case %d: a broken configuration was accepted", i)
		}
	}
}
