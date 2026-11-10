package prdash_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/tenntenn/prdash/prdash"
)

func TestModelRequestsDoNotClaimRuntimeApplication(t *testing.T) {
	t.Parallel()
	svc := prdash.NewService(prdash.Options{})
	sess, err := svc.UpsertSession(prdash.Session{ID: "sample-session", ModelRuntime: prdash.ModelRuntimeState{Connection: "attached", Observation: prdash.ModelObservation{Model: "sample-forged-model"}}})
	if err != nil {
		t.Fatal(err)
	}
	if sess.ModelRuntime.Connection != "unconnected" || sess.ModelRuntime.Observation.Model != "" {
		t.Fatalf("unverified runtime state: %+v", sess.ModelRuntime)
	}
	if _, err := svc.UpsertPR(prdash.PullRequest{Number: 42, Title: "sample-pr", SessionID: sess.ID, ModelTier: prdash.TierPlum}); err != nil {
		t.Fatal(err)
	}
	sess, err = svc.SetSessionModelTier(sess.ID, prdash.TierPine, prdash.TierBamboo)
	if err != nil {
		t.Fatal(err)
	}
	pr, err := svc.GetPR(42)
	if err != nil {
		t.Fatal(err)
	}
	if pr.ModelTier != prdash.TierPlum {
		t.Fatal("session setting overwrote PR request")
	}
	if _, err := svc.SetPRModelTier(42, prdash.TierBamboo); err != nil {
		t.Fatal(err)
	}
	got := svc.Snapshot().Sessions[0]
	if got.ModelTier != prdash.TierPine || got.ModelRuntime.Observation.Model != "" || got.ModelRuntime.Application.Status != "saved_not_applied" {
		t.Fatalf("unexpected session model: %+v", got)
	}
	if _, err := svc.SendPRMessage(42, "sample-author", "sample-task"); err != nil {
		t.Fatal(err)
	}
	pending := svc.PendingInstructions(sess.ID)
	if len(pending) != 1 || pending[0].RequestedModel != svc.Snapshot().ModelConfig.Resolve(prdash.TierBamboo) || pending[0].ModelSource != "pull_request" {
		t.Fatalf("PR precedence missing: %+v", pending)
	}
	if _, err := svc.SendDirectSessionInstruction(sess.ID, "sample-direct-task"); err != nil {
		t.Fatal(err)
	}
	pending = svc.PendingInstructions(sess.ID)
	if pending[len(pending)-1].RequestedModel != sess.ResolvedModel || pending[len(pending)-1].ModelSource != "session" {
		t.Fatalf("direct instruction precedence missing: %+v", pending)
	}
	denied, err := svc.ApplySessionModel(t.Context(), sess.ID, "current_turn")
	if !errors.Is(err, prdash.ErrModelRuntimeUnsupported) || denied.ModelRuntime.Application.Status != "unsupported" || denied.ModelRuntime.Observation.Model != "" {
		t.Fatalf("unconnected apply=%+v error=%v", denied, err)
	}
}

func TestModelApplicationCapabilitiesAndScopes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, scope, resultScope, resultModel string
		supported, accepted, fail             bool
		wantStatus                            string
		wantCalls                             int
		wantErr                               bool
	}{
		{"current turn", "current_turn", "current_turn", "sample-requested", true, true, false, "applied", 1, false},
		{"next turn", "next_turn", "next_turn", "sample-requested", true, true, false, "scheduled", 1, false},
		{"unsupported", "current_turn", "current_turn", "sample-requested", false, true, false, "unsupported", 0, true},
		{"not accepted", "current_turn", "current_turn", "sample-requested", true, false, false, "failed", 1, true},
		{"wrong scope", "next_turn", "current_turn", "sample-requested", true, true, false, "failed", 1, true},
		{"wrong model", "current_turn", "current_turn", "sample-other", true, true, false, "failed", 1, true},
		{"runtime failure", "current_turn", "current_turn", "sample-requested", true, true, true, "failed", 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runtime := &sampleModelRuntime{result: prdash.ModelApplyResult{Accepted: tc.accepted, Scope: tc.resultScope, Model: tc.resultModel}, fail: tc.fail}
			svc := newModelRuntimeService(t, runtime, prdash.ModelCapabilities{Observe: true, ApplyCurrentTurn: tc.supported, ApplyNextTurn: tc.supported}, "")
			if _, err := svc.RefreshSessionModel(t.Context(), "sample-session"); err != nil {
				t.Fatal(err)
			}
			got, err := svc.ApplySessionModel(t.Context(), "sample-session", tc.scope)
			if (err != nil) != tc.wantErr || got.ModelRuntime.Application.Status != tc.wantStatus || runtime.applyCalls != tc.wantCalls {
				t.Fatalf("application=%+v calls=%d error=%v", got.ModelRuntime.Application, runtime.applyCalls, err)
			}
			if got.ModelRuntime.Observation.Model != "sample-observed" {
				t.Fatal("apply overwrote observed model without measurement")
			}
			if tc.wantCalls > 0 && (runtime.request.Model != "sample-requested" || runtime.request.Scope != tc.scope) {
				t.Fatalf("wrong request: %+v", runtime.request)
			}
		})
	}
}

func TestModelObservationAndRestart(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sample-state.json")
	runtime := &sampleModelRuntime{}
	svc := newModelRuntimeService(t, runtime, prdash.ModelCapabilities{Observe: true}, path)
	got, err := svc.RefreshSessionModel(t.Context(), "sample-session")
	if err != nil {
		t.Fatal(err)
	}
	if got.ModelRuntime.Observation.Model != "sample-observed" || got.ModelRuntime.Observation.TurnID != "sample-turn" || got.ModelRuntime.Observation.ObservedAt.IsZero() {
		t.Fatalf("observation=%+v", got.ModelRuntime.Observation)
	}
	if _, err := svc.SetSessionModelTier("sample-session", prdash.TierPine, prdash.TierPlum); err != nil {
		t.Fatal(err)
	}
	if runtime.applyCalls != 0 || runtime.observeCalls != 1 {
		t.Fatal("saving requested settings called runtime")
	}
	restarted := prdash.NewService(prdash.Options{StateFile: path})
	state := restarted.Snapshot().Sessions[0]
	if state.ModelTier != prdash.TierPine || state.ModelRuntime.Connection != "unconnected" || state.ModelRuntime.Observation.Model != "" {
		t.Fatalf("restart inferred a live observation: %+v", state)
	}
}

func TestModelApplyPreservesNewerRequest(t *testing.T) {
	t.Parallel()
	runtime := &sampleModelRuntime{result: prdash.ModelApplyResult{Accepted: true, Scope: "current_turn", Model: "sample-requested"}, changeRequest: true}
	svc := newModelRuntimeService(t, runtime, prdash.ModelCapabilities{ApplyCurrentTurn: true}, "")
	runtime.service = svc
	got, err := svc.ApplySessionModel(t.Context(), "sample-session", "current_turn")
	if err != nil {
		t.Fatal(err)
	}
	if got.ModelTier != prdash.TierPine || got.ModelRuntime.Application.Status != "superseded" {
		t.Fatalf("newer request overwritten: %+v", got)
	}
	if !errors.Is(runtime.nestedErr, prdash.ErrModelRuntimeBusy) {
		t.Fatalf("concurrent apply error=%v", runtime.nestedErr)
	}
}

func newModelRuntimeService(t *testing.T, runtime *sampleModelRuntime, caps prdash.ModelCapabilities, stateFile string) *prdash.Service {
	t.Helper()
	svc := prdash.NewService(prdash.Options{StateFile: stateFile, ModelConfig: &prdash.ModelPresetConfig{Pine: "sample-pine", Bamboo: "sample-requested", Plum: "sample-plum"}, ModelRuntimes: map[string]prdash.ModelRuntimeBinding{"sample-session": {Provider: "sample-provider", Capabilities: caps, Runtime: runtime}}})
	if _, err := svc.UpsertSession(prdash.Session{ID: "sample-session"}); err != nil {
		t.Fatal(err)
	}
	return svc
}

type sampleModelRuntime struct {
	result                   prdash.ModelApplyResult
	request                  prdash.ModelChangeRequest
	applyCalls, observeCalls int
	fail, changeRequest      bool
	service                  *prdash.Service
	nestedErr                error
}

func (r *sampleModelRuntime) ObserveModel(context.Context) (prdash.ModelObservation, error) {
	r.observeCalls++
	return prdash.ModelObservation{Model: "sample-observed", TurnID: "sample-turn"}, nil
}
func (r *sampleModelRuntime) ApplyModel(ctx context.Context, req prdash.ModelChangeRequest) (prdash.ModelApplyResult, error) {
	r.applyCalls++
	r.request = req
	if r.changeRequest {
		_, r.nestedErr = r.service.ApplySessionModel(ctx, "sample-session", "current_turn")
		if _, err := r.service.SetSessionModelTier("sample-session", prdash.TierPine, prdash.TierPlum); err != nil {
			return prdash.ModelApplyResult{}, err
		}
	}
	if r.fail {
		return prdash.ModelApplyResult{}, errors.New("sample-runtime-failure")
	}
	return r.result, nil
}
