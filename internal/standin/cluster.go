// Package standin holds in-memory stand-ins for what Keelworks depends on
// and that does not exist yet: the cluster that runs Jobs, and Plainforge.
// Nothing here talks to a network or a disk.
package standin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/clearedstack/keelworks/internal/testjob"
)

// Scripted outcomes. The stand-in runs no tests; it plays back what the
// script says happens to a commit.
const (
	OutcomePass    = "pass"
	OutcomeFail    = "fail"
	OutcomeStall   = "stall"   // never finishes; the deadline ends it
	OutcomePreempt = "preempt" // evicted under memory pressure
)

// Script says how the tests of each commit end. A commit that is not in the
// script has no Job outcome: the stand-in loses the Job rather than invent
// one.
type Script struct {
	Outcomes []ScriptedOutcome `json:"outcomes"`
}

// ScriptedOutcome is one line of a Script.
type ScriptedOutcome struct {
	Repository string `json:"repository"`
	Commit     string `json:"commit"`
	Outcome    string `json:"outcome"`
}

// ParseScript reads a Script from JSON and refuses anything it does not know.
func ParseScript(data []byte) (Script, error) {
	var s Script
	if err := json.Unmarshal(data, &s); err != nil {
		return Script{}, errors.New("stand-in script is not valid JSON")
	}
	for i, o := range s.Outcomes {
		switch o.Outcome {
		case OutcomePass, OutcomeFail, OutcomeStall, OutcomePreempt:
		default:
			return Script{}, fmt.Errorf("stand-in script line %d has an unknown outcome", i+1)
		}
	}
	return s, nil
}

// Cluster is an in-memory testjob.Cluster. It checks every Job against the
// security rules of SPEC.md and refuses one that breaks them.
type Cluster struct {
	mu       sync.Mutex
	outcomes map[string]string // repository@commit -> outcome
	live     map[string]string // namespace/name -> outcome
	created  [][]byte
}

// keep is how many accepted manifests the stand-in remembers.
const keep = 64

// NewCluster returns a Cluster that plays back script.
func NewCluster(script Script) *Cluster {
	c := &Cluster{outcomes: map[string]string{}, live: map[string]string{}}
	for _, o := range script.Outcomes {
		c.outcomes[o.Repository+"@"+o.Commit] = o.Outcome
	}
	return c
}

// job is the part of a Job manifest the stand-in inspects.
type job struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Spec struct {
		ActiveDeadlineSeconds int64 `json:"activeDeadlineSeconds"`
		Template              struct {
			Spec struct {
				PriorityClassName string            `json:"priorityClassName"`
				HostNetwork       bool              `json:"hostNetwork"`
				HostPID           bool              `json:"hostPID"`
				Volumes           []json.RawMessage `json:"volumes"`
				SecurityContext   struct {
					RunAsNonRoot bool `json:"runAsNonRoot"`
				} `json:"securityContext"`
				Containers []struct {
					Image string `json:"image"`
					Env   []struct {
						Name  string `json:"name"`
						Value string `json:"value"`
					} `json:"env"`
					SecurityContext struct {
						AllowPrivilegeEscalation *bool `json:"allowPrivilegeEscalation"`
						Privileged               bool  `json:"privileged"`
						RunAsNonRoot             bool  `json:"runAsNonRoot"`
						Capabilities             struct {
							Add  []string `json:"add"`
							Drop []string `json:"drop"`
						} `json:"capabilities"`
					} `json:"securityContext"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

// Admit checks a manifest against the rules a Keelworks Job must keep. It
// returns the first broken rule, or "" when the Job is allowed.
func Admit(manifest []byte) string {
	var j job
	if err := json.Unmarshal(manifest, &j); err != nil {
		return "manifest is not valid JSON"
	}
	pod := j.Spec.Template.Spec
	switch {
	case j.APIVersion != "batch/v1" || j.Kind != "Job":
		return "not a batch/v1 Job"
	case j.Metadata.Namespace != testjob.Namespace:
		return "wrong namespace"
	case j.Metadata.Name == "":
		return "no name"
	case j.Spec.ActiveDeadlineSeconds <= 0:
		return "no deadline"
	case pod.PriorityClassName == "":
		return "no priority class"
	case pod.HostNetwork || pod.HostPID:
		return "uses the host's namespaces"
	case len(pod.Volumes) != 0:
		return "mounts a volume"
	case !pod.SecurityContext.RunAsNonRoot:
		return "pod may run as root"
	case len(pod.Containers) != 1:
		return "not exactly one container"
	}
	c := pod.Containers[0]
	sc := c.SecurityContext
	switch {
	case c.Image == "":
		return "no image"
	case sc.Privileged:
		return "privileged container"
	case sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation:
		return "privilege escalation allowed"
	case !sc.RunAsNonRoot:
		return "container may run as root"
	case len(sc.Capabilities.Add) != 0:
		return "adds a capability"
	case len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL":
		return "does not drop every capability"
	}
	return ""
}

// Create admits and stores a Job.
func (c *Cluster) Create(ctx context.Context, manifest []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if Admit(manifest) != "" {
		return testjob.ErrRefused
	}
	var j job
	_ = json.Unmarshal(manifest, &j)
	env := map[string]string{}
	for _, e := range j.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	key := j.Metadata.Namespace + "/" + j.Metadata.Name

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.live[key]; exists {
		return testjob.ErrRefused
	}
	c.live[key] = c.outcomes[env[testjob.EnvRepository]+"@"+env[testjob.EnvCommit]]
	if len(c.created) == keep {
		c.created = c.created[1:]
	}
	c.created = append(c.created, append([]byte(nil), manifest...))
	return nil
}

// Wait plays back the scripted outcome of a Job.
func (c *Cluster) Wait(ctx context.Context, namespace, name string) (testjob.Outcome, error) {
	c.mu.Lock()
	outcome, ok := c.live[namespace+"/"+name]
	c.mu.Unlock()
	if !ok {
		return 0, testjob.ErrLost
	}
	switch outcome {
	case OutcomePass:
		return testjob.Passed, nil
	case OutcomeFail:
		return testjob.Failed, nil
	case OutcomePreempt:
		return testjob.Preempted, nil
	case OutcomeStall:
		<-ctx.Done()
		return 0, ctx.Err()
	}
	return 0, testjob.ErrLost
}

// Delete removes a Job. Deleting a Job that is not there is not an error.
func (c *Cluster) Delete(ctx context.Context, namespace, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.live, namespace+"/"+name)
	return nil
}

// Created returns the manifests the cluster accepted, oldest first, the
// last 64 at most.
func (c *Cluster) Created() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.created...)
}

// Live returns how many Jobs exist right now.
func (c *Cluster) Live() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.live)
}
