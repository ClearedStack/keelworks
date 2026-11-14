// Command keelworks-builder is the deterministic entrypoint executed inside
// Keelworks' ephemeral ko and rootless BuildKit Kubernetes Jobs.
//
// It reads KEELWORKS_* environment variables set by internal/buildjob, checks
// out the exact commit from Plainforge over Git smart HTTP, runs the
// repository's tests offline, builds and pushes the OCI image by digest to
// Lazarette (using its built-in pure-stdlib Go OCI publisher by default for Go
// targets, or rootless BuildKit for web targets), signs the pushed digest with
// cosign.key as an OCI v1.1 referrer in Lazarette, and writes the verified
// sha256 digest to /dev/termination-log.
package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ClearedStack/keelworks/internal/buildjob"
	"github.com/ClearedStack/keelworks/internal/lazarette"
	"github.com/ClearedStack/keelworks/internal/plainforge"
	"github.com/ClearedStack/keelworks/internal/registry"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, "keelworks-builder:", err)
		os.Exit(1)
	}
}

type options struct {
	workDir         string
	tmpDir          string
	terminationLog  string
	lazaretteScheme string
	goBin           string
	koBin           string
	npmBin          string
	buildctlBin     string
	checkoutTimeout time.Duration
	publishTimeout  time.Duration
	signTimeout     time.Duration
}

type jobEnv struct {
	target        registry.Target
	ref           string
	commit        string
	plainforgeURL string
	imageRef      string
	registryHost  string
	privKey       *ecdsa.PrivateKey
}

func run(ctx context.Context, args []string, getenv func(string) string) error {
	fs := flag.NewFlagSet("keelworks-builder", flag.ContinueOnError)
	var opts options
	fs.StringVar(&opts.workDir, "work-dir", "/work", "writable scratch directory")
	fs.StringVar(&opts.tmpDir, "tmp-dir", "/tmp", "temporary directory")
	fs.StringVar(&opts.terminationLog, "termination-log", "/dev/termination-log", "path where the published sha256 digest is written")
	fs.StringVar(&opts.lazaretteScheme, "lazarette-scheme", "http", "URL scheme for Lazarette (http or https)")
	fs.StringVar(&opts.goBin, "go-bin", "go", "Go toolchain binary")
	fs.StringVar(&opts.koBin, "ko-bin", "", "optional external ko publisher binary; empty uses built-in Go OCI publisher")
	fs.StringVar(&opts.npmBin, "npm-bin", "npm", "npm binary for web test suites")
	fs.StringVar(&opts.buildctlBin, "buildctl-bin", "buildctl-daemonless.sh", "rootless BuildKit wrapper binary")
	fs.DurationVar(&opts.checkoutTimeout, "checkout-timeout", 2*time.Minute, "how long Plainforge source checkout may take")
	fs.DurationVar(&opts.publishTimeout, "publish-timeout", 2*time.Minute, "how long Lazarette OCI image upload may take")
	fs.DurationVar(&opts.signTimeout, "sign-timeout", 30*time.Second, "how long Lazarette signature upload may take")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	if opts.lazaretteScheme != "http" && opts.lazaretteScheme != "https" {
		return errors.New("-lazarette-scheme must be http or https")
	}

	env, err := parseJobEnv(getenv)
	if err != nil {
		return err
	}

	internalHTTP := &http.Client{
		Transport: &http.Transport{
			Proxy:               nil,
			MaxIdleConns:        4,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	lazaretteURL := opts.lazaretteScheme + "://" + env.registryHost
	publisher, err := lazarette.NewOCIPublisher(lazaretteURL, internalHTTP, opts.publishTimeout)
	if err != nil {
		return err
	}
	signer, err := lazarette.NewOCISigner(lazaretteURL, internalHTTP, env.privKey, opts.signTimeout)
	if err != nil {
		return err
	}
	forge, err := plainforge.NewSmartHTTPPinner(env.plainforgeURL, internalHTTP, opts.checkoutTimeout)
	if err != nil {
		return err
	}

	srcDir := filepath.Join(opts.workDir, "src")
	if err := os.RemoveAll(srcDir); err != nil {
		return errors.New("could not clean source directory")
	}
	if err := forge.Checkout(ctx, env.target.Repository, env.ref, env.commit, srcDir); err != nil {
		return err
	}

	var digest string
	switch env.target.Builder {
	case registry.BuilderKo:
		digest, err = buildWithKo(ctx, opts, env, srcDir, publisher)
	case registry.BuilderBuildKit:
		digest, err = buildWithBuildKit(ctx, opts, env, srcDir)
	default:
		return errors.New("no builder rule for target")
	}
	if err != nil {
		return err
	}

	if _, err := signer.SignAndPush(ctx, env.imageRef, env.target.Image, digest); err != nil {
		return err
	}

	if err := os.WriteFile(opts.terminationLog, []byte(digest+"\n"), 0o600); err != nil {
		return errors.New("could not write digest to termination log")
	}
	return nil
}

func parseJobEnv(getenv func(string) string) (jobEnv, error) {
	var out jobEnv
	repo := getenv(buildjob.EnvRepository)
	target, ok := registry.ByRepository(repo)
	if !ok {
		return out, errors.New("unfamiliar KEELWORKS_REPOSITORY")
	}
	if getenv(buildjob.EnvApp) != target.Slug {
		return out, errors.New("KEELWORKS_APP does not match repository target")
	}
	if getenv(buildjob.EnvBuilder) != string(target.Builder) {
		return out, errors.New("KEELWORKS_BUILDER does not match repository target")
	}

	ref := getenv(buildjob.EnvRef)
	if ref != "refs/heads/main" && !strings.HasPrefix(ref, "refs/tags/") {
		return out, errors.New("KEELWORKS_REF is neither refs/heads/main nor a tag")
	}
	if strings.ContainsAny(ref, " \t\r\n\x00") {
		return out, errors.New("KEELWORKS_REF contains forbidden characters")
	}

	commit := getenv(buildjob.EnvCommit)
	if !validHexCommit(commit) {
		return out, errors.New("KEELWORKS_COMMIT is not a 40-character lowercase hex SHA")
	}

	pfURL := strings.TrimRight(getenv(buildjob.EnvPlainforgeURL), "/")
	if pfURL == "" {
		return out, errors.New("KEELWORKS_PLAINFORGE_URL is empty")
	}

	imgRef := getenv(buildjob.EnvImage)
	regHost, imgName, hasSlash := strings.Cut(imgRef, "/")
	if !hasSlash || regHost == "" || strings.ContainsAny(regHost, " \t\r\n@/") || imgName != target.Image {
		return out, errors.New("KEELWORKS_IMAGE does not match repository target")
	}

	keyPath := getenv(buildjob.EnvCosignKey)
	if keyPath == "" {
		return out, errors.New("KEELWORKS_COSIGN_KEY is empty")
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return out, errors.New("cosign private key file could not be read")
	}
	privKey, err := lazarette.ParsePrivateKey(keyPEM)
	if err != nil {
		return out, err
	}

	return jobEnv{
		target:        target,
		ref:           ref,
		commit:        commit,
		plainforgeURL: pfURL,
		imageRef:      imgRef,
		registryHost:  regHost,
		privKey:       privKey,
	}, nil
}

func buildWithKo(ctx context.Context, opts options, env jobEnv, srcDir string, publisher *lazarette.OCIPublisher) (string, error) {
	goCache := filepath.Join(opts.workDir, "gocache")
	goPath := filepath.Join(opts.workDir, "gopath")
	for _, dir := range []string{goCache, goPath, opts.tmpDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", errors.New("could not prepare Go scratch directories")
		}
	}

	cmdEnv := cleanBuildEnv(opts, []string{
		"CGO_ENABLED=0",
		"GOCACHE=" + goCache,
		"GOPATH=" + goPath,
		"GOPROXY=off",
		"GOSUMDB=off",
		"GOTOOLCHAIN=local",
		"GOFLAGS=-mod=readonly",
		"KO_DOCKER_REPO=" + env.imageRef,
	})

	if _, err := runCmd(ctx, srcDir, cmdEnv, opts.goBin, "vet", "./..."); err != nil {
		return "", errors.New("go vet failed")
	}
	if _, err := runCmd(ctx, srcDir, cmdEnv, opts.goBin, "test", "./..."); err != nil {
		return "", errors.New("golden or unit tests failed")
	}

	mainPkg, baseImg, err := resolveKoConfig(srcDir, env.target.Slug)
	if err != nil {
		return "", err
	}

	if opts.koBin != "" {
		koArgs := []string{"publish", "--bare", "--platform=linux/arm64"}
		if opts.lazaretteScheme == "http" {
			koArgs = append(koArgs, "--insecure-registry")
		}
		koArgs = append(koArgs, mainPkg)

		stdout, err := runCmd(ctx, srcDir, cmdEnv, opts.koBin, koArgs...)
		if err != nil {
			return "", errors.New("ko publish failed")
		}
		return parseKoDigest(stdout, env.imageRef)
	}

	binDir := filepath.Join(opts.workDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", errors.New("could not prepare binary output directory")
	}
	binPath := filepath.Join(binDir, env.target.Slug)
	_ = os.Remove(binPath)

	buildEnv := append(append([]string(nil), cmdEnv...), "GOOS=linux", "GOARCH=arm64")
	if _, err := runCmd(ctx, srcDir, buildEnv, opts.goBin, "build", "-trimpath", "-ldflags=-s -w", "-o", binPath, mainPkg); err != nil {
		return "", errors.New("go build failed")
	}
	binBytes, err := os.ReadFile(binPath)
	if err != nil || len(binBytes) == 0 {
		return "", errors.New("compiled Go binary could not be read")
	}
	return publisher.PublishBinary(ctx, lazarette.ImageSpec{
		AppSlug:     env.target.Slug,
		Binary:      binBytes,
		TargetImage: env.target.Image,
		BaseImage:   baseImg,
		Arch:        "arm64",
	})
}

func buildWithBuildKit(ctx context.Context, opts options, env jobEnv, srcDir string) (string, error) {
	if err := os.MkdirAll(opts.tmpDir, 0o755); err != nil {
		return "", errors.New("could not prepare BuildKit scratch directory")
	}
	cmdEnv := cleanBuildEnv(opts, []string{
		"CI=true",
		"npm_config_offline=true",
		"npm_config_audit=false",
		"npm_config_fund=false",
	})

	hasTests, err := hasNPMTestScript(filepath.Join(srcDir, "package.json"))
	if err != nil {
		return "", err
	}
	if hasTests {
		if _, err := runCmd(ctx, srcDir, cmdEnv, opts.npmBin, "test", "--offline"); err != nil {
			return "", errors.New("web test suite failed")
		}
	}

	containerfile := ""
	for _, candidate := range []string{"Containerfile", "Dockerfile"} {
		if st, err := os.Stat(filepath.Join(srcDir, candidate)); err == nil && st.Mode().IsRegular() {
			containerfile = candidate
			break
		}
	}
	if containerfile == "" {
		return "", errors.New("web repository has no Containerfile")
	}

	metaPath := filepath.Join(opts.workDir, "buildkit-metadata.json")
	_ = os.Remove(metaPath)

	outputSpec := "type=image,name=" + env.imageRef + ",push=true,push-by-digest=true,oci-mediatypes=true"
	if opts.lazaretteScheme == "http" {
		outputSpec += ",registry.insecure=true"
	}

	args := []string{
		"build",
		"--frontend", "dockerfile.v0",
		"--local", "context=" + srcDir,
		"--local", "dockerfile=" + srcDir,
		"--opt", "filename=" + containerfile,
		"--opt", "platform=linux/arm64",
		"--output", outputSpec,
		"--metadata-file", metaPath,
	}
	if _, err := runCmd(ctx, srcDir, cmdEnv, opts.buildctlBin, args...); err != nil {
		return "", errors.New("rootless BuildKit build failed")
	}

	metaRaw, err := os.ReadFile(metaPath)
	if err != nil {
		return "", errors.New("BuildKit metadata file was not written")
	}
	var meta map[string]any
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		return "", errors.New("BuildKit metadata file is malformed")
	}
	dgst, _ := meta["containerimage.digest"].(string)
	if !registry.ValidDigest(dgst) {
		return "", errors.New("BuildKit did not report a valid sha256 digest")
	}
	return dgst, nil
}

func cleanBuildEnv(opts options, extra []string) []string {
	pathVal := os.Getenv("PATH")
	if pathVal == "" {
		pathVal = "/usr/local/bin:/usr/bin:/bin"
	}
	base := []string{
		"PATH=" + pathVal,
		"HOME=" + opts.workDir,
		"TMPDIR=" + opts.tmpDir,
		"LANG=C.UTF-8",
	}
	return append(base, extra...)
}

func runCmd(ctx context.Context, dir string, env []string, bin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return stdout.String(), nil
}

func resolveKoConfig(srcDir, appSlug string) (string, string, error) {
	var baseImg string
	koYAML, err := os.ReadFile(filepath.Join(srcDir, ".ko.yaml"))
	if err == nil {
		pkg, base, ok, perr := parseKoConfigFromYAML(string(koYAML), appSlug)
		if perr != nil {
			return "", "", perr
		}
		baseImg = base
		if ok {
			return pkg, baseImg, nil
		}
	}
	candidate := filepath.Join(srcDir, "cmd", appSlug)
	if st, err := os.Stat(candidate); err == nil && st.IsDir() {
		return "./cmd/" + appSlug, baseImg, nil
	}
	return ".", baseImg, nil
}

func parseKoMainFromYAML(text, appSlug string) (string, bool, error) {
	pkg, _, ok, err := parseKoConfigFromYAML(text, appSlug)
	return pkg, ok, err
}

func parseKoConfigFromYAML(text, appSlug string) (string, string, bool, error) {
	type buildItem struct {
		id        string
		main      string
		baseImage string
	}
	var items []buildItem
	var defaultBase string
	inBuilds := false
	cur := -1

	for _, rawLine := range strings.Split(text, "\n") {
		line := rawLine
		if idx := strings.IndexByte(line, '#'); idx >= 0 {
			line = line[:idx]
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			if val, ok := strings.CutPrefix(trimmed, "defaultBaseImage:"); ok {
				defaultBase = strings.Trim(strings.TrimSpace(val), `"'`)
				inBuilds = false
				cur = -1
				continue
			}
			inBuilds = trimmed == "builds:"
			cur = -1
			continue
		}
		if !inBuilds {
			continue
		}
		if rest, ok := strings.CutPrefix(trimmed, "- "); ok {
			items = append(items, buildItem{})
			cur = len(items) - 1
			trimmed = strings.TrimSpace(rest)
		} else if trimmed == "-" {
			items = append(items, buildItem{})
			cur = len(items) - 1
			continue
		}
		if cur < 0 {
			continue
		}
		if val, ok := strings.CutPrefix(trimmed, "id:"); ok {
			items[cur].id = strings.Trim(strings.TrimSpace(val), `"'`)
		} else if val, ok := strings.CutPrefix(trimmed, "main:"); ok {
			items[cur].main = strings.Trim(strings.TrimSpace(val), `"'`)
		} else if val, ok := strings.CutPrefix(trimmed, "baseImage:"); ok {
			items[cur].baseImage = strings.Trim(strings.TrimSpace(val), `"'`)
		}
	}

	var chosen *buildItem
	for i := range items {
		if items[i].id == appSlug && items[i].main != "" {
			chosen = &items[i]
			break
		}
	}
	if chosen == nil && len(items) == 1 && items[0].main != "" && (items[0].id == "" || items[0].id == appSlug) {
		chosen = &items[0]
	}
	if chosen == nil {
		return "", defaultBase, false, nil
	}
	if !validMainPackage(chosen.main) {
		return "", "", false, errors.New(".ko.yaml main package path is unsafe")
	}
	baseImg := defaultBase
	if chosen.baseImage != "" {
		baseImg = chosen.baseImage
	}
	return chosen.main, baseImg, true, nil
}

func validMainPackage(pkg string) bool {
	if pkg == "." {
		return true
	}
	if !strings.HasPrefix(pkg, "./") || strings.Contains(pkg, "..") || strings.ContainsAny(pkg, " \t\r\n\x00\\") {
		return false
	}
	clean := filepath.Clean(pkg)
	return clean != ".." && !strings.HasPrefix(clean, "../") && !strings.HasPrefix(clean, "-")
}

func parseKoDigest(stdout, imageRef string) (string, error) {
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if ref, dgst, ok := strings.Cut(line, "@"); ok {
			if ref == imageRef && registry.ValidDigest(dgst) {
				return dgst, nil
			}
			return "", errors.New("ko published an unexpected image reference")
		}
		if registry.ValidDigest(line) {
			return line, nil
		}
	}
	return "", errors.New("ko did not output a valid sha256 digest")
}

func hasNPMTestScript(pkgJSONPath string) (bool, error) {
	data, err := os.ReadFile(pkgJSONPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, errors.New("package.json could not be read")
	}
	var doc struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return false, errors.New("package.json is malformed")
	}
	return strings.TrimSpace(doc.Scripts["test"]) != "", nil
}

func validHexCommit(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
