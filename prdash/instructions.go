package prdash

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// InstructionState separates delivery from execution and terminal outcomes.
type InstructionState string

const (
	InstructionQueued    InstructionState = "queued"
	InstructionDelivered InstructionState = "delivered"
	InstructionRunning   InstructionState = "running"
	InstructionCompleted InstructionState = "completed"
	InstructionFailed    InstructionState = "failed"
)

var (
	ErrInstructionNotFound    = errors.New("instruction not found")
	ErrInstructionSession     = errors.New("instruction session does not match")
	ErrInstructionConflict    = errors.New("instruction execution conflict")
	ErrInstructionInput       = errors.New("invalid instruction input")
	ErrInstructionPersistence = errors.New("instruction state could not be persisted")
)

// InstructionClaimRequest identifies a stable consumer. Distinct consumers must
// use distinct IDs. A restarted consumer can recover its existing claim.
type InstructionClaimRequest struct {
	SessionID     string `json:"session_id"`
	InstructionID string `json:"instruction_id"`
	ConsumerID    string `json:"consumer_id"`
	TargetCommit  string `json:"target_commit,omitempty"`
}

// InstructionClaim returns the capability needed to report this attempt. The
// token is persisted privately and is never included in dashboard snapshots.
type InstructionClaim struct {
	Instruction SessionMessage `json:"instruction"`
	ClaimToken  string         `json:"claim_token"`
}

// InstructionResultRequest correlates an immutable result with one attempt.
type InstructionResultRequest struct {
	SessionID     string           `json:"session_id"`
	InstructionID string           `json:"instruction_id"`
	ConsumerID    string           `json:"consumer_id"`
	ClaimToken    string           `json:"claim_token"`
	Attempt       int              `json:"attempt"`
	TargetCommit  string           `json:"target_commit,omitempty"`
	Outcome       InstructionState `json:"outcome"`
	Status        PRStatus         `json:"status"`
	Report        string           `json:"report"`
}

// InstructionRetryRequest explicitly requeues a failed attempt. A running claim
// never expires automatically, since its external effects may still be running.
type InstructionRetryRequest struct {
	SessionID     string `json:"session_id"`
	InstructionID string `json:"instruction_id"`
	ConsumerID    string `json:"consumer_id"`
	ClaimToken    string `json:"claim_token"`
	Attempt       int    `json:"attempt"`
}

func (s *Service) GetInstruction(sessionID, instructionID string) (SessionMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadStateFileIfChangedLocked()
	if s.stateErr != nil {
		return SessionMessage{}, s.stateErr
	}
	return s.instructionLocked(sessionID, instructionID)
}

// AckInstruction acknowledges only delivery of the specified instruction.
func (s *Service) AckInstruction(sessionID, instructionID string) (SessionMessage, error) {
	return s.mutateInstruction(sessionID, instructionID, func(msg *SessionMessage) error {
		if msg.ExecutionState == InstructionQueued {
			msg.ExecutionState = InstructionDelivered
			msg.Handled = true
			msg.ExecutionUpdatedAt = time.Now().UTC()
		}
		return nil
	})
}

// ClaimInstruction atomically claims one instruction across Service instances
// sharing a persistent state file, or within this Service for in-memory state.
func (s *Service) ClaimInstruction(req InstructionClaimRequest) (InstructionClaim, error) {
	req.ConsumerID = strings.TrimSpace(req.ConsumerID)
	if req.ConsumerID == "" {
		return InstructionClaim{}, fmt.Errorf("%w: consumer_id is required", ErrInstructionInput)
	}
	var token string
	msg, err := s.mutateInstruction(req.SessionID, req.InstructionID, func(msg *SessionMessage) error {
		if req.TargetCommit != "" && msg.TargetCommit != "" && req.TargetCommit != msg.TargetCommit {
			return fmt.Errorf("%w: target commit differs", ErrInstructionConflict)
		}
		if msg.ExecutionState == InstructionRunning && msg.ConsumerID == req.ConsumerID {
			token = s.instructionClaims[msg.ID]
			if token == "" {
				return fmt.Errorf("%w: missing persisted claim", ErrInstructionConflict)
			}
			return nil
		}
		if msg.ExecutionState != InstructionQueued && msg.ExecutionState != InstructionDelivered {
			return fmt.Errorf("%w: instruction is %s", ErrInstructionConflict, msg.ExecutionState)
		}
		if msg.PRNumber > 0 {
			pr, _ := s.findPRLocked(msg.PRNumber)
			if pr == nil {
				return ErrPRNotFound
			}
			if pr.SessionID != msg.To {
				return ErrInstructionSession
			}
			if msg.TargetCommit == "" {
				msg.TargetCommit = strings.TrimSpace(req.TargetCommit)
			}
			if msg.TargetCommit == "" {
				return fmt.Errorf("%w: target_commit is required for this PR instruction", ErrInstructionInput)
			}
		}
		var bytes [32]byte
		if _, err := rand.Read(bytes[:]); err != nil {
			return err
		}
		token = hex.EncodeToString(bytes[:])
		if s.instructionClaims == nil {
			s.instructionClaims = make(map[string]string)
		}
		s.instructionClaims[msg.ID] = token
		msg.ExecutionState = InstructionRunning
		msg.ConsumerID = req.ConsumerID
		msg.Attempt++
		msg.Handled = true
		msg.ExecutionUpdatedAt = time.Now().UTC()
		return nil
	})
	if err != nil {
		return InstructionClaim{}, err
	}
	return InstructionClaim{Instruction: msg, ClaimToken: token}, nil
}

// ReportInstructionResult records one attempt's result. Duplicate identical
// reports are idempotent. Stale results remain in history but do not change PRs.
func (s *Service) ReportInstructionResult(req InstructionResultRequest) (SessionMessage, error) {
	req.Report = strings.TrimSpace(req.Report)
	if req.Report == "" {
		return SessionMessage{}, ErrEmptyMessage
	}
	if req.Outcome != InstructionCompleted && req.Outcome != InstructionFailed {
		return SessionMessage{}, fmt.Errorf("%w: outcome must be completed or failed", ErrInstructionInput)
	}
	if req.Status != StatusWorking && req.Status != StatusWaitingForHuman {
		return SessionMessage{}, ErrInvalidPRStatus
	}
	return s.mutateInstruction(req.SessionID, req.InstructionID, func(msg *SessionMessage) error {
		if err := s.validateInstructionAttemptLocked(*msg, req.ConsumerID, req.ClaimToken, req.Attempt); err != nil {
			return err
		}
		if req.TargetCommit != msg.TargetCommit {
			return fmt.Errorf("%w: result target commit differs", ErrInstructionConflict)
		}
		if msg.ExecutionState == InstructionCompleted || msg.ExecutionState == InstructionFailed {
			if msg.ExecutionState == req.Outcome && msg.Result == req.Report && msg.ResultStatus == req.Status {
				return nil
			}
			return fmt.Errorf("%w: terminal result is immutable", ErrInstructionConflict)
		}
		if msg.ExecutionState != InstructionRunning {
			return fmt.Errorf("%w: instruction is not running", ErrInstructionConflict)
		}
		now := time.Now().UTC()
		msg.ExecutionState = req.Outcome
		msg.Result = req.Report
		msg.ResultStatus = req.Status
		msg.ExecutionUpdatedAt = now
		msg.ResultApplied = false
		if pr, _ := s.findPRLocked(msg.PRNumber); pr != nil {
			latest := ""
			for _, other := range s.controller.Messages {
				if other.Direction == "instruction" && other.PRNumber == msg.PRNumber {
					latest = other.ID
				}
			}
			if latest == msg.ID && pr.SessionID == msg.To && msg.TargetCommit != "" && s.instructionTargetLocked(pr) == msg.TargetCommit {
				previousStatus := pr.Status
				pr.Status = req.Status
				pr.UpdatedAt = now
				msg.ResultApplied = true
				if req.Status == StatusWaitingForHuman && previousStatus != StatusWaitingForHuman {
					s.emitNotificationLocked(pr, previousStatus, req.Status, fmt.Sprintf("PR #%d is now Waiting for Human", pr.Number), req.Report, now)
				}
				pr.Thread = append(pr.Thread, cloneThreadMessage(ThreadMessage{AppliedConfig: msg.AppliedConfig, ID: s.nextID("msg"), Author: msg.To, Role: "session", Kind: "status_change", Text: req.Report, CreatedAt: now}))
			}
		}
		report := SessionMessage{AppliedConfig: msg.AppliedConfig, ID: s.nextID("smsg"), From: msg.To, To: s.controller.ID, Direction: "report", PRNumber: msg.PRNumber, Text: req.Report, CreatedAt: now, InstructionID: msg.ID, TargetCommit: msg.TargetCommit, ExecutionState: req.Outcome, ConsumerID: msg.ConsumerID, Attempt: msg.Attempt, Result: req.Report, ResultStatus: req.Status, ResultApplied: msg.ResultApplied}
		s.controller.Messages = append(s.controller.Messages, cloneSessionMessage(report))
		if sess, idx := s.findSessionLocked(msg.To); sess != nil && idx >= 0 {
			sess.Messages = append(sess.Messages, cloneSessionMessage(report))
		}
		return nil
	})
}

func (s *Service) RetryInstruction(req InstructionRetryRequest) (SessionMessage, error) {
	return s.mutateInstruction(req.SessionID, req.InstructionID, func(msg *SessionMessage) error {
		if err := s.validateInstructionAttemptLocked(*msg, req.ConsumerID, req.ClaimToken, req.Attempt); err != nil {
			return err
		}
		if msg.ExecutionState != InstructionFailed {
			return fmt.Errorf("%w: only failed instructions can be retried", ErrInstructionConflict)
		}
		msg.ExecutionState = InstructionQueued
		msg.Handled = false
		msg.ConsumerID = ""
		msg.Result = ""
		msg.ResultStatus = ""
		msg.ResultApplied = false
		msg.ExecutionUpdatedAt = time.Now().UTC()
		delete(s.instructionClaims, msg.ID)
		return nil
	})
}

func (s *Service) validateInstructionAttemptLocked(msg SessionMessage, consumer, token string, attempt int) error {
	if consumer == "" || consumer != msg.ConsumerID || token == "" || token != s.instructionClaims[msg.ID] || attempt != msg.Attempt {
		return fmt.Errorf("%w: claim does not match this attempt", ErrInstructionConflict)
	}
	return nil
}

func (s *Service) instructionTargetLocked(pr *PullRequest) string {
	if pr.HeadSHA != "" {
		return pr.HeadSHA
	}
	for _, wt := range s.worktrees {
		if wt.ID == pr.WorktreeID && pr.WorktreeID != "" {
			return wt.HeadSHA
		}
	}
	return ""
}

func (s *Service) instructionLocked(sessionID, id string) (SessionMessage, error) {
	if sessionID == "controller" {
		sessionID = s.controller.ID
	}
	for _, msg := range s.controller.Messages {
		if msg.ID != id || msg.Direction != "instruction" {
			continue
		}
		if sessionID == "" || sessionID != msg.To {
			return SessionMessage{}, ErrInstructionSession
		}
		if msg.ExecutionState == "" {
			msg.ExecutionState = InstructionQueued
			if msg.Handled {
				msg.ExecutionState = InstructionDelivered
			}
		}
		return cloneSessionMessage(msg), nil
	}
	return SessionMessage{}, ErrInstructionNotFound
}

func (s *Service) storeInstructionLocked(msg SessionMessage) {
	for i := range s.controller.Messages {
		if s.controller.Messages[i].ID == msg.ID {
			s.controller.Messages[i] = cloneSessionMessage(msg)
		}
	}
	for i := range s.sessions {
		for j := range s.sessions[i].Messages {
			if s.sessions[i].Messages[j].ID == msg.ID {
				s.sessions[i].Messages[j] = cloneSessionMessage(msg)
			}
		}
	}
}

// mutateInstruction keeps lifecycle transitions and result history atomic under
// the owner lock. A failed save rolls back memory and returns no claim token.
func (s *Service) mutateInstruction(sessionID, id string, change func(*SessionMessage) error) (SessionMessage, error) {
	if err := s.beginStateMutation(); err != nil {
		return SessionMessage{}, fmt.Errorf("%w: %w", ErrInstructionPersistence, err)
	}
	msg, err := s.instructionLocked(sessionID, id)
	if err == nil {
		err = change(&msg)
	}
	if err != nil {
		s.abortStateMutation()
		return SessionMessage{}, err
	}
	s.storeInstructionLocked(msg)
	snap, err := s.commitStateMutation()
	if err != nil {
		return SessionMessage{}, fmt.Errorf("%w: %w", ErrInstructionPersistence, err)
	}
	s.broadcast(snap)
	return msg, nil
}
