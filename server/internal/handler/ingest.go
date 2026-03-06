package handler

import (
	"net/http"

	"github.com/qiffang/mnemos/server/internal/service"
)

type ingestRequest struct {
	Messages  []service.IngestMessage `json:"messages"`
	SessionID string                  `json:"session_id"`
	AgentID   string                  `json:"agent_id"`
	Mode      service.IngestMode      `json:"mode,omitempty"`
}

func (s *Server) ingestMemories(w http.ResponseWriter, r *http.Request) {
	var req ingestRequest
	if err := decode(r, &req); err != nil {
		s.handleError(w, err)
		return
	}

	auth := authInfo(r)
	svc := s.resolveServices(auth)

	agentID := req.AgentID
	if agentID == "" {
		agentID = auth.AgentName
	}

	ingestReq := service.IngestRequest{
		Messages:  req.Messages,
		SessionID: req.SessionID,
		AgentID:   agentID,
		Mode:      req.Mode,
	}

	result, err := svc.ingest.Ingest(r.Context(), auth.AgentName, ingestReq)
	if err != nil {
		s.handleError(w, err)
		return
	}

	status := http.StatusOK
	switch result.Status {
	case "complete":
		status = http.StatusOK
	case "partial":
		status = 207 // Multi-Status
	case "failed":
		status = http.StatusBadGateway
	}

	respond(w, status, result)
}
