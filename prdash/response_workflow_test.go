package prdash_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/tenntenn/prdash/prdash"
)

func responseConfig() prdash.EffectiveConfig {
	return prdash.EffectiveConfig{Config: prdash.DefaultRepoConfig(), Instructions: map[string]string{"review_response.instructions_file": "sample-policy"}}
}

func TestResponseFindingPoliciesAndScope(t *testing.T) {
	t.Parallel()
	cfg := responseConfig()
	plan, err := prdash.PlanReviewResponse(cfg, prdash.ResponseRequest{Instruction: "sample-context", Findings: []prdash.ResponseFinding{{ID: "sample-ai", Kind: "ai", Author: "sample-agent"}, {ID: "sample-human", Kind: "human", Author: "sample-person"}, {ID: "sample-question", Kind: "question", Author: "sample-person"}}})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Steps[0].Action != "instruction_review" || plan.Steps[0].Gate != "confirm" {
		t.Fatal("free-form context bypassed review")
	}
	if plan.Findings[0].Policy != "auto" || plan.Findings[1].Policy != "prepare_reply" || plan.Findings[2].Policy != "human_decide" {
		t.Fatalf("policies=%+v", plan.Findings)
	}
	for _, step := range plan.Steps {
		if step.Action == "fix" && !slices.Equal(step.FindingIDs, []string{"sample-ai"}) {
			t.Fatalf("fix scope broadened to nonautomatic findings: %+v", step)
		}
	}
	cfg.Config.ReviewResponse.Push = "off"
	plan, err = prdash.PlanReviewResponse(cfg, prdash.ResponseRequest{RequestedActions: []string{"push", "unsupported"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Conflicts) != 2 {
		t.Fatalf("conflicts=%v", plan.Conflicts)
	}
	if _, err := prdash.NewResponseWorkflow(plan); !errors.Is(err, prdash.ErrResponsePolicyConflict) {
		t.Fatalf("conflicting workflow error=%v", err)
	}
}

func TestResponseCheckpointAndPartialFailure(t *testing.T) {
	t.Parallel()
	plan, err := prdash.PlanReviewResponse(responseConfig(), prdash.ResponseRequest{Findings: []prdash.ResponseFinding{{ID: "sample-ai", Kind: "ai"}}})
	if err != nil {
		t.Fatal(err)
	}
	run, err := prdash.NewResponseWorkflow(plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"fix", "test"} {
		run, err = prdash.BeginResponseStep(run, false)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, step := range run.Steps {
			if step.Status == "running" {
				found = step.Action == action
			}
		}
		if !found {
			t.Fatalf("expected running %s", action)
		}
		run, err = prdash.CompleteResponseStep(run, prdash.ResponseStepResult{Status: "completed", Detail: "sample-success"})
		if err != nil {
			t.Fatal(err)
		}
	}
	run, err = prdash.BeginResponseStep(run, false)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "waiting" || run.Steps[2].Action != "commit" {
		t.Fatalf("commit checkpoint=%+v", run)
	}
	repeated, err := prdash.BeginResponseStep(run, false)
	if err != nil || repeated.Status != "waiting" {
		t.Fatalf("automatic retry bypassed checkpoint: %v %+v", err, repeated)
	}
	run, err = prdash.BeginResponseStep(run, true)
	if err != nil {
		t.Fatal(err)
	}
	run, err = prdash.CompleteResponseStep(run, prdash.ResponseStepResult{Status: "failed", Detail: "sample-commit-failure"})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "failed" || run.Steps[0].Status != "completed" || run.Steps[2].Detail != "sample-commit-failure" {
		t.Fatalf("failure history=%+v", run)
	}
	if _, err := prdash.BeginResponseStep(run, true); !errors.Is(err, prdash.ErrResponseTransition) {
		t.Fatalf("failed workflow continued: %v", err)
	}
}

func TestResponseRereviewTermination(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, stopOn, verdict, want string
		open, rounds                int
	}{{"approved", "approved", "approved", "completed", 1, 1}, {"no comments", "no_open_comments", "commented", "completed", 0, 1}, {"bounded", "approved", "changes_requested", "waiting", 1, 2}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := responseConfig()
			rr := &cfg.Config.ReviewResponse
			rr.Fix, rr.Test, rr.Commit, rr.Push, rr.Reply, rr.Resolve = "off", "off", "off", "off", "off", "off"
			rr.Rereview.Gate = "auto"
			rr.Rereview.MaxRounds = 2
			rr.Rereview.StopOn = tc.stopOn
			plan, err := prdash.PlanReviewResponse(cfg, prdash.ResponseRequest{Findings: []prdash.ResponseFinding{{ID: "sample-ai", Kind: "ai"}}})
			if err != nil {
				t.Fatal(err)
			}
			run, err := prdash.NewResponseWorkflow(plan)
			if err != nil {
				t.Fatal(err)
			}
			for range tc.rounds {
				run, err = prdash.BeginResponseStep(run, false)
				if err != nil || run.Status != "running" {
					t.Fatalf("begin=%+v %v", run, err)
				}
				run, err = prdash.CompleteResponseStep(run, prdash.ResponseStepResult{Status: "completed", Reviewed: true, Verdict: tc.verdict, OpenComments: tc.open})
				if err != nil {
					t.Fatal(err)
				}
			}
			if run.Status != tc.want {
				t.Fatalf("termination=%s want %s", run.Status, tc.want)
			}
			if tc.want == "waiting" {
				run, err = prdash.BeginResponseStep(run, false)
				if err != nil || run.Status != "waiting" {
					t.Fatal("round limit bypassed")
				}
			}
		})
	}
}

func TestResponseFailureKindsAndCopies(t *testing.T) {
	t.Parallel()
	cases := []string{"failed", "disputed", "interrupted"}
	for _, status := range cases {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			cfg := responseConfig()
			plan, err := prdash.PlanReviewResponse(cfg, prdash.ResponseRequest{Findings: []prdash.ResponseFinding{{ID: "sample-ai", Kind: "ai"}}})
			if err != nil {
				t.Fatal(err)
			}
			run, err := prdash.NewResponseWorkflow(plan)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Instructions["review_response.instructions_file"] = "sample-mutated"
			plan.Steps[0].FindingIDs[0] = "sample-mutated"
			if run.Plan.Config.Instructions["review_response.instructions_file"] != "sample-policy" || run.Steps[0].FindingIDs[0] != "sample-ai" {
				t.Fatal("workflow policy aliases caller data")
			}
			run, err = prdash.BeginResponseStep(run, false)
			if err != nil {
				t.Fatal(err)
			}
			run, err = prdash.CompleteResponseStep(run, prdash.ResponseStepResult{Status: status, Detail: "sample-result"})
			if err != nil || run.Status != status {
				t.Fatalf("status=%s error=%v", run.Status, err)
			}
		})
	}
}

func TestResponseOrderValidation(t *testing.T) {
	t.Parallel()
	cases := []string{"review_response:\n  steps: [push, commit, fix, test, reply, resolve, rereview]\n", "review_response:\n  steps: [fix, fix]\n", "review_response:\n  steps: [unknown]\n", "review_response:\n  rereview:\n    gate: unknown\n"}
	for _, config := range cases {
		t.Run(config, func(t *testing.T) {
			t.Parallel()
			root := configRepo(t)
			writeRepoConfig(t, root, config)
			if _, err := prdash.LoadRepoConfig(t.Context(), root); !errors.Is(err, prdash.ErrConfigValidation) {
				t.Fatalf("configuration error=%v", err)
			}
		})
	}
}

func TestResponsePreviewPreservesHumanProvenance(t *testing.T) {
	t.Parallel()
	svc := prdash.NewService(prdash.Options{WorkDir: configRepo(t)})
	if _, err := svc.UpsertPR(prdash.PullRequest{Number: 42, Title: "sample-pr", Thread: []prdash.ThreadMessage{{ID: "sample-human", Kind: "review_comment", Role: "human", Author: "sample-person", Text: "sample-comment"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PreviewResponseWorkflow(42, prdash.ResponseRequest{Findings: []prdash.ResponseFinding{{ID: "sample-human", Kind: "ai"}}}); !errors.Is(err, prdash.ErrResponsePolicyConflict) {
		t.Fatalf("human finding promoted to automatic: %v", err)
	}
	plan, err := svc.PreviewResponseWorkflow(42, prdash.ResponseRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Findings) != 1 || plan.Findings[0].Policy != "prepare_reply" || plan.Findings[0].Author != "sample-person" {
		t.Fatalf("preview classification=%+v", plan.Findings)
	}
}
