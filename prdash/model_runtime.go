package prdash

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrModelRuntimeUnsupported = errors.New("model runtime operation unsupported")
	ErrModelRuntimeBusy        = errors.New("model runtime operation already in progress")
	ErrInvalidModelScope       = errors.New("model scope must be current_turn or next_turn")
)

// ModelCapabilities describes operations actually supported by an integration.
// Subagent model presets are currently save-only and are never sent implicitly.
type ModelCapabilities struct {
	Observe          bool `json:"observe"`
	ApplyCurrentTurn bool `json:"apply_current_turn"`
	ApplyNextTurn    bool `json:"apply_next_turn"`
}

// ModelRuntime is an optional, explicitly bound session integration. Implementations
// must address the bound conversation, preserve its permissions, honor Scope, and
// return Accepted only after the runtime has accepted the exact request.
type ModelRuntime interface {
	ObserveModel(context.Context) (ModelObservation, error)
	ApplyModel(context.Context, ModelChangeRequest) (ModelApplyResult, error)
}

// ModelRuntimeBinding is process-local. A dashboard ID or provider label alone
// is not a connection, and a persisted session cannot attach its own runtime.
type ModelRuntimeBinding struct {
	Provider     string
	Capabilities ModelCapabilities
	Runtime      ModelRuntime
}

type ModelChangeRequest struct {
	Model string `json:"model"`
	Scope string `json:"scope"`
}

type ModelApplyResult struct {
	Accepted bool   `json:"accepted"`
	Model    string `json:"model"`
	Scope    string `json:"scope"`
	Detail   string `json:"detail,omitempty"`
}

// ModelObservation is a measured runtime value, not a resolved preset.
type ModelObservation struct {
	Model      string    `json:"model,omitempty"`
	TurnID     string    `json:"turn_id,omitempty"`
	ObservedAt time.Time `json:"observed_at"`
}

type ModelApplication struct {
	Status string `json:"status"`
	Model  string `json:"model,omitempty"`
	Scope  string `json:"scope"`
	Detail string `json:"detail,omitempty"`
}

type ModelRuntimeState struct {
	Connection       string            `json:"connection"`
	Provider         string            `json:"provider,omitempty"`
	Capabilities     ModelCapabilities `json:"capabilities"`
	Observation      ModelObservation  `json:"observation"`
	ObservationError string            `json:"observation_error,omitempty"`
	Application      ModelApplication  `json:"application"`
}

func (s *Service) sessionWithModelRuntimeLocked(sess Session) Session {
	out := cloneSession(sess)
	state := s.modelRuntimeStates[sess.ID]
	state.Connection = "unconnected"
	state.Provider = ""
	state.Capabilities = ModelCapabilities{}
	if binding, ok := s.modelRuntimes[sess.ID]; ok && binding.Runtime != nil {
		state.Connection = "attached"
		state.Provider = binding.Provider
		state.Capabilities = binding.Capabilities
	}
	if state.Application.Status == "" {
		state.Application = ModelApplication{Status: "saved_not_applied", Model: sess.ResolvedModel, Scope: "none"}
	}
	out.ModelRuntime = state
	return out
}

func (s *Service) markModelSavedLocked(id string) {
	state := s.modelRuntimeStates[id]
	state.Application = ModelApplication{Status: "saved_not_applied", Scope: "none"}
	if sess, _ := s.findSessionLocked(id); sess != nil {
		state.Application.Model = sess.ResolvedModel
	}
	s.modelRuntimeStates[id] = state
}

// RefreshSessionModel reads the bound runtime without changing its model.
func (s *Service) RefreshSessionModel(ctx context.Context, id string) (Session, error) {
	s.mu.Lock()
	sess, _ := s.findSessionLocked(id)
	if sess == nil {
		s.mu.Unlock()
		return Session{}, fmt.Errorf("%w: %s", ErrSessionNotFound, id)
	}
	binding := s.modelRuntimes[sess.ID]
	if binding.Runtime == nil || !binding.Capabilities.Observe {
		res := s.sessionWithModelRuntimeLocked(*sess)
		s.mu.Unlock()
		return res, fmt.Errorf("%w: observation for %s", ErrModelRuntimeUnsupported, id)
	}
	id = sess.ID
	if s.modelRuntimeBusy[id] {
		s.mu.Unlock()
		return Session{}, ErrModelRuntimeBusy
	}
	s.modelRuntimeBusy[id] = true
	s.mu.Unlock()

	observation, observeErr := binding.Runtime.ObserveModel(ctx)
	if observeErr == nil && strings.TrimSpace(observation.Model) == "" {
		observeErr = errors.New("runtime returned no observed model")
	}
	s.mu.Lock()
	s.modelRuntimeBusy[id] = false
	sess, _ = s.findSessionLocked(id)
	state := s.modelRuntimeStates[id]
	state.ObservationError = ""
	if observeErr != nil {
		state.ObservationError = observeErr.Error()
	} else {
		observation.ObservedAt = time.Now().UTC()
		state.Observation = observation
	}
	s.modelRuntimeStates[id] = state
	res := s.sessionWithModelRuntimeLocked(*sess)
	snap := s.snapshotLocked()
	s.mu.Unlock()
	s.broadcast(snap)
	return res, observeErr
}

// ApplySessionModel explicitly requests the saved primary model for a named
// scope. Saving presets, dispatching instructions, and selecting PR models never
// call this method. A next-turn acceptance is not a current model observation.
func (s *Service) ApplySessionModel(ctx context.Context, id, scope string) (Session, error) {
	if scope != "current_turn" && scope != "next_turn" {
		return Session{}, ErrInvalidModelScope
	}
	s.mu.Lock()
	sess, _ := s.findSessionLocked(id)
	if sess == nil {
		s.mu.Unlock()
		return Session{}, fmt.Errorf("%w: %s", ErrSessionNotFound, id)
	}
	id = sess.ID
	if s.modelRuntimeBusy[id] {
		s.mu.Unlock()
		return Session{}, ErrModelRuntimeBusy
	}
	binding := s.modelRuntimes[id]
	req := ModelChangeRequest{Model: sess.ResolvedModel, Scope: scope}
	state := s.modelRuntimeStates[id]
	state.Application = ModelApplication{Status: "applying", Model: req.Model, Scope: scope}
	supported := binding.Runtime != nil && ((scope == "current_turn" && binding.Capabilities.ApplyCurrentTurn) || (scope == "next_turn" && binding.Capabilities.ApplyNextTurn))
	if !supported {
		state.Application.Status = "unsupported"
		state.Application.Detail = "No bound runtime supports this operation; requested settings were saved only."
		s.modelRuntimeStates[id] = state
		res := s.sessionWithModelRuntimeLocked(*sess)
		snap := s.snapshotLocked()
		s.mu.Unlock()
		s.broadcast(snap)
		return res, fmt.Errorf("%w: %s for %s", ErrModelRuntimeUnsupported, scope, id)
	}
	s.modelRuntimeStates[id] = state
	s.modelRuntimeBusy[id] = true
	snap := s.snapshotLocked()
	s.mu.Unlock()
	s.broadcast(snap)

	result, applyErr := binding.Runtime.ApplyModel(ctx, req)
	if applyErr == nil && (!result.Accepted || result.Model != req.Model || result.Scope != scope) {
		applyErr = errors.New("runtime did not confirm the requested model and scope")
	}
	s.mu.Lock()
	s.modelRuntimeBusy[id] = false
	sess, _ = s.findSessionLocked(id)
	state = s.modelRuntimeStates[id]
	application := ModelApplication{Model: req.Model, Scope: scope, Detail: result.Detail}
	switch {
	case applyErr != nil:
		application.Status = "failed"
		application.Detail = applyErr.Error()
	case sess.ResolvedModel != req.Model:
		application.Status = "superseded"
		application.Detail = "Runtime accepted an older request; the newly requested model has not been applied."
	case scope == "next_turn":
		application.Status = "scheduled"
	default:
		application.Status = "applied"
	}
	state.Application = application
	// Applying a setting does not establish which model is executing a turn.
	s.modelRuntimeStates[id] = state
	res := s.sessionWithModelRuntimeLocked(*sess)
	snap = s.snapshotLocked()
	s.mu.Unlock()
	s.broadcast(snap)
	return res, applyErr
}
