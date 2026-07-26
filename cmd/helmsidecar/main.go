// Command helmsidecar runs the Helm sidecar API server. It never execs a
// shell or the helm CLI: it links the Helm v4 Go SDK directly and speaks
// HTTP/JSON over loopback TCP to whichever container shares this pod.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"ainexushub.local/helm-sidecar/internal/api"
	"ainexushub.local/helm-sidecar/internal/helmrunner"
)

// version is set via -ldflags "-X main.version=..." at build time (see
// Containerfile, build.sh, and the VERSION file it reads from); "dev" is
// what a plain `go build` outside that pipeline produces.
var version = "dev"

const (
	defaultPort = "8080"

	// modeSidecar is the original deployment shape: one container among
	// others in a pod, reachable only via loopback from its pod-mates.
	modeSidecar = "sidecar"
	// modeDeployment runs this binary as its own standalone Deployment,
	// fronted by a Kubernetes Service, so it must accept connections
	// arriving on the pod's network interface rather than only loopback.
	modeDeployment = "deployment"
)

func main() {
	os.Exit(run())
}

// run contains main's logic and returns the process exit code, so that
// every deferred cleanup (notably signal.NotifyContext's stop func) always
// runs before the process exits - os.Exit itself never runs a defer.
func run() int {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	// The bind address is never taken from an env var directly - only
	// HELM_SIDECAR_MODE is, and it selects between exactly two known-safe
	// addresses. That preserves the original guarantee (no stray env var
	// can widen the bind address to something unintended) while still
	// letting deployment mode listen on the pod's interface, which it must
	// do for a Kubernetes Service to route to it at all.
	mode := os.Getenv("HELM_SIDECAR_MODE")
	if mode == "" {
		mode = modeSidecar
	}
	var bindHost string
	switch mode {
	case modeSidecar:
		bindHost = "127.0.0.1"
	case modeDeployment:
		bindHost = "0.0.0.0"
	default:
		log.Error("invalid HELM_SIDECAR_MODE", "mode", mode, "want", []string{modeSidecar, modeDeployment})
		return 1
	}

	port := os.Getenv("HELM_SIDECAR_PORT")
	if port == "" {
		port = defaultPort
	}
	addr := fmt.Sprintf("%s:%s", bindHost, port)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	runner := helmrunner.New()
	srv := api.New(runner, log, version)

	log.Info("helm sidecar starting", "version", version, "mode", mode, "addr", addr)
	if err := srv.ListenAndServe(ctx, addr); err != nil {
		log.Error("helm sidecar exited", "error", err)
		return 1
	}
	log.Info("helm sidecar stopped")
	return 0
}
