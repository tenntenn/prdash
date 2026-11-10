package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/tenntenn/prdash/prdash"
	"github.com/tenntenn/prdash/skills"
)

const version = "0.1.0"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "prdash:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) > 0 && args[0] == "skill" {
		return runSkill(args[1:], stdout, stderr)
	}

	fs := flag.NewFlagSet("prdash", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		addr        = fs.String("addr", "127.0.0.1:0", "HTTP listen address (defaults to an automatically assigned free port)")
		repo        = fs.String("repo", "", "Target repository in owner/name format (auto-detected from git remote if omitted)")
		workDir     = fs.String("dir", "", "Target git repository directory (defaults to current working directory)")
		noSync      = fs.Bool("no-sync", false, "Skip automatic git worktree and GitHub PR discovery on startup")
		stateFile   = fs.String("state", "", "Optional JSON file path to synchronize state between Web and MCP processes")
		mcpMode     = fs.Bool("mcp", false, "Run as a Model Context Protocol (MCP) server over stdio")
		pineModel   = fs.String("model-pine", "claude-opus-4-6", "Model identifier for the Pine (High) preset")
		bambooModel = fs.String("model-bamboo", "claude-sonnet-4-6", "Model identifier for the Bamboo (Standard) preset")
		plumModel   = fs.String("model-plum", "claude-haiku-4-5", "Model identifier for the Plum (Fast) preset")
		showVersion = fs.Bool("version", false, "Print version and exit")
	)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	if *showVersion {
		_, err := fmt.Fprintf(stdout, "prdash %s\n", version)
		return err
	}

	svc := prdash.NewService(prdash.Options{
		Repository: *repo,
		WorkDir:    *workDir,
		StateFile:  *stateFile,
		ModelConfig: &prdash.ModelPresetConfig{
			Pine:   *pineModel,
			Bamboo: *bambooModel,
			Plum:   *plumModel,
		},
	})
	if !*noSync && len(svc.Snapshot().PullRequests) == 0 {
		_, _ = svc.SyncRepository(ctx, *workDir)
	}

	if *mcpMode {
		if err := prdash.RunMCP(ctx, svc, stdin, stdout); err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("mcp: %w", err)
		}
		return nil
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *addr, err)
	}

	var wg sync.WaitGroup
	if *stateFile != "" {
		wg.Go(func() {
			svc.WatchStateFile(ctx, 250*time.Millisecond)
		})
	}
	defer wg.Wait()

	url := fmt.Sprintf("http://%s", ln.Addr().String())
	if _, err := fmt.Fprintf(stdout, "prdash listening on %s (repository: %s)\n", url, svc.Snapshot().Repository.FullName); err != nil {
		_ = ln.Close()
		return err
	}

	srv := &http.Server{
		Handler:           prdash.NewHandler(svc),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	wg.Go(func() {
		if serveErr := srv.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- serveErr
		}
		close(errCh)
	})

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case serveErr := <-errCh:
		return serveErr
	}
}

func runSkill(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("prdash skill", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		installDir = fs.String("install", "", "Directory to install the skill into (<dir>/prdash/SKILL.md)")
		force      = fs.Bool("force", false, "Overwrite existing skill files")
		list       = fs.Bool("list", false, "List the files of the embedded skill")
	)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	switch {
	case *list:
		return walkSkillList(stdout)
	case *installDir != "":
		return installSkill(*installDir, *force, stderr)
	default:
		md, err := skills.Markdown()
		if err != nil {
			return err
		}
		_, err = stdout.Write(md)
		return err
	}
}

func walkSkillList(stdout io.Writer) error {
	return fs.WalkDir(skills.FS(), ".", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return walkErr
		}
		_, writeErr := fmt.Fprintln(stdout, path)
		return writeErr
	})
}

func installSkill(dir string, force bool, stderr io.Writer) error {
	root, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	written := 0
	walkErr := fs.WalkDir(skills.FS(), ".", func(path string, d fs.DirEntry, inErr error) error {
		if inErr != nil {
			return inErr
		}
		dst := filepath.Join(root, filepath.FromSlash(path))
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		if !force {
			if _, statErr := os.Stat(dst); statErr == nil {
				return fmt.Errorf("%s already exists (pass --force to overwrite)", dst)
			}
		}
		data, readErr := fs.ReadFile(skills.FS(), path)
		if readErr != nil {
			return readErr
		}
		if writeErr := os.WriteFile(dst, data, 0o644); writeErr != nil {
			return writeErr
		}
		_, _ = fmt.Fprintln(stderr, "prdash: wrote", dst)
		written++
		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	_, _ = fmt.Fprintf(stderr, "prdash: installed the prdash skill (%d file(s)) into %s\n", written, root)
	return nil
}
