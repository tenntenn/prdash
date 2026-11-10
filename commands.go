package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/tenntenn/prdash/prdash"
)

type commandTarget struct {
	repo, dir, state string
	json             bool
}

func (t *commandTarget) flags(fs *flag.FlagSet) {
	fs.StringVar(&t.repo, "repo", "", "Target repository in owner/name format")
	fs.StringVar(&t.dir, "dir", "", "Target git repository directory")
	fs.StringVar(&t.state, "state", "", "State file path")
	fs.BoolVar(&t.json, "json", false, "Print the resulting entity as JSON")
}

func (t commandTarget) open(ctx context.Context) (*prdash.Service, error) {
	svc := openCLIService(ctx, t.repo, t.dir, t.state)
	if requested := strings.TrimSpace(t.repo); requested != "" {
		if actual := svc.Snapshot().Repository.FullName; actual != requested {
			return nil, fmt.Errorf("state belongs to repository %q, not requested repository %q", actual, requested)
		}
	}
	return svc, nil
}

func runRegisterCmd(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: prdash register worktree|session|pr [--repo owner/name] [--input path|-] [--json]")
	}
	entity := args[0]
	if entity != "worktree" && entity != "session" && entity != "pr" {
		return fmt.Errorf("unknown registration type %q; use worktree, session, or pr", entity)
	}
	fs := flag.NewFlagSet("prdash register "+entity, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var target commandTarget
	target.flags(fs)
	input := fs.String("input", "-", "Read one JSON object from this file, or - for stdin")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	reader := stdin
	if *input != "-" {
		file, err := os.Open(*input)
		if err != nil {
			return fmt.Errorf("open registration input: %w", err)
		}
		defer file.Close()
		reader = file
	}
	var value any
	switch entity {
	case "worktree":
		value = &prdash.Worktree{}
	case "session":
		value = &prdash.Session{}
	case "pr":
		value = &prdash.PullRequest{}
	}
	if err := decodeRegistration(reader, value); err != nil {
		return err
	}
	svc, err := target.open(ctx)
	if err != nil {
		return err
	}
	var result any
	var label string
	switch v := value.(type) {
	case *prdash.Worktree:
		wt, upsertErr := svc.UpsertWorktree(*v)
		if upsertErr != nil {
			return upsertErr
		}
		result, label = wt, wt.ID
	case *prdash.Session:
		session, upsertErr := svc.UpsertSession(*v)
		if upsertErr != nil {
			return upsertErr
		}
		result, label = session, session.ID
	case *prdash.PullRequest:
		pr, upsertErr := svc.UpsertPR(*v)
		if upsertErr != nil {
			return upsertErr
		}
		result, label = pr, fmt.Sprintf("#%d", pr.Number)
	}
	if target.json {
		return writePrettyJSON(stdout, result)
	}
	_, err = fmt.Fprintf(stdout, "registered %s %s\n", entity, label)
	return err
}

func decodeRegistration(reader io.Reader, value any) error {
	dec := json.NewDecoder(reader)
	dec.DisallowUnknownFields()
	if err := dec.Decode(value); err != nil {
		return fmt.Errorf("decode registration JSON: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return fmt.Errorf("decode trailing registration input: %w", err)
		}
		return errors.New("registration input must contain exactly one JSON object")
	}
	return nil
}

func runAssignCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("prdash assign", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var target commandTarget
	target.flags(fs)
	number := fs.Int("pr", 0, "Pull request number")
	worktree := fs.String("worktree", "", "Registered execution worktree ID")
	session := fs.String("session", "", "Registered destination session ID")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *number <= 0 || strings.TrimSpace(*worktree) == "" || strings.TrimSpace(*session) == "" {
		return errors.New("--pr, --worktree, and --session are required")
	}
	svc, err := target.open(ctx)
	if err != nil {
		return err
	}
	pr, err := svc.AssignPR(*number, *worktree, *session)
	if err != nil {
		return err
	}
	if target.json {
		return writePrettyJSON(stdout, pr)
	}
	_, err = fmt.Fprintf(stdout, "assigned PR #%d to worktree %s and session %s\n", pr.Number, pr.WorktreeID, pr.SessionID)
	return err
}

func runReviewCmd(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: prdash review request|address --pr number [-m text | --stdin] [--repo owner/name] [--json]")
	}
	action := args[0]
	if action != "request" && action != "address" {
		return fmt.Errorf("unknown review action %q; use request or address", action)
	}
	fs := flag.NewFlagSet("prdash review "+action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var target commandTarget
	target.flags(fs)
	number := fs.Int("pr", 0, "Pull request number")
	message := fs.String("m", "", "Review focus (request) or response instruction (address)")
	fromStdin := fs.Bool("stdin", false, "Read review focus or response instruction from stdin")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *number <= 0 {
		return errors.New("--pr is required")
	}
	if *fromStdin {
		supplied := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "m" {
				supplied = true
			}
		})
		if supplied {
			return errors.New("-m and --stdin cannot be combined")
		}
		data, err := io.ReadAll(stdin)
		if err != nil {
			return fmt.Errorf("read review input: %w", err)
		}
		*message = strings.TrimSpace(string(data))
		if *message == "" {
			return errors.New("review input from stdin must not be empty")
		}
	}
	svc, err := target.open(ctx)
	if err != nil {
		return err
	}
	var pr prdash.PullRequest
	switch action {
	case "request":
		pr, err = svc.RequestReview(ctx, *number, *message)
	case "address":
		pr, err = svc.AddressReview(*number, *message)
	}
	if err != nil {
		return err
	}
	if target.json {
		return writePrettyJSON(stdout, pr)
	}
	_, err = fmt.Fprintf(stdout, "PR #%d review %s: status=%s review_status=%s\n", pr.Number, action, pr.Status, pr.ReviewStatus)
	return err
}
