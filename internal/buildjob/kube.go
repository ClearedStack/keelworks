package buildjob

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ClearedStack/keelworks/internal/registry"
)

const (
	defaultServiceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"
	defaultPollInterval      = 2 * time.Second
	maxAPIResponseBytes      = 256 << 10
	terminationSignFailed    = "sign-failed"
)

// InClusterConfig returns the base URL, TLS HTTP client, and token file path
// for a pod running inside a Kubernetes cluster.
func InClusterConfig() (string, *http.Client, string, error) {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return "", nil, "", errors.New("not running in a cluster: KUBERNETES_SERVICE_HOST or KUBERNETES_SERVICE_PORT is unset")
	}
	client, err := TLSClient(defaultServiceAccountDir + "/ca.crt")
	if err != nil {
		return "", nil, "", err
	}
	return "https://" + net.JoinHostPort(host, port), client, defaultServiceAccountDir + "/token", nil
}

// TLSClient builds an HTTP client that trusts only the PEM certificates in
// caFile, uses no proxy, and never follows redirects.
func TLSClient(caFile string) (*http.Client, error) {
	pemData, err := os.ReadFile(caFile)
	if err != nil {
		return nil, errors.New("cluster CA file could not be read")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemData) {
		return nil, errors.New("cluster CA file holds no valid certificate")
	}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:               nil,
			TLSClientConfig:     &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			MaxIdleConns:        4,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     90 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

// APICluster implements Cluster against a real Kubernetes API server.
type APICluster struct {
	base         string
	client       *http.Client
	credential   func() (string, error)
	pollInterval time.Duration
}

// NewAPICluster returns an APICluster talking to the Kubernetes API at base.
// credential is called on each request because projected ServiceAccount tokens
// rotate automatically.
func NewAPICluster(base string, client *http.Client, credential func() (string, error)) (*APICluster, error) {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("kubernetes API URL must be http or https with a host")
	}
	if client == nil || credential == nil {
		return nil, errors.New("kubernetes HTTP client and credential are required")
	}
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &APICluster{
		base:         u.String(),
		client:       &c,
		credential:   credential,
		pollInterval: defaultPollInterval,
	}, nil
}

// Create submits a rendered batch/v1 Job manifest to Namespace.
func (a *APICluster) Create(ctx context.Context, manifest []byte) error {
	status, _, err := a.do(ctx, http.MethodPost, "/apis/batch/v1/namespaces/"+Namespace+"/jobs", manifest)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrRefused
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return ErrRefused
	}
	return nil
}

type jobStatusResponse struct {
	Status struct {
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
			Reason string `json:"reason"`
		} `json:"conditions"`
	} `json:"status"`
}

type podListResponse struct {
	Items []struct {
		Status struct {
			Reason            string `json:"reason"`
			ContainerStatuses []struct {
				State struct {
					Terminated *struct {
						ExitCode int    `json:"exitCode"`
						Reason   string `json:"reason"`
						Message  string `json:"message"`
					} `json:"terminated"`
				} `json:"state"`
			} `json:"containerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

// Wait polls the named Job until it reaches Complete or Failed, then inspects
// its pod's termination message to extract the built image digest or failure
// classification.
func (a *APICluster) Wait(ctx context.Context, namespace, name string) (Outcome, error) {
	path := "/apis/batch/v1/namespaces/" + url.PathEscape(namespace) + "/jobs/" + url.PathEscape(name)
	for {
		status, body, err := a.do(ctx, http.MethodGet, path, nil)
		if err != nil {
			if ctx.Err() != nil {
				return Outcome{}, ctx.Err()
			}
			return Outcome{}, ErrLost
		}
		if status != http.StatusOK {
			return Outcome{}, ErrLost
		}
		var js jobStatusResponse
		if json.Unmarshal(body, &js) != nil {
			return Outcome{}, ErrLost
		}
		for _, cond := range js.Status.Conditions {
			if cond.Status != "True" {
				continue
			}
			switch cond.Type {
			case "Complete":
				return a.inspectPod(ctx, namespace, name, true)
			case "Failed":
				switch cond.Reason {
				case "DeadlineExceeded":
					return Outcome{Status: DeadlineExceeded}, nil
				case "Evicted":
					return Outcome{Status: Preempted}, nil
				}
				return a.inspectPod(ctx, namespace, name, false)
			}
		}

		timer := time.NewTimer(a.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Outcome{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (a *APICluster) inspectPod(ctx context.Context, namespace, jobName string, succeeded bool) (Outcome, error) {
	path := "/api/v1/namespaces/" + url.PathEscape(namespace) + "/pods?labelSelector=" + url.QueryEscape("job-name="+jobName)
	status, body, err := a.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		if ctx.Err() != nil {
			return Outcome{}, ctx.Err()
		}
		return Outcome{}, ErrLost
	}
	if status != http.StatusOK {
		return Outcome{}, ErrLost
	}
	var pods podListResponse
	if json.Unmarshal(body, &pods) != nil || len(pods.Items) == 0 {
		return Outcome{}, ErrLost
	}
	for _, pod := range pods.Items {
		if pod.Status.Reason == "Evicted" {
			return Outcome{Status: Preempted}, nil
		}
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.State.Terminated == nil {
				continue
			}
			msg := strings.TrimSpace(cs.State.Terminated.Message)
			if succeeded {
				if registry.ValidDigest(msg) && cs.State.Terminated.ExitCode == 0 {
					return Outcome{Status: Succeeded, Digest: msg}, nil
				}
				return Outcome{Status: SignFailed}, nil
			}
			if msg == terminationSignFailed {
				return Outcome{Status: SignFailed}, nil
			}
			return Outcome{Status: TestFailed}, nil
		}
	}
	if !succeeded {
		return Outcome{Status: TestFailed}, nil
	}
	return Outcome{}, ErrLost
}

// Delete removes the Job and its pods with Background propagation.
func (a *APICluster) Delete(ctx context.Context, namespace, name string) error {
	path := "/apis/batch/v1/namespaces/" + url.PathEscape(namespace) + "/jobs/" + url.PathEscape(name) + "?propagationPolicy=Background"
	status, _, err := a.do(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusAccepted && status != http.StatusNotFound {
		return ErrLost
	}
	return nil
}

func (a *APICluster) do(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	token, err := a.credential()
	if err != nil || token == "" {
		return 0, nil, errors.New("missing service account token")
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseBytes+1))
	if err != nil || len(data) > maxAPIResponseBytes {
		return 0, nil, errors.New("cluster response could not be read")
	}
	return resp.StatusCode, data, nil
}
