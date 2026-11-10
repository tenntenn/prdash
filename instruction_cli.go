package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/tenntenn/prdash/prdash"
)

// runInstructionCmd uses the running server as the sole instruction-state owner.
// It deliberately never opens a shared state file in this client process.
func runInstructionCmd(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: prdash instruction get|ack|claim|result|retry -server URL -session ID -id ID")
	}
	action := args[0]
	switch action {
	case "get", "ack", "claim", "result", "retry":
	default:
		return fmt.Errorf("unknown instruction action: %s", action)
	}
	fs := flag.NewFlagSet("prdash instruction "+action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	server := fs.String("server", "", "Running prdash server URL (single state owner)")
	session := fs.String("session", "", "Assigned session ID")
	id := fs.String("id", "", "Instruction ID")
	consumer := fs.String("consumer", "", "Stable unique consumer ID")
	target := fs.String("target-commit", "", "Target commit from the claim")
	token := fs.String("claim-token", "", "Claim capability (or PRDASH_CLAIM_TOKEN)")
	attempt := fs.Int("attempt", 0, "Claim attempt number")
	outcome := fs.String("outcome", "completed", "completed or failed")
	status := fs.String("status", string(prdash.StatusWaitingForHuman), "working or waiting_for_human")
	report := fs.String("m", "", "Result report")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *server == "" || *session == "" || *id == "" {
		return errors.New("-server, -session and -id are required")
	}
	base, err := url.Parse(*server)
	if err != nil {
		return err
	}
	if (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return errors.New("-server must be an HTTP(S) URL without credentials, query or fragment")
	}
	if *token == "" {
		*token = os.Getenv("PRDASH_CLAIM_TOKEN")
	}
	var body any
	switch action {
	case "claim":
		body = prdash.InstructionClaimRequest{ConsumerID: *consumer, TargetCommit: *target}
	case "result":
		body = prdash.InstructionResultRequest{ConsumerID: *consumer, ClaimToken: *token, Attempt: *attempt, TargetCommit: *target, Outcome: prdash.InstructionState(*outcome), Status: prdash.PRStatus(*status), Report: *report}
	case "retry":
		body = prdash.InstructionRetryRequest{ConsumerID: *consumer, ClaimToken: *token, Attempt: *attempt}
	default:
		body = struct{}{}
	}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	endpoint := strings.TrimRight(base.String(), "/") + "/api/sessions/" + url.PathEscape(*session) + "/instructions/" + url.PathEscape(*id)
	method := http.MethodPost
	if action == "get" {
		method = http.MethodGet
	} else {
		endpoint += "/" + action
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	auth := os.Getenv("PRDASH_API_TOKEN")
	code, response, err := instructionRequest(ctx, client, method, endpoint, data, auth, false)
	if err != nil {
		return err
	}
	// Retry only a server's explicit pre-operation authentication rejection.
	var rejection struct {
		Code string `json:"code"`
	}
	if code == http.StatusUnauthorized && json.Unmarshal(response, &rejection) == nil && rejection.Code == "authentication_required" {
		authCode, authBody, authErr := instructionRequest(ctx, client, http.MethodGet, strings.TrimRight(base.String(), "/")+"/api/auth", nil, "", true)
		if authErr != nil {
			return authErr
		}
		if authCode != http.StatusOK {
			return fmt.Errorf("authentication bootstrap: HTTP %d", authCode)
		}
		var credentials struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(authBody, &credentials); err != nil {
			return err
		}
		if credentials.Token == "" {
			return errors.New("authentication bootstrap returned no token")
		}
		code, response, err = instructionRequest(ctx, client, method, endpoint, data, credentials.Token, false)
		if err != nil {
			return err
		}
	}
	if code < 200 || code >= 300 {
		return fmt.Errorf("instruction %s: HTTP %d: %s", action, code, strings.TrimSpace(string(response)))
	}
	var result any
	if err := json.Unmarshal(response, &result); err != nil {
		return err
	}
	return writePrettyJSON(stdout, result)
}

func instructionRequest(ctx context.Context, client *http.Client, method, endpoint string, data []byte, token string, bootstrap bool) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(data))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if bootstrap {
		req.Header.Set("X-Prdash-Client", "dashboard")
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, payload, err
}
