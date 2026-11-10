package main

import (
	"context"
	"flag"
	"fmt"
	"io"
)

func runReviewPreviewCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("prdash review-preview", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "Repository directory")
	repo := fs.String("repo", "", "Repository owner/name")
	state := fs.String("state", "", "State file")
	number := fs.Int("pr", 0, "Pull request number")
	focus := fs.String("focus", "", "Additional review focus")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *number <= 0 {
		return fmt.Errorf("-pr must be positive")
	}
	svc := openCLIService(ctx, *repo, *dir, *state)
	plan, err := svc.PreviewReview(ctx, *number, *focus)
	if err != nil {
		return err
	}
	return writePrettyJSON(stdout, plan)
}
