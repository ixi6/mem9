package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/qiffang/mnemos/server/internal/domain"
	"github.com/qiffang/mnemos/server/internal/service"
)

// computeAge sets the Age field on memories based on their UpdatedAt timestamp.
func computeAge(memories []domain.Memory) {
	for i := range memories {
		memories[i].Age = domain.FormatAge(memories[i].UpdatedAt)
	}
}

// computeAgePtr sets the Age field on a single memory pointer.
func computeAgePtr(mem *domain.Memory) {
	if mem != nil {
		mem.Age = domain.FormatAge(mem.UpdatedAt)
	}
}

type createMemoryRequest struct {
	Content   string                  `json:"content,omitempty"`
	AgentID   string                  `json:"agent_id,omitempty"`
	Tags      []string                `json:"tags,omitempty"`
	Metadata  json.RawMessage         `json:"metadata,omitempty"`
	Messages  []service.IngestMessage `json:"messages,omitempty"`
	SessionID string                  `json:"session_id,omitempty"`
	Mode      service.IngestMode      `json:"mode,omitempty"`
}

func (s *Server) createMemory(w http.ResponseWriter, r *http.Request) {
	var req createMemoryRequest
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

	hasMessages := len(req.Messages) > 0
	hasContent := strings.TrimSpace(req.Content) != ""

	if hasMessages && hasContent {
		s.handleError(w, &domain.ValidationError{Field: "body", Message: "provide either content or messages, not both"})
		return
	}

	if hasMessages {
		messages := append([]service.IngestMessage(nil), req.Messages...)
		ingestReq := service.IngestRequest{
			Messages:  messages,
			SessionID: req.SessionID,
			AgentID:   agentID,
			Mode:      req.Mode,
		}

		go func(agentName string, req service.IngestRequest) {
			result, err := svc.ingest.Ingest(context.Background(), agentName, req)
			if err != nil {
				slog.Error("async memories ingest failed", "agent", req.AgentID, "session", req.SessionID, "err", err)
				return
			}
			slog.Info("async memories ingest complete", "agent", req.AgentID, "session", req.SessionID, "status", result.Status, "memories_changed", result.MemoriesChanged)
		}(auth.AgentName, ingestReq)

		respond(w, http.StatusAccepted, map[string]string{"status": "accepted"})
		return
	}

	if !hasContent {
		s.handleError(w, &domain.ValidationError{Field: "content", Message: "content or messages required"})
		return
	}
	if req.Mode != "" {
		s.handleError(w, &domain.ValidationError{Field: "body", Message: "content mode does not accept mode"})
		return
	}

	tags := append([]string(nil), req.Tags...)
	metadata := append(json.RawMessage(nil), req.Metadata...)
	content := req.Content

	go func(agentName, actorAgentID, content string, tags []string, metadata json.RawMessage) {
		mem, err := svc.memory.Create(context.Background(), actorAgentID, content, tags, metadata)
		if err != nil {
			slog.Error("async memory create failed", "agent", actorAgentID, "actor", agentName, "err", err)
			return
		}
		if mem != nil {
			slog.Info("async memory create complete", "agent", actorAgentID, "actor", agentName, "memory_id", mem.ID)
			return
		}
		slog.Info("async memory create complete", "agent", actorAgentID, "actor", agentName, "memory_id", "")
	}(auth.AgentName, agentID, content, tags, metadata)

	respond(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

type listResponse struct {
	Memories []domain.Memory `json:"memories"`
	Total    int             `json:"total"`
	Limit    int             `json:"limit"`
	Offset   int             `json:"offset"`
}

func (s *Server) listMemories(w http.ResponseWriter, r *http.Request) {
	auth := authInfo(r)
	q := r.URL.Query()

	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	var tags []string
	if t := q.Get("tags"); t != "" {
		tags = strings.Split(t, ",")
	}

	includeHistory := q.Get("include_history") == "true"

	filter := domain.MemoryFilter{
		Query:          q.Get("q"),
		Tags:           tags,
		Source:         q.Get("source"),
		State:          q.Get("state"),
		MemoryType:     q.Get("memory_type"),
		AgentID:        q.Get("agent_id"),
		SessionID:      q.Get("session_id"),
		Limit:          limit,
		Offset:         offset,
		IncludeHistory: includeHistory,
	}
	svc := s.resolveServices(auth)
	memories, total, err := svc.memory.Search(r.Context(), filter)
	if err != nil {
		s.handleError(w, err)
		return
	}

	if memories == nil {
		memories = []domain.Memory{}
	}

	// Compute age for all memories
	computeAge(memories)

	// Fetch history chain if requested
	if includeHistory {
		s.attachHistory(r.Context(), svc, memories)
	}

	respond(w, http.StatusOK, listResponse{
		Memories: memories,
		Total:    total,
		Limit:    limit,
		Offset:   offset,
	})
}

func (s *Server) getMemory(w http.ResponseWriter, r *http.Request) {
	auth := authInfo(r)
	svc := s.resolveServices(auth)
	id := chi.URLParam(r, "id")

	mem, err := svc.memory.Get(r.Context(), id)
	if err != nil {
		s.handleError(w, err)
		return
	}

	// Compute age
	computeAgePtr(mem)

	// Check if history is requested
	if r.URL.Query().Get("include_history") == "true" {
		history, histErr := svc.memory.GetPredecessorChain(r.Context(), id, 2)
		if histErr == nil && len(history) > 0 {
			computeAge(history)
			mem.History = history
		}
	}

	respond(w, http.StatusOK, mem)
}

type updateMemoryRequest struct {
	Content  string          `json:"content,omitempty"`
	Tags     []string        `json:"tags,omitempty"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

func (s *Server) updateMemory(w http.ResponseWriter, r *http.Request) {
	var req updateMemoryRequest
	if err := decode(r, &req); err != nil {
		s.handleError(w, err)
		return
	}

	auth := authInfo(r)
	svc := s.resolveServices(auth)
	id := chi.URLParam(r, "id")

	var ifMatch int
	if h := r.Header.Get("If-Match"); h != "" {
		ifMatch, _ = strconv.Atoi(h)
	}

	mem, err := svc.memory.Update(r.Context(), auth.AgentName, id, req.Content, req.Tags, req.Metadata, ifMatch)
	if err != nil {
		s.handleError(w, err)
		return
	}

	// Compute age
	computeAgePtr(mem)

	w.Header().Set("ETag", strconv.Itoa(mem.Version))
	respond(w, http.StatusOK, mem)
}

func (s *Server) deleteMemory(w http.ResponseWriter, r *http.Request) {
	auth := authInfo(r)
	svc := s.resolveServices(auth)
	id := chi.URLParam(r, "id")

	if err := svc.memory.Delete(r.Context(), id, auth.AgentName); err != nil {
		s.handleError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

type bulkCreateRequest struct {
	Memories []service.BulkMemoryInput `json:"memories"`
}

func (s *Server) bulkCreateMemories(w http.ResponseWriter, r *http.Request) {
	var req bulkCreateRequest
	if err := decode(r, &req); err != nil {
		s.handleError(w, err)
		return
	}

	auth := authInfo(r)
	svc := s.resolveServices(auth)
	memories, err := svc.memory.BulkCreate(r.Context(), auth.AgentName, req.Memories)
	if err != nil {
		s.handleError(w, err)
		return
	}

	// Compute age for all memories
	computeAge(memories)

	respond(w, http.StatusCreated, map[string]any{
		"ok":       true,
		"memories": memories,
	})
}

func (s *Server) bootstrapMemories(w http.ResponseWriter, r *http.Request) {
	auth := authInfo(r)
	svc := s.resolveServices(auth)

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 20
	}

	memories, err := svc.memory.Bootstrap(r.Context(), limit)
	if err != nil {
		s.handleError(w, err)
		return
	}

	if memories == nil {
		memories = []domain.Memory{}
	}

	// Compute age for all memories
	computeAge(memories)

	respond(w, http.StatusOK, map[string]any{
		"memories": memories,
		"total":    len(memories),
	})
}

// attachHistory fetches and attaches the predecessor chain for each memory.
func (s *Server) attachHistory(ctx context.Context, svc *resolvedServices, memories []domain.Memory) {
	for i := range memories {
		history, err := svc.memory.GetPredecessorChain(ctx, memories[i].ID, 2)
		if err == nil && len(history) > 0 {
			computeAge(history)
			memories[i].History = history
		}
	}
}
