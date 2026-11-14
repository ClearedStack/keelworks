package controller

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ClearedStack/keelworks/internal/buildjob"
	"github.com/ClearedStack/keelworks/internal/plainforge"
	"github.com/ClearedStack/keelworks/internal/push"
	"github.com/ClearedStack/keelworks/internal/standin"
)

const (
	earlier   = "abababababababababababababababababababab"
	passing   = "1111111111111111111111111111111111111111"
	stalling  = "4444444444444444444444444444444444444444"
	preempted = "5555555555555555555555555555555555555555"
)

type fixture struct {
	ctl        *Controller
	cluster    *standin.Cluster
	reviewer   *standin.Reviewer
	lazarette  *standin.Lazarette
	forgeStore *standin.ForgeStore
	forge      standin.Plainforge
}

func setup(t *testing.T, cfg Config) fixture {
	t.Helper()
	sc := script(t)
	laz := standin.NewLazarette()
	cluster := standin.NewCluster(sc, laz)
	reviewer := standin.NewReviewer(sc)
	forgeStore := standin.NewForgeStore(sc)
	ctl, err := New(cfg, cluster, reviewer, laz, forgeStore)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ctl.Close)
	return fixture{
		ctl:        ctl,
		cluster:    cluster,
		reviewer:   reviewer,
		lazarette:  laz,
		forgeStore: forgeStore,
		forge:      standin.Plainforge{Keelworks: ctl, Token: plainforgeToken},
	}
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

func runID(repository, ref, after string) string {
	return push.Event{Repository: repository, Ref: ref, After: after}.RunID()
}

func TestPassingBuildVerifiesAndPinsWithSkipCI(t *testing.T) {
	f := setup(t, testConfig())
	id := runID("muster", "refs/heads/main", passing)
	status, _ := f.forge.Push("muster", "refs/heads/main", earlier, passing)
	if status != http.StatusAccepted {
		t.Fatalf("status %d, want 202", status)
	}
	r := wait(t, f.ctl, id)
	if r.State != buildjob.StatePassed || r.Digest == "" {
		t.Fatalf("report %+v", r)
	}
	commits := f.forgeStore.Commits()
	if len(commits) != 1 || !strings.Contains(commits[0].Message, plainforge.SkipMarker) {
		t.Fatalf("commits = %+v", commits)
	}
	manifest, ok := f.forgeStore.Manifest("muster")
	if !ok || !strings.Contains(string(manifest), "digest: "+r.Digest+"\n") {
		t.Fatalf("app.yaml after pin:\n%s", manifest)
	}
}

func TestSecondDeliveryStartsNothing(t *testing.T) {
	f := setup(t, testConfig())
	id := runID("muster", "refs/heads/main", passing)

	status, body := f.forge.Push("muster", "refs/heads/main", earlier, passing)
	if status != http.StatusAccepted {
		t.Fatalf("first delivery: status %d, body %s", status, body)
	}
	wait(t, f.ctl, id)
	status, body = f.forge.Push("muster", "refs/heads/main", earlier, passing)
	want := `{"status":"known","run":"` + id + `"}` + "\n"
	if status != http.StatusOK || string(body) != want {
		t.Fatalf("second delivery: status %d, body %s, want 200 %s", status, body, want)
	}
	if n := len(f.cluster.Created()); n != 1 {
		t.Fatalf("%d Jobs were created, want 1", n)
	}
}

func TestInterruptedRunIsRetriedOnRedelivery(t *testing.T) {
	f := setup(t, testConfig())
	id := runID("plainforge", "refs/heads/main", preempted)
	for i := 1; i <= 2; i++ {
		status, _ := f.forge.Push("plainforge", "refs/heads/main", earlier, preempted)
		if status != http.StatusAccepted {
			t.Fatalf("delivery %d: status %d, want 202", i, status)
		}
		if r := wait(t, f.ctl, id); r.State != buildjob.StateInterrupted || r.Reason != buildjob.ReasonPreempted {
			t.Fatalf("delivery %d: report %+v", i, r)
		}
	}
	if n := len(f.cluster.Created()); n != 2 {
		t.Fatalf("%d Jobs were created, want 2", n)
	}
}

func TestBodyOverTheLimitIsRefused(t *testing.T) {
	f := setup(t, testConfig())
	body := append(standin.Notification("muster", "refs/heads/main", earlier, passing), bytes.Repeat([]byte(" "), push.MaxBody)...)
	status, answer := f.forge.Deliver(body)
	want := `{"status":"refused","code":"E_PARSE","detail":"body is too large"}` + "\n"
	if status != http.StatusBadRequest || string(answer) != want {
		t.Fatalf("status %d, body %s", status, answer)
	}
	if len(f.cluster.Created()) != 0 {
		t.Fatal("a Job was created")
	}
}

func TestTheCommitBeforeDoesNotChangeTheRun(t *testing.T) {
	f := setup(t, testConfig())
	id := runID("muster", "refs/heads/main", passing)
	if status, _ := f.forge.Push("muster", "refs/heads/main", earlier, passing); status != http.StatusAccepted {
		t.Fatalf("status %d, want 202", status)
	}
	wait(t, f.ctl, id)
	status, body := f.forge.Push("muster", "refs/heads/main", preempted, passing)
	want := `{"status":"known","run":"` + id + `"}` + "\n"
	if status != http.StatusOK || string(body) != want {
		t.Fatalf("status %d, body %s, want 200 %s", status, body, want)
	}
}

func TestCallerIsCheckedBeforeAnythingElse(t *testing.T) {
	f := setup(t, testConfig())
	for _, body := range []string{"", "{", `{"repository":"muster"}`} {
		status, header, answer := standin.Call(f.ctl, nil, []byte(body))
		want := `{"status":"refused","detail":"no bearer token"}` + "\n"
		if status != http.StatusUnauthorized || string(answer) != want || header.Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("body %q: status %d, header %v, answer %s", body, status, header, answer)
		}
	}
	if n := f.reviewer.Reviews(); n != 0 {
		t.Fatalf("the cluster was asked %d times about calls that carried no token", n)
	}

	status, header, _ := standin.Call(f.ctl, []string{"Bearer made-up-token-nobody-holds"}, []byte("{"))
	if status != http.StatusUnauthorized || header.Get("WWW-Authenticate") != "Bearer" {
		t.Errorf("unknown token: status %d, header %v", status, header)
	}
	status, _, _ = standin.Call(f.ctl, []string{"Bearer made-up-token-of-muster"}, []byte("{"))
	if status != http.StatusForbidden {
		t.Errorf("another service account: status %d, want 403", status)
	}
	if n := f.reviewer.Reviews(); n != 2 {
		t.Fatalf("the cluster was asked %d times, want 2", n)
	}
	if n := len(f.cluster.Created()); n != 0 {
		t.Fatalf("%d Jobs were created", n)
	}
}

func TestEveryDeliveryIsCheckedAgain(t *testing.T) {
	f := setup(t, testConfig())
	for i := 0; i < 3; i++ {
		f.forge.Push("muster", "refs/heads/main", earlier, passing)
	}
	if n := f.reviewer.Reviews(); n != 3 {
		t.Fatalf("the cluster was asked %d times for 3 deliveries", n)
	}
}

func TestMalformedBearerTokensNeverReachTheCluster(t *testing.T) {
	f := setup(t, testConfig())
	body := standin.Notification("muster", "refs/heads/main", earlier, passing)
	for _, value := range []string{
		"",
		"Bearer",
		"Bearer ",
		"bearer " + plainforgeToken,
		"Bearer  " + plainforgeToken,
		"Bearer " + plainforgeToken + " ",
		"Bearer made up",
		"Bearer ====",
		"Bearer made=up",
		"Bearer " + string(bytes.Repeat([]byte("a"), 4097)),
		plainforgeToken,
	} {
		status, _, answer := standin.Call(f.ctl, []string{value}, body)
		want := `{"status":"refused","detail":"no bearer token"}` + "\n"
		if status != http.StatusUnauthorized || string(answer) != want {
			t.Errorf("Authorization %.40q: status %d, answer %s", value, status, answer)
		}
	}
	if n := f.reviewer.Reviews(); n != 0 {
		t.Fatalf("the cluster was asked %d times", n)
	}
	if n := len(f.cluster.Created()); n != 0 {
		t.Fatalf("%d Jobs were created", n)
	}
}

func TestTableFullRefusesAndFinishedRunsMakeRoom(t *testing.T) {
	cfg := testConfig()
	cfg.MaxRuns = 1
	f := setup(t, cfg)

	if status, _ := f.forge.Push("shipkeeper", "refs/heads/main", earlier, stalling); status != http.StatusAccepted {
		t.Fatalf("status %d, want 202", status)
	}
	status, body := f.forge.Push("muster", "refs/heads/main", earlier, passing)
	want := `{"status":"refused","detail":"too many runs in progress"}` + "\n"
	if status != http.StatusServiceUnavailable || string(body) != want {
		t.Fatalf("status %d, body %s", status, body)
	}

	g := setup(t, cfg)
	first := runID("muster", "refs/heads/main", passing)
	g.forge.Push("muster", "refs/heads/main", earlier, passing)
	wait(t, g.ctl, first)
	if status, _ := g.forge.Push("muster", "refs/tags/v1", earlier, passing); status != http.StatusAccepted {
		t.Fatalf("status %d, want 202", status)
	}
	if _, ok := g.ctl.Report(first); ok {
		t.Fatal("the oldest finished run was not forgotten")
	}
}

func TestCloseInterruptsARunningJobAndLeavesNoneBehind(t *testing.T) {
	f := setup(t, testConfig())
	id := runID("shipkeeper", "refs/heads/main", stalling)
	f.forge.Push("shipkeeper", "refs/heads/main", earlier, stalling)
	if r, _ := f.ctl.Report(id); r.State != StateRunning {
		t.Fatalf("state %q, want running", r.State)
	}
	f.ctl.Close()
	if r, _ := f.ctl.Report(id); r.State != buildjob.StateInterrupted || r.Reason != buildjob.ReasonShutdown {
		t.Fatalf("report %+v", r)
	}
	if n := f.cluster.Live(); n != 0 {
		t.Fatalf("%d Jobs were left behind", n)
	}
	if status, _ := f.forge.Push("muster", "refs/heads/main", earlier, passing); status != http.StatusServiceUnavailable {
		t.Fatalf("status %d after Close, want 503", status)
	}
}

func TestOtherPathsAndMethods(t *testing.T) {
	f := setup(t, testConfig())
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
		f.ctl.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, bytes.NewReader(nil)))
		if rec.Code != c.status || rec.Body.String() != c.body+"\n" {
			t.Errorf("%s %s: status %d, body %s", c.method, c.path, rec.Code, rec.Body)
		}
	}
}

func TestConfigWithAnythingMissingIsRefused(t *testing.T) {
	broken := []func(*Config){
		func(c *Config) { c.KoImage = "" },
		func(c *Config) { c.BuildKitImage = "" },
		func(c *Config) { c.PlainforgeURL = "" },
		func(c *Config) { c.ImageRegistry = "" },
		func(c *Config) { c.CosignSecret = "" },
		func(c *Config) { c.PriorityClass = "" },
		func(c *Config) { c.JobTimeout = 0 },
		func(c *Config) { c.ReviewTimeout = 0 },
		func(c *Config) { c.MaxRuns = 0 },
	}
	sc := standin.Script{}
	for i, breakIt := range broken {
		cfg := testConfig()
		breakIt(&cfg)
		laz := standin.NewLazarette()
		if _, err := New(cfg, standin.NewCluster(sc, laz), standin.NewReviewer(sc), laz, standin.NewForgeStore(sc)); err == nil {
			t.Errorf("case %d: a broken configuration was accepted", i)
		}
	}
	laz := standin.NewLazarette()
	cl := standin.NewCluster(sc, laz)
	rev := standin.NewReviewer(sc)
	pin := standin.NewForgeStore(sc)
	if _, err := New(testConfig(), nil, rev, laz, pin); err == nil {
		t.Error("a controller without a cluster was accepted")
	}
	if _, err := New(testConfig(), cl, nil, laz, pin); err == nil {
		t.Error("a controller without a token reviewer was accepted")
	}
	if _, err := New(testConfig(), cl, rev, nil, pin); err == nil {
		t.Error("a controller without a Lazarette verifier was accepted")
	}
	if _, err := New(testConfig(), cl, rev, laz, nil); err == nil {
		t.Error("a controller without a Plainforge pinner was accepted")
	}
}
