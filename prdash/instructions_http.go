package prdash

import "net/http"

func registerInstructionHandlers(mux *http.ServeMux, svc *Service) {
	mux.HandleFunc("GET /api/sessions/{id}/instructions/{instruction}", func(w http.ResponseWriter, r *http.Request) {
		msg, err := svc.GetInstruction(r.PathValue("id"), r.PathValue("instruction"))
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, msg)
	})
	mux.HandleFunc("POST /api/sessions/{id}/instructions/{instruction}/ack", func(w http.ResponseWriter, r *http.Request) {
		msg, err := svc.AckInstruction(r.PathValue("id"), r.PathValue("instruction"))
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, msg)
	})
	mux.HandleFunc("POST /api/sessions/{id}/instructions/{instruction}/claim", func(w http.ResponseWriter, r *http.Request) {
		var req InstructionClaimRequest
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		req.SessionID = r.PathValue("id")
		req.InstructionID = r.PathValue("instruction")
		claim, err := svc.ClaimInstruction(req)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, claim)
	})
	mux.HandleFunc("POST /api/sessions/{id}/instructions/{instruction}/result", func(w http.ResponseWriter, r *http.Request) {
		var req InstructionResultRequest
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		req.SessionID = r.PathValue("id")
		req.InstructionID = r.PathValue("instruction")
		msg, err := svc.ReportInstructionResult(req)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, msg)
	})
	mux.HandleFunc("POST /api/sessions/{id}/instructions/{instruction}/retry", func(w http.ResponseWriter, r *http.Request) {
		var req InstructionRetryRequest
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		req.SessionID = r.PathValue("id")
		req.InstructionID = r.PathValue("instruction")
		msg, err := svc.RetryInstruction(req)
		if err != nil {
			writeServiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, msg)
	})
}
