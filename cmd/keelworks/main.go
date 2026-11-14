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
	if err := run(ctx, os.Args[1:], nil); err != nil {
		fmt.Fprintln(os.Stderr, "keelworks:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, ready chan<- string) error {
	fs := flag.NewFlagSet("keelworks", flag.ContinueOnError)
	addr := fs.String("addr", ":8080", "address to listen on")
	script := fs.String("standin", "", "run against in-memory stand-ins playing back this script `file`")

	var cfg controller.Config
	fs.StringVar(&cfg.KoImage, "ko-image", "", "pinned ko builder image in Lazarette (required)")
	fs.StringVar(&cfg.BuildKitImage, "buildkit-image", "", "pinned rootless BuildKit builder image in Lazarette (required)")
	fs.StringVar(&cfg.PlainforgeURL, "plainforge-url", "http://plainforge.clearedstack.svc.cluster.local:8080", "cluster-private Plainforge URL")
	fs.StringVar(&cfg.ImageRegistry, "image-registry", "lazarette.clearedstack.svc.cluster.local:5000", "Lazarette registry host:port")
	fs.StringVar(&cfg.CosignSecret, "cosign-secret", "keelworks-cosign", "Secret in clearedstack holding cosign.key for build Jobs")
	fs.StringVar(&cfg.PriorityClass, "priority-class", "", "PriorityClass for ephemeral build Jobs (required)")
	fs.DurationVar(&cfg.JobTimeout, "job-timeout", 0, "how long a build Job may take, for example 10m (required)")
	fs.DurationVar(&cfg.ReviewTimeout, "token-review-timeout", 0, "how long TokenReview may take, for example 5s (required)")
	fs.IntVar(&cfg.MaxRuns, "max-runs", 256, "how many runs are remembered")

	lazaretteURL := fs.String("lazarette-url", "http://lazarette.clearedstack.svc.cluster.local:5000", "cluster-private Lazarette URL")
	cosignPubFile := fs.String("cosign-pub-file", "/var/run/secrets/keelworks/cosign/cosign.pub", "PEM public key file used to verify cosign signatures in Lazarette")
	verifyTimeout := fs.Duration("verify-timeout", 15*time.Second, "how long Lazarette verification may take")
	pinTimeout := fs.Duration("pin-timeout", 30*time.Second, "how long pinning app.yaml in Plainforge may take")

	kubeURL := fs.String("kube-url", "", "Kubernetes API base URL (defaults to in-cluster)")
	kubeCAFile := fs.String("kube-ca-file", "", "Kubernetes CA PEM file (defaults to in-cluster)")
	kubeTokenFile := fs.String("kube-token-file", "", "ServiceAccount token file (defaults to in-cluster)")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected arguments")
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
	pubPEM, err := os.ReadFile(opts.cosignPubFile)
	if err != nil {
		return nil, errors.New("cosign public key file could not be read")
	}
	pubKey, err := lazarette.ParsePublicKey(pubPEM)
	if err != nil {
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
	verifier, err := lazarette.NewOCIVerifier(opts.lazaretteURL, internalHTTP, pubKey, opts.verifyTimeout)
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
