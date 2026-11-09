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

	"github.com/tenntenn/prdash/prdash"
)

func main() {
	var (
		addr      = flag.String("addr", "127.0.0.1:0", "HTTP listen address (defaults to an automatically assigned free port)")
		repo      = flag.String("repo", "acme/nebula-api", "Target repository in owner/name format")
		stateFile = flag.String("state", "", "Optional JSON file path to synchronize state between Web and MCP processes")
		mcpMode   = flag.Bool("mcp", false, "Run as a Model Context Protocol (MCP) server over stdio")
		pineModel = flag.String("model-pine", "claude-opus-4-6", "Model identifier for the Pine (High) preset")
		bambooMod = flag.String("model-bamboo", "claude-sonnet-4-6", "Model identifier for the Bamboo (Standard) preset")
		plumModel = flag.String("model-plum", "claude-haiku-4-5", "Model identifier for the Plum (Fast) preset")
	)
	flag.Parse()

	svc := prdash.NewService(prdash.Options{
		Repository: *repo,
		StateFile:  *stateFile,
		ModelConfig: &prdash.ModelPresetConfig{
			Pine:   *pineModel,
			Bamboo: *bambooMod,
			Plum:   *plumModel,
		},
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *mcpMode {
		if err := prdash.RunMCP(ctx, svc, os.Stdin, os.Stdout); err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintf(os.Stderr, "prdash mcp error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *stateFile != "" {
		go svc.WatchStateFile(ctx, 250*time.Millisecond)
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to listen on %s: %v\n", *addr, err)
		os.Exit(1)
	}

	url := fmt.Sprintf("http://%s", ln.Addr().String())
	fmt.Printf("prdash listening on %s (repository: %s)\n", url, svc.Snapshot().Repository.FullName)

	srv := &http.Server{
		Handler:           prdash.NewHandler(svc),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if err != nil {
			fmt.Fprintf(os.Stderr, "server error: %v\n", err)
			os.Exit(1)
		}
	}
}
