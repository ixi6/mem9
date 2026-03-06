import type {
  Memory,
  SearchResult,
  CreateMemoryInput,
  UpdateMemoryInput,
  SearchInput,
  IngestInput,
  IngestResult,
} from "./types.js";

/**
 * MemoryBackend — the abstraction that both direct and server mode implement.
 * All tools call through this interface, making them mode-agnostic.
 */
export interface MemoryBackend {
  store(input: CreateMemoryInput): Promise<Memory>;
  search(input: SearchInput): Promise<SearchResult>;
  get(id: string): Promise<Memory | null>;
  update(id: string, input: UpdateMemoryInput): Promise<Memory | null>;
  remove(id: string): Promise<boolean>;

  /**
   * Ingest messages into the smart memory pipeline.
   * Server mode: POST /api/memories/ingest → LLM extraction + reconciliation.
   * Direct mode: Falls back to store() with raw content (no LLM).
   */
  ingest(input: IngestInput): Promise<IngestResult>;
}
