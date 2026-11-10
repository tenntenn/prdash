// Package prdash provides the core domain model, state store, HTTP dashboard server,
// and Model Context Protocol (MCP) server for managing pull requests created by AI agent sessions.
package prdash

import (
	"strings"
	"time"
)

// PRStatus represents who currently owns the next action on a pull request.
type PRStatus string

const (
	// StatusWaitingForHuman indicates the PR is ready for human inspection or decision.
	StatusWaitingForHuman PRStatus = "waiting_for_human"
	// StatusWorking indicates the assigned agent session is actively modifying the PR.
	StatusWorking PRStatus = "working"
)

// ModelTier represents the three-tier model selection preset (Pine / Bamboo / Plum).
type ModelTier string

const (
	// TierPine represents the high-capability, high-cost model preset (Matsu / Pine).
	TierPine ModelTier = "pine"
	// TierBamboo represents the standard balanced model preset (Take / Bamboo).
	TierBamboo ModelTier = "bamboo"
	// TierPlum represents the fast, lightweight, low-cost model preset (Ume / Plum).
	TierPlum ModelTier = "plum"
)

// ValidModelTier reports whether tier is a supported three-tier preset.
func ValidModelTier(tier ModelTier) bool {
	switch tier {
	case TierPine, TierBamboo, TierPlum:
		return true
	default:
		return false
	}
}

// RoutineAction identifies a built-in one-click operation for a pull request.
type RoutineAction string

const (
	ActionResolveConflicts RoutineAction = "resolve_conflicts"
	ActionRebaseStack      RoutineAction = "rebase_stack"
	ActionFixCI            RoutineAction = "fix_ci"
	ActionSyncBranch       RoutineAction = "sync_branch"
	ActionUpdateSummary    RoutineAction = "update_summary"
)

// ModelPresetConfig maps the three-tier presets (Pine, Bamboo, Plum) to concrete model identifiers.
type ModelPresetConfig struct {
	Pine   string `json:"pine"`
	Bamboo string `json:"bamboo"`
	Plum   string `json:"plum"`
}

// Resolve returns the concrete model name for the given tier.
func (c ModelPresetConfig) Resolve(tier ModelTier) string {
	switch tier {
	case TierPine:
		return c.Pine
	case TierBamboo:
		return c.Bamboo
	case TierPlum:
		return c.Plum
	default:
		return c.Bamboo
	}
}

// DefaultModelPresetConfig returns the default mapping for Pine, Bamboo, and Plum model presets.
func DefaultModelPresetConfig() ModelPresetConfig {
	return ModelPresetConfig{
		Pine:   "claude-opus-4-6",
		Bamboo: "claude-sonnet-4-6",
		Plum:   "claude-haiku-4-5",
	}
}

// ParseRepositoryInfo builds RepositoryInfo from a "owner/name" string.
func ParseRepositoryInfo(repoFullName string) RepositoryInfo {
	cleaned := strings.TrimSpace(repoFullName)
	if cleaned == "" {
		return RepositoryInfo{
			DefaultBranch: "main",
		}
	}
	owner := ""
	name := cleaned
	if parts := strings.SplitN(cleaned, "/", 2); len(parts) == 2 && parts[0] != "" && parts[1] != "" {
		owner = parts[0]
		name = parts[1]
		cleaned = owner + "/" + name
	}
	return RepositoryInfo{
		Owner:         owner,
		Name:          name,
		FullName:      cleaned,
		DefaultBranch: "main",
	}
}

// RepositoryInfo describes the target repository managed by a dashboard instance.
type RepositoryInfo struct {
	Owner         string `json:"owner"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	LocalPath     string `json:"local_path"`
}

// Worktree represents a git worktree bound to an agent session.
type Worktree struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	Branch    string `json:"branch"`
	HeadSHA   string `json:"head_sha"`
	Clean     bool   `json:"clean"`
	SessionID string `json:"session_id"`
}

// SessionMessage represents an instruction or report exchanged with an agent session.
type SessionMessage struct {
	InstructionID      string           `json:"instruction_id,omitempty"`
	TargetCommit       string           `json:"target_commit,omitempty"`
	ExecutionState     InstructionState `json:"execution_state,omitempty"`
	ConsumerID         string           `json:"consumer_id,omitempty"`
	Attempt            int              `json:"attempt,omitempty"`
	Result             string           `json:"result,omitempty"`
	ResultStatus       PRStatus         `json:"result_status,omitempty"`
	ResultApplied      bool             `json:"result_applied,omitempty"`
	ExecutionUpdatedAt time.Time        `json:"execution_updated_at"`

	AppliedConfig *EffectiveConfig `json:"applied_config,omitempty"`
	ID            string           `json:"id"`
	From          string           `json:"from"`
	To            string           `json:"to"`
	Direction     string           `json:"direction"` // "instruction" or "report"
	PRNumber      int              `json:"pr_number,omitempty"`
	Text          string           `json:"text"`
	Handled       bool             `json:"handled,omitempty"`
	CreatedAt     time.Time        `json:"created_at"`
}

// Session represents an AI coding agent session bound to a git worktree,
// or the controller session running the dashboard itself.
type Session struct {
	ID                    string           `json:"id"`
	Name                  string           `json:"name"`
	Role                  string           `json:"role"` // "controller" or "worker"
	AgentType             string           `json:"agent_type"`
	WorktreeID            string           `json:"worktree_id,omitempty"`
	Status                string           `json:"status"` // "idle", "working", "coordinating"
	ModelTier             ModelTier        `json:"model_tier"`
	ResolvedModel         string           `json:"resolved_model"`
	SubagentModelTier     ModelTier        `json:"subagent_model_tier"`
	ResolvedSubagentModel string           `json:"resolved_subagent_model"`
	ActiveTask            string           `json:"active_task"`
	Messages              []SessionMessage `json:"messages"`
	UpdatedAt             time.Time        `json:"updated_at"`
}

// ThreadMessage represents a message or event in a pull request's interaction thread.
type ThreadMessage struct {
	AppliedConfig *EffectiveConfig `json:"applied_config,omitempty"`
	ID            string           `json:"id"`
	Author        string           `json:"author"`
	Role          string           `json:"role"` // "human", "session", "controller", "sbnn", "system"
	Kind          string           `json:"kind"` // "message", "review_comment", "review_request", "review_fix", "routine_action", "status_change"
	Text          string           `json:"text"`
	FilePath      string           `json:"file_path,omitempty"`
	Line          int              `json:"line,omitempty"`
	CreatedAt     time.Time        `json:"created_at"`
}

// PullRequest represents a pull request created and maintained by an agent session.
type PullRequest struct {
	HeadSHA        string          `json:"head_sha,omitempty"`
	ID             string          `json:"id"`
	Number         int             `json:"number"`
	Title          string          `json:"title"`
	Description    string          `json:"description"`
	Branch         string          `json:"branch"`
	BaseBranch     string          `json:"base_branch"`
	SessionID      string          `json:"session_id"`
	WorktreeID     string          `json:"worktree_id"`
	StackID        string          `json:"stack_id,omitempty"`
	StackOrder     int             `json:"stack_order,omitempty"`
	StackSize      int             `json:"stack_size,omitempty"`
	ParentPRNumber int             `json:"parent_pr_number,omitempty"`
	Status         PRStatus        `json:"status"`
	AutoMode       bool            `json:"auto_mode"`
	ModelTier      ModelTier       `json:"model_tier"`
	ResolvedModel  string          `json:"resolved_model"`
	CIStatus       string          `json:"ci_status"` // "passing", "running", "failing"
	HasConflicts   bool            `json:"has_conflicts"`
	ReviewStatus   string          `json:"review_status"` // "approved", "changes_requested", "review_pending"
	Additions      int             `json:"additions"`
	Deletions      int             `json:"deletions"`
	ChangedFiles   int             `json:"changed_files"`
	Thread         []ThreadMessage `json:"thread"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

// StackGroup represents an ordered chain of stacked pull requests or a single standalone pull request.
type StackGroup struct {
	ID           string        `json:"id"`
	Title        string        `json:"title"`
	IsStacked    bool          `json:"is_stacked"`
	PullRequests []PullRequest `json:"pull_requests"`
}

// Notification represents a human-facing alert emitted when a pull request changes state.
type Notification struct {
	ID         string    `json:"id"`
	PRNumber   int       `json:"pr_number"`
	PRTitle    string    `json:"pr_title"`
	SessionID  string    `json:"session_id"`
	Title      string    `json:"title"`
	Message    string    `json:"message"`
	FromStatus PRStatus  `json:"from_status"`
	ToStatus   PRStatus  `json:"to_status"`
	Read       bool      `json:"read"`
	CreatedAt  time.Time `json:"created_at"`
}

// DashboardSummary holds aggregate metrics for the repository dashboard header.
type DashboardSummary struct {
	TotalPRs         int `json:"total_prs"`
	WaitingForHuman  int `json:"waiting_for_human"`
	AgentWorking     int `json:"agent_working"`
	StackedChains    int `json:"stacked_chains"`
	AutoModeEnabled  int `json:"auto_mode_enabled"`
	UnreadAlerts     int `json:"unread_alerts"`
	ActiveSessions   int `json:"active_sessions"`
	ManagedWorktrees int `json:"managed_worktrees"`
}

// Snapshot contains the complete state of the dashboard for a target repository.
type Snapshot struct {
	StatePath         string            `json:"state_path,omitempty"`
	StateError        string            `json:"state_error,omitempty"`
	Config            EffectiveConfig   `json:"config"`
	ConfigError       string            `json:"config_error,omitempty"`
	Repository        RepositoryInfo    `json:"repository"`
	ModelConfig       ModelPresetConfig `json:"model_config"`
	ControllerSession Session           `json:"controller_session"`
	Sessions          []Session         `json:"sessions"`
	Worktrees         []Worktree        `json:"worktrees"`
	PullRequests      []PullRequest     `json:"pull_requests"`
	Stacks            []StackGroup      `json:"stacks"`
	Notifications     []Notification    `json:"notifications"`
	Summary           DashboardSummary  `json:"summary"`
}
