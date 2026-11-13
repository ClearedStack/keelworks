// Package testjob describes the temporary Job that runs a repository's
// tests, and runs it on a cluster behind a small interface.
package testjob

import (
	"encoding/json"
	"time"
)

// Namespace is where Keelworks and its Jobs run. It comes from component.json.
const Namespace = "clearedstack"

// Environment variables that tell the test image what to test.
const (
	EnvRepository = "KEELWORKS_REPOSITORY"
	EnvRef        = "KEELWORKS_REF"
	EnvCommit     = "KEELWORKS_COMMIT"
)

// Spec is everything that decides the Job. Two equal Specs render the same
// bytes.
type Spec struct {
	RunID         string
	Repository    string
	Ref           string
	Commit        string
	Image         string        // the test image; it fetches the commit and runs the tests
	PriorityClass string        // the low PriorityClass for build Jobs
	Deadline      time.Duration // how long the Job may run
}

// Name is the Job's name. It is a valid DNS label of at most 60 characters.
func (s Spec) Name() string { return "keelworks-test-" + s.RunID }

type manifest struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   metadata `json:"metadata"`
	Spec       jobSpec  `json:"spec"`
}

type metadata struct {
	Name      string            `json:"name,omitempty"`
	Namespace string            `json:"namespace,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

type jobSpec struct {
	ActiveDeadlineSeconds int64       `json:"activeDeadlineSeconds"`
	BackoffLimit          int         `json:"backoffLimit"`
	Template              podTemplate `json:"template"`
}

type podTemplate struct {
	Metadata metadata `json:"metadata"`
	Spec     podSpec  `json:"spec"`
}

type podSpec struct {
	AutomountServiceAccountToken bool               `json:"automountServiceAccountToken"`
	Containers                   []container        `json:"containers"`
	PriorityClassName            string             `json:"priorityClassName"`
	RestartPolicy                string             `json:"restartPolicy"`
	SecurityContext              podSecurityContext `json:"securityContext"`
}

type podSecurityContext struct {
	RunAsNonRoot   bool           `json:"runAsNonRoot"`
	SeccompProfile seccompProfile `json:"seccompProfile"`
}

type seccompProfile struct {
	Type string `json:"type"`
}

type container struct {
	Name            string                   `json:"name"`
	Image           string                   `json:"image"`
	Env             []envVar                 `json:"env"`
	SecurityContext containerSecurityContext `json:"securityContext"`
}

type envVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type containerSecurityContext struct {
	AllowPrivilegeEscalation bool         `json:"allowPrivilegeEscalation"`
	Capabilities             capabilities `json:"capabilities"`
	Privileged               bool         `json:"privileged"`
	RunAsNonRoot             bool         `json:"runAsNonRoot"`
}

type capabilities struct {
	Drop []string `json:"drop"`
}

// Render returns the Kubernetes batch/v1 Job for s as indented JSON ending in
// a newline. The Job runs once, as a non-root user, with every Linux
// capability dropped, no privilege escalation, no volumes and no service
// account token.
func Render(s Spec) []byte {
	seconds := int64((s.Deadline + time.Second - 1) / time.Second)
	labels := map[string]string{
		"app.kubernetes.io/component":  "test",
		"app.kubernetes.io/instance":   s.RunID,
		"app.kubernetes.io/managed-by": "keelworks",
		"app.kubernetes.io/part-of":    s.Repository,
	}
	m := manifest{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Metadata:   metadata{Name: s.Name(), Namespace: Namespace, Labels: labels},
		Spec: jobSpec{
			ActiveDeadlineSeconds: seconds,
			BackoffLimit:          0,
			Template: podTemplate{
				Metadata: metadata{Labels: labels},
				Spec: podSpec{
					AutomountServiceAccountToken: false,
					Containers: []container{{
						Name:  "test",
						Image: s.Image,
						Env: []envVar{
							{Name: EnvRepository, Value: s.Repository},
							{Name: EnvRef, Value: s.Ref},
							{Name: EnvCommit, Value: s.Commit},
						},
						SecurityContext: containerSecurityContext{
							AllowPrivilegeEscalation: false,
							Capabilities:             capabilities{Drop: []string{"ALL"}},
							Privileged:               false,
							RunAsNonRoot:             true,
						},
					}},
					PriorityClassName: s.PriorityClass,
					RestartPolicy:     "Never",
					SecurityContext: podSecurityContext{
						RunAsNonRoot:   true,
						SeccompProfile: seccompProfile{Type: "RuntimeDefault"},
					},
				},
			},
		},
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		panic("testjob: manifest cannot be encoded") // unreachable: plain structs
	}
	return append(out, '\n')
}
