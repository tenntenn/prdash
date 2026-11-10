package prdash

import (
	"encoding/json"
	"fmt"
)

func dispatchInstructionMCPTool(svc *Service, name string, raw json.RawMessage) (any, error) {
	switch name {
	case "get_instruction", "ack_instruction":
		var req InstructionClaimRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		if name == "get_instruction" {
			return svc.GetInstruction(req.SessionID, req.InstructionID)
		}
		return svc.AckInstruction(req.SessionID, req.InstructionID)
	case "claim_instruction":
		var req InstructionClaimRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		return svc.ClaimInstruction(req)
	case "report_instruction_result":
		var req InstructionResultRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		return svc.ReportInstructionResult(req)
	case "retry_instruction":
		var req InstructionRetryRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		return svc.RetryInstruction(req)
	default:
		return nil, fmt.Errorf("unknown instruction tool: %s", name)
	}
}

func instructionMCPTools() []mcpTool {
	var result []mcpTool
	for _, name := range []string{"get_instruction", "ack_instruction", "claim_instruction", "report_instruction_result", "retry_instruction"} {
		properties := map[string]any{
			"session_id":     map[string]any{"type": "string", "description": "Assigned session ID"},
			"instruction_id": map[string]any{"type": "string", "description": "Exact instruction ID"},
		}
		required := []string{"session_id", "instruction_id"}
		description := "Get one instruction and its execution state."
		switch name {
		case "ack_instruction":
			description = "Acknowledge delivery of exactly one instruction; does not complete execution."
		case "claim_instruction", "report_instruction_result", "retry_instruction":
			properties["consumer_id"] = map[string]any{"type": "string", "description": "Stable ID unique to this consumer"}
			required = append(required, "consumer_id")
			if name == "claim_instruction" {
				description = "Atomically claim one instruction through this running Service owner. Same consumer can recover its running claim."
				properties["target_commit"] = map[string]any{"type": "string", "description": "Required when the PR target is not known; must match an already captured target"}
			} else {
				properties["claim_token"] = map[string]any{"type": "string"}
				properties["attempt"] = map[string]any{"type": "integer"}
				required = append(required, "claim_token", "attempt")
				description = "Explicitly requeue a failed instruction; preserves result history and increments the attempt on the next claim."
				if name == "report_instruction_result" {
					description = "Record the result of exactly one claimed attempt and target commit. Identical retries are idempotent; stale results cannot update PR status."
					properties["target_commit"] = map[string]any{"type": "string"}
					properties["outcome"] = map[string]any{"type": "string", "enum": []string{"completed", "failed"}}
					properties["status"] = map[string]any{"type": "string", "enum": []string{string(StatusWorking), string(StatusWaitingForHuman)}}
					properties["report"] = map[string]any{"type": "string"}
					required = append(required, "outcome", "status", "report")
				}
			}
		}
		result = append(result, mcpTool{Name: name, Description: description, InputSchema: map[string]any{"type": "object", "properties": properties, "required": required}})
	}
	return result
}
