package prdash

import (
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

var ErrResponsePolicyConflict = errors.New("review response conflicts with repository policy")
var ErrResponseTransition = errors.New("invalid review response transition")

// ResponseFinding carries explicit provenance. Free-form text is never used to
// guess whether a finding is human-authored or a question.
type ResponseFinding struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Author string `json:"author"`
	Text   string `json:"text"`
	Policy string `json:"policy"`
}
type ResponseRequest struct {
	Findings         []ResponseFinding `json:"findings"`
	Instruction      string            `json:"instruction,omitempty"`
	RequestedActions []string          `json:"requested_actions,omitempty"`
}
type ResponseStep struct {
	Action     string    `json:"action"`
	Gate       string    `json:"gate"`
	Status     string    `json:"status"`
	FindingIDs []string  `json:"finding_ids"`
	Round      int       `json:"round"`
	Detail     string    `json:"detail,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
}
type ResponsePlan struct {
	Config      EffectiveConfig   `json:"config"`
	Findings    []ResponseFinding `json:"findings"`
	Instruction string            `json:"instruction,omitempty"`
	Steps       []ResponseStep    `json:"steps"`
	Conflicts   []string          `json:"conflicts"`
}
type ResponseWorkflow struct {
	ID     string         `json:"id"`
	Plan   ResponsePlan   `json:"plan"`
	Steps  []ResponseStep `json:"steps"`
	Status string         `json:"status"`
	Round  int            `json:"round"`
}
type ResponseStepResult struct {
	Reviewed     bool   `json:"reviewed"`
	Status       string `json:"status"` // completed, failed, disputed, interrupted
	Detail       string `json:"detail,omitempty"`
	Verdict      string `json:"verdict,omitempty"`
	OpenComments int    `json:"open_comments"`
}

func responseGates(rr ReviewResponseConfig) map[string]string {
	return map[string]string{"fix": rr.Fix, "test": rr.Test, "commit": rr.Commit, "push": rr.Push, "reply": rr.Reply, "resolve": rr.Resolve, "rereview": rr.Rereview.Gate}
}
func validateResponseOrder(rr ReviewResponseConfig) error {
	positions := map[string]int{}
	gates := responseGates(rr)
	for i, action := range rr.Steps {
		if _, ok := gates[action]; !ok {
			return fmt.Errorf("unsupported step %q", action)
		}
		if _, ok := positions[action]; ok {
			return fmt.Errorf("duplicate step %q", action)
		}
		positions[action] = i
	}
	for action, gate := range gates {
		if gate != "off" {
			if _, ok := positions[action]; !ok {
				return fmt.Errorf("enabled action %q must occur in steps", action)
			}
		}
	}
	for _, dependency := range [][2]string{{"fix", "test"}, {"test", "commit"}, {"commit", "push"}, {"reply", "resolve"}, {"test", "rereview"}} {
		before, beforeOK := positions[dependency[0]]
		after, afterOK := positions[dependency[1]]
		if beforeOK && afterOK && gates[dependency[0]] != "off" && gates[dependency[1]] != "off" && before > after {
			return fmt.Errorf("%s must precede %s", dependency[0], dependency[1])
		}
	}
	return nil
}

// PlanReviewResponse only computes policy. It never executes commands. An
// instruction is context, not an override; free-form context gets an explicit
// human review checkpoint instead of unreliable keyword-based permission checks.
func PlanReviewResponse(config EffectiveConfig, request ResponseRequest) (ResponsePlan, error) {
	plan := ResponsePlan{Config: cloneEffectiveConfig(config), Instruction: request.Instruction, Findings: slices.Clone(request.Findings), Steps: []ResponseStep{}, Conflicts: []string{}}
	rr := config.Config.ReviewResponse
	if err := validateRepoConfig(config.SourcePath, config.Config, nil); err != nil {
		return plan, fmt.Errorf("%w: %v", ErrResponsePolicyConflict, err)
	}
	gates := responseGates(rr)
	for _, action := range request.RequestedActions {
		gate, ok := gates[action]
		if !ok || gate == "off" {
			plan.Conflicts = append(plan.Conflicts, fmt.Sprintf("requested action %q is unsupported or disabled", action))
		}
	}
	ids := map[string]bool{}
	autoIDs := []string{}
	replyIDs := []string{}
	decisionIDs := []string{}
	for i := range plan.Findings {
		finding := &plan.Findings[i]
		if strings.TrimSpace(finding.ID) == "" || ids[finding.ID] {
			return plan, fmt.Errorf("%w: finding IDs must be nonempty and unique", ErrResponsePolicyConflict)
		}
		ids[finding.ID] = true
		switch finding.Kind {
		case "ai":
			finding.Policy = rr.FindingPolicies.AI
		case "human":
			finding.Policy = rr.FindingPolicies.Human
		case "question":
			finding.Policy = rr.FindingPolicies.Question
		default:
			return plan, fmt.Errorf("%w: unsupported finding kind %q", ErrResponsePolicyConflict, finding.Kind)
		}
		switch finding.Policy {
		case "auto":
			autoIDs = append(autoIDs, finding.ID)
		case "prepare_reply":
			replyIDs = append(replyIDs, finding.ID)
		case "human_decide":
			decisionIDs = append(decisionIDs, finding.ID)
		default:
			return plan, fmt.Errorf("%w: unsupported finding policy", ErrResponsePolicyConflict)
		}
	}
	if strings.TrimSpace(request.Instruction) != "" {
		plan.Steps = append(plan.Steps, ResponseStep{Action: "instruction_review", Gate: "confirm", Status: "pending", Detail: "Review task-specific context; repository gates remain mandatory."})
	}
	if len(decisionIDs) > 0 {
		plan.Steps = append(plan.Steps, ResponseStep{Action: "human_decision", Gate: "confirm", Status: "pending", FindingIDs: decisionIDs})
	}
	if len(replyIDs) > 0 {
		plan.Steps = append(plan.Steps, ResponseStep{Action: "prepare_reply", Gate: "auto", Status: "pending", FindingIDs: replyIDs})
	}
	for _, action := range rr.Steps {
		gate := gates[action]
		step := ResponseStep{Action: action, Gate: gate, Status: "pending", FindingIDs: slices.Clone(autoIDs)}
		if action == "reply" {
			step.FindingIDs = append(step.FindingIDs, replyIDs...)
		}
		if gate == "off" || (action == "rereview" && rr.Rereview.MaxRounds == 0) {
			step.Status = "skipped"
			step.Detail = "Disabled by repository policy."
		}
		if len(step.FindingIDs) == 0 && action != "rereview" {
			step.Status = "skipped"
			step.Detail = "No findings authorize this action."
		}
		plan.Steps = append(plan.Steps, step)
	}
	return plan, nil
}
func cloneResponseSteps(in []ResponseStep) []ResponseStep {
	out := slices.Clone(in)
	for i := range out {
		out[i].FindingIDs = slices.Clone(out[i].FindingIDs)
	}
	return out
}
func cloneResponsePlan(in ResponsePlan) ResponsePlan {
	out := in
	out.Config = cloneEffectiveConfig(in.Config)
	out.Findings = slices.Clone(in.Findings)
	out.Steps = cloneResponseSteps(in.Steps)
	out.Conflicts = slices.Clone(in.Conflicts)
	return out
}
func cloneResponseWorkflow(in ResponseWorkflow) ResponseWorkflow {
	out := in
	out.Plan = cloneResponsePlan(in.Plan)
	out.Steps = cloneResponseSteps(in.Steps)
	return out
}
func NewResponseWorkflow(plan ResponsePlan) (ResponseWorkflow, error) {
	if len(plan.Conflicts) > 0 {
		return ResponseWorkflow{}, fmt.Errorf("%w: %s", ErrResponsePolicyConflict, strings.Join(plan.Conflicts, "; "))
	}
	rebuilt, err := PlanReviewResponse(plan.Config, ResponseRequest{Findings: plan.Findings, Instruction: plan.Instruction})
	if err != nil {
		return ResponseWorkflow{}, err
	}
	return ResponseWorkflow{ID: "response-" + rand.Text(), Plan: cloneResponsePlan(rebuilt), Steps: cloneResponseSteps(rebuilt.Steps), Status: "pending"}, nil
}

// BeginResponseStep returns a copy with at most one running step. approved is
// explicit approval for this checkpoint only; Auto-Mode must never supply it.
func BeginResponseStep(in ResponseWorkflow, approved bool) (ResponseWorkflow, error) {
	out := cloneResponseWorkflow(in)
	if out.Status != "pending" && out.Status != "waiting" {
		return out, fmt.Errorf("%w: workflow is %s", ErrResponseTransition, out.Status)
	}
	for i := range out.Steps {
		step := &out.Steps[i]
		if step.Status == "completed" || step.Status == "skipped" {
			continue
		}
		if step.Status != "pending" && step.Status != "waiting" {
			return out, fmt.Errorf("%w: step is %s", ErrResponseTransition, step.Status)
		}
		if step.Gate == "confirm" && !approved {
			step.Status = "waiting"
			out.Status = "waiting"
			return out, nil
		}
		step.Status = "running"
		step.UpdatedAt = time.Now().UTC()
		out.Status = "running"
		return out, nil
	}
	out.Status = "completed"
	return out, nil
}

func CompleteResponseStep(in ResponseWorkflow, result ResponseStepResult) (ResponseWorkflow, error) {
	out := cloneResponseWorkflow(in)
	if out.Status != "running" {
		return out, fmt.Errorf("%w: no running step", ErrResponseTransition)
	}
	if !slices.Contains([]string{"completed", "failed", "disputed", "interrupted"}, result.Status) {
		return out, fmt.Errorf("%w: unsupported result %q", ErrResponseTransition, result.Status)
	}
	for i := range out.Steps {
		step := &out.Steps[i]
		if step.Status != "running" {
			continue
		}
		if step.Action == "rereview" && result.Status == "completed" && (!result.Reviewed || !slices.Contains([]string{"approved", "changes_requested", "commented"}, result.Verdict) || result.OpenComments < 0) {
			return out, fmt.Errorf("%w: re-review completion requires a confirmed verdict and comment count", ErrResponseTransition)
		}
		step.Status = result.Status
		step.Detail = result.Detail
		step.UpdatedAt = time.Now().UTC()
		if result.Status != "completed" {
			out.Status = result.Status
			return out, nil
		}
		out.Status = "pending"
		if step.Action != "rereview" {
			return out, nil
		}
		rr := out.Plan.Config.Config.ReviewResponse.Rereview
		if (rr.StopOn == "approved" && result.Verdict == "approved") || (rr.StopOn == "no_open_comments" && result.OpenComments == 0) {
			out.Status = "completed"
			return out, nil
		}
		out.Round++
		if out.Round >= rr.MaxRounds {
			out.Status = "waiting"
			out.Steps = append(out.Steps, ResponseStep{Action: "round_limit", Gate: "confirm", Status: "waiting", Round: out.Round, Detail: "Re-review round limit reached; no further automatic actions."})
			return out, nil
		}
		next := cloneResponseSteps(out.Plan.Steps)
		for j := range next {
			next[j].Round = out.Round
		}
		out.Steps = append(out.Steps, next...)
		return out, nil
	}
	return out, fmt.Errorf("%w: no running step", ErrResponseTransition)
}

// PreviewResponseWorkflow binds classifications to existing PR thread entries.
// Unknown provenance is handled as human-authored, never automatically as AI.
func (s *Service) PreviewResponseWorkflow(number int, request ResponseRequest) (ResponsePlan, error) {
	if err := s.ConfigLoadError(); err != nil {
		return ResponsePlan{}, err
	}
	pr, err := s.GetPR(number)
	if err != nil {
		return ResponsePlan{}, err
	}
	messages := map[string]ThreadMessage{}
	for _, message := range pr.Thread {
		if message.Kind == "review_comment" {
			messages[message.ID] = message
		}
	}
	request.Findings = slices.Clone(request.Findings)
	if len(request.Findings) == 0 {
		for _, message := range pr.Thread {
			if message.Kind != "review_comment" {
				continue
			}
			kind := "human"
			if message.Role == "reviewer" || message.Role == "session" {
				kind = "ai"
			}
			request.Findings = append(request.Findings, ResponseFinding{ID: message.ID, Kind: kind})
		}
	}
	for i := range request.Findings {
		finding := &request.Findings[i]
		message, ok := messages[finding.ID]
		if !ok {
			return ResponsePlan{}, fmt.Errorf("%w: finding %q is not a PR review comment", ErrResponsePolicyConflict, finding.ID)
		}
		if finding.Kind == "ai" && message.Role != "reviewer" && message.Role != "session" {
			return ResponsePlan{}, fmt.Errorf("%w: finding %q has no AI provenance", ErrResponsePolicyConflict, finding.ID)
		}
		finding.Author = message.Author
		finding.Text = message.Text
	}
	return PlanReviewResponse(s.EffectiveConfig(), request)
}
