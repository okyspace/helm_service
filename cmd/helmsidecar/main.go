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

const defaultPort = "8080"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	// Only the port is configurable, not the bind address: this always
	// binds 127.0.0.1, never 0.0.0.0, so a misconfigured env var can't
	// accidentally expose Helm install/upgrade/uninstall to the pod
	// network or beyond.
	port := os.Getenv("HELM_SIDECAR_PORT")
	if port == "" {
		port = defaultPort
	}
	addr := fmt.Sprintf("127.0.0.1:%s", port)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	runner := helmrunner.New()
	srv := api.New(runner, log)

	log.Info("helm sidecar starting", "addr", addr)
	if err := srv.ListenAndServe(ctx, addr); err != nil {
		log.Error("helm sidecar exited", "error", err)
		os.Exit(1)
	}
	log.Info("helm sidecar stopped")
}
