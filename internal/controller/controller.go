// Package controller is Keelworks' HTTP surface and its table of runs: it
// takes push notifications in from Plainforge and from nobody else, starts an
// ephemeral ko or rootless BuildKit Job for each push that must be built,
// verifies the cosign-signed digest in Lazarette, pins app.yaml in Plainforge
// with [skip-ci], and reports how each run ended.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ClearedStack/keelworks/internal/buildjob"
	"github.com/ClearedStack/keelworks/internal/caller"
	"github.com/ClearedStack/keelworks/internal/fault"
	"github.com/ClearedStack/keelworks/internal/lazarette"
	"github.com/ClearedStack/keelworks/internal/plainforge"
	"github.com/ClearedStack/keelworks/internal/push"
	"github.com/ClearedStack/keelworks/internal/registry"
)

// StateRunning is the state of a run that has not finished yet.
const StateRunning = "running"

// Config holds the operator-supplied settings of the controller. Nothing here
// is guessed when missing.
type Config struct {
	KoImage       string        // pinned builder image for Go repositories
	BuildKitImage string        // pinned rootless BuildKit image for Conn
	PlainforgeURL string        // cluster-private Plainforge URL
	ImageRegistry string        // Lazarette host:port
	CosignSecret  string        // Secret holding cosign.key in clearedstack
	PriorityClass string        // low PriorityClass for ephemeral build Jobs
	JobTimeout    time.Duration // how long a build Job may take
	ReviewTimeout time.Duration // how long the cluster may take to check a caller's token
	MaxRuns       int           // how many runs are remembered
}

// Validate refuses a Config with anything missing.
func (c Config) Validate() error {
	switch {
	case c.KoImage == "":
		return errors.New("no ko builder image is configured")
	case c.BuildKitImage == "":
		return errors.New("no BuildKit builder image is configured")
	case c.PlainforgeURL == "":
		return errors.New("no Plainforge URL is configured")
	case c.ImageRegistry == "":
		return errors.New("no Lazarette image registry is configured")
	case c.CosignSecret == "":
		return errors.New("no cosign secret name is configured")
	case c.PriorityClass == "":
		return errors.New("no priority class is configured")
	case c.JobTimeout < time.Second:
		return errors.New("the job timeout must be at least one second")
	case c.ReviewTimeout <= 0:
		return errors.New("no token review timeout is configured")
	case c.MaxRuns < 1:
		return errors.New("the number of remembered runs must be at least one")
	}
	return nil
}

// Report is the public record of one run.
type Report struct {
	Run        string     `json:"run"`
	Repository string     `json:"repository"`
	App        string     `json:"app"`
	Builder    string     `json:"builder"`
	Ref        string     `json:"ref"`
	Commit     string     `json:"commit"`
	Job        string     `json:"job"`
	State      string     `json:"state"`
	Digest     string     `json:"digest,omitempty"`
	Code       fault.Code `json:"code,omitempty"`
	Reason     string     `json:"reason,omitempty"`
}

type answer struct {
	Status string     `json:"status"`
	Run    string     `json:"run,omitempty"`
	Reason string     `json:"reason,omitempty"`
	Code   fault.Code `json:"code,omitempty"`
	Detail string     `json:"detail,omitempty"`
}

type run struct {
	report Report
	done   chan struct{}
}

// Controller implements http.Handler.
type Controller struct {
	cfg      Config
	cluster  buildjob.Cluster
	reviewer caller.Reviewer
	verifier lazarette.Verifier
	pinner   plainforge.Pinner
	ctx      context.Context
	stop     context.CancelFunc
	wg       sync.WaitGroup

	mu    sync.Mutex
	runs  map[string]*run
	order []string // run ids, oldest first
}

// New returns a Controller wired to cluster, reviewer, verifier and pinner.
func New(
	cfg Config,
	cluster buildjob.Cluster,
	reviewer caller.Reviewer,
	verifier lazarette.Verifier,
	pinner plainforge.Pinner,
) (*Controller, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cluster == nil || reviewer == nil || verifier == nil || pinner == nil {
		return nil, errors.New("cluster, token reviewer, Lazarette verifier and Plainforge pinner are all required")
	}
	ctx, stop := context.WithCancel(context.Background())
	return &Controller{
		cfg:      cfg,
		cluster:  cluster,
		reviewer: reviewer,
		verifier: verifier,
		pinner:   pinner,
		ctx:      ctx,
		stop:     stop,
		runs:     map[string]*run{},
	}, nil
}

// Close stops every running Job and waits for the runs to be recorded.
func (c *Controller) Close() {
	c.stop()
	c.wg.Wait()
}

// ServeHTTP routes the three paths Keelworks serves.
func (c *Controller) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/healthz":
		if !allow(w, r, http.MethodGet) {
			return
		}
		write(w, http.StatusOK, answer{Status: "ok"})
	case r.URL.Path == "/webhook":
		if !allow(w, r, http.MethodPost) {
			return
		}
		c.webhook(w, r)
	case strings.HasPrefix(r.URL.Path, "/runs/"):
		if !allow(w, r, http.MethodGet) {
			return
		}
		report, ok := c.Report(strings.TrimPrefix(r.URL.Path, "/runs/"))
		if !ok {
			write(w, http.StatusNotFound, answer{Status: "refused", Detail: "no such run"})
			return
		}
		write(w, http.StatusOK, report)
	default:
		write(w, http.StatusNotFound, answer{Status: "refused", Detail: "no such path"})
	}
}

func (c *Controller) webhook(w http.ResponseWriter, r *http.Request) {
	// Who is calling comes first. Nothing of the body is read for a caller
	// that is not Plainforge.
	switch caller.Check(r.Context(), c.reviewer, c.cfg.ReviewTimeout, r.Header.Values("Authorization")) {
	case caller.Allowed:
	case caller.NoToken:
		w.Header().Set("WWW-Authenticate", "Bearer")
		write(w, http.StatusUnauthorized, answer{Status: "refused", Detail: "no bearer token"})
		return
	case caller.BadToken:
		w.Header().Set("WWW-Authenticate", "Bearer")
		write(w, http.StatusUnauthorized, answer{Status: "refused", Detail: "bearer token is not valid"})
		return
	case caller.NotPlainforge:
		write(w, http.StatusForbidden, answer{Status: "refused", Detail: "caller is not Plainforge"})
		return
	case caller.TimedOut:
		write(w, http.StatusServiceUnavailable, answer{Status: "refused", Code: fault.Timeout, Detail: "token check took too long"})
		return
	default:
		write(w, http.StatusServiceUnavailable, answer{Status: "refused", Detail: "token could not be checked"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, push.MaxBody+1))
	if err != nil {
		write(w, http.StatusBadRequest, answer{Status: "refused", Code: fault.Parse, Detail: "body could not be read"})
		return
	}
	decision, err := push.Decide(body)
	if err != nil {
		var f *fault.Fault
		if !errors.As(err, &f) {
			f = fault.New(fault.Parse, "body could not be read")
		}
		status := http.StatusBadRequest
		if f.Code == fault.NeedsSubtool {
			status = http.StatusUnprocessableEntity
		}
		write(w, status, answer{Status: "refused", Code: f.Code, Detail: f.Detail})
		return
	}
	if decision.Ignore != "" {
		write(w, http.StatusOK, answer{Status: "ignored", Reason: decision.Ignore})
		return
	}
	id, started, full := c.start(decision)
	switch {
	case full:
		w.Header().Set("Retry-After", "60")
		write(w, http.StatusServiceUnavailable, answer{Status: "refused", Detail: "too many runs in progress"})
	case started:
		write(w, http.StatusAccepted, answer{Status: "accepted", Run: id})
	default:
		write(w, http.StatusOK, answer{Status: "known", Run: id})
	}
}

func (c *Controller) builderImageFor(b registry.Builder) string {
	if b == registry.BuilderBuildKit {
		return c.cfg.BuildKitImage
	}
	return c.cfg.KoImage
}

// start records a run for d and launches its build pipeline. A notification
// for a run that is already recorded starts nothing, unless that run was
// interrupted: SPEC.md says an interrupted build can safely be retried.
func (c *Controller) start(d push.Decision) (id string, started, full bool) {
	ev := d.Event
	id = ev.RunID()
	spec := buildjob.Spec{
		RunID:         id,
		Target:        d.Target,
		Ref:           ev.Ref,
		Commit:        ev.After,
		BuilderImage:  c.builderImageFor(d.Target.Builder),
		PlainforgeURL: c.cfg.PlainforgeURL,
		ImageRegistry: c.cfg.ImageRegistry,
		CosignSecret:  c.cfg.CosignSecret,
		PriorityClass: c.cfg.PriorityClass,
		Deadline:      c.cfg.JobTimeout,
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ctx.Err() != nil {
		return id, false, true
	}
	if old, ok := c.runs[id]; ok {
		if old.report.State != buildjob.StateInterrupted {
			return id, false, false
		}
		c.forget(id)
	}
	if len(c.order) >= c.cfg.MaxRuns && !c.evict() {
		return id, false, true
	}
	rn := &run{
		report: Report{
			Run:        id,
			Repository: ev.Repository,
			App:        d.Target.Slug,
			Builder:    string(d.Target.Builder),
			Ref:        ev.Ref,
			Commit:     ev.After,
			Job:        spec.Name(),
			State:      StateRunning,
		},
		done: make(chan struct{}),
	}
	c.runs[id] = rn
	c.order = append(c.order, id)

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		result := buildjob.Run(c.ctx, c.cluster, c.verifier, c.pinner, spec)
		c.mu.Lock()
		rn.report.State = result.State
		rn.report.Digest = result.Digest
		rn.report.Code = result.Code
		rn.report.Reason = result.Reason
		c.mu.Unlock()
		close(rn.done)
	}()
	return id, true, false
}

func (c *Controller) evict() bool {
	for _, id := range c.order {
		if c.runs[id].report.State != StateRunning {
			c.forget(id)
			return true
		}
	}
	return false
}

func (c *Controller) forget(id string) {
	delete(c.runs, id)
	for i, other := range c.order {
		if other == id {
			c.order = append(c.order[:i], c.order[i+1:]...)
			return
		}
	}
}

// Report returns the record of a run.
func (c *Controller) Report(id string) (Report, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rn, ok := c.runs[id]
	if !ok {
		return Report{}, false
	}
	return rn.report, true
}

// Done returns a channel that is closed when the run has ended, or nil when
// there is no such run.
func (c *Controller) Done(id string) <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if rn, ok := c.runs[id]; ok {
		return rn.done
	}
	return nil
}

func allow(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	write(w, http.StatusMethodNotAllowed, answer{Status: "refused", Detail: "method not allowed"})
	return false
}

func write(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		status, body = http.StatusInternalServerError, []byte(`{"status":"refused"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}
