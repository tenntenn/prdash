package prdash

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrPRNotFound           = errors.New("pull request not found")
	ErrSessionNotFound      = errors.New("session not found")
	ErrNotificationNotFound = errors.New("notification not found")
	ErrInvalidModelTier     = errors.New("invalid model tier preset (must be pine, bamboo, or plum)")
	ErrInvalidPRStatus      = errors.New("invalid pull request status")
	ErrInvalidRoutineAction = errors.New("invalid routine action")
	ErrEmptyMessage         = errors.New("message text must not be empty")
)

// Options configures a new Service instance.
type Options struct {
	Repository  string
	ModelConfig *ModelPresetConfig
	Reviewer    SBNNReviewer
}

// Service manages the repository dashboard state, inter-session communication,
// sbnn review integration, and human notifications.
type Service struct {
	mu          sync.RWMutex
	seq         atomic.Int64
	repo        RepositoryInfo
	modelConfig ModelPresetConfig
	reviewer    SBNNReviewer

	controller    Session
	worktrees     []Worktree
	sessions      []Session
	prs           []PullRequest
	notifications []Notification

	subMu       sync.Mutex
	subscribers map[int64]chan Snapshot
}

// NewService creates a Service initialized with seeded repository data.
func NewService(opts Options) *Service {
	repo := ParseRepositoryInfo(opts.Repository)
	models := DefaultModelPresetConfig()
	if opts.ModelConfig != nil {
		if strings.TrimSpace(opts.ModelConfig.Pine) != "" {
			models.Pine = strings.TrimSpace(opts.ModelConfig.Pine)
		}
		if strings.TrimSpace(opts.ModelConfig.Bamboo) != "" {
			models.Bamboo = strings.TrimSpace(opts.ModelConfig.Bamboo)
		}
		if strings.TrimSpace(opts.ModelConfig.Plum) != "" {
			models.Plum = strings.TrimSpace(opts.ModelConfig.Plum)
		}
	}
	reviewer := opts.Reviewer
	if reviewer == nil {
		reviewer = NewStubSBNNClient()
	}

	controller, worktrees, sessions, prs, notifications := seedData(repo, models)
	svc := &Service{
		repo:          repo,
		modelConfig:   models,
		reviewer:      reviewer,
		controller:    controller,
		worktrees:     worktrees,
		sessions:      sessions,
		prs:           prs,
		notifications: notifications,
		subscribers:   make(map[int64]chan Snapshot),
	}
	svc.seq.Store(1000)
	return svc
}

func (s *Service) nextID(prefix string) string {
	n := s.seq.Add(1)
	return fmt.Sprintf("%s-%d", prefix, n)
}

// Subscribe registers a listener channel that receives updated snapshots whenever state changes.
func (s *Service) Subscribe() (<-chan Snapshot, func()) {
	id := s.seq.Add(1)
	ch := make(chan Snapshot, 8)

	s.subMu.Lock()
	s.subscribers[id] = ch
	s.subMu.Unlock()

	cancel := func() {
		s.subMu.Lock()
		if existing, ok := s.subscribers[id]; ok {
			delete(s.subscribers, id)
			close(existing)
		}
		s.subMu.Unlock()
	}
	return ch, cancel
}

func (s *Service) broadcast(snap Snapshot) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for _, ch := range s.subscribers {
		select {
		case ch <- snap:
		default:
		}
	}
}

// Snapshot returns a deep copy of the current dashboard state.
func (s *Service) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshotLocked()
}

func (s *Service) snapshotLocked() Snapshot {
	controller := cloneSession(s.controller)
	worktrees := slices.Clone(s.worktrees)
	sessions := make([]Session, len(s.sessions))
	for i, sess := range s.sessions {
		sessions[i] = cloneSession(sess)
	}
	prs := make([]PullRequest, len(s.prs))
	for i, pr := range s.prs {
		prs[i] = clonePR(pr)
	}
	notifications := slices.Clone(s.notifications)

	stacks := buildStackGroups(prs)
	summary := DashboardSummary{
		TotalPRs:         len(prs),
		ActiveSessions:   len(sessions),
		ManagedWorktrees: len(worktrees),
	}
	seenStacks := make(map[string]bool)
	for _, pr := range prs {
		switch pr.Status {
		case StatusWaitingForHuman:
			summary.WaitingForHuman++
		case StatusWorking:
			summary.AgentWorking++
		}
		if pr.AutoMode {
			summary.AutoModeEnabled++
		}
		if pr.StackID != "" && !seenStacks[pr.StackID] {
			seenStacks[pr.StackID] = true
			summary.StackedChains++
		}
	}
	for _, n := range notifications {
		if !n.Read {
			summary.UnreadAlerts++
		}
	}

	return Snapshot{
		Repository:        s.repo,
		ModelConfig:       s.modelConfig,
		ControllerSession: controller,
		Sessions:          sessions,
		Worktrees:         worktrees,
		PullRequests:      prs,
		Stacks:            stacks,
		Notifications:     notifications,
		Summary:           summary,
	}
}

func cloneSession(in Session) Session {
	out := in
	out.Messages = slices.Clone(in.Messages)
	return out
}

func clonePR(in PullRequest) PullRequest {
	out := in
	out.Thread = slices.Clone(in.Thread)
	return out
}

func buildStackGroups(prs []PullRequest) []StackGroup {
	var groups []StackGroup
	stackIndex := make(map[string]int)

	stackTitles := map[string]string{
		"stack-oauth":   "OAuth2 & Session Security Stack",
		"stack-storage": "Storage Cursor & Snapshot Retention Stack",
	}

	for _, pr := range prs {
		if pr.StackID == "" {
			groups = append(groups, StackGroup{
				ID:           fmt.Sprintf("single-%d", pr.Number),
				Title:        fmt.Sprintf("PR #%d", pr.Number),
				IsStacked:    false,
				PullRequests: []PullRequest{pr},
			})
			continue
		}
		idx, exists := stackIndex[pr.StackID]
		if !exists {
			title := stackTitles[pr.StackID]
			if title == "" {
				title = "Stacked PR Chain (" + pr.StackID + ")"
			}
			idx = len(groups)
			stackIndex[pr.StackID] = idx
			groups = append(groups, StackGroup{
				ID:        pr.StackID,
				Title:     title,
				IsStacked: true,
			})
		}
		groups[idx].PullRequests = append(groups[idx].PullRequests, pr)
	}

	for i := range groups {
		if groups[i].IsStacked {
			slices.SortFunc(groups[i].PullRequests, func(a, b PullRequest) int {
				return a.StackOrder - b.StackOrder
			})
			size := len(groups[i].PullRequests)
			for j := range groups[i].PullRequests {
				groups[i].PullRequests[j].StackSize = size
			}
		}
	}
	return groups
}

func (s *Service) findPRLocked(number int) (*PullRequest, int) {
	for i := range s.prs {
		if s.prs[i].Number == number {
			return &s.prs[i], i
		}
	}
	return nil, -1
}

func (s *Service) findSessionLocked(id string) (*Session, int) {
	if s.controller.ID == id || id == "controller" {
		return &s.controller, -1
	}
	for i := range s.sessions {
		if s.sessions[i].ID == id {
			return &s.sessions[i], i
		}
	}
	return nil, -1
}

func (s *Service) emitNotificationLocked(pr *PullRequest, fromStatus, toStatus PRStatus, title, msg string, now time.Time) {
	notif := Notification{
		ID:         s.nextID("notif"),
		PRNumber:   pr.Number,
		PRTitle:    pr.Title,
		SessionID:  pr.SessionID,
		Title:      title,
		Message:    msg,
		FromStatus: fromStatus,
		ToStatus:   toStatus,
		Read:       false,
		CreatedAt:  now,
	}
	s.notifications = slices.Insert(s.notifications, 0, notif)
}

func (s *Service) recordInterSessionLocked(from, to, direction string, prNumber int, text string, now time.Time) {
	msg := SessionMessage{
		ID:        s.nextID("smsg"),
		From:      from,
		To:        to,
		Direction: direction,
		PRNumber:  prNumber,
		Text:      text,
		CreatedAt: now,
	}
	s.controller.Messages = append(s.controller.Messages, msg)
	s.controller.UpdatedAt = now

	targetID := to
	if direction == "report" {
		targetID = from
	}
	if sess, idx := s.findSessionLocked(targetID); sess != nil && idx >= 0 {
		sess.Messages = append(sess.Messages, msg)
		sess.UpdatedAt = now
	}
}

// GetPR returns a single pull request by number.
func (s *Service) GetPR(number int) (PullRequest, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	pr, _ := s.findPRLocked(number)
	if pr == nil {
		return PullRequest{}, fmt.Errorf("%w: #%d", ErrPRNotFound, number)
	}
	return clonePR(*pr), nil
}

// ListPRs returns pull requests optionally filtered by status or stack ID.
func (s *Service) ListPRs(statusFilter string, stackID string) []PullRequest {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []PullRequest
	for _, pr := range s.prs {
		if statusFilter != "" && string(pr.Status) != statusFilter {
			continue
		}
		if stackID != "" && pr.StackID != stackID {
			continue
		}
		out = append(out, clonePR(pr))
	}
	return out
}

// SendPRMessage appends an instruction to a PR's session thread, routes it to the assigned session,
// and updates the PR state (running automatically to completion if Auto-Mode is enabled).
func (s *Service) SendPRMessage(prNumber int, author, text string) (PullRequest, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return PullRequest{}, ErrEmptyMessage
	}
	author = strings.TrimSpace(author)
	if author == "" {
		author = "Human Operator"
	}

	s.mu.Lock()
	pr, _ := s.findPRLocked(prNumber)
	if pr == nil {
		s.mu.Unlock()
		return PullRequest{}, fmt.Errorf("%w: #%d", ErrPRNotFound, prNumber)
	}
	now := time.Now().UTC()
	prevStatus := pr.Status

	pr.Thread = append(pr.Thread, ThreadMessage{
		ID:        s.nextID("msg"),
		Author:    author,
		Role:      "human",
		Kind:      "message",
		Text:      text,
		CreatedAt: now,
	})

	sessName := pr.SessionID
	if sess, _ := s.findSessionLocked(pr.SessionID); sess != nil {
		sessName = sess.Name
		sess.ActiveTask = fmt.Sprintf("Processing instruction on PR #%d: %s", pr.Number, text)
		sess.Status = "working"
	}

	s.recordInterSessionLocked(s.controller.ID, pr.SessionID, "instruction", pr.Number, text, now)

	if pr.AutoMode {
		replyNow := now.Add(time.Millisecond)
		replyText := fmt.Sprintf("Auto-Mode executed instruction (%q), verified tests in worktree %s, and updated branch %s.", text, pr.WorktreeID, pr.Branch)
		pr.Thread = append(pr.Thread, ThreadMessage{
			ID:        s.nextID("msg"),
			Author:    sessName,
			Role:      "session",
			Kind:      "message",
			Text:      replyText,
			CreatedAt: replyNow,
		})
		pr.Status = StatusWaitingForHuman
		pr.CIStatus = "passing"
		pr.UpdatedAt = replyNow
		s.recordInterSessionLocked(pr.SessionID, s.controller.ID, "report", pr.Number, replyText, replyNow)
		s.emitNotificationLocked(
			pr,
			StatusWorking,
			StatusWaitingForHuman,
			fmt.Sprintf("PR #%d is Waiting for Human", pr.Number),
			fmt.Sprintf("%s completed Auto-Mode instruction on PR #%d.", sessName, pr.Number),
			replyNow,
		)
	} else {
		ackNow := now.Add(time.Millisecond)
		ackText := fmt.Sprintf("Received instruction on branch %s: %q. Session is now working on changes.", pr.Branch, text)
		pr.Thread = append(pr.Thread, ThreadMessage{
			ID:        s.nextID("msg"),
			Author:    sessName,
			Role:      "session",
			Kind:      "message",
			Text:      ackText,
			CreatedAt: ackNow,
		})
		pr.Status = StatusWorking
		pr.UpdatedAt = ackNow
		_ = prevStatus
	}

	res := clonePR(*pr)
	snap := s.snapshotLocked()
	s.mu.Unlock()

	s.broadcast(snap)
	return res, nil
}

// PostSBNNComment submits a review comment through the sbnn integration boundary,
// appends it to the PR thread, and triggers automatic handling if Auto-Mode is enabled.
func (s *Service) PostSBNNComment(ctx context.Context, prNumber int, filePath string, line int, body string) (PullRequest, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return PullRequest{}, ErrEmptyMessage
	}

	s.mu.RLock()
	prCopy, _ := s.findPRLocked(prNumber)
	if prCopy == nil {
		s.mu.RUnlock()
		return PullRequest{}, fmt.Errorf("%w: #%d", ErrPRNotFound, prNumber)
	}
	req := SBNNCommentRequest{
		Repository: s.repo.FullName,
		PRNumber:   prCopy.Number,
		Branch:     prCopy.Branch,
		Worktree:   prCopy.WorktreeID,
		FilePath:   strings.TrimSpace(filePath),
		Line:       line,
		Body:       body,
	}
	s.mu.RUnlock()

	result, err := s.reviewer.SubmitComment(ctx, req)
	if err != nil {
		return PullRequest{}, err
	}

	s.mu.Lock()
	pr, _ := s.findPRLocked(prNumber)
	if pr == nil {
		s.mu.Unlock()
		return PullRequest{}, fmt.Errorf("%w: #%d", ErrPRNotFound, prNumber)
	}

	now := result.CreatedAt
	pr.Thread = append(pr.Thread, ThreadMessage{
		ID:        result.CommentID,
		Author:    "sbnn",
		Role:      "sbnn",
		Kind:      "review_comment",
		FilePath:  req.FilePath,
		Line:      req.Line,
		Text:      fmt.Sprintf("%s (invoked: `%s`)", result.Formatted, result.CommandLine),
		CreatedAt: now,
	})
	pr.ReviewStatus = "changes_requested"
	s.recordInterSessionLocked(s.controller.ID, pr.SessionID, "instruction", pr.Number, "New sbnn review comment: "+result.Formatted, now)

	sessName := pr.SessionID
	if sess, _ := s.findSessionLocked(pr.SessionID); sess != nil {
		sessName = sess.Name
	}

	if pr.AutoMode {
		fixTime := now.Add(time.Millisecond)
		fixMsg := fmt.Sprintf("Auto-Mode addressed sbnn comment (%s) and verified tests pass.", result.CommentID)
		pr.Thread = append(pr.Thread, ThreadMessage{
			ID:        s.nextID("msg"),
			Author:    sessName,
			Role:      "session",
			Kind:      "review_fix",
			Text:      fixMsg,
			CreatedAt: fixTime,
		})
		pr.ReviewStatus = "approved"
		pr.CIStatus = "passing"
		pr.Status = StatusWaitingForHuman
		pr.Additions += 6
		pr.UpdatedAt = fixTime
		s.recordInterSessionLocked(pr.SessionID, s.controller.ID, "report", pr.Number, fixMsg, fixTime)
		s.emitNotificationLocked(
			pr,
			StatusWorking,
			StatusWaitingForHuman,
			fmt.Sprintf("PR #%d Auto-Fixed sbnn Comment", pr.Number),
			fmt.Sprintf("%s automatically resolved the sbnn comment and is now Waiting for Human.", sessName),
			fixTime,
		)
	} else {
		pr.UpdatedAt = now
	}

	res := clonePR(*pr)
	snap := s.snapshotLocked()
	s.mu.Unlock()

	s.broadcast(snap)
	return res, nil
}

// RequestReview runs an automated review via the sbnn integration boundary and updates the PR thread.
func (s *Service) RequestReview(ctx context.Context, prNumber int, focus string) (PullRequest, error) {
	s.mu.RLock()
	prCopy, _ := s.findPRLocked(prNumber)
	if prCopy == nil {
		s.mu.RUnlock()
		return PullRequest{}, fmt.Errorf("%w: #%d", ErrPRNotFound, prNumber)
	}
	req := SBNNReviewRequest{
		Repository: s.repo.FullName,
		PRNumber:   prCopy.Number,
		Branch:     prCopy.Branch,
		BaseBranch: prCopy.BaseBranch,
		Worktree:   prCopy.WorktreeID,
		Focus:      strings.TrimSpace(focus),
	}
	s.mu.RUnlock()

	reviewRes, err := s.reviewer.RunReview(ctx, req)
	if err != nil {
		return PullRequest{}, err
	}

	s.mu.Lock()
	pr, _ := s.findPRLocked(prNumber)
	if pr == nil {
		s.mu.Unlock()
		return PullRequest{}, fmt.Errorf("%w: #%d", ErrPRNotFound, prNumber)
	}

	now := reviewRes.CreatedAt
	prevStatus := pr.Status
	reqLabel := "Requested code review via sbnn."
	if req.Focus != "" {
		reqLabel = fmt.Sprintf("Requested code review via sbnn (focus: %s).", req.Focus)
	}
	pr.Thread = append(pr.Thread,
		ThreadMessage{
			ID:        s.nextID("msg"),
			Author:    "Dashboard Orchestrator",
			Role:      "controller",
			Kind:      "review_request",
			Text:      reqLabel,
			CreatedAt: now,
		},
		ThreadMessage{
			ID:        s.nextID("msg"),
			Author:    "sbnn",
			Role:      "sbnn",
			Kind:      "review_comment",
			Text:      fmt.Sprintf("%s (invoked: `%s`)", reviewRes.Summary, reviewRes.CommandLine),
			CreatedAt: now.Add(time.Millisecond),
		},
	)

	sessName := pr.SessionID
	if sess, _ := s.findSessionLocked(pr.SessionID); sess != nil {
		sessName = sess.Name
	}

	if pr.AutoMode {
		fixTime := now.Add(2 * time.Millisecond)
		fixText := fmt.Sprintf("Auto-Mode addressed all %d review findings from sbnn and re-ran test suite.", len(reviewRes.Findings))
		pr.Thread = append(pr.Thread, ThreadMessage{
			ID:        s.nextID("msg"),
			Author:    sessName,
			Role:      "session",
			Kind:      "review_fix",
			Text:      fixText,
			CreatedAt: fixTime,
		})
		pr.ReviewStatus = "approved"
		pr.CIStatus = "passing"
		pr.Status = StatusWaitingForHuman
		pr.Additions += 9
		pr.UpdatedAt = fixTime
		s.recordInterSessionLocked(pr.SessionID, s.controller.ID, "report", pr.Number, fixText, fixTime)
		s.emitNotificationLocked(
			pr,
			StatusWorking,
			StatusWaitingForHuman,
			fmt.Sprintf("PR #%d Review Completed (Waiting for Human)", pr.Number),
			fmt.Sprintf("%s automatically addressed the review findings on PR #%d.", sessName, pr.Number),
			fixTime,
		)
	} else {
		pr.ReviewStatus = "changes_requested"
		pr.Status = StatusWaitingForHuman
		pr.UpdatedAt = now.Add(time.Millisecond)
		if prevStatus == StatusWorking {
			s.emitNotificationLocked(
				pr,
				StatusWorking,
				StatusWaitingForHuman,
				fmt.Sprintf("PR #%d Review Ready for Human", pr.Number),
				fmt.Sprintf("sbnn review finished for PR #%d; awaiting human decision or review-fix dispatch.", pr.Number),
				pr.UpdatedAt,
			)
		}
	}

	res := clonePR(*pr)
	snap := s.snapshotLocked()
	s.mu.Unlock()

	s.broadcast(snap)
	return res, nil
}

// AddressReview instructs the assigned session to address review feedback on a pull request.
func (s *Service) AddressReview(prNumber int, instruction string) (PullRequest, error) {
	instruction = strings.TrimSpace(instruction)
	if instruction == "" {
		instruction = "Address all open review comments and verify test suite passes."
	}

	s.mu.Lock()
	pr, _ := s.findPRLocked(prNumber)
	if pr == nil {
		s.mu.Unlock()
		return PullRequest{}, fmt.Errorf("%w: #%d", ErrPRNotFound, prNumber)
	}

	now := time.Now().UTC()
	sessName := pr.SessionID
	if sess, _ := s.findSessionLocked(pr.SessionID); sess != nil {
		sessName = sess.Name
		sess.ActiveTask = fmt.Sprintf("Addressing review feedback on PR #%d", pr.Number)
	}

	pr.Thread = append(pr.Thread,
		ThreadMessage{
			ID:        s.nextID("msg"),
			Author:    "Dashboard Orchestrator",
			Role:      "controller",
			Kind:      "review_fix",
			Text:      "Dispatch review response to session: " + instruction,
			CreatedAt: now,
		},
		ThreadMessage{
			ID:        s.nextID("msg"),
			Author:    sessName,
			Role:      "session",
			Kind:      "review_fix",
			Text:      "Applied review fixes, updated unit tests, and pushed new commit to " + pr.Branch + ".",
			CreatedAt: now.Add(time.Millisecond),
		},
	)

	s.recordInterSessionLocked(s.controller.ID, pr.SessionID, "instruction", pr.Number, instruction, now)
	s.recordInterSessionLocked(pr.SessionID, s.controller.ID, "report", pr.Number, fmt.Sprintf("Addressed review feedback on PR #%d and verified CI checks.", pr.Number), now.Add(time.Millisecond))

	pr.ReviewStatus = "approved"
	pr.CIStatus = "passing"
	pr.Additions += 11
	pr.Deletions += 4
	pr.Status = StatusWaitingForHuman
	pr.UpdatedAt = now.Add(time.Millisecond)

	s.emitNotificationLocked(
		pr,
		StatusWorking,
		StatusWaitingForHuman,
		fmt.Sprintf("PR #%d Review Fixes Ready for Human", pr.Number),
		fmt.Sprintf("%s finished applying review fixes on PR #%d.", sessName, pr.Number),
		pr.UpdatedAt,
	)

	res := clonePR(*pr)
	snap := s.snapshotLocked()
	s.mu.Unlock()

	s.broadcast(snap)
	return res, nil
}

// RunRoutineAction executes a preset one-click operation on a pull request.
func (s *Service) RunRoutineAction(prNumber int, action RoutineAction) (PullRequest, error) {
	switch action {
	case ActionResolveConflicts, ActionRebaseStack, ActionFixCI, ActionSyncBranch, ActionUpdateSummary:
	default:
		return PullRequest{}, fmt.Errorf("%w: %s", ErrInvalidRoutineAction, action)
	}

	s.mu.Lock()
	pr, _ := s.findPRLocked(prNumber)
	if pr == nil {
		s.mu.Unlock()
		return PullRequest{}, fmt.Errorf("%w: #%d", ErrPRNotFound, prNumber)
	}

	now := time.Now().UTC()
	sessName := pr.SessionID
	if sess, _ := s.findSessionLocked(pr.SessionID); sess != nil {
		sessName = sess.Name
	}

	var actionDesc, reportDesc string
	switch action {
	case ActionResolveConflicts:
		actionDesc = fmt.Sprintf("Routine action: Resolve merge conflicts on %s against %s.", pr.Branch, pr.BaseBranch)
		reportDesc = fmt.Sprintf("Resolved merge conflicts in worktree %s, kept clean commit history, and verified tests pass.", pr.WorktreeID)
		pr.HasConflicts = false
		pr.CIStatus = "passing"
		for i := range s.worktrees {
			if s.worktrees[i].ID == pr.WorktreeID {
				s.worktrees[i].Clean = true
			}
		}
	case ActionRebaseStack:
		if pr.StackID != "" {
			actionDesc = fmt.Sprintf("Routine action: Rebase stacked PR chain %s in topological order.", pr.StackID)
			reportDesc = fmt.Sprintf("Rebased all pull requests in stack %s onto updated base branches without conflicts.", pr.StackID)
			for i := range s.prs {
				if s.prs[i].StackID == pr.StackID {
					s.prs[i].HasConflicts = false
					s.prs[i].CIStatus = "passing"
					s.prs[i].UpdatedAt = now
				}
			}
		} else {
			actionDesc = fmt.Sprintf("Routine action: Rebase branch %s onto %s.", pr.Branch, pr.BaseBranch)
			reportDesc = fmt.Sprintf("Rebased %s onto latest %s cleanly.", pr.Branch, pr.BaseBranch)
			pr.HasConflicts = false
			pr.CIStatus = "passing"
		}
	case ActionFixCI:
		actionDesc = fmt.Sprintf("Routine action: Diagnose and fix failing CI checks on PR #%d.", pr.Number)
		reportDesc = "Fixed race condition in failing test suite and confirmed all CI checks pass."
		pr.CIStatus = "passing"
		pr.Additions += 8
	case ActionSyncBranch:
		actionDesc = fmt.Sprintf("Routine action: Sync %s with upstream %s.", pr.Branch, s.repo.DefaultBranch)
		reportDesc = fmt.Sprintf("Fast-forwarded worktree %s with latest %s changes.", pr.WorktreeID, s.repo.DefaultBranch)
		pr.HasConflicts = false
	case ActionUpdateSummary:
		actionDesc = fmt.Sprintf("Routine action: Refresh PR #%d description and diff summary.", pr.Number)
		reportDesc = "Regenerated PR description and test plan from latest commit range."
	}

	pr.Thread = append(pr.Thread,
		ThreadMessage{
			ID:        s.nextID("msg"),
			Author:    "Dashboard Orchestrator",
			Role:      "controller",
			Kind:      "routine_action",
			Text:      actionDesc,
			CreatedAt: now,
		},
		ThreadMessage{
			ID:        s.nextID("msg"),
			Author:    sessName,
			Role:      "session",
			Kind:      "routine_action",
			Text:      reportDesc,
			CreatedAt: now.Add(time.Millisecond),
		},
	)

	s.recordInterSessionLocked(s.controller.ID, pr.SessionID, "instruction", pr.Number, actionDesc, now)
	s.recordInterSessionLocked(pr.SessionID, s.controller.ID, "report", pr.Number, reportDesc, now.Add(time.Millisecond))

	prevStatus := pr.Status
	pr.Status = StatusWaitingForHuman
	pr.UpdatedAt = now.Add(time.Millisecond)

	if prevStatus == StatusWorking || action == ActionResolveConflicts || action == ActionFixCI || action == ActionRebaseStack {
		s.emitNotificationLocked(
			pr,
			StatusWorking,
			StatusWaitingForHuman,
			fmt.Sprintf("PR #%d Routine Task Complete (Waiting for Human)", pr.Number),
			fmt.Sprintf("%s completed %s on PR #%d.", sessName, action, pr.Number),
			pr.UpdatedAt,
		)
	}

	res := clonePR(*pr)
	snap := s.snapshotLocked()
	s.mu.Unlock()

	s.broadcast(snap)
	return res, nil
}

// SetAutoMode toggles Auto-Mode for a pull request. When enabled on a PR that is currently
// working or has blockers, the assigned session automatically completes pending routine work.
func (s *Service) SetAutoMode(prNumber int, enabled bool) (PullRequest, error) {
	s.mu.Lock()
	pr, _ := s.findPRLocked(prNumber)
	if pr == nil {
		s.mu.Unlock()
		return PullRequest{}, fmt.Errorf("%w: #%d", ErrPRNotFound, prNumber)
	}

	now := time.Now().UTC()
	pr.AutoMode = enabled
	sessName := pr.SessionID
	if sess, _ := s.findSessionLocked(pr.SessionID); sess != nil {
		sessName = sess.Name
	}

	modeText := "Auto-Mode disabled: PR is now in manual instruction mode."
	if enabled {
		modeText = "Auto-Mode enabled: Session will autonomously handle routine actions and review responses."
	}
	pr.Thread = append(pr.Thread, ThreadMessage{
		ID:        s.nextID("msg"),
		Author:    "Dashboard Orchestrator",
		Role:      "controller",
		Kind:      "status_change",
		Text:      modeText,
		CreatedAt: now,
	})

	if enabled && (pr.Status == StatusWorking || pr.HasConflicts || pr.CIStatus == "failing") {
		doneTime := now.Add(time.Millisecond)
		autoReport := "Auto-Mode resolved pending checks, verified clean worktree state, and marked PR ready for human review."
		pr.HasConflicts = false
		pr.CIStatus = "passing"
		pr.ReviewStatus = "approved"
		pr.Status = StatusWaitingForHuman
		pr.Thread = append(pr.Thread, ThreadMessage{
			ID:        s.nextID("msg"),
			Author:    sessName,
			Role:      "session",
			Kind:      "status_change",
			Text:      autoReport,
			CreatedAt: doneTime,
		})
		pr.UpdatedAt = doneTime
		s.recordInterSessionLocked(pr.SessionID, s.controller.ID, "report", pr.Number, autoReport, doneTime)
		s.emitNotificationLocked(
			pr,
			StatusWorking,
			StatusWaitingForHuman,
			fmt.Sprintf("PR #%d Auto-Mode Completed (Waiting for Human)", pr.Number),
			fmt.Sprintf("%s finished pending tasks automatically on PR #%d.", sessName, pr.Number),
			doneTime,
		)
	} else {
		pr.UpdatedAt = now
	}

	res := clonePR(*pr)
	snap := s.snapshotLocked()
	s.mu.Unlock()

	s.broadcast(snap)
	return res, nil
}

// SetPRStatus explicitly sets the status of a pull request and emits a notification
// whenever transitioning to StatusWaitingForHuman.
func (s *Service) SetPRStatus(prNumber int, status PRStatus, summary string) (PullRequest, error) {
	if status != StatusWaitingForHuman && status != StatusWorking {
		return PullRequest{}, fmt.Errorf("%w: %s", ErrInvalidPRStatus, status)
	}

	s.mu.Lock()
	pr, _ := s.findPRLocked(prNumber)
	if pr == nil {
		s.mu.Unlock()
		return PullRequest{}, fmt.Errorf("%w: #%d", ErrPRNotFound, prNumber)
	}

	now := time.Now().UTC()
	prevStatus := pr.Status
	pr.Status = status
	pr.UpdatedAt = now

	sessName := pr.SessionID
	if sess, _ := s.findSessionLocked(pr.SessionID); sess != nil {
		sessName = sess.Name
	}

	summary = strings.TrimSpace(summary)
	if summary == "" {
		if status == StatusWaitingForHuman {
			summary = "Session completed current iteration. Now Waiting for Human."
		} else {
			summary = "Session resumed active work on pull request."
		}
	}

	pr.Thread = append(pr.Thread, ThreadMessage{
		ID:        s.nextID("msg"),
		Author:    sessName,
		Role:      "session",
		Kind:      "status_change",
		Text:      summary,
		CreatedAt: now,
	})

	if status == StatusWaitingForHuman && prevStatus != StatusWaitingForHuman {
		s.recordInterSessionLocked(pr.SessionID, s.controller.ID, "report", pr.Number, summary, now)
		s.emitNotificationLocked(
			pr,
			prevStatus,
			StatusWaitingForHuman,
			fmt.Sprintf("PR #%d is now Waiting for Human", pr.Number),
			fmt.Sprintf("%s: %s", sessName, summary),
			now,
		)
	} else {
		s.recordInterSessionLocked(s.controller.ID, pr.SessionID, "instruction", pr.Number, summary, now)
	}

	res := clonePR(*pr)
	snap := s.snapshotLocked()
	s.mu.Unlock()

	s.broadcast(snap)
	return res, nil
}

// SetSessionModelTier updates the Pine/Bamboo/Plum model preset for a session and its subagents.
func (s *Service) SetSessionModelTier(sessionID string, tier ModelTier, subagentTier ModelTier) (Session, error) {
	if !ValidModelTier(tier) {
		return Session{}, fmt.Errorf("%w: %s", ErrInvalidModelTier, tier)
	}
	if subagentTier == "" {
		subagentTier = tier
	}
	if !ValidModelTier(subagentTier) {
		return Session{}, fmt.Errorf("%w: %s", ErrInvalidModelTier, subagentTier)
	}

	s.mu.Lock()
	sess, _ := s.findSessionLocked(sessionID)
	if sess == nil {
		s.mu.Unlock()
		return Session{}, fmt.Errorf("%w: %s", ErrSessionNotFound, sessionID)
	}

	now := time.Now().UTC()
	sess.ModelTier = tier
	sess.ResolvedModel = s.modelConfig.Resolve(tier)
	sess.SubagentModelTier = subagentTier
	sess.ResolvedSubagentModel = s.modelConfig.Resolve(subagentTier)
	sess.UpdatedAt = now

	for i := range s.prs {
		if s.prs[i].SessionID == sess.ID {
			s.prs[i].ModelTier = tier
			s.prs[i].ResolvedModel = s.modelConfig.Resolve(tier)
			s.prs[i].UpdatedAt = now
		}
	}

	res := cloneSession(*sess)
	snap := s.snapshotLocked()
	s.mu.Unlock()

	s.broadcast(snap)
	return res, nil
}

// SetPRModelTier updates the Pine/Bamboo/Plum model preset for a pull request and its session.
func (s *Service) SetPRModelTier(prNumber int, tier ModelTier) (PullRequest, error) {
	if !ValidModelTier(tier) {
		return PullRequest{}, fmt.Errorf("%w: %s", ErrInvalidModelTier, tier)
	}

	s.mu.Lock()
	pr, _ := s.findPRLocked(prNumber)
	if pr == nil {
		s.mu.Unlock()
		return PullRequest{}, fmt.Errorf("%w: #%d", ErrPRNotFound, prNumber)
	}

	now := time.Now().UTC()
	pr.ModelTier = tier
	pr.ResolvedModel = s.modelConfig.Resolve(tier)
	pr.UpdatedAt = now

	if sess, _ := s.findSessionLocked(pr.SessionID); sess != nil {
		sess.ModelTier = tier
		sess.ResolvedModel = s.modelConfig.Resolve(tier)
		sess.UpdatedAt = now
	}

	pr.Thread = append(pr.Thread, ThreadMessage{
		ID:        s.nextID("msg"),
		Author:    "Dashboard Orchestrator",
		Role:      "controller",
		Kind:      "status_change",
		Text:      fmt.Sprintf("Switched model tier preset to %s (%s).", strings.ToUpper(string(tier)), pr.ResolvedModel),
		CreatedAt: now,
	})

	res := clonePR(*pr)
	snap := s.snapshotLocked()
	s.mu.Unlock()

	s.broadcast(snap)
	return res, nil
}

// UpdateModelConfig updates the concrete model names mapped to the Pine, Bamboo, and Plum presets.
func (s *Service) UpdateModelConfig(cfg ModelPresetConfig) (ModelPresetConfig, error) {
	s.mu.Lock()
	if strings.TrimSpace(cfg.Pine) != "" {
		s.modelConfig.Pine = strings.TrimSpace(cfg.Pine)
	}
	if strings.TrimSpace(cfg.Bamboo) != "" {
		s.modelConfig.Bamboo = strings.TrimSpace(cfg.Bamboo)
	}
	if strings.TrimSpace(cfg.Plum) != "" {
		s.modelConfig.Plum = strings.TrimSpace(cfg.Plum)
	}

	s.controller.ResolvedModel = s.modelConfig.Resolve(s.controller.ModelTier)
	s.controller.ResolvedSubagentModel = s.modelConfig.Resolve(s.controller.SubagentModelTier)
	for i := range s.sessions {
		s.sessions[i].ResolvedModel = s.modelConfig.Resolve(s.sessions[i].ModelTier)
		s.sessions[i].ResolvedSubagentModel = s.modelConfig.Resolve(s.sessions[i].SubagentModelTier)
	}
	for i := range s.prs {
		s.prs[i].ResolvedModel = s.modelConfig.Resolve(s.prs[i].ModelTier)
	}

	updated := s.modelConfig
	snap := s.snapshotLocked()
	s.mu.Unlock()

	s.broadcast(snap)
	return updated, nil
}

// SendDirectSessionInstruction sends a direct instruction to a worker session outside any specific PR thread.
func (s *Service) SendDirectSessionInstruction(sessionID string, text string) (Session, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Session{}, ErrEmptyMessage
	}

	s.mu.Lock()
	sess, _ := s.findSessionLocked(sessionID)
	if sess == nil {
		s.mu.Unlock()
		return Session{}, fmt.Errorf("%w: %s", ErrSessionNotFound, sessionID)
	}

	now := time.Now().UTC()
	sess.ActiveTask = text
	sess.Status = "working"
	s.recordInterSessionLocked(s.controller.ID, sess.ID, "instruction", 0, text, now)

	ack := fmt.Sprintf("Session %s acknowledged direct instruction in worktree %s: %q", sess.Name, sess.WorktreeID, text)
	s.recordInterSessionLocked(sess.ID, s.controller.ID, "report", 0, ack, now.Add(time.Millisecond))

	res := cloneSession(*sess)
	snap := s.snapshotLocked()
	s.mu.Unlock()

	s.broadcast(snap)
	return res, nil
}

// SendControllerInstruction sends a user instruction to the session running the dashboard itself.
func (s *Service) SendControllerInstruction(text string) (Session, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Session{}, ErrEmptyMessage
	}

	s.mu.Lock()
	now := time.Now().UTC()
	userMsg := SessionMessage{
		ID:        s.nextID("cmsg"),
		From:      "human",
		To:        s.controller.ID,
		Direction: "instruction",
		Text:      text,
		CreatedAt: now,
	}
	s.controller.Messages = append(s.controller.Messages, userMsg)
	s.controller.ActiveTask = "Executing dashboard instruction: " + text

	var waitingNums []string
	var workingNums []string
	for _, pr := range s.prs {
		if pr.Status == StatusWaitingForHuman {
			waitingNums = append(waitingNums, fmt.Sprintf("#%d", pr.Number))
		} else {
			workingNums = append(workingNums, fmt.Sprintf("#%d", pr.Number))
		}
	}

	replyText := fmt.Sprintf(
		"Controller session processed %q for %s. Current state: %d Waiting for Human (%s), %d Agent Working (%s).",
		text,
		s.repo.FullName,
		len(waitingNums),
		strings.Join(waitingNums, ", "),
		len(workingNums),
		strings.Join(workingNums, ", "),
	)
	replyMsg := SessionMessage{
		ID:        s.nextID("cmsg"),
		From:      s.controller.ID,
		To:        "human",
		Direction: "report",
		Text:      replyText,
		CreatedAt: now.Add(time.Millisecond),
	}
	s.controller.Messages = append(s.controller.Messages, replyMsg)
	s.controller.UpdatedAt = now.Add(time.Millisecond)

	res := cloneSession(s.controller)
	snap := s.snapshotLocked()
	s.mu.Unlock()

	s.broadcast(snap)
	return res, nil
}

// ReportSessionResult records a status update and result from a worker session back to the dashboard.
func (s *Service) ReportSessionResult(sessionID string, prNumber int, status PRStatus, report string) (PullRequest, error) {
	report = strings.TrimSpace(report)
	if report == "" {
		return PullRequest{}, ErrEmptyMessage
	}
	if status != StatusWaitingForHuman && status != StatusWorking {
		return PullRequest{}, fmt.Errorf("%w: %s", ErrInvalidPRStatus, status)
	}

	s.mu.Lock()
	pr, _ := s.findPRLocked(prNumber)
	if pr == nil {
		s.mu.Unlock()
		return PullRequest{}, fmt.Errorf("%w: #%d", ErrPRNotFound, prNumber)
	}

	now := time.Now().UTC()
	prevStatus := pr.Status
	sessName := sessionID
	if sess, _ := s.findSessionLocked(sessionID); sess != nil {
		sessName = sess.Name
		sess.ActiveTask = report
		if status == StatusWaitingForHuman {
			sess.Status = "idle"
		} else {
			sess.Status = "working"
		}
	}

	pr.Status = status
	pr.UpdatedAt = now
	pr.Thread = append(pr.Thread, ThreadMessage{
		ID:        s.nextID("msg"),
		Author:    sessName,
		Role:      "session",
		Kind:      "status_change",
		Text:      report,
		CreatedAt: now,
	})
	s.recordInterSessionLocked(sessionID, s.controller.ID, "report", pr.Number, report, now)

	if status == StatusWaitingForHuman && prevStatus != StatusWaitingForHuman {
		s.emitNotificationLocked(
			pr,
			prevStatus,
			StatusWaitingForHuman,
			fmt.Sprintf("PR #%d is now Waiting for Human", pr.Number),
			fmt.Sprintf("%s reported completion: %s", sessName, report),
			now,
		)
	}

	res := clonePR(*pr)
	snap := s.snapshotLocked()
	s.mu.Unlock()

	s.broadcast(snap)
	return res, nil
}

// MarkNotificationRead marks a single notification or all notifications ("all") as read.
func (s *Service) MarkNotificationRead(id string) error {
	s.mu.Lock()
	if id == "all" || id == "" {
		for i := range s.notifications {
			s.notifications[i].Read = true
		}
		snap := s.snapshotLocked()
		s.mu.Unlock()
		s.broadcast(snap)
		return nil
	}

	found := false
	for i := range s.notifications {
		if s.notifications[i].ID == id {
			s.notifications[i].Read = true
			found = true
			break
		}
	}
	if !found {
		s.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNotificationNotFound, id)
	}
	snap := s.snapshotLocked()
	s.mu.Unlock()
	s.broadcast(snap)
	return nil
}
