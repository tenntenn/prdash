package main

import (
	"context"
	"encoding/json"
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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tenntenn/prdash/prdash"
	"github.com/tenntenn/prdash/skills"
)

const (
	version         = "0.1.0"
	exitWaitTimeout = 2
)

var errWaitTimeout = errors.New("timed out waiting for pending instructions")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "prdash:", err)
		if errors.Is(err, errWaitTimeout) {
			os.Exit(exitWaitTimeout)
		}
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "register":
			return runRegisterCmd(ctx, args[1:], stdin, stdout, stderr)
		case "assign":
			return runAssignCmd(ctx, args[1:], stdout, stderr)
		case "review":
			return runReviewCmd(ctx, args[1:], stdin, stdout, stderr)
		case "config":
			return runConfigCmd(ctx, args[1:], stdout, stderr)
		case "skill":
			return runSkill(args[1:], stdout, stderr)
		case "status":
			return runStatusCmd(ctx, args[1:], stdout, stderr)
		case "sync":
			return runSyncCmd(ctx, args[1:], stdout, stderr)
		case "instruction":
			return runInstructionCmd(ctx, args[1:], stdout, stderr)
		case "instructions":
			return runInstructionsCmd(ctx, args[1:], stdout, stderr)
		case "wait":
			return runWaitCmd(ctx, args[1:], stdout, stderr)
		case "report":
			return runReportCmd(ctx, args[1:], stdout, stderr)
		case "send":
			return runSendCmd(ctx, args[1:], stdout, stderr)
		}
	}

	fs := flag.NewFlagSet("prdash", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		addr        = fs.String("addr", "127.0.0.1:0", "HTTP listen address (defaults to an automatically assigned free port)")
		repo        = fs.String("repo", "", "Target repository in owner/name format (auto-detected from git remote if omitted)")
		workDir     = fs.String("dir", "", "Target git repository directory (defaults to current working directory)")
		noSync      = fs.Bool("no-sync", false, "Skip automatic git worktree and GitHub PR discovery on startup")
		stateFile   = fs.String("state", "", "JSON state file path (defaults to $XDG_STATE_HOME/prdash/state-<repo>.json; use 'off' for in-memory)")
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

	resolvedRepo, resolvedState := resolveRepoAndState(ctx, *repo, *workDir, *stateFile)
	svc := prdash.NewService(prdash.Options{
		Repository: resolvedRepo,
		WorkDir:    *workDir,
		StateFile:  resolvedState,
		ModelConfig: &prdash.ModelPresetConfig{
			Pine:   *pineModel,
			Bamboo: *bambooModel,
			Plum:   *plumModel,
		},
	})
	if err := svc.StateError(); err != nil {
		return err
	}
	if err := svc.ConfigLoadError(); err != nil {
		return err
	}
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
	if resolvedState != "" {
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

func resolveRepoAndState(ctx context.Context, repoFlag, dirFlag, stateFlag string) (string, string) {
	repoName := strings.TrimSpace(repoFlag)
	if repoName == "" {
		detected := prdash.DetectRepositoryInfo(ctx, dirFlag)
		repoName = detected.FullName
	}
	sf := strings.TrimSpace(stateFlag)
	switch strings.ToLower(sf) {
	case "off", "none", "no":
		return repoName, ""
	case "":
		if defPath, err := prdash.DefaultStateFile(repoName); err == nil {
			return repoName, defPath
		}
		return repoName, ""
	default:
		return repoName, sf
	}
}

func openCLIService(ctx context.Context, repoFlag, dirFlag, stateFlag string) *prdash.Service {
	resolvedRepo, resolvedState := resolveRepoAndState(ctx, repoFlag, dirFlag, stateFlag)
	return prdash.NewService(prdash.Options{
		Repository: resolvedRepo,
		WorkDir:    dirFlag,
		StateFile:  resolvedState,
	})
}

func runStatusCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("prdash status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		repo      = fs.String("repo", "", "Target repository in owner/name format")
		dir       = fs.String("dir", "", "Target git repository directory")
		stateFile = fs.String("state", "", "State file path")
		asJSON    = fs.Bool("json", false, "Print full snapshot as JSON")
	)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	svc := openCLIService(ctx, *repo, *dir, *stateFile)
	if err := svc.StateError(); err != nil {
		return err
	}
	if err := svc.ConfigLoadError(); err != nil {
		return err
	}
	snap := svc.Snapshot()
	if *asJSON {
		return writePrettyJSON(stdout, snap)
	}
	_, err := fmt.Fprintf(
		stdout,
		"repository: %s (default: %s)\nworktrees: %d | sessions: %d | pull requests: %d (waiting_for_human: %d, working: %d, stacks: %d)\n",
		snap.Repository.FullName,
		snap.Repository.DefaultBranch,
		snap.Summary.ManagedWorktrees,
		snap.Summary.ActiveSessions,
		snap.Summary.TotalPRs,
		snap.Summary.WaitingForHuman,
		snap.Summary.AgentWorking,
		snap.Summary.StackedChains,
	)
	return err
}

func runSyncCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("prdash sync", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		repo      = fs.String("repo", "", "Target repository in owner/name format")
		dir       = fs.String("dir", "", "Target git repository directory")
		stateFile = fs.String("state", "", "State file path")
		asJSON    = fs.Bool("json", false, "Print synchronized snapshot as JSON")
	)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	svc := openCLIService(ctx, *repo, *dir, *stateFile)
	if err := svc.StateError(); err != nil {
		return err
	}
	if err := svc.ConfigLoadError(); err != nil {
		return err
	}
	snap, err := svc.SyncRepository(ctx, *dir)
	if err != nil {
		return err
	}
	if *asJSON {
		return writePrettyJSON(stdout, snap)
	}
	_, writeErr := fmt.Fprintf(
		stdout,
		"synced %s: %d worktree(s), %d pull request(s), %d stack(s)\n",
		snap.Repository.FullName,
		len(snap.Worktrees),
		len(snap.PullRequests),
		snap.Summary.StackedChains,
	)
	return writeErr
}

func runInstructionsCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("prdash instructions", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		sessionID = fs.String("session", "", "Filter by session ID (or 'controller')")
		ack       = fs.Bool("ack", false, "Mark returned instructions as handled")
		prNum     = fs.Int("pr", 0, "Optional PR number filter when acknowledging")
		repo      = fs.String("repo", "", "Target repository in owner/name format")
		dir       = fs.String("dir", "", "Target git repository directory")
		stateFile = fs.String("state", "", "State file path")
		asJSON    = fs.Bool("json", false, "Print pending instructions as JSON")
	)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	svc := openCLIService(ctx, *repo, *dir, *stateFile)
	if err := svc.StateError(); err != nil {
		return err
	}
	if err := svc.ConfigLoadError(); err != nil {
		return err
	}
	pending := svc.PendingInstructions(*sessionID)
	if *ack && len(pending) > 0 {
		for _, msg := range pending {
			if *prNum != 0 && msg.PRNumber != *prNum {
				continue
			}
			if _, err := svc.AckInstruction(msg.To, msg.ID); err != nil {
				return err
			}
		}
	}
	if *asJSON {
		return writePrettyJSON(stdout, pending)
	}
	for _, m := range pending {
		if m.PRNumber > 0 {
			_, _ = fmt.Fprintf(stdout, "[%s] %s -> %s (PR #%d): %s\n", m.ID, m.From, m.To, m.PRNumber, m.Text)
		} else {
			_, _ = fmt.Fprintf(stdout, "[%s] %s -> %s: %s\n", m.ID, m.From, m.To, m.Text)
		}
	}
	return nil
}

func runWaitCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("prdash wait", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		sessionID = fs.String("session", "", "Session ID to wait on (or 'controller')")
		timeout   = fs.Duration("timeout", 0, "Maximum duration to wait (0 waits until interrupted)")
		ack       = fs.Bool("ack", false, "Mark instructions as handled once received")
		repo      = fs.String("repo", "", "Target repository in owner/name format")
		dir       = fs.String("dir", "", "Target git repository directory")
		stateFile = fs.String("state", "", "State file path")
		asJSON    = fs.Bool("json", false, "Print received instructions as JSON")
	)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	svc := openCLIService(ctx, *repo, *dir, *stateFile)
	if err := svc.StateError(); err != nil {
		return err
	}
	if err := svc.ConfigLoadError(); err != nil {
		return err
	}

	waitCtx := ctx
	if *timeout > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		pending := svc.PendingInstructions(*sessionID)
		if len(pending) > 0 {
			if *ack {
				for _, msg := range pending {
					if _, err := svc.AckInstruction(msg.To, msg.ID); err != nil {
						return err
					}
				}
			}
			if *asJSON {
				return writePrettyJSON(stdout, pending)
			}
			for _, m := range pending {
				if m.PRNumber > 0 {
					_, _ = fmt.Fprintf(stdout, "[%s] %s -> %s (PR #%d): %s\n", m.ID, m.From, m.To, m.PRNumber, m.Text)
				} else {
					_, _ = fmt.Fprintf(stdout, "[%s] %s -> %s: %s\n", m.ID, m.From, m.To, m.Text)
				}
			}
			return nil
		}
		select {
		case <-waitCtx.Done():
			if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
				return errWaitTimeout
			}
			return waitCtx.Err()
		case <-ticker.C:
		}
	}
}

func runReportCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("prdash report", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		prNum     = fs.Int("pr", 0, "Pull request number")
		sessionID = fs.String("session", "", "Worker session ID (defaults to PR's assigned session)")
		status    = fs.String("status", string(prdash.StatusWaitingForHuman), "Target PR status: waiting_for_human or working")
		message   = fs.String("m", "", "Result summary / report message")
		repo      = fs.String("repo", "", "Target repository in owner/name format")
		dir       = fs.String("dir", "", "Target git repository directory")
		stateFile = fs.String("state", "", "State file path")
		asJSON    = fs.Bool("json", false, "Print updated pull request as JSON")
	)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *prNum <= 0 {
		return errors.New("--pr is required")
	}
	svc := openCLIService(ctx, *repo, *dir, *stateFile)
	if err := svc.StateError(); err != nil {
		return err
	}
	if err := svc.ConfigLoadError(); err != nil {
		return err
	}
	sid := strings.TrimSpace(*sessionID)
	if sid == "" {
		if pr, err := svc.GetPR(*prNum); err == nil {
			sid = pr.SessionID
		}
	}
	pr, err := svc.ReportSessionResult(sid, *prNum, prdash.PRStatus(*status), *message)
	if err != nil {
		return err
	}
	if *asJSON {
		return writePrettyJSON(stdout, pr)
	}
	_, writeErr := fmt.Fprintf(stdout, "PR #%d status=%s (%s)\n", pr.Number, pr.Status, *message)
	return writeErr
}

func runSendCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("prdash send", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		prNum      = fs.Int("pr", 0, "Target pull request number")
		sessionID  = fs.String("session", "", "Target worker session ID")
		controller = fs.Bool("controller", false, "Send instruction to the dashboard controller session")
		author     = fs.String("author", "CLI Operator", "Instruction author name")
		message    = fs.String("m", "", "Instruction text")
		repo       = fs.String("repo", "", "Target repository in owner/name format")
		dir        = fs.String("dir", "", "Target git repository directory")
		stateFile  = fs.String("state", "", "State file path")
		asJSON     = fs.Bool("json", false, "Print updated entity as JSON")
	)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	svc := openCLIService(ctx, *repo, *dir, *stateFile)
	if err := svc.StateError(); err != nil {
		return err
	}
	if err := svc.ConfigLoadError(); err != nil {
		return err
	}
	switch {
	case *prNum > 0:
		pr, err := svc.SendPRMessage(*prNum, *author, *message)
		if err != nil {
			return err
		}
		if *asJSON {
			return writePrettyJSON(stdout, pr)
		}
		_, writeErr := fmt.Fprintf(stdout, "queued instruction for PR #%d (%s)\n", pr.Number, pr.SessionID)
		return writeErr
	case *sessionID != "":
		sess, err := svc.SendDirectSessionInstruction(*sessionID, *message)
		if err != nil {
			return err
		}
		if *asJSON {
			return writePrettyJSON(stdout, sess)
		}
		_, writeErr := fmt.Fprintf(stdout, "queued instruction for session %s\n", sess.ID)
		return writeErr
	case *controller:
		sess, err := svc.SendControllerInstruction(*message)
		if err != nil {
			return err
		}
		if *asJSON {
			return writePrettyJSON(stdout, sess)
		}
		_, writeErr := fmt.Fprintf(stdout, "queued instruction for controller session %s\n", sess.ID)
		return writeErr
	default:
		return errors.New("specify one of --pr, --session, or --controller")
	}
}

func writePrettyJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
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

// runConfigCmd uses the same loader as HTTP/MCP startup without executing work.
func runConfigCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("prdash config", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "Target repository directory")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	cfg, err := prdash.LoadRepoConfig(ctx, *dir)
	if err != nil {
		return err
	}
	return writePrettyJSON(stdout, cfg)
}
