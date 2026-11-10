package prdash_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tenntenn/prdash/prdash"
)

const instructionHead = "0123456789abcdef0123456789abcdef01234567"

func instructionService(t *testing.T, state string) *prdash.Service {
	t.Helper()
	svc := prdash.NewService(prdash.Options{Repository: "example/project", StateFile: state})
	if _, err := svc.UpsertSession(prdash.Session{ID: "worker"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpsertPR(prdash.PullRequest{Number: 1, Title: "Instruction tracking", SessionID: "worker", HeadSHA: instructionHead, Status: prdash.StatusWorking, CIStatus: "failing", HasConflicts: true}); err != nil {
		t.Fatal(err)
	}
	return svc
}

func queueInstruction(t *testing.T, svc *prdash.Service, text string) prdash.SessionMessage {
	t.Helper()
	if _, err := svc.AddressReview(1, text); err != nil {
		t.Fatal(err)
	}
	pending := svc.PendingInstructions("worker")
	if len(pending) == 0 {
		t.Fatal("no pending instruction")
	}
	return pending[len(pending)-1]
}

func claimInstruction(t *testing.T, svc *prdash.Service, msg prdash.SessionMessage) prdash.InstructionClaim {
	t.Helper()
	claim, err := svc.ClaimInstruction(prdash.InstructionClaimRequest{SessionID: msg.To, InstructionID: msg.ID, ConsumerID: "consumer"})
	if err != nil {
		t.Fatal(err)
	}
	return claim
}

func instructionResult(claim prdash.InstructionClaim) prdash.InstructionResultRequest {
	return prdash.InstructionResultRequest{SessionID: claim.Instruction.To, InstructionID: claim.Instruction.ID, ConsumerID: claim.Instruction.ConsumerID, ClaimToken: claim.ClaimToken, Attempt: claim.Instruction.Attempt, TargetCommit: claim.Instruction.TargetCommit, Outcome: prdash.InstructionCompleted, Status: prdash.StatusWaitingForHuman, Report: "verification completed"}
}

func TestInstructionOverlappingResults(t *testing.T) {
	t.Parallel()
	svc := instructionService(t, "")
	first := queueInstruction(t, svc, "first")
	firstClaim := claimInstruction(t, svc, first)
	second := queueInstruction(t, svc, "second")
	result, err := svc.ReportInstructionResult(instructionResult(firstClaim))
	if err != nil {
		t.Fatal(err)
	}
	if result.ResultApplied {
		t.Fatal("older instruction changed PR status")
	}
	pending := svc.PendingInstructions("worker")
	if len(pending) != 1 || pending[0].ID != second.ID {
		t.Fatalf("new instruction was acknowledged: %+v", pending)
	}
	acked, err := svc.AckInstruction("worker", second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if acked.ExecutionState != prdash.InstructionDelivered {
		t.Fatalf("delivery completed execution: %+v", acked)
	}
	secondClaim := claimInstruction(t, svc, second)
	req := instructionResult(secondClaim)
	result, err = svc.ReportInstructionResult(req)
	if err != nil {
		t.Fatal(err)
	}
	if !result.ResultApplied {
		t.Fatal("current result not applied")
	}
	count := len(svc.Snapshot().ControllerSession.Messages)
	if _, err := svc.ReportInstructionResult(req); err != nil {
		t.Fatalf("idempotent result: %v", err)
	}
	if got := len(svc.Snapshot().ControllerSession.Messages); got != count {
		t.Fatalf("duplicate result added history: %d != %d", got, count)
	}
	req.Report = "different"
	if _, err := svc.ReportInstructionResult(req); !errors.Is(err, prdash.ErrInstructionConflict) {
		t.Fatalf("conflicting result: %v", err)
	}
	// Even a delayed duplicate of the older result cannot roll back newer state.
	if _, err := svc.ReportInstructionResult(instructionResult(firstClaim)); err != nil {
		t.Fatal(err)
	}
	pr, err := svc.GetPR(1)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Status != prdash.StatusWaitingForHuman || pr.CIStatus != "failing" || !pr.HasConflicts {
		t.Fatalf("result changed technical observations: %+v", pr)
	}
}

func TestInstructionConcurrentClaim(t *testing.T) {
	t.Parallel()
	svc := instructionService(t, "")
	msg := queueInstruction(t, svc, "one owner")
	var wg sync.WaitGroup
	successes := make(chan prdash.InstructionClaim, 16)
	failures := make(chan error, 16)
	for i := range 16 {
		wg.Go(func() {
			claim, err := svc.ClaimInstruction(prdash.InstructionClaimRequest{SessionID: "worker", InstructionID: msg.ID, ConsumerID: fmt.Sprintf("consumer-%d", i)})
			if err == nil {
				successes <- claim
			} else {
				failures <- err
			}
		})
	}
	wg.Wait()
	close(successes)
	close(failures)
	if len(successes) != 1 {
		t.Fatalf("successful concurrent claims = %d", len(successes))
	}
	for err := range failures {
		if !errors.Is(err, prdash.ErrInstructionConflict) {
			t.Fatal(err)
		}
	}
	claim := <-successes
	recovered, err := svc.ClaimInstruction(prdash.InstructionClaimRequest{SessionID: "worker", InstructionID: msg.ID, ConsumerID: claim.Instruction.ConsumerID})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.ClaimToken != claim.ClaimToken || recovered.Instruction.Attempt != 1 {
		t.Fatal("same consumer did not recover its claim")
	}
}

func TestInstructionRestartAndRetry(t *testing.T) {
	t.Parallel()
	state := filepath.Join(t.TempDir(), "state.json")
	svc := instructionService(t, state)
	msg := queueInstruction(t, svc, "persist")
	claim := claimInstruction(t, svc, msg)
	restarted := prdash.NewService(prdash.Options{Repository: "example/project", StateFile: state})
	recovered := claimInstruction(t, restarted, msg)
	if recovered.ClaimToken != claim.ClaimToken {
		t.Fatal("restart lost claim capability")
	}
	req := instructionResult(recovered)
	req.Outcome = prdash.InstructionFailed
	req.Report = "tests failed"
	req.Status = prdash.StatusWorking
	if _, err := restarted.ReportInstructionResult(req); err != nil {
		t.Fatal(err)
	}
	retry := prdash.InstructionRetryRequest{SessionID: "worker", InstructionID: msg.ID, ConsumerID: "consumer", ClaimToken: claim.ClaimToken, Attempt: 1}
	queued, err := restarted.RetryInstruction(retry)
	if err != nil {
		t.Fatal(err)
	}
	if queued.Handled || queued.ExecutionState != prdash.InstructionQueued {
		t.Fatalf("not requeued: %+v", queued)
	}
	next := claimInstruction(t, restarted, msg)
	if next.Instruction.Attempt != 2 || next.ClaimToken == claim.ClaimToken {
		t.Fatal("retry reused attempt capability")
	}
	if _, err := restarted.ReportInstructionResult(req); !errors.Is(err, prdash.ErrInstructionConflict) {
		t.Fatalf("late attempt accepted: %v", err)
	}
	if _, err := restarted.ReportInstructionResult(instructionResult(next)); err != nil {
		t.Fatal(err)
	}
	var outcomes []prdash.InstructionState
	for _, m := range restarted.Snapshot().ControllerSession.Messages {
		if m.InstructionID == msg.ID {
			outcomes = append(outcomes, m.ExecutionState)
		}
	}
	if len(outcomes) != 2 || outcomes[0] != prdash.InstructionFailed || outcomes[1] != prdash.InstructionCompleted {
		t.Fatalf("lost result history: %v", outcomes)
	}
	snapshot, err := json.Marshal(restarted.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(snapshot), next.ClaimToken) {
		t.Fatal("claim token exposed in dashboard")
	}
}

func TestInstructionValidatesCorrelation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, session, consumer, token, target string
		attempt                                int
	}{
		{name: "session", session: "other", consumer: "consumer", token: "valid", target: instructionHead, attempt: 1},
		{name: "consumer", session: "worker", consumer: "other", token: "valid", target: instructionHead, attempt: 1},
		{name: "token", session: "worker", consumer: "consumer", token: "wrong", target: instructionHead, attempt: 1},
		{name: "target", session: "worker", consumer: "consumer", token: "valid", target: "different", attempt: 1},
		{name: "attempt", session: "worker", consumer: "consumer", token: "valid", target: instructionHead, attempt: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := instructionService(t, "")
			claim := claimInstruction(t, svc, queueInstruction(t, svc, "validate"))
			req := instructionResult(claim)
			req.SessionID = tc.session
			req.ConsumerID = tc.consumer
			req.TargetCommit = tc.target
			req.Attempt = tc.attempt
			if tc.token != "valid" {
				req.ClaimToken = tc.token
			}
			if _, err := svc.ReportInstructionResult(req); err == nil {
				t.Fatal("invalid correlation accepted")
			}
			msg, err := svc.GetInstruction("worker", claim.Instruction.ID)
			if err != nil {
				t.Fatal(err)
			}
			if msg.ExecutionState != prdash.InstructionRunning {
				t.Fatal("rejection changed execution state")
			}
		})
	}
}

func TestInstructionStaleAssignmentAndHead(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, session, head string }{{"reassigned", "other", instructionHead}, {"new head", "worker", "new-head"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := instructionService(t, "")
			claim := claimInstruction(t, svc, queueInstruction(t, svc, "before change"))
			pr, err := svc.GetPR(1)
			if err != nil {
				t.Fatal(err)
			}
			pr.SessionID = tc.session
			pr.HeadSHA = tc.head
			if _, err := svc.UpsertPR(pr); err != nil {
				t.Fatal(err)
			}
			result, err := svc.ReportInstructionResult(instructionResult(claim))
			if err != nil {
				t.Fatal(err)
			}
			if result.ResultApplied || result.ExecutionState != prdash.InstructionCompleted {
				t.Fatalf("stale result = %+v", result)
			}
		})
	}
}

func TestInstructionPersistenceFailureRollsBackClaim(t *testing.T) {
	t.Parallel()
	state := filepath.Join(t.TempDir(), "state.json")
	svc := instructionService(t, state)
	msg := queueInstruction(t, svc, "durable")
	if err := os.Remove(state); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	claim, err := svc.ClaimInstruction(prdash.InstructionClaimRequest{SessionID: "worker", InstructionID: msg.ID, ConsumerID: "consumer"})
	if !errors.Is(err, prdash.ErrInstructionPersistence) || claim.ClaimToken != "" {
		t.Fatalf("undurable claim returned: %+v %v", claim, err)
	}
	current, err := svc.GetInstruction("worker", msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.ExecutionState != prdash.InstructionQueued || current.Attempt != 0 {
		t.Fatalf("claim not rolled back: %+v", current)
	}
}

func TestInstructionHTTP(t *testing.T) {
	t.Parallel()
	svc := instructionService(t, "")
	msg := queueInstruction(t, svc, "http")
	handler := prdash.NewHandler(svc)
	path := "/api/sessions/worker/instructions/" + msg.ID
	req := httptest.NewRequest(http.MethodPost, path+"/claim", strings.NewReader(`{"consumer_id":"consumer"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("claim HTTP %d: %s", rec.Code, rec.Body.String())
	}
	var claim prdash.InstructionClaim
	if err := json.Unmarshal(rec.Body.Bytes(), &claim); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(instructionResult(claim))
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, path+"/result", strings.NewReader(string(payload)))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("result HTTP %d: %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodPost, path+"/claim", strings.NewReader(`{"consumer_id":"other"}`))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate claim HTTP %d", rec.Code)
	}
}

func TestInstructionClaimRequiresAssignmentAndTarget(t *testing.T) {
	t.Parallel()
	svc := instructionService(t, "")
	msg := queueInstruction(t, svc, "original")
	cases := []struct{ name, session, consumer, target string }{
		{"wrong session", "other", "consumer", instructionHead},
		{"empty consumer", "worker", "", instructionHead},
		{"wrong target", "worker", "consumer", "different"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.ClaimInstruction(prdash.InstructionClaimRequest{SessionID: tc.session, InstructionID: msg.ID, ConsumerID: tc.consumer, TargetCommit: tc.target})
			if err == nil {
				t.Fatal("invalid claim accepted")
			}
		})
	}
	pr, err := svc.GetPR(1)
	if err != nil {
		t.Fatal(err)
	}
	pr.SessionID = "other"
	if _, err := svc.UpsertPR(pr); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ClaimInstruction(prdash.InstructionClaimRequest{SessionID: "worker", InstructionID: msg.ID, ConsumerID: "consumer"}); !errors.Is(err, prdash.ErrInstructionSession) {
		t.Fatalf("reassigned claim: %v", err)
	}
	pr.SessionID = "worker"
	pr.HeadSHA = ""
	if _, err := svc.UpsertPR(pr); err != nil {
		t.Fatal(err)
	}
	unknown := queueInstruction(t, svc, "unknown target")
	req := prdash.InstructionClaimRequest{SessionID: "worker", InstructionID: unknown.ID, ConsumerID: "consumer"}
	if _, err := svc.ClaimInstruction(req); !errors.Is(err, prdash.ErrInstructionInput) {
		t.Fatalf("missing target: %v", err)
	}
	req.TargetCommit = instructionHead
	if _, err := svc.ClaimInstruction(req); err != nil {
		t.Fatal(err)
	}
}

func TestInstructionMCPClaim(t *testing.T) {
	t.Parallel()
	svc := instructionService(t, "")
	msg := queueInstruction(t, svc, "MCP")
	input := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"claim_instruction","arguments":{"session_id":"worker","instruction_id":%q,"consumer_id":"consumer"}}}`, msg.ID) + "\n"
	var out strings.Builder
	if err := prdash.RunMCP(t.Context(), svc, strings.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	var response struct {
		Error  any `json:"error"`
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out.String()), &response); err != nil {
		t.Fatal(err)
	}
	if response.Error != nil || response.Result.IsError || len(response.Result.Content) != 1 {
		t.Fatalf("MCP error: %s", out.String())
	}
	var claim prdash.InstructionClaim
	if err := json.Unmarshal([]byte(response.Result.Content[0].Text), &claim); err != nil {
		t.Fatal(err)
	}
	if claim.ClaimToken == "" || claim.Instruction.ID != msg.ID {
		t.Fatalf("MCP did not return correlated claim: %+v", claim)
	}
}

func TestInstructionOutOfOrderCompletion(t *testing.T) {
	t.Parallel()
	svc := instructionService(t, "")
	old := claimInstruction(t, svc, queueInstruction(t, svc, "old"))
	latest := claimInstruction(t, svc, queueInstruction(t, svc, "latest"))
	if _, err := svc.ReportInstructionResult(instructionResult(latest)); err != nil {
		t.Fatal(err)
	}
	delayed := instructionResult(old)
	delayed.Status = prdash.StatusWorking
	result, err := svc.ReportInstructionResult(delayed)
	if err != nil {
		t.Fatal(err)
	}
	if result.ResultApplied {
		t.Fatal("late older result applied")
	}
	pr, err := svc.GetPR(1)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Status != prdash.StatusWaitingForHuman {
		t.Fatalf("latest status overwritten: %s", pr.Status)
	}
}

func TestInstructionStatusChangeDoesNotAcknowledge(t *testing.T) {
	t.Parallel()
	svc := instructionService(t, "")
	msg := queueInstruction(t, svc, "still pending")
	if _, err := svc.SetPRStatus(1, prdash.StatusWaitingForHuman, "handoff"); err != nil {
		t.Fatal(err)
	}
	pending := svc.PendingInstructions("worker")
	if len(pending) != 1 || pending[0].ID != msg.ID {
		t.Fatalf("status swallowed instruction: %+v", pending)
	}
	if _, err := svc.ReportSessionResult("other", 1, prdash.StatusWorking, "wrong session"); !errors.Is(err, prdash.ErrInstructionSession) {
		t.Fatalf("unassigned legacy report: %v", err)
	}
}
