package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/qiffang/mnemos/server/internal/domain"
	"github.com/qiffang/mnemos/server/internal/embed"
	"github.com/qiffang/mnemos/server/internal/llm"
	"github.com/qiffang/mnemos/server/internal/repository"
)

// IngestMode controls which pipeline stages run.
type IngestMode string

const (
	ModeSmart   IngestMode = "smart"   // Extract + Digest + Reconcile
	ModeExtract IngestMode = "extract" // Extract + Reconcile (no digest)
	ModeDigest  IngestMode = "digest"  // Digest only (no extract/reconcile)
	ModeRaw     IngestMode = "raw"     // Store as-is (no LLM)
)

// IngestRequest is the input for the ingest pipeline.
type IngestRequest struct {
	Messages  []IngestMessage `json:"messages"`
	SessionID string          `json:"session_id"`
	AgentID   string          `json:"agent_id"`
	Mode      IngestMode      `json:"mode"`
}

// IngestMessage represents a single conversation message.
type IngestMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// IngestResult is the output of the ingest pipeline.
type IngestResult struct {
	Status        string   `json:"status"` // complete | partial | failed
	DigestStored  bool     `json:"digest_stored"`
	DigestID      string   `json:"digest_id,omitempty"`
	InsightsAdded int      `json:"insights_added"`
	InsightIDs    []string `json:"insight_ids,omitempty"`
	Warnings      int      `json:"warnings,omitempty"`
	Error         string   `json:"error,omitempty"`
}

// IngestService orchestrates the two-phase smart memory pipeline.
type IngestService struct {
	memories  repository.MemoryRepo
	llm       *llm.Client
	embedder  *embed.Embedder
	autoModel string
	mode      IngestMode
}

// NewIngestService creates a new IngestService.
func NewIngestService(
	memories repository.MemoryRepo,
	llmClient *llm.Client,
	embedder *embed.Embedder,
	autoModel string,
	defaultMode IngestMode,
) *IngestService {
	if defaultMode == "" {
		defaultMode = ModeSmart
	}
	return &IngestService{
		memories:  memories,
		llm:       llmClient,
		embedder:  embedder,
		autoModel: autoModel,
		mode:      defaultMode,
	}
}

// Ingest runs the pipeline: extract insights, generate digest, reconcile with existing memories.
func (s *IngestService) Ingest(ctx context.Context, agentName string, req IngestRequest) (*IngestResult, error) {
	if len(req.Messages) == 0 {
		return nil, &domain.ValidationError{Field: "messages", Message: "required"}
	}

	mode := req.Mode
	if mode == "" {
		mode = s.mode
	}

	// For raw mode or no LLM, skip pipeline.
	if mode == ModeRaw || s.llm == nil {
		return s.ingestRaw(ctx, agentName, req)
	}

	// Strip previously injected memory context from messages.
	cleaned := stripInjectedContext(req.Messages)

	// Format conversation for LLM.
	formatted := formatConversation(cleaned)
	if formatted == "" {
		return &IngestResult{Status: "complete"}, nil
	}

	result := &IngestResult{Status: "complete"}

	// Phase 1b: Generate session digest.
	if mode == ModeSmart || mode == ModeDigest {
		digestID, digestErr := s.generateDigest(ctx, agentName, req.AgentID, req.SessionID, formatted)
		if digestErr != nil {
			slog.Error("digest generation failed", "err", digestErr)
			if mode == ModeDigest {
				result.Status = "failed"
			} else {
				result.Status = "partial"
			}
		} else if digestID != "" {
			result.DigestStored = true
			result.DigestID = digestID
		}
	}

	// Phase 1a + Phase 2: Extract insights and reconcile.
	if mode == ModeSmart || mode == ModeExtract {
		insightIDs, warnings, extractErr := s.extractAndReconcile(ctx, agentName, req.AgentID, req.SessionID, formatted)
		if extractErr != nil {
			slog.Error("insight extraction failed", "err", extractErr)
			if result.DigestStored {
				result.Status = "partial"
			} else {
				result.Status = "failed"
			}
		} else {
			result.InsightsAdded = len(insightIDs)
			result.InsightIDs = insightIDs
		}
		result.Warnings = warnings
	}

	return result, nil
}

// ingestRaw stores messages as a single raw memory (legacy behavior).
func (s *IngestService) ingestRaw(ctx context.Context, agentName string, req IngestRequest) (*IngestResult, error) {
	content := strings.TrimSpace(formatConversation(req.Messages))
	if content == "" {
		return &IngestResult{Status: "complete"}, nil
	}

	var embedding []float32
	if s.autoModel == "" && s.embedder != nil {
		var err error
		embedding, err = s.embedder.Embed(ctx, content)
		if err != nil {
			slog.Warn("embedding failed for raw ingest", "err", err)
		}
	}

	now := time.Now()
	m := &domain.Memory{
		ID:         uuid.New().String(),
		Content:    content,
		MemoryType: domain.TypeDigest,
		Source:     agentName,
		AgentID:    req.AgentID,
		SessionID:  req.SessionID,
		Embedding:  embedding,
		State:      domain.StateActive,
		Version:    1,
		UpdatedBy:  agentName,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	if err := s.memories.Create(ctx, m); err != nil {
		return nil, fmt.Errorf("create raw memory: %w", err)
	}
	return &IngestResult{
		Status:       "complete",
		DigestStored: true,
		DigestID:     m.ID,
	}, nil
}

// generateDigest calls the LLM to generate a session summary and stores it.
func (s *IngestService) generateDigest(ctx context.Context, agentName, agentID, sessionID, conversation string) (string, error) {
	currentDate := time.Now().Format("2006-01-02")

	systemPrompt := `You are a technical session summarizer. Your task is to condense a conversation 
into a single concise paragraph capturing the key activities, decisions, and outcomes.

## Rules

1. Focus on WHAT was done, WHY, and the OUTCOME.
2. Include specific technical details (file names, error messages, config values) when they have future value.
3. Keep the summary between 1-3 sentences. Be dense, not verbose.
4. Preserve the user's language. If the conversation is in Chinese, write the summary in Chinese.
5. If the conversation is trivial (greeting, small talk), return an empty string.

## Output Format

Return ONLY valid JSON. No markdown fences.

{"summary": "..."}`

	userPrompt := fmt.Sprintf("Summarize this conversation. Today's date is %s.\n\n%s", currentDate, conversation)

	raw, err := s.llm.Complete(ctx, systemPrompt, userPrompt)
	if err != nil {
		return "", fmt.Errorf("digest LLM call: %w", err)
	}

	type digestResponse struct {
		Summary string `json:"summary"`
	}
	parsed, err := llm.ParseJSON[digestResponse](raw)
	if err != nil {
		// Retry once with stricter prompt.
		raw2, retryErr := s.llm.Complete(ctx, systemPrompt,
			"Your previous response was not valid JSON. Return ONLY the JSON object.\n\n"+userPrompt)
		if retryErr != nil {
			return "", fmt.Errorf("digest retry: %w", retryErr)
		}
		parsed, err = llm.ParseJSON[digestResponse](raw2)
		if err != nil {
			return "", fmt.Errorf("digest JSON parse after retry: %w", err)
		}
	}

	if parsed.Summary == "" {
		return "", nil // Trivial conversation, no digest needed.
	}

	var embedding []float32
	if s.autoModel == "" && s.embedder != nil {
		embedding, _ = s.embedder.Embed(ctx, parsed.Summary)
	}

	now := time.Now()
	m := &domain.Memory{
		ID:         uuid.New().String(),
		Content:    parsed.Summary,
		MemoryType: domain.TypeDigest,
		Source:     agentName,
		AgentID:    agentID,
		SessionID:  sessionID,
		Embedding:  embedding,
		State:      domain.StateActive,
		Version:    1,
		UpdatedBy:  agentName,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	if err := s.memories.Create(ctx, m); err != nil {
		return "", fmt.Errorf("store digest: %w", err)
	}

	return m.ID, nil
}

// extractAndReconcile runs Phase 1a (extraction) + Phase 2 (reconciliation).
func (s *IngestService) extractAndReconcile(ctx context.Context, agentName, agentID, sessionID, conversation string) ([]string, int, error) {
	// Phase 1a: Extract facts.
	facts, err := s.extractFacts(ctx, conversation)
	if err != nil {
		return nil, 0, fmt.Errorf("extract facts: %w", err)
	}
	if len(facts) == 0 {
		return nil, 0, nil
	}

	// Phase 2: Reconcile each fact against existing memories.
	return s.reconcile(ctx, agentName, agentID, sessionID, facts)
}

// extractFacts calls the LLM to extract atomic facts from the conversation.
func (s *IngestService) extractFacts(ctx context.Context, conversation string) ([]string, error) {
	currentDate := time.Now().Format("2006-01-02")

	systemPrompt := `You are an information extraction engine. Your task is to identify distinct, 
atomic facts from a conversation and return them as a structured JSON array.

## Rules

1. Extract facts ONLY from the user's messages. Ignore assistant and system messages entirely.
2. Each fact must be a single, self-contained statement (one idea per fact).
3. Prefer specific details over vague summaries.
   - Good: "Uses Go 1.22 for backend services"
   - Bad: "Knows some programming languages"
4. Preserve the user's original language. If the user writes in Chinese, extract facts in Chinese.
5. Omit ephemeral information (greetings, filler, debugging chatter with no lasting value).
6. Omit information that is only relevant to the current task and has no future reuse value.
7. If no meaningful facts exist in the conversation, return an empty array.

## Output Format

Return ONLY valid JSON. No markdown fences, no explanation.

{"facts": ["fact one", "fact two", ...]}`

	userPrompt := fmt.Sprintf("Extract facts from this conversation. Today's date is %s.\n\n%s", currentDate, conversation)

	raw, err := s.llm.Complete(ctx, systemPrompt, userPrompt)
	if err != nil {
		return nil, fmt.Errorf("extraction LLM call: %w", err)
	}

	type extractResponse struct {
		Facts []string `json:"facts"`
	}
	parsed, err := llm.ParseJSON[extractResponse](raw)
	if err != nil {
		// Retry once.
		raw2, retryErr := s.llm.Complete(ctx, systemPrompt,
			"Your previous response was not valid JSON. Return ONLY the JSON object.\n\n"+userPrompt)
		if retryErr != nil {
			return nil, fmt.Errorf("extraction retry: %w", retryErr)
		}
		parsed, err = llm.ParseJSON[extractResponse](raw2)
		if err != nil {
			return nil, nil // Give up, treat as no facts.
		}
	}

	// Filter out empty strings.
	var facts []string
	for _, f := range parsed.Facts {
		f = strings.TrimSpace(f)
		if f != "" {
			facts = append(facts, f)
		}
	}
	return facts, nil
}

// reconcile takes extracted facts and reconciles them against existing memories.
func (s *IngestService) reconcile(ctx context.Context, agentName, agentID, sessionID string, facts []string) ([]string, int, error) {
	// Fetch existing active insights + pinned memories for context.
	existingMemories, err := s.fetchExistingForReconcile(ctx, facts)
	if err != nil {
		slog.Warn("failed to fetch existing memories for reconciliation, proceeding with ADD-all", "err", err)
		return s.addAllFacts(ctx, agentName, agentID, sessionID, facts)
	}

	if len(existingMemories) == 0 {
		// No existing memories — add all facts directly.
		return s.addAllFacts(ctx, agentName, agentID, sessionID, facts)
	}

	// Build context for the reconciliation prompt using integer IDs.
	type memoryRef struct {
		IntID int    `json:"id"`
		Text  string `json:"text"`
	}
	refs := make([]memoryRef, len(existingMemories))
	idMap := make(map[int]string, len(existingMemories)) // intID -> real UUID
	for i, m := range existingMemories {
		refs[i] = memoryRef{IntID: i, Text: m.Content}
		idMap[i] = m.ID
	}

	refsJSON, _ := json.Marshal(refs)
	factsJSON, _ := json.Marshal(facts)

	systemPrompt := `You are a memory reconciliation engine. You manage a knowledge base by comparing 
newly extracted facts against existing memories and deciding the correct action for each.

## Actions

- **ADD**: The fact is genuinely new information not covered by any existing memory.
- **UPDATE**: The fact refines, corrects, or supersedes an existing memory. Keep the same ID.
  - Update when: new info is more specific, more recent, or contradicts the old memory.
  - Do NOT update when: old and new convey the same meaning (even if worded differently).
- **DELETE**: The fact directly contradicts an existing memory, making it obsolete.
- **NOOP**: The fact is already captured by an existing memory. No action needed.

	## Rules

1. Reference existing memories by their integer ID ONLY (0, 1, 2...). Never invent IDs.
2. For UPDATE, always include the original text in "old_memory" for audit.
3. For ADD, generate the next sequential integer ID.
4. When in doubt between UPDATE and NOOP, prefer ADD (never lose information — slight duplication is better than missing a new detail).
5. When in doubt between ADD and UPDATE, prefer UPDATE if an existing memory covers a related topic.
6. Preserve the language of the original facts. Do not translate.

## Example

Input memories:
[{"id": 0, "text": "Uses PostgreSQL for the main database"}]

New facts:
["Recently migrated to TiDB from PostgreSQL", "Prefers dark mode in IDE"]

Expected output:
{"memory": [
  {"id": "0", "text": "Uses TiDB for the main database (migrated from PostgreSQL)", "event": "UPDATE", "old_memory": "Uses PostgreSQL for the main database"},
  {"id": "1", "text": "Prefers dark mode in IDE", "event": "ADD"}
]}

## Output Format

Return ONLY valid JSON. No markdown fences.

{
  "memory": [
    {"id": "0", "text": "...", "event": "NOOP"},
    {"id": "1", "text": "updated text", "event": "UPDATE", "old_memory": "original text"},
    {"id": "2", "text": "...", "event": "DELETE"},
    {"id": "3", "text": "brand new fact", "event": "ADD"}
  ]
}`

	userPrompt := fmt.Sprintf(`Current memory contents:

%s

New facts extracted from recent conversation:

%s

Reconcile the new facts with current memory. Return the full memory state after reconciliation.`, string(refsJSON), string(factsJSON))

	raw, err := s.llm.Complete(ctx, systemPrompt, userPrompt)
	if err != nil {
		slog.Warn("reconciliation LLM call failed, falling back to ADD-all", "err", err)
		return s.addAllFacts(ctx, agentName, agentID, sessionID, facts)
	}

	type reconcileEvent struct {
		ID        string `json:"id"`
		Text      string `json:"text"`
		Event     string `json:"event"`
		OldMemory string `json:"old_memory,omitempty"`
	}
	type reconcileResponse struct {
		Memory []reconcileEvent `json:"memory"`
	}

	parsed, err := llm.ParseJSON[reconcileResponse](raw)
	if err != nil {
		// Retry once.
		raw2, retryErr := s.llm.Complete(ctx, systemPrompt,
			"Your previous response was not valid JSON. Return ONLY the JSON object.\n\n"+userPrompt)
		if retryErr != nil {
			slog.Warn("reconciliation retry failed, falling back to ADD-all", "err", retryErr)
			return s.addAllFacts(ctx, agentName, agentID, sessionID, facts)
		}
		parsed, err = llm.ParseJSON[reconcileResponse](raw2)
		if err != nil {
			slog.Warn("reconciliation JSON parse failed after retry, falling back to ADD-all", "err", err)
			return s.addAllFacts(ctx, agentName, agentID, sessionID, facts)
		}
	}

	// Execute reconciliation decisions.
	var resultIDs []string
	var warnings int

	for _, event := range parsed.Memory {
		switch strings.ToUpper(event.Event) {
		case "ADD":
			if event.Text == "" {
				continue
			}
			newID, addErr := s.addInsight(ctx, agentName, agentID, sessionID, event.Text)
			if addErr != nil {
				slog.Warn("failed to add insight", "err", addErr, "text", event.Text)
				warnings++
				continue
			}
			resultIDs = append(resultIDs, newID)

		case "UPDATE":
			// Validate the ID references an existing memory.
			intID := parseIntID(event.ID)
			realID, ok := idMap[intID]
			if !ok || event.Text == "" {
				slog.Warn("skipping UPDATE with invalid ID", "id", event.ID)
				continue
			}
			// Guard: never auto-update pinned memories — treat as ADD instead.
			if intID >= 0 && intID < len(existingMemories) && existingMemories[intID].MemoryType == domain.TypePinned {
				slog.Warn("skipping UPDATE for pinned memory — treating as ADD", "id", realID)
				newID, addErr := s.addInsight(ctx, agentName, agentID, sessionID, event.Text)
				if addErr != nil {
					slog.Warn("failed to add insight (pinned fallback)", "err", addErr)
					warnings++
					continue
				}
				resultIDs = append(resultIDs, newID)
				continue
			}
			newID, updateErr := s.updateInsight(ctx, agentName, agentID, sessionID, realID, event.Text)
			if updateErr != nil {
				slog.Warn("failed to update insight", "err", updateErr, "id", event.ID)
				warnings++
				continue
			}
			resultIDs = append(resultIDs, newID)

		case "DELETE":
			intID := parseIntID(event.ID)
			realID, ok := idMap[intID]
			if !ok {
				slog.Warn("skipping DELETE with invalid ID", "id", event.ID)
				continue
			}
			// Guard: never auto-delete pinned memories.
			if intID >= 0 && intID < len(existingMemories) && existingMemories[intID].MemoryType == domain.TypePinned {
				slog.Warn("skipping DELETE for pinned memory", "id", realID)
				warnings++
				continue
			}
			if delErr := s.memories.SetState(ctx, realID, domain.StateDeleted); delErr != nil {
				if !errors.Is(delErr, domain.ErrNotFound) {
					// ErrNotFound = row already archived/moved by concurrent operation — safe to skip
					slog.Warn("failed to delete memory", "err", delErr, "id", event.ID)
					warnings++
				}
			}

		case "NOOP":
			// No action needed.

		default:
			slog.Warn("unknown reconciliation event", "event", event.Event, "id", event.ID)
		}
	}

	return resultIDs, warnings, nil
}

// fetchExistingForReconcile gets existing active insights and pinned memories for reconciliation.
func (s *IngestService) fetchExistingForReconcile(ctx context.Context, facts []string) ([]domain.Memory, error) {
	const reconcileMemoryCap = 30
	const reconcileContentMaxLen = 150

	if s.embedder == nil && s.autoModel == "" {
		filter := domain.MemoryFilter{
			State:      "active",
			MemoryType: "insight,pinned",
			Limit:      reconcileMemoryCap,
		}
		memories, _, err := s.memories.List(ctx, filter)
		if err != nil {
			return nil, err
		}
		for i := range memories {
			if len(memories[i].Content) > reconcileContentMaxLen {
				memories[i].Content = memories[i].Content[:reconcileContentMaxLen] + "..."
			}
		}
		return memories, nil
	}

	seen := make(map[string]struct{})
	var result []domain.Memory

	filter := domain.MemoryFilter{
		State:      "active",
		MemoryType: "insight,pinned",
	}

	for _, fact := range facts {
		var matches []domain.Memory
		var err error

		if s.autoModel != "" {
			matches, err = s.memories.AutoVectorSearch(ctx, fact, filter, 10)
		} else {
			vec, embedErr := s.embedder.Embed(ctx, fact)
			if embedErr != nil {
				slog.Warn("embedding failed for fact during reconcile", "err", embedErr)
				continue
			}
			matches, err = s.memories.VectorSearch(ctx, vec, filter, 10)
		}

		if err != nil {
			slog.Warn("vector search failed for fact during reconcile", "err", err)
			continue
		}

		for _, m := range matches {
			if _, ok := seen[m.ID]; !ok {
				seen[m.ID] = struct{}{}
				result = append(result, m)
				if len(result) >= reconcileMemoryCap {
					return result, nil
				}
			}
		}
	}

	return result, nil
}

// addAllFacts adds all facts as new insights (fallback when reconciliation fails).
func (s *IngestService) addAllFacts(ctx context.Context, agentName, agentID, sessionID string, facts []string) ([]string, int, error) {
	var ids []string
	var warnings int
	for _, fact := range facts {
		id, err := s.addInsight(ctx, agentName, agentID, sessionID, fact)
		if err != nil {
			slog.Warn("failed to add fact", "err", err, "fact", fact)
			warnings++
			continue
		}
		ids = append(ids, id)
	}
	return ids, warnings, nil
}

// addInsight creates a new insight memory.
func (s *IngestService) addInsight(ctx context.Context, agentName, agentID, sessionID, content string) (string, error) {
	var embedding []float32
	if s.autoModel == "" && s.embedder != nil {
		var err error
		embedding, err = s.embedder.Embed(ctx, content)
		if err != nil {
			slog.Warn("embedding failed for insight", "err", err)
		}
	}

	now := time.Now()
	m := &domain.Memory{
		ID:         uuid.New().String(),
		Content:    content,
		MemoryType: domain.TypeInsight,
		Source:     agentName,
		AgentID:    agentID,
		SessionID:  sessionID,
		Embedding:  embedding,
		State:      domain.StateActive,
		Version:    1,
		UpdatedBy:  agentName,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	if err := s.memories.Create(ctx, m); err != nil {
		return "", fmt.Errorf("create insight: %w", err)
	}
	return m.ID, nil
}

// updateInsight archives the old memory and creates a new one atomically (append-new + archive-old model).
func (s *IngestService) updateInsight(ctx context.Context, agentName, agentID, sessionID, oldID, newContent string) (string, error) {
	newID := uuid.New().String()

	// Create new memory object.
	var embedding []float32
	if s.autoModel == "" && s.embedder != nil {
		var err error
		embedding, err = s.embedder.Embed(ctx, newContent)
		if err != nil {
			slog.Warn("embedding failed for updated insight", "err", err)
		}
	}

	now := time.Now()
	m := &domain.Memory{
		ID:         newID,
		Content:    newContent,
		MemoryType: domain.TypeInsight,
		Source:     agentName,
		AgentID:    agentID,
		SessionID:  sessionID,
		Embedding:  embedding,
		State:      domain.StateActive,
		Version:    1,
		UpdatedBy:  agentName,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	// Archive old + create new in a single transaction.
	if err := s.memories.ArchiveAndCreate(ctx, oldID, newID, m); err != nil {
		return "", fmt.Errorf("archive and create for %s: %w", oldID, err)
	}
	return newID, nil
}

// stripInjectedContext removes <relevant-memories>...</relevant-memories> tags from messages.
func stripInjectedContext(messages []IngestMessage) []IngestMessage {
	result := make([]IngestMessage, 0, len(messages))
	for _, msg := range messages {
		cleaned := stripMemoryTags(msg.Content)
		cleaned = strings.TrimSpace(cleaned)
		if cleaned != "" {
			result = append(result, IngestMessage{Role: msg.Role, Content: cleaned})
		}
	}
	return result
}

// stripMemoryTags removes <relevant-memories>...</relevant-memories> from text.
func stripMemoryTags(s string) string {
	for {
		start := strings.Index(s, "<relevant-memories>")
		if start == -1 {
			break
		}
		end := strings.Index(s, "</relevant-memories>")
		if end == -1 {
			// Malformed tag, remove from start to end.
			s = s[:start]
			break
		}
		s = s[:start] + s[end+len("</relevant-memories>"):]
	}
	return s
}

// formatConversation formats messages into a conversation string for LLM.
func formatConversation(messages []IngestMessage) string {
	var sb strings.Builder
	for _, msg := range messages {
		role := msg.Role
		if r, _ := utf8.DecodeRuneInString(role); r != utf8.RuneError {
			role = strings.ToUpper(string(r)) + role[utf8.RuneLen(r):]
		}
		sb.WriteString(role)
		sb.WriteString(": ")
		sb.WriteString(msg.Content)
		sb.WriteString("\n\n")
	}
	return strings.TrimSpace(sb.String())
}

// parseIntID parses a string integer ID, returning -1 on failure.
func parseIntID(s string) int {
	id, err := strconv.Atoi(s)
	if err != nil {
		return -1
	}
	return id
}
