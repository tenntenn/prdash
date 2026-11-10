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
	WorkDir     string
	StateFile   string
	ModelConfig *ModelPresetConfig
	Reviewer    SBNNReviewer
}

// Service manages the repository dashboard state, inter-session communication,
// sbnn review integration, and human notifications.
type Service struct {
	config      EffectiveConfig
	configErr   error
	mu          sync.RWMutex
	seq         atomic.Int64
	stateFile   string
	workDir     string
	stateRev    int64
	dirty       bool
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

// NewService creates a Service for the target repository.
func NewService(opts Options) *Service {
	repo := ParseRepositoryInfo(opts.Repository)
	if opts.WorkDir != "" && repo.LocalPath == "" {
		repo.LocalPath = opts.WorkDir
	}
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
		reviewer = NewCLISBNNClient("sbnn", repo.LocalPath)
	}

	activeTask := "Coordinating repository"
	if repo.FullName != "" {
		activeTask = "Coordinating repository " + repo.FullName
	}
	controller := Session{
		ID:                    "sess-controller",
		Name:                  "Dashboard Orchestrator",
		Role:                  "controller",
		AgentType:             "Controller Session",
		Status:                "coordinating",
		ModelTier:             TierBamboo,
		ResolvedModel:         models.Resolve(TierBamboo),
		SubagentModelTier:     TierPlum,
		ResolvedSubagentModel: models.Resolve(TierPlum),
		ActiveTask:            activeTask,
		Messages:              make([]SessionMessage, 0),
		UpdatedAt:             time.Now().UTC(),
	}
	config, configErr := LoadRepoConfig(context.Background(), opts.WorkDir)
	svc := &Service{
		config:        config,
		configErr:     configErr,
		stateFile:     strings.TrimSpace(opts.StateFile),
		workDir:       strings.TrimSpace(opts.WorkDir),
		repo:          repo,
		modelConfig:   models,
		reviewer:      reviewer,
		controller:    controller,
		worktrees:     make([]Worktree, 0),
		sessions:      make([]Session, 0),
		prs:           make([]PullRequest, 0),
		notifications: make([]Notification, 0),
		subscribers:   make(map[int64]chan Snapshot),
	}
	svc.seq.Store(1000)
	if svc.stateFile != "" {
		if !svc.reloadStateFileIfChangedLocked() {
			svc.saveStateFileLocked()
		}
	}
	return svc
}

// SyncRepository inspects the git repository at dir (or s.workDir / current directory),
// discovers local git worktrees and open GitHub pull requests, and merges them into the service state.
func (s *Service) SyncRepository(ctx context.Context, dir string) (Snapshot, error) {
	s.mu.RLock()
	if strings.TrimSpace(dir) == "" {
		dir = s.workDir
	}
	if dir == "" {
		dir = s.repo.LocalPath
	}
	configuredRepo := s.repo.FullName
	models := s.modelConfig
	s.mu.RUnlock()

	detectedRepo := DetectRepositoryInfo(ctx, dir)
	if configuredRepo != "" && strings.Contains(configuredRepo, "/") {
		parsed := ParseRepositoryInfo(configuredRepo)
		detectedRepo.Owner = parsed.Owner
		detectedRepo.Name = parsed.Name
		detectedRepo.FullName = parsed.FullName
	}

	worktrees, wtErr := DiscoverWorktrees(ctx, detectedRepo.LocalPath)
	prs, prErr := DiscoverPullRequests(ctx, detectedRepo.FullName, detectedRepo.LocalPath, detectedRepo.DefaultBranch, worktrees, models)

	s.mu.Lock()
	s.reloadStateFileIfChangedLocked()
	s.dirty = true
	if detectedRepo.FullName != "" {
		s.repo.Owner = detectedRepo.Owner
		s.repo.Name = detectedRepo.Name
		s.repo.FullName = detectedRepo.FullName
	}
	if detectedRepo.DefaultBranch != "" {
		s.repo.DefaultBranch = detectedRepo.DefaultBranch
	}
	if detectedRepo.LocalPath != "" {
		s.repo.LocalPath = detectedRepo.LocalPath
	}

	now := time.Now().UTC()
	if wtErr == nil {
		for _, wt := range worktrees {
			merged := false
			for i := range s.worktrees {
				if s.worktrees[i].ID == wt.ID || (wt.Path != "" && s.worktrees[i].Path == wt.Path) {
					wt.ID = s.worktrees[i].ID
					if wt.SessionID == "" {
						wt.SessionID = s.worktrees[i].SessionID
					}
					s.worktrees[i] = wt
					merged = true
					break
				}
			}
			if !merged {
				if wt.SessionID == "" {
					sessID := "sess-" + strings.TrimPrefix(wt.ID, "wt-")
					wt.SessionID = sessID
					if sess, _ := s.findSessionLocked(sessID); sess == nil {
						s.sessions = append(s.sessions, Session{
							ID:                    sessID,
							Name:                  fmt.Sprintf("Session (%s)", wt.Branch),
							Role:                  "worker",
							AgentType:             "Worker Session",
							WorktreeID:            wt.ID,
							Status:                "idle",
							ModelTier:             TierBamboo,
							ResolvedModel:         s.modelConfig.Resolve(TierBamboo),
							SubagentModelTier:     TierPlum,
							ResolvedSubagentModel: s.modelConfig.Resolve(TierPlum),
							ActiveTask:            fmt.Sprintf("Bound to worktree %s (%s)", wt.Path, wt.Branch),
							Messages:              make([]SessionMessage, 0),
							UpdatedAt:             now,
						})
					}
				}
				s.worktrees = append(s.worktrees, wt)
			}
		}
	}

	if prErr == nil {
		wtByBranch := make(map[string]Worktree, len(s.worktrees))
		for _, wt := range s.worktrees {
			if wt.Branch != "" {
				wtByBranch[wt.Branch] = wt
			}
		}
		for _, pr := range prs {
			if wt, ok := wtByBranch[pr.Branch]; ok {
				pr.WorktreeID = wt.ID
				if pr.SessionID == "" {
					pr.SessionID = wt.SessionID
				}
			}
			existing, idx := s.findPRLocked(pr.Number)
			if existing != nil && idx >= 0 {
				pr.Status = existing.Status
				pr.AutoMode = existing.AutoMode
				pr.ModelTier = existing.ModelTier
				pr.ResolvedModel = s.modelConfig.Resolve(existing.ModelTier)
				pr.Thread = existing.Thread
				if pr.WorktreeID == "" {
					pr.WorktreeID = existing.WorktreeID
				}
				if pr.SessionID == "" {
					pr.SessionID = existing.SessionID
				}
				s.prs[idx] = pr
			} else {
				pr.Thread = []ThreadMessage{
					{
						ID:        s.nextID("msg"),
						Author:    "Dashboard Orchestrator",
						Role:      "controller",
						Kind:      "status_change",
						Text:      fmt.Sprintf("Discovered PR #%d (%s) from GitHub.", pr.Number, pr.Branch),
						CreatedAt: now,
					},
				}
				s.prs = append(s.prs, pr)
			}
		}
	}

	s.controller.ActiveTask = fmt.Sprintf("Monitoring %d pull request(s) across %d worktree(s) in %s", len(s.prs), len(s.worktrees), s.repo.FullName)
	snap := s.snapshotLocked()
	s.mu.Unlock()

	s.broadcast(snap)
	if wtErr != nil && prErr != nil {
		return snap, errors.Join(wtErr, prErr)
	}
	return snap, nil
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
	if s.stateFile != "" {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.reloadStateFileIfChangedLocked()
		return s.snapshotLocked()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshotLocked()
}

func (s *Service) snapshotLocked() Snapshot {
	if s.dirty {
		s.saveStateFileLocked()
		s.dirty = false
	}
	controller := cloneSession(s.controller)
	worktrees := slices.Clone(s.worktrees)
	if worktrees == nil {
		worktrees = make([]Worktree, 0)
	}
	sessions := make([]Session, len(s.sessions))
	for i, sess := range s.sessions {
		sessions[i] = cloneSession(sess)
	}
	prs := make([]PullRequest, len(s.prs))
	for i, pr := range s.prs {
		prs[i] = clonePR(pr)
	}
	notifications := slices.Clone(s.notifications)
	if notifications == nil {
		notifications = make([]Notification, 0)
	}

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
		Config:      cloneEffectiveConfig(s.config),
		ConfigError: s.configErrorStringLocked(), Repository: s.repo,
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
	out.Messages = make([]SessionMessage, len(in.Messages))
	for i, msg := range in.Messages {
		out.Messages[i] = cloneSessionMessage(msg)
	}
	if out.Messages == nil {
		out.Messages = make([]SessionMessage, 0)
	}
	return out
}

func clonePR(in PullRequest) PullRequest {
	out := in
	out.Thread = make([]ThreadMessage, len(in.Thread))
	for i, msg := range in.Thread {
		out.Thread[i] = cloneThreadMessage(msg)
	}
	if out.Thread == nil {
		out.Thread = make([]ThreadMessage, 0)
	}
	return out
}

func buildStackGroups(prs []PullRequest) []StackGroup {
	groups := make([]StackGroup, 0, len(prs))
	stackIndex := make(map[string]int)

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
			idx = len(groups)
			stackIndex[pr.StackID] = idx
			groups = append(groups, StackGroup{
				ID:        pr.StackID,
				Title:     "Stacked PR Chain (" + pr.StackID + ")",
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
			if size > 0 && groups[i].PullRequests[0].Title != "" {
				groups[i].Title = fmt.Sprintf("Stack: %s (#%d)", groups[i].PullRequests[0].Title, groups[i].PullRequests[0].Number)
			}
		}
	}
	return groups
}

func (s *Service) findPRLocked(number int) (*PullRequest, int) {
	s.reloadStateFileIfChangedLocked()
	s.dirty = true
	for i := range s.prs {
		if s.prs[i].Number == number {
			return &s.prs[i], i
		}
	}
	return nil, -1
}

func (s *Service) findSessionLocked(id string) (*Session, int) {
	s.reloadStateFileIfChangedLocked()
	s.dirty = true
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
	s.recordInterSessionWithConfigLocked(from, to, direction, prNumber, text, now, s.config)
}

func (s *Service) recordInterSessionWithConfigLocked(from, to, direction string, prNumber int, text string, now time.Time, applied EffectiveConfig) {
	cfg := cloneEffectiveConfig(applied)
	msg := SessionMessage{
		AppliedConfig: &cfg, ID: s.nextID("smsg"),
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
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadStateFileIfChangedLocked()
	for i := range s.prs {
		if s.prs[i].Number == number {
			return clonePR(s.prs[i]), nil
		}
	}
	return PullRequest{}, fmt.Errorf("%w: #%d", ErrPRNotFound, number)
}

// ListPRs returns pull requests optionally filtered by status or stack ID.
func (s *Service) ListPRs(statusFilter string, stackID string) []PullRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadStateFileIfChangedLocked()
	out := make([]PullRequest, 0, len(s.prs))
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

// PendingInstructions returns unhandled instructions queued for sessionID (or all sessions if empty).
func (s *Service) PendingInstructions(sessionID string) []SessionMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadStateFileIfChangedLocked()

	source := s.controller.Messages
	if sessionID != "" && sessionID != "controller" && sessionID != s.controller.ID {
		if sess, _ := s.findSessionLocked(sessionID); sess != nil {
			source = sess.Messages
		} else {
			return make([]SessionMessage, 0)
		}
	}

	out := make([]SessionMessage, 0, len(source))
	for _, msg := range source {
		if msg.Direction != "instruction" || msg.Handled {
			continue
		}
		if sessionID != "" && msg.To != sessionID && !(sessionID == "controller" && msg.To == s.controller.ID) {
			continue
		}
		out = append(out, cloneSessionMessage(msg))
	}
	return out
}

// AckInstructions marks unhandled instructions for sessionID (and optional prNumber) as handled.
func (s *Service) AckInstructions(sessionID string, prNumber int) int {
	s.mu.Lock()
	s.reloadStateFileIfChangedLocked()
	s.dirty = true
	acked := s.markInstructionsHandledLocked(sessionID, prNumber)
	snap := s.snapshotLocked()
	s.mu.Unlock()
	if acked > 0 {
		s.broadcast(snap)
	}
	return acked
}

func (s *Service) markInstructionsHandledLocked(sessionID string, prNumber int) int {
	targetID := strings.TrimSpace(sessionID)
	if targetID == "controller" {
		targetID = s.controller.ID
	}
	acked := 0
	for i := range s.controller.Messages {
		m := &s.controller.Messages[i]
		if m.Direction != "instruction" || m.Handled {
			continue
		}
		if targetID != "" && m.To != targetID {
			continue
		}
		if prNumber > 0 && m.PRNumber != prNumber {
			continue
		}
		m.Handled = true
		acked++
	}
	for si := range s.sessions {
		if targetID != "" && s.sessions[si].ID != targetID {
			continue
		}
		for mi := range s.sessions[si].Messages {
			m := &s.sessions[si].Messages[mi]
			if m.Direction != "instruction" || m.Handled {
				continue
			}
			if prNumber > 0 && m.PRNumber != prNumber {
				continue
			}
			m.Handled = true
		}
	}
	if targetID == "" || targetID == s.controller.ID {
		hasPending := false
		for _, m := range s.controller.Messages {
			if m.Direction == "instruction" && !m.Handled && m.To == s.controller.ID {
				hasPending = true
				break
			}
		}
		if !hasPending {
			s.controller.Status = "coordinating"
		}
	}
	return acked
}

// SendPRMessage appends an instruction to a PR's session thread, routes it to the assigned session,
// and transitions the PR to StatusWorking until the session reports completion.
func (s *Service) SendPRMessage(prNumber int, author, text string) (PullRequest, error) {
	if err := s.ConfigLoadError(); err != nil {
		return PullRequest{}, err
	}
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

	pr.Thread = append(pr.Thread, ThreadMessage{
		ID:        s.nextID("msg"),
		Author:    author,
		Role:      "human",
		Kind:      "message",
		Text:      text,
		CreatedAt: now,
	})
	pr.Status = StatusWorking
	pr.UpdatedAt = now

	if sess, _ := s.findSessionLocked(pr.SessionID); sess != nil {
		sess.ActiveTask = fmt.Sprintf("PR #%d: %s", pr.Number, text)
		sess.Status = "working"
		sess.UpdatedAt = now
	}

	s.recordInterSessionLocked(s.controller.ID, pr.SessionID, "instruction", pr.Number, text, now)

	res := clonePR(*pr)
	snap := s.snapshotLocked()
	s.mu.Unlock()

	s.broadcast(snap)
	return res, nil
}

// PostSBNNComment submits a review comment through the sbnn integration boundary,
// appends it to the PR thread, and queues a review-fix instruction if Auto-Mode is enabled.
func (s *Service) PostSBNNComment(ctx context.Context, prNumber int, filePath string, line int, body string) (PullRequest, error) {
	if err := s.ConfigLoadError(); err != nil {
		return PullRequest{}, err
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return PullRequest{}, ErrEmptyMessage
	}

	prCopy, err := s.GetPR(prNumber)
	if err != nil {
		return PullRequest{}, err
	}
	s.mu.RLock()
	repoFullName := s.repo.FullName
	applied := cloneEffectiveConfig(s.config)
	s.mu.RUnlock()
	req := SBNNCommentRequest{
		AppliedConfig: cloneEffectiveConfig(applied),
		Repository:    repoFullName,
		PRNumber:      prCopy.Number,
		Branch:        prCopy.Branch,
		Worktree:      prCopy.WorktreeID,
		FilePath:      strings.TrimSpace(filePath),
		Line:          line,
		Body:          body,
	}

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
		AppliedConfig: &applied,
		ID:            result.CommentID,
		Author:        "sbnn",
		Role:          "sbnn",
		Kind:          "review_comment",
		FilePath:      req.FilePath,
		Line:          req.Line,
		Text:          fmt.Sprintf("%s (invoked: `%s`)", result.Formatted, result.CommandLine),
		CreatedAt:     now,
	})
	pr.ReviewStatus = "changes_requested"
	pr.UpdatedAt = now

	if pr.AutoMode {
		taskText := "Address sbnn review comment: " + result.Formatted
		pr.Status = StatusWorking
		if sess, _ := s.findSessionLocked(pr.SessionID); sess != nil {
			sess.ActiveTask = fmt.Sprintf("PR #%d: %s", pr.Number, taskText)
			sess.Status = "working"
			sess.UpdatedAt = now
		}
		s.recordInterSessionWithConfigLocked(s.controller.ID, pr.SessionID, "instruction", pr.Number, taskText, now, applied)
	}

	res := clonePR(*pr)
	snap := s.snapshotLocked()
	s.mu.Unlock()

	s.broadcast(snap)
	return res, nil
}

// RequestReview runs a review via the sbnn integration boundary and updates the PR thread.
func (s *Service) RequestReview(ctx context.Context, prNumber int, focus string) (PullRequest, error) {
	if err := s.ConfigLoadError(); err != nil {
		return PullRequest{}, err
	}
	prCopy, err := s.GetPR(prNumber)
	if err != nil {
		return PullRequest{}, err
	}
	s.mu.RLock()
	repoFullName := s.repo.FullName
	applied := cloneEffectiveConfig(s.config)
	s.mu.RUnlock()
	req := SBNNReviewRequest{
		AppliedConfig: cloneEffectiveConfig(applied),
		Repository:    repoFullName,
		PRNumber:      prCopy.Number,
		Branch:        prCopy.Branch,
		BaseBranch:    prCopy.BaseBranch,
		Worktree:      prCopy.WorktreeID,
		Focus:         strings.TrimSpace(focus),
	}

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
			AppliedConfig: &applied,
			ID:            s.nextID("msg"),
			Author:        "Dashboard Orchestrator",
			Role:          "controller",
			Kind:          "review_request",
			Text:          reqLabel,
			CreatedAt:     now,
		},
		ThreadMessage{
			AppliedConfig: &applied,
			ID:            s.nextID("msg"),
			Author:        "sbnn",
			Role:          "sbnn",
			Kind:          "review_comment",
			Text:          fmt.Sprintf("%s (invoked: `%s`)", reviewRes.Summary, reviewRes.CommandLine),
			CreatedAt:     now.Add(time.Millisecond),
		},
	)
	pr.UpdatedAt = now.Add(time.Millisecond)

	if len(reviewRes.Findings) > 0 {
		pr.ReviewStatus = "changes_requested"
		if pr.AutoMode {
			taskText := fmt.Sprintf("Address %d sbnn review finding(s): %s", len(reviewRes.Findings), reviewRes.Summary)
			pr.Status = StatusWorking
			if sess, _ := s.findSessionLocked(pr.SessionID); sess != nil {
				sess.ActiveTask = fmt.Sprintf("PR #%d: %s", pr.Number, taskText)
				sess.Status = "working"
				sess.UpdatedAt = pr.UpdatedAt
			}
			s.recordInterSessionWithConfigLocked(s.controller.ID, pr.SessionID, "instruction", pr.Number, taskText, pr.UpdatedAt, applied)
		} else {
			pr.Status = StatusWaitingForHuman
			if prevStatus == StatusWorking {
				s.emitNotificationLocked(
					pr,
					StatusWorking,
					StatusWaitingForHuman,
					fmt.Sprintf("PR #%d Review Ready for Human", pr.Number),
					fmt.Sprintf("sbnn review finished for PR #%d; awaiting human decision.", pr.Number),
					pr.UpdatedAt,
				)
			}
		}
	} else {
		pr.Status = StatusWaitingForHuman
		if prevStatus == StatusWorking {
			s.emitNotificationLocked(
				pr,
				StatusWorking,
				StatusWaitingForHuman,
				fmt.Sprintf("PR #%d Review Clean (Waiting for Human)", pr.Number),
				fmt.Sprintf("sbnn review found no open comments on PR #%d.", pr.Number),
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

// AddressReview queues an instruction for the assigned session to address review feedback on a pull request.
func (s *Service) AddressReview(prNumber int, instruction string) (PullRequest, error) {
	if err := s.ConfigLoadError(); err != nil {
		return PullRequest{}, err
	}
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
	if sess, _ := s.findSessionLocked(pr.SessionID); sess != nil {
		sess.ActiveTask = fmt.Sprintf("Addressing review feedback on PR #%d: %s", pr.Number, instruction)
		sess.Status = "working"
		sess.UpdatedAt = now
	}

	pr.Thread = append(pr.Thread, ThreadMessage{
		ID:        s.nextID("msg"),
		Author:    "Dashboard Orchestrator",
		Role:      "controller",
		Kind:      "review_fix",
		Text:      "Dispatch review response to session: " + instruction,
		CreatedAt: now,
	})
	pr.Status = StatusWorking
	pr.UpdatedAt = now

	s.recordInterSessionLocked(s.controller.ID, pr.SessionID, "instruction", pr.Number, instruction, now)

	res := clonePR(*pr)
	snap := s.snapshotLocked()
	s.mu.Unlock()

	s.broadcast(snap)
	return res, nil
}

// RunRoutineAction queues a preset routine operation on the assigned session for a pull request.
func (s *Service) RunRoutineAction(prNumber int, action RoutineAction) (PullRequest, error) {
	if err := s.ConfigLoadError(); err != nil {
		return PullRequest{}, err
	}
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

	var actionDesc string
	switch action {
	case ActionResolveConflicts:
		actionDesc = fmt.Sprintf("Routine action: Resolve merge conflicts on %s against %s.", pr.Branch, pr.BaseBranch)
	case ActionRebaseStack:
		if pr.StackID != "" {
			actionDesc = fmt.Sprintf("Routine action: Rebase stacked PR chain %s in topological order.", pr.StackID)
		} else {
			actionDesc = fmt.Sprintf("Routine action: Rebase branch %s onto %s.", pr.Branch, pr.BaseBranch)
		}
	case ActionFixCI:
		actionDesc = fmt.Sprintf("Routine action: Diagnose and fix failing CI checks on PR #%d.", pr.Number)
	case ActionSyncBranch:
		actionDesc = fmt.Sprintf("Routine action: Sync %s with upstream %s.", pr.Branch, s.repo.DefaultBranch)
	case ActionUpdateSummary:
		actionDesc = fmt.Sprintf("Routine action: Refresh PR #%d description and diff summary.", pr.Number)
	}

	pr.Thread = append(pr.Thread, ThreadMessage{
		ID:        s.nextID("msg"),
		Author:    "Dashboard Orchestrator",
		Role:      "controller",
		Kind:      "routine_action",
		Text:      actionDesc,
		CreatedAt: now,
	})
	pr.Status = StatusWorking
	pr.UpdatedAt = now

	if sess, _ := s.findSessionLocked(pr.SessionID); sess != nil {
		sess.ActiveTask = actionDesc
		sess.Status = "working"
		sess.UpdatedAt = now
	}

	s.recordInterSessionLocked(s.controller.ID, pr.SessionID, "instruction", pr.Number, actionDesc, now)

	res := clonePR(*pr)
	snap := s.snapshotLocked()
	s.mu.Unlock()

	s.broadcast(snap)
	return res, nil
}

// SetAutoMode toggles Auto-Mode for a pull request. When enabled on a PR with open blockers
// (conflicts, failing CI, or requested review changes), an instruction is queued for the assigned session.
func (s *Service) SetAutoMode(prNumber int, enabled bool) (PullRequest, error) {
	if err := s.ConfigLoadError(); enabled && err != nil {
		return PullRequest{}, err
	}
	s.mu.Lock()
	pr, _ := s.findPRLocked(prNumber)
	if pr == nil {
		s.mu.Unlock()
		return PullRequest{}, fmt.Errorf("%w: #%d", ErrPRNotFound, prNumber)
	}

	now := time.Now().UTC()
	pr.AutoMode = enabled

	modeText := "Auto-Mode disabled: PR is now in manual instruction mode."
	if enabled {
		modeText = "Auto-Mode enabled: Session will autonomously process routine actions and review responses."
	}
	pr.Thread = append(pr.Thread, ThreadMessage{
		ID:        s.nextID("msg"),
		Author:    "Dashboard Orchestrator",
		Role:      "controller",
		Kind:      "status_change",
		Text:      modeText,
		CreatedAt: now,
	})
	pr.UpdatedAt = now

	if enabled && (pr.HasConflicts || pr.CIStatus == "failing" || pr.ReviewStatus == "changes_requested") {
		taskText := fmt.Sprintf("Auto-Mode enabled on PR #%d: resolve pending conflicts, CI failures, or review comments.", pr.Number)
		pr.Status = StatusWorking
		if sess, _ := s.findSessionLocked(pr.SessionID); sess != nil {
			sess.ActiveTask = taskText
			sess.Status = "working"
			sess.UpdatedAt = now
		}
		s.recordInterSessionLocked(s.controller.ID, pr.SessionID, "instruction", pr.Number, taskText, now)
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
		if status == StatusWaitingForHuman {
			sess.Status = "idle"
		} else {
			sess.Status = "working"
		}
		sess.UpdatedAt = now
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
		s.markInstructionsHandledLocked(pr.SessionID, pr.Number)
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
	s.reloadStateFileIfChangedLocked()
	s.dirty = true
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

// SendDirectSessionInstruction queues a direct instruction for a worker session outside any specific PR thread.
func (s *Service) SendDirectSessionInstruction(sessionID string, text string) (Session, error) {
	if err := s.ConfigLoadError(); err != nil {
		return Session{}, err
	}
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

	res := cloneSession(*sess)
	snap := s.snapshotLocked()
	s.mu.Unlock()

	s.broadcast(snap)
	return res, nil
}

// SendControllerInstruction queues a human orchestration instruction for the dashboard controller session.
func (s *Service) SendControllerInstruction(text string) (Session, error) {
	if err := s.ConfigLoadError(); err != nil {
		return Session{}, err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return Session{}, ErrEmptyMessage
	}

	s.mu.Lock()
	s.reloadStateFileIfChangedLocked()
	s.dirty = true
	now := time.Now().UTC()
	cfg := cloneEffectiveConfig(s.config)
	userMsg := SessionMessage{
		AppliedConfig: &cfg,
		ID:            s.nextID("cmsg"),
		From:          "human",
		To:            s.controller.ID,
		Direction:     "instruction",
		Text:          text,
		CreatedAt:     now,
	}
	s.controller.Messages = append(s.controller.Messages, userMsg)
	s.controller.ActiveTask = text
	s.controller.Status = "working"
	s.controller.UpdatedAt = now

	res := cloneSession(s.controller)
	snap := s.snapshotLocked()
	s.mu.Unlock()

	s.broadcast(snap)
	return res, nil
}

// ReportSessionResult records a status update and execution result from a worker session back to the dashboard.
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
		sess.UpdatedAt = now
	}

	pr.Status = status
	if status == StatusWaitingForHuman {
		pr.HasConflicts = false
		if pr.CIStatus == "failing" || pr.CIStatus == "running" {
			pr.CIStatus = "passing"
		}
	}
	pr.UpdatedAt = now
	pr.Thread = append(pr.Thread, ThreadMessage{
		ID:        s.nextID("msg"),
		Author:    sessName,
		Role:      "session",
		Kind:      "status_change",
		Text:      report,
		CreatedAt: now,
	})
	s.markInstructionsHandledLocked(sessionID, pr.Number)
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
	s.reloadStateFileIfChangedLocked()
	s.dirty = true
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

// UpsertWorktree registers a new git worktree or updates an existing worktree by ID.
func (s *Service) UpsertWorktree(wt Worktree) (Worktree, error) {
	wt.ID = strings.TrimSpace(wt.ID)
	if wt.ID == "" {
		return Worktree{}, errors.New("worktree id is required")
	}
	if strings.TrimSpace(wt.Branch) == "" {
		wt.Branch = "main"
	}

	s.mu.Lock()
	s.reloadStateFileIfChangedLocked()
	s.dirty = true
	found := false
	for i := range s.worktrees {
		if s.worktrees[i].ID == wt.ID {
			s.worktrees[i] = wt
			found = true
			break
		}
	}
	if !found {
		s.worktrees = append(s.worktrees, wt)
	}
	snap := s.snapshotLocked()
	s.mu.Unlock()

	s.broadcast(snap)
	return wt, nil
}

// UpsertSession registers a new worker session or updates an existing session by ID.
func (s *Service) UpsertSession(sess Session) (Session, error) {
	sess.ID = strings.TrimSpace(sess.ID)
	if sess.ID == "" {
		return Session{}, errors.New("session id is required")
	}
	if sess.ModelTier == "" {
		sess.ModelTier = TierBamboo
	}
	if !ValidModelTier(sess.ModelTier) {
		return Session{}, fmt.Errorf("%w: %s", ErrInvalidModelTier, sess.ModelTier)
	}
	if sess.SubagentModelTier == "" {
		sess.SubagentModelTier = TierPlum
	}
	if !ValidModelTier(sess.SubagentModelTier) {
		return Session{}, fmt.Errorf("%w: %s", ErrInvalidModelTier, sess.SubagentModelTier)
	}

	s.mu.Lock()
	s.reloadStateFileIfChangedLocked()
	s.dirty = true
	now := time.Now().UTC()
	sess.Role = "worker"
	if strings.TrimSpace(sess.Name) == "" {
		sess.Name = sess.ID
	}
	if strings.TrimSpace(sess.Status) == "" {
		sess.Status = "working"
	}
	sess.ResolvedModel = s.modelConfig.Resolve(sess.ModelTier)
	sess.ResolvedSubagentModel = s.modelConfig.Resolve(sess.SubagentModelTier)
	sess.UpdatedAt = now

	existing, idx := s.findSessionLocked(sess.ID)
	if existing != nil && idx >= 0 {
		if len(sess.Messages) == 0 {
			sess.Messages = existing.Messages
		}
		s.sessions[idx] = sess
	} else {
		if sess.Messages == nil {
			sess.Messages = make([]SessionMessage, 0)
		}
		s.sessions = append(s.sessions, sess)
	}

	res := cloneSession(sess)
	snap := s.snapshotLocked()
	s.mu.Unlock()

	s.broadcast(snap)
	return res, nil
}

// UpsertPR registers a new pull request or updates an existing pull request by number.
func (s *Service) UpsertPR(in PullRequest) (PullRequest, error) {
	if in.Number <= 0 {
		return PullRequest{}, errors.New("pull request number must be positive")
	}
	if strings.TrimSpace(in.Title) == "" {
		return PullRequest{}, errors.New("pull request title is required")
	}
	if in.Status == "" {
		in.Status = StatusWorking
	}
	if in.Status != StatusWaitingForHuman && in.Status != StatusWorking {
		return PullRequest{}, fmt.Errorf("%w: %s", ErrInvalidPRStatus, in.Status)
	}
	if in.ModelTier == "" {
		in.ModelTier = TierBamboo
	}
	if !ValidModelTier(in.ModelTier) {
		return PullRequest{}, fmt.Errorf("%w: %s", ErrInvalidModelTier, in.ModelTier)
	}

	s.mu.Lock()
	s.reloadStateFileIfChangedLocked()
	s.dirty = true
	now := time.Now().UTC()

	if in.ID == "" {
		in.ID = fmt.Sprintf("pr-%d", in.Number)
	}
	if in.BaseBranch == "" {
		in.BaseBranch = s.repo.DefaultBranch
	}
	if in.CIStatus == "" {
		in.CIStatus = "passing"
	}
	if in.ReviewStatus == "" {
		in.ReviewStatus = "review_pending"
	}
	in.ResolvedModel = s.modelConfig.Resolve(in.ModelTier)
	in.UpdatedAt = now

	existing, idx := s.findPRLocked(in.Number)
	var prevStatus PRStatus
	if existing != nil && idx >= 0 {
		prevStatus = existing.Status
		if len(in.Thread) == 0 {
			in.Thread = existing.Thread
		}
		s.prs[idx] = in
	} else {
		if len(in.Thread) == 0 {
			in.Thread = []ThreadMessage{
				{
					ID:        s.nextID("msg"),
					Author:    in.SessionID,
					Role:      "session",
					Kind:      "status_change",
					Text:      fmt.Sprintf("Registered PR #%d (%s) in prdash.", in.Number, in.Title),
					CreatedAt: now,
				},
			}
		}
		s.prs = slices.Insert(s.prs, 0, in)
	}

	if in.Status == StatusWaitingForHuman && prevStatus != StatusWaitingForHuman {
		s.emitNotificationLocked(
			&in,
			prevStatus,
			StatusWaitingForHuman,
			fmt.Sprintf("PR #%d is Waiting for Human", in.Number),
			fmt.Sprintf("PR #%d (%s) is ready for human review.", in.Number, in.Title),
			now,
		)
	}

	res := clonePR(in)
	snap := s.snapshotLocked()
	s.mu.Unlock()

	s.broadcast(snap)
	return res, nil
}
