package prdash

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"time"
)

//go:embed web/*
var webEmbedFS embed.FS

// NewHandler returns an http.Handler serving the prdash web UI and JSON/SSE APIs.
func NewHandler(svc *Service) http.Handler {
	mux := http.NewServeMux()
	registerInstructionHandlers(mux, svc)

	webSub, err := fs.Sub(webEmbedFS, "web")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(webSub))

	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"config": svc.EffectiveConfig(), "error": svc.Snapshot().ConfigError})
	})
	mux.HandleFunc("POST /api/config/reload", func(w http.ResponseWriter, r *http.Request) {
		cfg, err := svc.ReloadConfig(r.Context())
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, cfg)
	})

	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, svc.Snapshot())
	})

	mux.HandleFunc("POST /api/sync", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Dir string `json:"dir"`
		}
		if decErr := decodeJSON(r, &body); decErr != nil {
			writeError(w, http.StatusBadRequest, decErr)
			return
		}
		snap, syncErr := svc.SyncRepository(r.Context(), body.Dir)
		if syncErr != nil {
			writeServiceError(w, syncErr)
			return
		}
		writeJSON(w, http.StatusOK, snap)
	})

	mux.HandleFunc("GET /api/sessions/{id}/instructions", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		writeJSON(w, http.StatusOK, svc.PendingInstructions(id))
	})

	mux.HandleFunc("POST /api/sessions/{id}/ack", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var body struct {
			PRNumber int `json:"pr_number"`
		}
		if decErr := decodeJSON(r, &body); decErr != nil {
			writeError(w, http.StatusBadRequest, decErr)
			return
		}
		acked := svc.AckInstructions(id, body.PRNumber)
		writeJSON(w, http.StatusOK, map[string]any{"acked": acked})
	})

	mux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeError(w, http.StatusInternalServerError, errors.New("streaming unsupported"))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		ch, cancel := svc.Subscribe()
		defer cancel()

		sendSnap := func(snap Snapshot) error {
			data, marshalErr := json.Marshal(snap)
			if marshalErr != nil {
				return marshalErr
			}
			if _, writeErr := fmt.Fprintf(w, "data: %s\n\n", data); writeErr != nil {
				return writeErr
			}
			flusher.Flush()
			return nil
		}

		if sendErr := sendSnap(svc.Snapshot()); sendErr != nil {
			return
		}

		ticker := time.NewTicker(25 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-r.Context().Done():
				return
			case snap, ok := <-ch:
				if !ok {
					return
				}
				if sendErr := sendSnap(snap); sendErr != nil {
					return
				}
			case <-ticker.C:
				_, _ = fmt.Fprintf(w, ": heartbeat\n\n")
				flusher.Flush()
			}
		}
	})

	mux.HandleFunc("GET /api/prs/{number}", func(w http.ResponseWriter, r *http.Request) {
		num, parseErr := parsePRNumber(r)
		if parseErr != nil {
			writeError(w, http.StatusBadRequest, parseErr)
			return
		}
		pr, getErr := svc.GetPR(num)
		if getErr != nil {
			writeServiceError(w, getErr)
			return
		}
		writeJSON(w, http.StatusOK, pr)
	})

	mux.HandleFunc("POST /api/prs/{number}/messages", func(w http.ResponseWriter, r *http.Request) {
		num, parseErr := parsePRNumber(r)
		if parseErr != nil {
			writeError(w, http.StatusBadRequest, parseErr)
			return
		}
		var body struct {
			Author string `json:"author"`
			Text   string `json:"text"`
		}
		if decErr := decodeJSON(r, &body); decErr != nil {
			writeError(w, http.StatusBadRequest, decErr)
			return
		}
		pr, sendErr := svc.SendPRMessage(num, body.Author, body.Text)
		if sendErr != nil {
			writeServiceError(w, sendErr)
			return
		}
		writeJSON(w, http.StatusOK, pr)
	})

	mux.HandleFunc("POST /api/prs/{number}/sbnn-comment", func(w http.ResponseWriter, r *http.Request) {
		num, parseErr := parsePRNumber(r)
		if parseErr != nil {
			writeError(w, http.StatusBadRequest, parseErr)
			return
		}
		var body struct {
			FilePath string `json:"file_path"`
			Line     int    `json:"line"`
			Body     string `json:"body"`
		}
		if decErr := decodeJSON(r, &body); decErr != nil {
			writeError(w, http.StatusBadRequest, decErr)
			return
		}
		pr, postErr := svc.PostSBNNComment(r.Context(), num, body.FilePath, body.Line, body.Body)
		if postErr != nil {
			writeServiceError(w, postErr)
			return
		}
		writeJSON(w, http.StatusOK, pr)
	})

	mux.HandleFunc("POST /api/prs/{number}/validate-worktree", func(w http.ResponseWriter, r *http.Request) {
		num, err := parsePRNumber(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		wt, validationErr := svc.ValidatePRWorktree(r.Context(), num)
		if validationErr != nil {
			writeServiceError(w, validationErr)
			return
		}
		writeJSON(w, http.StatusOK, wt)
	})

	mux.HandleFunc("POST /api/prs/{number}/request-review", func(w http.ResponseWriter, r *http.Request) {
		num, parseErr := parsePRNumber(r)
		if parseErr != nil {
			writeError(w, http.StatusBadRequest, parseErr)
			return
		}
		var body struct {
			Focus string `json:"focus"`
		}
		if decErr := decodeJSON(r, &body); decErr != nil {
			writeError(w, http.StatusBadRequest, decErr)
			return
		}
		pr, reqErr := svc.RequestReview(r.Context(), num, body.Focus)
		if reqErr != nil {
			writeServiceError(w, reqErr)
			return
		}
		writeJSON(w, http.StatusOK, pr)
	})

	mux.HandleFunc("POST /api/prs/{number}/address-review", func(w http.ResponseWriter, r *http.Request) {
		num, parseErr := parsePRNumber(r)
		if parseErr != nil {
			writeError(w, http.StatusBadRequest, parseErr)
			return
		}
		var body struct {
			Instruction string `json:"instruction"`
		}
		if decErr := decodeJSON(r, &body); decErr != nil {
			writeError(w, http.StatusBadRequest, decErr)
			return
		}
		pr, addrErr := svc.AddressReview(num, body.Instruction)
		if addrErr != nil {
			writeServiceError(w, addrErr)
			return
		}
		writeJSON(w, http.StatusOK, pr)
	})

	mux.HandleFunc("POST /api/prs/{number}/routine", func(w http.ResponseWriter, r *http.Request) {
		num, parseErr := parsePRNumber(r)
		if parseErr != nil {
			writeError(w, http.StatusBadRequest, parseErr)
			return
		}
		var body struct {
			Action RoutineAction `json:"action"`
		}
		if decErr := decodeJSON(r, &body); decErr != nil {
			writeError(w, http.StatusBadRequest, decErr)
			return
		}
		pr, runErr := svc.RunRoutineAction(num, body.Action)
		if runErr != nil {
			writeServiceError(w, runErr)
			return
		}
		writeJSON(w, http.StatusOK, pr)
	})

	mux.HandleFunc("POST /api/prs/{number}/auto-mode", func(w http.ResponseWriter, r *http.Request) {
		num, parseErr := parsePRNumber(r)
		if parseErr != nil {
			writeError(w, http.StatusBadRequest, parseErr)
			return
		}
		var body struct {
			Enabled bool `json:"enabled"`
		}
		if decErr := decodeJSON(r, &body); decErr != nil {
			writeError(w, http.StatusBadRequest, decErr)
			return
		}
		pr, modeErr := svc.SetAutoMode(num, body.Enabled)
		if modeErr != nil {
			writeServiceError(w, modeErr)
			return
		}
		writeJSON(w, http.StatusOK, pr)
	})

	mux.HandleFunc("POST /api/prs/{number}/status", func(w http.ResponseWriter, r *http.Request) {
		num, parseErr := parsePRNumber(r)
		if parseErr != nil {
			writeError(w, http.StatusBadRequest, parseErr)
			return
		}
		var body struct {
			Status  PRStatus `json:"status"`
			Summary string   `json:"summary"`
		}
		if decErr := decodeJSON(r, &body); decErr != nil {
			writeError(w, http.StatusBadRequest, decErr)
			return
		}
		pr, statErr := svc.SetPRStatus(num, body.Status, body.Summary)
		if statErr != nil {
			writeServiceError(w, statErr)
			return
		}
		writeJSON(w, http.StatusOK, pr)
	})

	mux.HandleFunc("POST /api/prs/{number}/model-tier", func(w http.ResponseWriter, r *http.Request) {
		num, parseErr := parsePRNumber(r)
		if parseErr != nil {
			writeError(w, http.StatusBadRequest, parseErr)
			return
		}
		var body struct {
			Tier ModelTier `json:"tier"`
		}
		if decErr := decodeJSON(r, &body); decErr != nil {
			writeError(w, http.StatusBadRequest, decErr)
			return
		}
		pr, tierErr := svc.SetPRModelTier(num, body.Tier)
		if tierErr != nil {
			writeServiceError(w, tierErr)
			return
		}
		writeJSON(w, http.StatusOK, pr)
	})

	mux.HandleFunc("POST /api/sessions/{id}/instruction", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var body struct {
			Text string `json:"text"`
		}
		if decErr := decodeJSON(r, &body); decErr != nil {
			writeError(w, http.StatusBadRequest, decErr)
			return
		}
		sess, instErr := svc.SendDirectSessionInstruction(id, body.Text)
		if instErr != nil {
			writeServiceError(w, instErr)
			return
		}
		writeJSON(w, http.StatusOK, sess)
	})

	mux.HandleFunc("POST /api/sessions/{id}/model-tier", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var body struct {
			Tier         ModelTier `json:"tier"`
			SubagentTier ModelTier `json:"subagent_tier"`
		}
		if decErr := decodeJSON(r, &body); decErr != nil {
			writeError(w, http.StatusBadRequest, decErr)
			return
		}
		sess, tierErr := svc.SetSessionModelTier(id, body.Tier, body.SubagentTier)
		if tierErr != nil {
			writeServiceError(w, tierErr)
			return
		}
		writeJSON(w, http.StatusOK, sess)
	})

	mux.HandleFunc("POST /api/sessions/{id}/report", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var body struct {
			PRNumber int      `json:"pr_number"`
			Status   PRStatus `json:"status"`
			Report   string   `json:"report"`
		}
		if decErr := decodeJSON(r, &body); decErr != nil {
			writeError(w, http.StatusBadRequest, decErr)
			return
		}
		pr, repErr := svc.ReportSessionResult(id, body.PRNumber, body.Status, body.Report)
		if repErr != nil {
			writeServiceError(w, repErr)
			return
		}
		writeJSON(w, http.StatusOK, pr)
	})

	mux.HandleFunc("POST /api/controller/instruction", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		if decErr := decodeJSON(r, &body); decErr != nil {
			writeError(w, http.StatusBadRequest, decErr)
			return
		}
		sess, instErr := svc.SendControllerInstruction(body.Text)
		if instErr != nil {
			writeServiceError(w, instErr)
			return
		}
		writeJSON(w, http.StatusOK, sess)
	})

	mux.HandleFunc("POST /api/models", func(w http.ResponseWriter, r *http.Request) {
		var cfg ModelPresetConfig
		if decErr := decodeJSON(r, &cfg); decErr != nil {
			writeError(w, http.StatusBadRequest, decErr)
			return
		}
		updated, updErr := svc.UpdateModelConfig(cfg)
		if updErr != nil {
			writeServiceError(w, updErr)
			return
		}
		writeJSON(w, http.StatusOK, updated)
	})

	mux.HandleFunc("POST /api/notifications/read", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ID string `json:"id"`
		}
		if decErr := decodeJSON(r, &body); decErr != nil {
			writeError(w, http.StatusBadRequest, decErr)
			return
		}
		if readErr := svc.MarkNotificationRead(body.ID); readErr != nil {
			writeServiceError(w, readErr)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	mux.HandleFunc("POST /api/worktrees", func(w http.ResponseWriter, r *http.Request) {
		var wt Worktree
		if decErr := decodeJSON(r, &wt); decErr != nil {
			writeError(w, http.StatusBadRequest, decErr)
			return
		}
		updated, upErr := svc.UpsertWorktree(wt)
		if upErr != nil {
			writeServiceError(w, upErr)
			return
		}
		writeJSON(w, http.StatusOK, updated)
	})

	mux.HandleFunc("POST /api/sessions", func(w http.ResponseWriter, r *http.Request) {
		var sess Session
		if decErr := decodeJSON(r, &sess); decErr != nil {
			writeError(w, http.StatusBadRequest, decErr)
			return
		}
		updated, upErr := svc.UpsertSession(sess)
		if upErr != nil {
			writeServiceError(w, upErr)
			return
		}
		writeJSON(w, http.StatusOK, updated)
	})

	mux.HandleFunc("POST /api/prs", func(w http.ResponseWriter, r *http.Request) {
		var pr PullRequest
		if decErr := decodeJSON(r, &pr); decErr != nil {
			writeError(w, http.StatusBadRequest, decErr)
			return
		}
		updated, upErr := svc.UpsertPR(pr)
		if upErr != nil {
			writeServiceError(w, upErr)
			return
		}
		writeJSON(w, http.StatusOK, updated)
	})

	mux.Handle("/", fileServer)
	return protectLocalHTTP(mux)
}

func parsePRNumber(r *http.Request) (int, error) {
	raw := r.PathValue("number")
	num, err := strconv.Atoi(raw)
	if err != nil || num <= 0 {
		return 0, fmt.Errorf("invalid pull request number: %q", raw)
	}
	return num, nil
}

func decodeJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain one JSON value")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrConfigValidation):
		writeError(w, http.StatusBadRequest, err)
	case errors.Is(err, ErrInstructionNotFound), errors.Is(err, ErrPRNotFound), errors.Is(err, ErrSessionNotFound), errors.Is(err, ErrNotificationNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, ErrInstructionSession), errors.Is(err, ErrInstructionConflict):
		writeError(w, http.StatusConflict, err)
	case errors.Is(err, ErrInstructionInput), errors.Is(err, ErrInvalidModelTier), errors.Is(err, ErrInvalidPRStatus), errors.Is(err, ErrInvalidRoutineAction), errors.Is(err, ErrEmptyMessage):
		writeError(w, http.StatusBadRequest, err)
	case errors.Is(err, ErrReviewWorktreeUnavailable):
		writeError(w, http.StatusConflict, err)
	default:
		writeError(w, http.StatusInternalServerError, err)
	}
}
