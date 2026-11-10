package prdash

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"
)

var ErrReviewerUnavailable = errors.New("reviewer unavailable")

// ReviewRuntime is an explicitly bound live review execution endpoint. It must
// honor the requested target, model and instructions or fail without substituting.
// AppliedModel is reported only after that model was used for this request.
type ReviewRuntime interface {
	RunReview(context.Context, SBNNReviewRequest) (SBNNReviewResult, error)
}

// ReviewRuntimeBinding is process-local: persisted session labels are not proof
// of a connection. SupportedModels lists explicit models the runtime can honor.
type ReviewRuntimeBinding struct {
	Provider        string
	Runtime         ReviewRuntime
	SupportedModels []string
	Dedicated       bool
}

// ReviewExecution carries both requested policy and confirmed execution facts.
// Before execution AppliedModel and ActualReviewer are empty.
type ReviewExecution struct {
	PreviewKey              string          `json:"preview_key"`
	RequestID               string          `json:"request_id,omitempty"`
	PRNumber                int             `json:"pr_number"`
	TargetCommit            string          `json:"target_commit"`
	BaseBranch              string          `json:"base_branch"`
	HeadBranch              string          `json:"head_branch"`
	TargetSource            string          `json:"target_source"`
	Agent                   string          `json:"agent"`
	SessionRole             string          `json:"session_role"`
	ReviewerSessionID       string          `json:"reviewer_session_id,omitempty"`
	ImplementationSessionID string          `json:"implementation_session_id,omitempty"`
	WorktreeID              string          `json:"worktree_id,omitempty"`
	RequestedModel          string          `json:"requested_model,omitempty"`
	AppliedModel            string          `json:"applied_model,omitempty"`
	ActualReviewer          string          `json:"actual_reviewer,omitempty"`
	Instructions            string          `json:"instructions,omitempty"`
	Focus                   string          `json:"focus,omitempty"`
	Status                  string          `json:"status"`
	Error                   string          `json:"error,omitempty"`
	Config                  EffectiveConfig `json:"config"`
}

func cloneReviewExecution(in ReviewExecution) ReviewExecution {
	out := in
	out.Config = cloneEffectiveConfig(in.Config)
	return out
}

// PreviewReview resolves portable roles against explicit live bindings. It does
// not dispatch work or apply a runtime model setting.
func (s *Service) PreviewReview(ctx context.Context, number int, focus string) (ReviewExecution, error) {
	s.mu.Lock()
	s.reloadStateFileIfChangedLocked()
	if s.configErr != nil {
		err := s.configErr
		s.mu.Unlock()
		return ReviewExecution{}, err
	}
	pr, _ := s.findPRLocked(number)
	if pr == nil {
		s.mu.Unlock()
		return ReviewExecution{}, fmt.Errorf("%w: #%d", ErrPRNotFound, number)
	}
	plan, err := s.resolveReviewLocked(*pr, focus)
	dir, branch := s.workDir, pr.Branch
	s.mu.Unlock()
	if err != nil {
		return plan, err
	}
	if plan.TargetCommit == "" {
		sha, gitErr := runGit(ctx, dir, "rev-parse", "--verify", "--end-of-options", branch+"^{commit}")
		if gitErr != nil {
			return plan, fmt.Errorf("%w: cannot resolve review target commit for %q", ErrReviewerUnavailable, branch)
		}
		plan.TargetCommit = strings.TrimSpace(sha)
		plan.TargetSource = "git"
	}
	if decoded, err := hex.DecodeString(plan.TargetCommit); err != nil || (len(decoded) != 20 && len(decoded) != 32) {
		return plan, fmt.Errorf("%w: target commit must be a full Git object ID", ErrReviewerUnavailable)
	}
	encoded, _ := json.Marshal(plan)
	sum := sha256.Sum256(encoded)
	plan.PreviewKey = hex.EncodeToString(sum[:])
	return plan, nil
}

func (s *Service) resolveReviewLocked(pr PullRequest, focus string) (ReviewExecution, error) {
	cfg := cloneEffectiveConfig(s.config)
	settings := cfg.Config.Review
	plan := ReviewExecution{PRNumber: pr.Number, BaseBranch: pr.BaseBranch, HeadBranch: pr.Branch, Agent: settings.Agent, SessionRole: settings.Session, ImplementationSessionID: pr.SessionID, WorktreeID: pr.WorktreeID, RequestedModel: settings.Model, Instructions: cfg.Instructions["review.instructions_file"], Focus: strings.TrimSpace(focus), Status: "preview", Config: cfg}
	if settings.ModelTier != "" {
		plan.RequestedModel = s.modelConfig.Resolve(settings.ModelTier)
	}
	for _, wt := range s.worktrees {
		if wt.ID == pr.WorktreeID && wt.HeadSHA != "" {
			plan.TargetCommit = wt.HeadSHA
			plan.TargetSource = "worktree_snapshot"
			break
		}
	}
	fail := func(reason string) (ReviewExecution, error) {
		return plan, fmt.Errorf("%w: %s", ErrReviewerUnavailable, reason)
	}
	if settings.Agent == "sbnn" {
		if checker, ok := s.reviewer.(interface{ CheckReviewAvailability() error }); ok {
			if err := checker.CheckReviewAvailability(); err != nil {
				return fail(err.Error())
			}
		}
		if settings.Session != "separate" {
			return fail("sbnn supports only the separate tool role")
		}
		if plan.RequestedModel != "" || plan.Instructions != "" {
			return fail("sbnn comment synchronization does not support model or instruction overrides; bind a review runtime")
		}
		return plan, nil
	}
	var matches []string
	for id, binding := range s.reviewRuntimes {
		if binding.Runtime == nil || binding.Provider != settings.Agent {
			continue
		}
		sess, _ := s.findSessionLocked(id)
		if sess == nil || sess.Status == "disconnected" || sess.Status == "offline" {
			continue
		}
		switch settings.Session {
		case "working":
			if id != pr.SessionID {
				continue
			}
		case "controller":
			if id != s.controller.ID {
				continue
			}
		case "separate":
			if id == pr.SessionID || id == s.controller.ID {
				continue
			}
		case "dedicated":
			if !binding.Dedicated || id == pr.SessionID {
				continue
			}
		}
		matches = append(matches, id)
	}
	if len(matches) == 0 {
		return fail("no connected runtime matches the requested agent and session role")
	}
	if len(matches) > 1 {
		return fail("multiple live reviewers match the requested role; bind one unambiguous reviewer")
	}
	plan.ReviewerSessionID = matches[0]
	binding := s.reviewRuntimes[matches[0]]
	if plan.RequestedModel != "" && !slices.Contains(binding.SupportedModels, plan.RequestedModel) {
		return fail("selected runtime does not support the requested model")
	}
	return plan, nil
}

func (s *Service) beginReview(ctx context.Context, number int, focus, previewKey string) (SBNNReviewRequest, ReviewRuntime, error) {
	plan, err := s.PreviewReview(ctx, number, focus)
	if err != nil {
		return SBNNReviewRequest{}, nil, err
	}
	if previewKey != "" && plan.PreviewKey != previewKey {
		return SBNNReviewRequest{}, nil, fmt.Errorf("%w: preview changed; inspect the current review plan", ErrReviewerUnavailable)
	}
	s.mu.Lock()
	// Preview and dispatch must use one config and ownership. A caller can retry
	// after a concurrent reload instead of unknowingly executing another policy.
	pr, _ := s.findPRLocked(number)
	if s.configErr != nil {
		err := s.configErr
		s.mu.Unlock()
		return SBNNReviewRequest{}, nil, err
	}
	if pr == nil {
		s.mu.Unlock()
		return SBNNReviewRequest{}, nil, ErrPRNotFound
	}
	if s.config.Revision != plan.Config.Revision || pr.SessionID != plan.ImplementationSessionID || pr.WorktreeID != plan.WorktreeID {
		s.mu.Unlock()
		return SBNNReviewRequest{}, nil, fmt.Errorf("%w: review configuration or ownership changed; preview again", ErrReviewerUnavailable)
	}
	current, resolveErr := s.resolveReviewLocked(*pr, focus)
	if resolveErr != nil {
		s.mu.Unlock()
		return SBNNReviewRequest{}, nil, resolveErr
	}
	if current.RequestedModel != plan.RequestedModel || current.Instructions != plan.Instructions || current.Agent != plan.Agent || current.SessionRole != plan.SessionRole || current.Focus != plan.Focus || current.ReviewerSessionID != plan.ReviewerSessionID || current.BaseBranch != plan.BaseBranch || current.HeadBranch != plan.HeadBranch || (current.TargetCommit != "" && current.TargetCommit != plan.TargetCommit) {
		s.mu.Unlock()
		return SBNNReviewRequest{}, nil, fmt.Errorf("%w: reviewer, policy or target changed; preview again", ErrReviewerUnavailable)
	}
	if s.activeReviews[number] {
		s.mu.Unlock()
		return SBNNReviewRequest{}, nil, fmt.Errorf("%w: a review is already running for this pull request", ErrReviewerUnavailable)
	}
	s.activeReviews[number] = true
	plan.RequestID = "review-" + rand.Text()
	plan.Status = "running"
	var runtime ReviewRuntime = s.reviewer
	if plan.ReviewerSessionID != "" {
		runtime = s.reviewRuntimes[plan.ReviewerSessionID].Runtime
	}
	req := SBNNReviewRequest{AppliedConfig: cloneEffectiveConfig(plan.Config), Execution: cloneReviewExecution(plan), Repository: s.repo.FullName, PRNumber: number, Branch: pr.Branch, BaseBranch: pr.BaseBranch, Worktree: pr.WorktreeID, Focus: plan.Focus}
	event := cloneReviewExecution(plan)
	config := cloneEffectiveConfig(plan.Config)
	pr.Thread = append(pr.Thread, ThreadMessage{ID: s.nextID("msg"), Author: "Dashboard Orchestrator", Role: "controller", Kind: "review_request", Text: fmt.Sprintf("Requested review from %s for commit %s.", plan.Agent, plan.TargetCommit), ReviewExecution: &event, AppliedConfig: &config, CreatedAt: time.Now().UTC()})
	s.dirty = true
	snap := s.snapshotLocked()
	s.mu.Unlock()
	s.broadcast(snap)
	return req, runtime, nil
}

func (s *Service) finishReviewAudit(req SBNNReviewRequest, result SBNNReviewResult, runErr error) error {
	execution := cloneReviewExecution(req.Execution)
	execution.Status = "completed"
	if runErr == nil {
		execution.AppliedModel = result.AppliedModel
		execution.ActualReviewer = execution.ReviewerSessionID
		if execution.ActualReviewer == "" {
			execution.ActualReviewer = execution.Agent
		}
		if execution.RequestedModel != "" && result.AppliedModel != execution.RequestedModel {
			runErr = fmt.Errorf("%w: runtime did not confirm the requested model", ErrReviewerUnavailable)
		}
	}
	if runErr != nil {
		execution.Status = "failed"
		execution.Error = runErr.Error()
	}
	s.mu.Lock()
	delete(s.activeReviews, req.PRNumber)
	if pr, _ := s.findPRLocked(req.PRNumber); pr != nil {
		for i := range pr.Thread {
			if event := pr.Thread[i].ReviewExecution; event != nil && event.RequestID == execution.RequestID {
				copy := cloneReviewExecution(execution)
				pr.Thread[i].ReviewExecution = &copy
			}
		}
	}
	s.dirty = true
	snap := s.snapshotLocked()
	s.mu.Unlock()
	s.broadcast(snap)
	return runErr
}

// CheckReviewAvailability verifies the configured local executable before dispatch.
func (c *CLISBNNClient) CheckReviewAvailability() error {
	if _, err := exec.LookPath(c.Bin); err != nil {
		return fmt.Errorf("sbnn executable unavailable: %w", err)
	}
	return nil
}
