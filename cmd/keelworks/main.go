// Command keelworks is the Keelworks build controller.
//
// In the cluster it checks incoming Plainforge webhooks via Kubernetes
// TokenReview, spawns ephemeral ko and rootless BuildKit Jobs in clearedstack,
// verifies cosign-signed digests in Lazarette, and commits the new digest to
// app.yaml in Plainforge with [skip-ci]. With -standin FILE it runs the same
// controller against deterministic in-memory stand-ins.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ClearedStack/keelworks/internal/buildjob"
	"github.com/ClearedStack/keelworks/internal/caller"
	"github.com/ClearedStack/keelworks/internal/controller"
	"github.com/ClearedStack/keelworks/internal/lazarette"
	"github.com/ClearedStack/keelworks/internal/plainforge"
	"github.com/ClearedStack/keelworks/internal/standin"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runWithEnv(ctx, os.Args[1:], os.Getenv, nil); err != nil {
		fmt.Fprintln(os.Stderr, "keelworks:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, ready chan<- string) error {
	return runWithEnv(ctx, args, os.Getenv, ready)
}

func runWithEnv(ctx context.Context, args []string, getenv func(string) string, ready chan<- string) error {
	fs := flag.NewFlagSet("keelworks", flag.ContinueOnError)
	addr := fs.String("addr", ":8080", "address to listen on (env KEELWORKS_ADDR)")
	script := fs.String("standin", "", "run against in-memory stand-ins playing back this script `file` (env KEELWORKS_STANDIN)")

	var cfg controller.Config
	fs.StringVar(&cfg.KoImage, "ko-image", "", "pinned ko builder image in Lazarette (required; env KEELWORKS_KO_IMAGE)")
	fs.StringVar(&cfg.BuildKitImage, "buildkit-image", "", "pinned rootless BuildKit builder image in Lazarette (required; env KEELWORKS_BUILDKIT_IMAGE)")
	fs.StringVar(&cfg.PlainforgeURL, "plainforge-url", "http://plainforge.clearedstack.svc.cluster.local:8080", "cluster-private Plainforge URL (env KEELWORKS_PLAINFORGE_URL)")
	fs.StringVar(&cfg.ImageRegistry, "image-registry", "lazarette.clearedstack.svc.cluster.local:5000", "Lazarette registry host:port (env KEELWORKS_IMAGE_REGISTRY)")
	fs.StringVar(&cfg.CosignSecret, "cosign-secret", "keelworks-cosign", "Secret in clearedstack holding cosign.key for build Jobs (env KEELWORKS_COSIGN_SECRET)")
	fs.StringVar(&cfg.PriorityClass, "priority-class", "", "PriorityClass for ephemeral build Jobs (required; env KEELWORKS_PRIORITY_CLASS)")
	fs.DurationVar(&cfg.JobTimeout, "job-timeout", 0, "how long a build Job may take, for example 10m (required; env KEELWORKS_JOB_TIMEOUT)")
	fs.DurationVar(&cfg.ReviewTimeout, "token-review-timeout", 0, "how long TokenReview may take, for example 5s (required; env KEELWORKS_TOKEN_REVIEW_TIMEOUT)")
	fs.IntVar(&cfg.MaxRuns, "max-runs", 256, "how many runs are remembered (env KEELWORKS_MAX_RUNS)")

	lazaretteURL := fs.String("lazarette-url", "http://lazarette.clearedstack.svc.cluster.local:5000", "cluster-private Lazarette URL (env KEELWORKS_LAZARETTE_URL)")
	cosignPubFile := fs.String("cosign-pub-file", "/var/run/secrets/keelworks/cosign/cosign.pub", "PEM public key file used to verify cosign signatures in Lazarette (env KEELWORKS_COSIGN_PUB_FILE)")
	verifyTimeout := fs.Duration("verify-timeout", 15*time.Second, "how long Lazarette verification may take (env KEELWORKS_VERIFY_TIMEOUT)")
	pinTimeout := fs.Duration("pin-timeout", 30*time.Second, "how long pinning app.yaml in Plainforge may take (env KEELWORKS_PIN_TIMEOUT)")

	kubeURL := fs.String("kube-url", "", "Kubernetes API base URL (defaults to in-cluster; env KEELWORKS_KUBE_URL)")
	kubeCAFile := fs.String("kube-ca-file", "", "Kubernetes CA PEM file (defaults to in-cluster; env KEELWORKS_KUBE_CA_FILE)")
	kubeTokenFile := fs.String("kube-token-file", "", "ServiceAccount token file (defaults to in-cluster; env KEELWORKS_KUBE_TOKEN_FILE)")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected arguments")
	}

	if getenv != nil {
		explicit := make(map[string]bool)
		fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

		applyStringEnv(explicit, getenv, "addr", "KEELWORKS_ADDR", addr)
		applyStringEnv(explicit, getenv, "standin", "KEELWORKS_STANDIN", script)
		applyStringEnv(explicit, getenv, "ko-image", "KEELWORKS_KO_IMAGE", &cfg.KoImage)
		applyStringEnv(explicit, getenv, "buildkit-image", "KEELWORKS_BUILDKIT_IMAGE", &cfg.BuildKitImage)
		applyStringEnv(explicit, getenv, "plainforge-url", "KEELWORKS_PLAINFORGE_URL", &cfg.PlainforgeURL)
		applyStringEnv(explicit, getenv, "lazarette-url", "KEELWORKS_LAZARETTE_URL", lazaretteURL)
		applyStringEnv(explicit, getenv, "image-registry", "KEELWORKS_IMAGE_REGISTRY", &cfg.ImageRegistry)
		applyStringEnv(explicit, getenv, "cosign-secret", "KEELWORKS_COSIGN_SECRET", &cfg.CosignSecret)
		applyStringEnv(explicit, getenv, "cosign-pub-file", "KEELWORKS_COSIGN_PUB_FILE", cosignPubFile)
		applyStringEnv(explicit, getenv, "priority-class", "KEELWORKS_PRIORITY_CLASS", &cfg.PriorityClass)
		applyStringEnv(explicit, getenv, "kube-url", "KEELWORKS_KUBE_URL", kubeURL)
		applyStringEnv(explicit, getenv, "kube-ca-file", "KEELWORKS_KUBE_CA_FILE", kubeCAFile)
		applyStringEnv(explicit, getenv, "kube-token-file", "KEELWORKS_KUBE_TOKEN_FILE", kubeTokenFile)

		for _, d := range []struct {
			flag string
			env  string
			dst  *time.Duration
		}{
			{"job-timeout", "KEELWORKS_JOB_TIMEOUT", &cfg.JobTimeout},
			{"token-review-timeout", "KEELWORKS_TOKEN_REVIEW_TIMEOUT", &cfg.ReviewTimeout},
			{"verify-timeout", "KEELWORKS_VERIFY_TIMEOUT", verifyTimeout},
			{"pin-timeout", "KEELWORKS_PIN_TIMEOUT", pinTimeout},
		} {
			if err := applyDurationEnv(explicit, getenv, d.flag, d.env, d.dst); err != nil {
				return err
			}
		}
		if err := applyIntEnv(explicit, getenv, "max-runs", "KEELWORKS_MAX_RUNS", &cfg.MaxRuns); err != nil {
			return err
		}
	}

	ctl, err := buildController(cfg, *script, clusterOptions{
		lazaretteURL:  *lazaretteURL,
		cosignPubFile: *cosignPubFile,
		verifyTimeout: *verifyTimeout,
		pinTimeout:    *pinTimeout,
		kubeURL:       *kubeURL,
		kubeCAFile:    *kubeCAFile,
		kubeTokenFile: *kubeTokenFile,
	})
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		ctl.Close()
		return errors.New("could not bind listener address")
	}
	if ready != nil {
		ready <- ln.Addr().String()
	}

	srv := &http.Server{
		Handler:           ctl,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}
	failed := make(chan error, 1)
	go func() { failed <- srv.Serve(ln) }()

	select {
	case err := <-failed:
		ctl.Close()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return errors.New("the listener stopped")
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	ctl.Close()
	return nil
}

func applyStringEnv(explicit map[string]bool, getenv func(string) string, flagName, envName string, dst *string) {
	if explicit[flagName] {
		return
	}
	if v := strings.TrimSpace(getenv(envName)); v != "" {
		*dst = v
	}
}

func applyDurationEnv(explicit map[string]bool, getenv func(string) string, flagName, envName string, dst *time.Duration) error {
	if explicit[flagName] {
		return nil
	}
	v := strings.TrimSpace(getenv(envName))
	if v == "" {
		return nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return errors.New("invalid duration in " + envName)
	}
	*dst = d
	return nil
}

func applyIntEnv(explicit map[string]bool, getenv func(string) string, flagName, envName string, dst *int) error {
	if explicit[flagName] {
		return nil
	}
	v := strings.TrimSpace(getenv(envName))
	if v == "" {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return errors.New("invalid integer in " + envName)
	}
	*dst = n
	return nil
}

type clusterOptions struct {
	lazaretteURL  string
	cosignPubFile string
	verifyTimeout time.Duration
	pinTimeout    time.Duration
	kubeURL       string
	kubeCAFile    string
	kubeTokenFile string
}

func buildController(cfg controller.Config, scriptPath string, opts clusterOptions) (*controller.Controller, error) {
	if scriptPath != "" {
		data, err := os.ReadFile(scriptPath)
		if err != nil {
			return nil, errors.New("the stand-in script could not be read")
		}
		parsed, err := standin.ParseScript(data)
		if err != nil {
			return nil, err
		}
		laz := standin.NewLazarette()
		return controller.New(
			cfg,
			standin.NewCluster(parsed, laz),
			standin.NewReviewer(parsed),
			laz,
			standin.NewForgeStore(parsed),
		)
	}

	baseURL, kubeClient, tokenPath, err := resolveKube(opts)
	if err != nil {
		return nil, err
	}
	keySource := lazarette.FilePublicKey(opts.cosignPubFile)
	if _, err := keySource(); err != nil {
		return nil, err
	}

	internalHTTP := &http.Client{
		Transport: &http.Transport{
			Proxy:               nil,
			MaxIdleConns:        4,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     90 * time.Second,
		},
	}
	verifier, err := lazarette.NewDynamicOCIVerifier(opts.lazaretteURL, internalHTTP, keySource, opts.verifyTimeout)
	if err != nil {
		return nil, err
	}
	pinner, err := plainforge.NewSmartHTTPPinner(cfg.PlainforgeURL, internalHTTP, opts.pinTimeout)
	if err != nil {
		return nil, err
	}

	cred := caller.FileCredential(tokenPath)
	apiCluster, err := buildjob.NewAPICluster(baseURL, kubeClient, cred)
	if err != nil {
		return nil, err
	}
	reviewer := caller.NewAPIReviewer(baseURL, kubeClient, cred)
	return controller.New(cfg, apiCluster, reviewer, verifier, pinner)
}

func resolveKube(opts clusterOptions) (string, *http.Client, string, error) {
	if opts.kubeURL == "" && opts.kubeCAFile == "" && opts.kubeTokenFile == "" {
		return buildjob.InClusterConfig()
	}
	if opts.kubeURL == "" || opts.kubeCAFile == "" || opts.kubeTokenFile == "" {
		return "", nil, "", errors.New("-kube-url, -kube-ca-file and -kube-token-file must all be set together")
	}
	client, err := buildjob.TLSClient(opts.kubeCAFile)
	if err != nil {
		return "", nil, "", err
	}
	return opts.kubeURL, client, opts.kubeTokenFile, nil
}
