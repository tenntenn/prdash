package prdash

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
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

	webSub, err := fs.Sub(webEmbedFS, "web")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(webSub))

	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, svc.Snapshot())
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
			data, err := json.Marshal(snap)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(w, "data: %s\n\n", data)
			if err != nil {
				return err
			}
			flusher.Flush()
			return nil
		}

		if err := sendSnap(svc.Snapshot()); err != nil {
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
				if err := sendSnap(snap); err != nil {
					return
				}
			case <-ticker.C:
				_, _ = fmt.Fprintf(w, ": heartbeat\n\n")
				flusher.Flush()
			}
		}
	})

	mux.HandleFunc("GET /api/prs/{number}", func(w http.ResponseWriter, r *http.Request) {
		num, err := parsePRNumber(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		pr, err := svc.GetPR(num)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, pr)
	})

	mux.HandleFunc("POST /api/prs/{number}/messages", func(w http.ResponseWriter, r *http.Request) {
		num, err := parsePRNumber(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		var body struct {
			Author string `json:"author"`
			Text   string `json:"text"`
		}
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		pr, err := svc.SendPRMessage(num, body.Author, body.Text)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, pr)
	})

	mux.HandleFunc("POST /api/prs/{number}/sbnn-comment", func(w http.ResponseWriter, r *http.Request) {
		num, err := parsePRNumber(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		var body struct {
			FilePath string `json:"file_path"`
			Line     int    `json:"line"`
			Body     string `json:"body"`
		}
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		pr, err := svc.PostSBNNComment(r.Context(), num, body.FilePath, body.Line, body.Body)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, pr)
	})

	mux.HandleFunc("POST /api/prs/{number}/request-review", func(w http.ResponseWriter, r *http.Request) {
		num, err := parsePRNumber(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		var body struct {
			Focus string `json:"focus"`
		}
		_ = decodeJSON(r, &body)
		pr, err := svc.RequestReview(r.Context(), num, body.Focus)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, pr)
	})

	mux.HandleFunc("POST /api/prs/{number}/address-review", func(w http.ResponseWriter, r *http.Request) {
		num, err := parsePRNumber(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		var body struct {
			Instruction string `json:"instruction"`
		}
		_ = decodeJSON(r, &body)
		pr, err := svc.AddressReview(num, body.Instruction)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, pr)
	})

	mux.HandleFunc("POST /api/prs/{number}/routine", func(w http.ResponseWriter, r *http.Request) {
		num, err := parsePRNumber(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		var body struct {
			Action RoutineAction `json:"action"`
		}
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		pr, err := svc.RunRoutineAction(num, body.Action)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, pr)
	})

	mux.HandleFunc("POST /api/prs/{number}/auto-mode", func(w http.ResponseWriter, r *http.Request) {
		num, err := parsePRNumber(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		var body struct {
			Enabled bool `json:"enabled"`
		}
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		pr, err := svc.SetAutoMode(num, body.Enabled)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, pr)
	})

	mux.HandleFunc("POST /api/prs/{number}/status", func(w http.ResponseWriter, r *http.Request) {
		num, err := parsePRNumber(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		var body struct {
			Status  PRStatus `json:"status"`
			Summary string   `json:"summary"`
		}
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		pr, err := svc.SetPRStatus(num, body.Status, body.Summary)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, pr)
	})

	mux.HandleFunc("POST /api/prs/{number}/model-tier", func(w http.ResponseWriter, r *http.Request) {
		num, err := parsePRNumber(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		var body struct {
			Tier ModelTier `json:"tier"`
		}
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		pr, err := svc.SetPRModelTier(num, body.Tier)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, pr)
	})

	mux.HandleFunc("POST /api/sessions/{id}/instruction", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var body struct {
			Text string `json:"text"`
		}
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		sess, err := svc.SendDirectSessionInstruction(id, body.Text)
		if err != nil {
			writeServiceError(w, err)
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
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		sess, err := svc.SetSessionModelTier(id, body.Tier, body.SubagentTier)
		if err != nil {
			writeServiceError(w, err)
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
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		pr, err := svc.ReportSessionResult(id, body.PRNumber, body.Status, body.Report)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, pr)
	})

	mux.HandleFunc("POST /api/controller/instruction", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		sess, err := svc.SendControllerInstruction(body.Text)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, sess)
	})

	mux.HandleFunc("POST /api/models", func(w http.ResponseWriter, r *http.Request) {
		var cfg ModelPresetConfig
		if err := decodeJSON(r, &cfg); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		updated, err := svc.UpdateModelConfig(cfg)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, updated)
	})

	mux.HandleFunc("POST /api/notifications/read", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ID string `json:"id"`
		}
		_ = decodeJSON(r, &body)
		if err := svc.MarkNotificationRead(body.ID); err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	mux.Handle("/", fileServer)
	return mux
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
	return dec.Decode(dst)
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
	case errors.Is(err, ErrPRNotFound), errors.Is(err, ErrSessionNotFound), errors.Is(err, ErrNotificationNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, ErrInvalidModelTier), errors.Is(err, ErrInvalidPRStatus), errors.Is(err, ErrInvalidRoutineAction), errors.Is(err, ErrEmptyMessage):
		writeError(w, http.StatusBadRequest, err)
	default:
		writeError(w, http.StatusInternalServerError, err)
	}
}
