package prdash

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type mcpTextContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mcpCallResult struct {
	Content []mcpTextContent `json:"content"`
	IsError bool             `json:"isError,omitempty"`
}

// RunMCP serves the Model Context Protocol (MCP) JSON-RPC 2.0 interface over r and w.
func RunMCP(ctx context.Context, svc *Service, r io.Reader, w io.Writer) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	enc := json.NewEncoder(w)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var req rpcRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			_ = enc.Encode(rpcResponse{
				JSONRPC: "2.0",
				ID:      json.RawMessage("null"),
				Error:   &rpcError{Code: -32700, Message: "parse error: " + err.Error()},
			})
			continue
		}

		if len(req.ID) == 0 {
			// JSON-RPC notification requires no response.
			continue
		}

		resp := handleMCPRequest(ctx, svc, req)
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func handleMCPRequest(ctx context.Context, svc *Service, req rpcRequest) rpcResponse {
	switch req.Method {
	case "initialize":
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities": map[string]any{
					"tools": map[string]any{},
				},
				"serverInfo": map[string]any{
					"name":    "prdash",
					"version": "0.1.0",
				},
			},
		}
	case "ping":
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  map[string]any{},
		}
	case "tools/list":
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"tools": mcpToolsList(),
			},
		}
	case "tools/call":
		var call struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &call); err != nil {
			return rpcResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   &rpcError{Code: -32602, Message: "invalid tool call params: " + err.Error()},
			}
		}
		res, err := dispatchMCPTool(ctx, svc, call.Name, call.Arguments)
		if err != nil {
			return rpcResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: mcpCallResult{
					Content: []mcpTextContent{{Type: "text", Text: err.Error()}},
					IsError: true,
				},
			}
		}
		data, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return rpcResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   &rpcError{Code: -32603, Message: err.Error()},
			}
		}
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: mcpCallResult{
				Content: []mcpTextContent{{Type: "text", Text: string(data)}},
			},
		}
	default:
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &rpcError{Code: -32601, Message: "method not found: " + req.Method},
		}
	}
}

func dispatchMCPTool(ctx context.Context, svc *Service, name string, rawArgs json.RawMessage) (any, error) {
	if len(rawArgs) == 0 {
		rawArgs = json.RawMessage("{}")
	}

	switch name {
	case "get_dashboard":
		return svc.Snapshot(), nil

	case "sync_repository":
		var args struct {
			Dir string `json:"dir"`
		}
		_ = json.Unmarshal(rawArgs, &args)
		return svc.SyncRepository(ctx, args.Dir)

	case "validate_pr_worktree":
		var args struct { PRNumber int `json:"pr_number"` }
		if err := json.Unmarshal(rawArgs,&args); err != nil { return nil,err }
		return svc.ValidatePRWorktree(ctx,args.PRNumber)

	case "list_pending_instructions":
		var args struct {
			SessionID string `json:"session_id"`
		}
		_ = json.Unmarshal(rawArgs, &args)
		return svc.PendingInstructions(args.SessionID), nil

	case "ack_instructions":
		var args struct {
			SessionID string `json:"session_id"`
			PRNumber  int    `json:"pr_number"`
		}
		_ = json.Unmarshal(rawArgs, &args)
		return map[string]any{"acked": svc.AckInstructions(args.SessionID, args.PRNumber)}, nil

	case "list_prs":
		var args struct {
			Status  string `json:"status"`
			StackID string `json:"stack_id"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return nil, err
		}
		return svc.ListPRs(args.Status, args.StackID), nil

	case "get_pr":
		var args struct {
			PRNumber int `json:"pr_number"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return nil, err
		}
		return svc.GetPR(args.PRNumber)

	case "send_pr_message":
		var args struct {
			PRNumber int    `json:"pr_number"`
			Author   string `json:"author"`
			Text     string `json:"text"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return nil, err
		}
		return svc.SendPRMessage(args.PRNumber, args.Author, args.Text)

	case "post_sbnn_comment":
		var args struct {
			PRNumber int    `json:"pr_number"`
			FilePath string `json:"file_path"`
			Line     int    `json:"line"`
			Body     string `json:"body"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return nil, err
		}
		return svc.PostSBNNComment(ctx, args.PRNumber, args.FilePath, args.Line, args.Body)

	case "request_review":
		var args struct {
			PRNumber int    `json:"pr_number"`
			Focus    string `json:"focus"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return nil, err
		}
		return svc.RequestReview(ctx, args.PRNumber, args.Focus)

	case "address_review":
		var args struct {
			PRNumber    int    `json:"pr_number"`
			Instruction string `json:"instruction"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return nil, err
		}
		return svc.AddressReview(args.PRNumber, args.Instruction)

	case "run_routine_action":
		var args struct {
			PRNumber int           `json:"pr_number"`
			Action   RoutineAction `json:"action"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return nil, err
		}
		return svc.RunRoutineAction(args.PRNumber, args.Action)

	case "set_auto_mode":
		var args struct {
			PRNumber int  `json:"pr_number"`
			Enabled  bool `json:"enabled"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return nil, err
		}
		return svc.SetAutoMode(args.PRNumber, args.Enabled)

	case "set_pr_status":
		var args struct {
			PRNumber int      `json:"pr_number"`
			Status   PRStatus `json:"status"`
			Summary  string   `json:"summary"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return nil, err
		}
		return svc.SetPRStatus(args.PRNumber, args.Status, args.Summary)

	case "send_session_instruction":
		var args struct {
			SessionID string `json:"session_id"`
			Text      string `json:"text"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return nil, err
		}
		return svc.SendDirectSessionInstruction(args.SessionID, args.Text)

	case "send_controller_instruction":
		var args struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return nil, err
		}
		return svc.SendControllerInstruction(args.Text)

	case "report_session_result":
		var args struct {
			SessionID string   `json:"session_id"`
			PRNumber  int      `json:"pr_number"`
			Status    PRStatus `json:"status"`
			Report    string   `json:"report"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return nil, err
		}
		return svc.ReportSessionResult(args.SessionID, args.PRNumber, args.Status, args.Report)

	case "set_model_tier":
		var args struct {
			SessionID    string    `json:"session_id"`
			PRNumber     int       `json:"pr_number"`
			Tier         ModelTier `json:"tier"`
			SubagentTier ModelTier `json:"subagent_tier"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return nil, err
		}
		if args.PRNumber > 0 {
			return svc.SetPRModelTier(args.PRNumber, args.Tier)
		}
		return svc.SetSessionModelTier(args.SessionID, args.Tier, args.SubagentTier)

	case "configure_model_presets":
		var args ModelPresetConfig
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return nil, err
		}
		return svc.UpdateModelConfig(args)

	case "mark_notification_read":
		var args struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return nil, err
		}
		if err := svc.MarkNotificationRead(args.ID); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil

	case "upsert_worktree":
		var wt Worktree
		if err := json.Unmarshal(rawArgs, &wt); err != nil {
			return nil, err
		}
		return svc.UpsertWorktree(wt)

	case "upsert_session":
		var sess Session
		if err := json.Unmarshal(rawArgs, &sess); err != nil {
			return nil, err
		}
		return svc.UpsertSession(sess)

	case "upsert_pr":
		var pr PullRequest
		if err := json.Unmarshal(rawArgs, &pr); err != nil {
			return nil, err
		}
		return svc.UpsertPR(pr)

	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
}

func mcpToolsList() []mcpTool {
	return []mcpTool{
		{
			Name:        "get_dashboard",
			Description: "Get the complete prdash snapshot for the target repository, including pull requests, stacked chains, sessions, worktrees, model presets, and notifications.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			Name:        "sync_repository",
			Description: "Discover local git worktrees (git worktree list) and open GitHub pull requests (gh pr list) and synchronize them into the dashboard.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"dir": map[string]any{"type": "string", "description": "Optional repository directory path"},
				},
			},
		},
		{
			Name: "validate_pr_worktree",
			Description: "Recheck the PR execution directory and current HEAD before applying queued changes.",
			InputSchema: map[string]any{
				"type":"object", "required":[]string{"pr_number"},
				"properties":map[string]any{"pr_number":map[string]any{"type":"integer"}},
			},
		},
		{
			Name:        "list_pending_instructions",
			Description: "List queued instructions for a worker session or the controller session.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"session_id": map[string]any{"type": "string", "description": "Optional session ID (or 'controller')"},
				},
			},
		},
		{
			Name:        "ack_instructions",
			Description: "Mark queued instructions for a session (or 'controller') as handled.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"session_id": map[string]any{"type": "string", "description": "Session ID (or 'controller')"},
					"pr_number":  map[string]any{"type": "integer", "description": "Optional pull request number filter"},
				},
			},
		},
		{
			Name:        "list_prs",
			Description: "List pull requests managed by agent sessions, optionally filtered by status (waiting_for_human or working) or stack_id.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"status":   map[string]any{"type": "string", "description": "Optional status filter: waiting_for_human or working"},
					"stack_id": map[string]any{"type": "string", "description": "Optional stack identifier filter"},
				},
			},
		},
		{
			Name:        "get_pr",
			Description: "Get detailed information and message thread for a single pull request by number.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"pr_number"},
				"properties": map[string]any{
					"pr_number": map[string]any{"type": "integer", "description": "Pull request number"},
				},
			},
		},
		{
			Name:        "send_pr_message",
			Description: "Send an instruction or message to the session responsible for a pull request.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"pr_number", "text"},
				"properties": map[string]any{
					"pr_number": map[string]any{"type": "integer", "description": "Pull request number"},
					"author":    map[string]any{"type": "string", "description": "Message author label"},
					"text":      map[string]any{"type": "string", "description": "Instruction or message text"},
				},
			},
		},
		{
			Name:        "post_sbnn_comment",
			Description: "Submit a code review comment on a pull request via the local sbnn review tool integration boundary.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"pr_number", "body"},
				"properties": map[string]any{
					"pr_number": map[string]any{"type": "integer", "description": "Pull request number"},
					"file_path": map[string]any{"type": "string", "description": "Optional file path for inline comment"},
					"line":      map[string]any{"type": "integer", "description": "Optional line number for inline comment"},
					"body":      map[string]any{"type": "string", "description": "Review comment text"},
				},
			},
		},
		{
			Name:        "request_review",
			Description: "Trigger an automated review on a pull request via sbnn.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"pr_number"},
				"properties": map[string]any{
					"pr_number": map[string]any{"type": "integer", "description": "Pull request number"},
					"focus":     map[string]any{"type": "string", "description": "Optional review focus area"},
				},
			},
		},
		{
			Name:        "address_review",
			Description: "Instruct the assigned agent session to address open review comments on a pull request.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"pr_number"},
				"properties": map[string]any{
					"pr_number":   map[string]any{"type": "integer", "description": "Pull request number"},
					"instruction": map[string]any{"type": "string", "description": "Optional guidance for addressing the review"},
				},
			},
		},
		{
			Name:        "run_routine_action",
			Description: "Execute a preset routine operation on a pull request (resolve_conflicts, rebase_stack, fix_ci, sync_branch, update_summary).",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"pr_number", "action"},
				"properties": map[string]any{
					"pr_number": map[string]any{"type": "integer", "description": "Pull request number"},
					"action":    map[string]any{"type": "string", "description": "One of: resolve_conflicts, rebase_stack, fix_ci, sync_branch, update_summary"},
				},
			},
		},
		{
			Name:        "set_auto_mode",
			Description: "Enable or disable Auto-Mode on a pull request.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"pr_number", "enabled"},
				"properties": map[string]any{
					"pr_number": map[string]any{"type": "integer", "description": "Pull request number"},
					"enabled":   map[string]any{"type": "boolean", "description": "True to enable Auto-Mode, false for manual mode"},
				},
			},
		},
		{
			Name:        "set_pr_status",
			Description: "Transition a pull request between waiting_for_human and working, notifying the human when ready.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"pr_number", "status"},
				"properties": map[string]any{
					"pr_number": map[string]any{"type": "integer", "description": "Pull request number"},
					"status":    map[string]any{"type": "string", "description": "waiting_for_human or working"},
					"summary":   map[string]any{"type": "string", "description": "Status change summary"},
				},
			},
		},
		{
			Name:        "send_session_instruction",
			Description: "Send a direct instruction to a worker session outside of a specific pull request thread.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"session_id", "text"},
				"properties": map[string]any{
					"session_id": map[string]any{"type": "string", "description": "Target worker session ID"},
					"text":       map[string]any{"type": "string", "description": "Direct instruction text"},
				},
			},
		},
		{
			Name:        "send_controller_instruction",
			Description: "Send an instruction to the dashboard controller session itself.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"text"},
				"properties": map[string]any{
					"text": map[string]any{"type": "string", "description": "Instruction for the dashboard controller session"},
				},
			},
		},
		{
			Name:        "report_session_result",
			Description: "Report progress or completion from a worker session back to the dashboard controller session.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"session_id", "pr_number", "status", "report"},
				"properties": map[string]any{
					"session_id": map[string]any{"type": "string", "description": "Worker session ID"},
					"pr_number":  map[string]any{"type": "integer", "description": "Pull request number"},
					"status":     map[string]any{"type": "string", "description": "waiting_for_human or working"},
					"report":     map[string]any{"type": "string", "description": "Result report from the session"},
				},
			},
		},
		{
			Name:        "set_model_tier",
			Description: "Set the three-tier model preset (pine, bamboo, plum) for a session or pull request.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"tier"},
				"properties": map[string]any{
					"session_id":    map[string]any{"type": "string", "description": "Session ID to update"},
					"pr_number":     map[string]any{"type": "integer", "description": "Or pull request number to update"},
					"tier":          map[string]any{"type": "string", "description": "Preset tier: pine, bamboo, or plum"},
					"subagent_tier": map[string]any{"type": "string", "description": "Optional subagent preset tier: pine, bamboo, or plum"},
				},
			},
		},
		{
			Name:        "configure_model_presets",
			Description: "Configure the concrete model identifiers mapped to the pine, bamboo, and plum presets.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pine":   map[string]any{"type": "string", "description": "High-performance model identifier"},
					"bamboo": map[string]any{"type": "string", "description": "Standard balanced model identifier"},
					"plum":   map[string]any{"type": "string", "description": "Fast lightweight model identifier"},
				},
			},
		},
		{
			Name:        "mark_notification_read",
			Description: "Mark a notification ID or 'all' notifications as read.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{"type": "string", "description": "Notification ID or 'all'"},
				},
			},
		},
		{
			Name:        "upsert_worktree",
			Description: "Register or update a git worktree bound to an agent session.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"id", "path", "branch"},
				"properties": map[string]any{
					"id":         map[string]any{"type": "string"},
					"path":       map[string]any{"type": "string"},
					"branch":     map[string]any{"type": "string"},
					"head_sha":   map[string]any{"type": "string"},
					"clean":      map[string]any{"type": "boolean"},
					"session_id": map[string]any{"type": "string"},
				},
			},
		},
		{
			Name:        "upsert_session",
			Description: "Register or update a worker session bound to a git worktree.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"id", "name"},
				"properties": map[string]any{
					"id":                  map[string]any{"type": "string"},
					"name":                map[string]any{"type": "string"},
					"worktree_id":         map[string]any{"type": "string"},
					"status":              map[string]any{"type": "string"},
					"model_tier":          map[string]any{"type": "string"},
					"subagent_model_tier": map[string]any{"type": "string"},
					"active_task":         map[string]any{"type": "string"},
				},
			},
		},
		{
			Name:        "upsert_pr",
			Description: "Register or update a pull request managed by an agent session.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"number", "title", "branch", "session_id", "worktree_id"},
				"properties": map[string]any{
					"number":           map[string]any{"type": "integer"},
					"title":            map[string]any{"type": "string"},
					"description":      map[string]any{"type": "string"},
					"branch":           map[string]any{"type": "string"},
					"base_branch":      map[string]any{"type": "string"},
					"head_sha": map[string]any{"type":"string","description":"Full target commit SHA"},
					"session_id":       map[string]any{"type": "string"},
					"worktree_id":      map[string]any{"type": "string"},
					"stack_id":         map[string]any{"type": "string"},
					"stack_order":      map[string]any{"type": "integer"},
					"parent_pr_number": map[string]any{"type": "integer"},
					"status":           map[string]any{"type": "string"},
					"auto_mode":        map[string]any{"type": "boolean"},
					"model_tier":       map[string]any{"type": "string"},
				},
			},
		},
	}
}
