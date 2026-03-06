import { connect } from "@tidbcloud/serverless";
import type { Connection } from "@tidbcloud/serverless";
import type { MemoryBackend } from "./backend.js";
import type { Embedder } from "./embedder.js";
import { vecToString } from "./embedder.js";
import { initSchema } from "./schema.js";
import type {
  Memory,
  SearchResult,
  CreateMemoryInput,
  UpdateMemoryInput,
  SearchInput,
  IngestInput,
  IngestResult,
} from "./types.js";

const SPACE_ID = "default";
const MAX_CONTENT_LENGTH = 50_000;
const MAX_TAGS = 20;
const RRF_K = 60;

interface MemoryRow {
  id: string;
  space_id: string;
  content: string;
  key_name: string | null;
  source: string | null;
  tags: string[] | string | null;
  metadata: Record<string, unknown> | string | null;
  embedding: string | null;
  version: number;
  updated_by: string | null;
  created_at: string;
  updated_at: string;
  distance?: string;
  fts_score?: string;
}

function generateId(): string {
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  bytes[6] = (bytes[6] & 0x0f) | 0x40;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  const hex = Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join(
    ""
  );
  return [
    hex.slice(0, 8),
    hex.slice(8, 12),
    hex.slice(12, 16),
    hex.slice(16, 20),
    hex.slice(20, 32),
  ].join("-");
}

function formatMemory(row: MemoryRow, score?: number): Memory {
  return {
    id: row.id,
    content: row.content,
    key: row.key_name,
    source: row.source,
    tags: typeof row.tags === "string" ? JSON.parse(row.tags) : row.tags,
    metadata:
      typeof row.metadata === "string"
        ? JSON.parse(row.metadata)
        : row.metadata,
    version: row.version,
    updated_by: row.updated_by,
    created_at: row.created_at,
    updated_at: row.updated_at,
    ...(score !== undefined ? { score } : {}),
  };
}

function clamp(value: number, min: number, max: number): number {
  return Math.max(min, Math.min(max, value));
}

function rrfMerge(
  ftsRows: MemoryRow[],
  vecRows: MemoryRow[]
): { scores: Map<string, number>; rows: Map<string, MemoryRow> } {
  const scores = new Map<string, number>();
  const rows = new Map<string, MemoryRow>();

  for (let rank = 0; rank < ftsRows.length; rank++) {
    const r = ftsRows[rank];
    scores.set(r.id, (scores.get(r.id) ?? 0) + 1 / (RRF_K + rank + 1));
    rows.set(r.id, r);
  }
  for (let rank = 0; rank < vecRows.length; rank++) {
    const r = vecRows[rank];
    scores.set(r.id, (scores.get(r.id) ?? 0) + 1 / (RRF_K + rank + 1));
    if (!rows.has(r.id)) rows.set(r.id, r);
  }
  return { scores, rows };
}

export class DirectBackend implements MemoryBackend {
  private conn: Connection;
  private embedder: Embedder | null;
  private autoEmbedModel: string | null;
  private ftsAvailable = false;
  private vectorLegEnabled = true;
  private initialized: Promise<void>;

  constructor(
    host: string,
    username: string,
    password: string,
    database: string,
    embedder: Embedder | null,
    autoEmbedModel?: string,
    autoEmbedDims?: number
  ) {
    this.conn = connect({ host, username, password, database });
    this.embedder = embedder;
    this.autoEmbedModel = autoEmbedModel ?? null;
    const dims = autoEmbedModel
      ? (autoEmbedDims ?? 1024)
      : (embedder?.dims ?? 1536);
    this.initialized = initSchema(this.conn, dims, autoEmbedModel)
      .then((result) => {
        this.ftsAvailable = result.ftsAvailable;
        this.vectorLegEnabled = result.vectorLegEnabled;
      })
      .catch(() => {
        // Schema init failed — table may already exist. Continue.
      });
  }

  async store(input: CreateMemoryInput): Promise<Memory> {
    await this.initialized;
    this.validateContent(input.content);

    const id = generateId();

    if (this.autoEmbedModel) {
      await this.conn.execute(
        `INSERT INTO memories (id, space_id, content, key_name, source, tags, metadata, version, updated_by)
         VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?)`,
        [
          id,
          SPACE_ID,
          input.content,
          input.key ?? null,
          input.source ?? null,
          input.tags ? JSON.stringify(input.tags) : null,
          input.metadata ? JSON.stringify(input.metadata) : null,
          input.source ?? null,
        ]
      );
    } else {
      const embedding = this.embedder
        ? await this.embedder.embed(input.content)
        : null;

      await this.conn.execute(
        `INSERT INTO memories (id, space_id, content, key_name, source, tags, metadata, embedding, version, updated_by)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?)`,
        [
          id,
          SPACE_ID,
          input.content,
          input.key ?? null,
          input.source ?? null,
          input.tags ? JSON.stringify(input.tags) : null,
          input.metadata ? JSON.stringify(input.metadata) : null,
          embedding ? vecToString(embedding) : null,
          input.source ?? null,
        ]
      );
    }

    const rows = (await this.conn.execute(
      "SELECT * FROM memories WHERE id = ?",
      [id]
    )) as unknown as MemoryRow[];
    return formatMemory(rows[0]);
  }

  async search(input: SearchInput): Promise<SearchResult> {
    await this.initialized;
    const limit = clamp(input.limit ?? 20, 1, 200);
    const offset = Math.max(input.offset ?? 0, 0);

    if (input.q && (this.autoEmbedModel || this.embedder)) {
      return this.hybridSearch(input.q, input, limit, offset);
    }
    return this.keywordSearch(input, limit, offset);
  }

  async get(id: string): Promise<Memory | null> {
    await this.initialized;
    const rows = (await this.conn.execute(
      "SELECT * FROM memories WHERE id = ? AND space_id = ?",
      [id, SPACE_ID]
    )) as unknown as MemoryRow[];
    return rows.length > 0 ? formatMemory(rows[0]) : null;
  }

  async update(id: string, input: UpdateMemoryInput): Promise<Memory | null> {
    await this.initialized;
    const existing = (await this.conn.execute(
      "SELECT id FROM memories WHERE id = ? AND space_id = ?",
      [id, SPACE_ID]
    )) as unknown as MemoryRow[];
    if (existing.length === 0) return null;

    const sets: string[] = [];
    const values: unknown[] = [];

    if (input.content !== undefined) {
      this.validateContent(input.content);
      sets.push("content = ?");
      values.push(input.content);

      if (!this.autoEmbedModel && this.embedder) {
        const embedding = await this.embedder.embed(input.content);
        sets.push("embedding = ?");
        values.push(vecToString(embedding));
      }
    }
    if (input.key !== undefined) {
      sets.push("key_name = ?");
      values.push(input.key);
    }
    if (input.source !== undefined) {
      sets.push("source = ?");
      values.push(input.source);
    }
    if (input.tags !== undefined) {
      sets.push("tags = ?");
      values.push(JSON.stringify(input.tags));
    }
    if (input.metadata !== undefined) {
      sets.push("metadata = ?");
      values.push(JSON.stringify(input.metadata));
    }

    if (sets.length === 0) throw new Error("no fields to update");

    sets.push("version = version + 1");

    await this.conn.execute(
      `UPDATE memories SET ${sets.join(", ")} WHERE id = ? AND space_id = ?`,
      [...values, id, SPACE_ID]
    );

    const rows = (await this.conn.execute(
      "SELECT * FROM memories WHERE id = ?",
      [id]
    )) as unknown as MemoryRow[];
    return formatMemory(rows[0]);
  }

  async remove(id: string): Promise<boolean> {
    await this.initialized;
    const existing = (await this.conn.execute(
      "SELECT id FROM memories WHERE id = ? AND space_id = ?",
      [id, SPACE_ID]
    )) as unknown as MemoryRow[];
    if (existing.length === 0) return false;

    await this.conn.execute(
      "DELETE FROM memories WHERE id = ? AND space_id = ?",
      [id, SPACE_ID]
    );
    return true;
  }

  /**
   * Direct mode: no LLM pipeline available. Store raw conversation as a single digest memory.
   */
  async ingest(input: IngestInput): Promise<IngestResult> {
    await this.initialized;

    const content = input.messages
      .map((m) => `${m.role}: ${m.content}`)
      .join("\n\n")
      .trim();

    if (!content) {
      return {
        ingest_id: input.ingest_id ?? `ing_direct_${Date.now()}`,
        status: "complete",
        digest_stored: false,
        insights_added: 0,
      };
    }

    const mem = await this.store({
      content: `[session-digest] ${content.slice(0, 5000)}`,
      source: input.agent_id,
      tags: ["auto-capture", "session-digest"],
    });

    return {
      ingest_id: input.ingest_id ?? `ing_direct_${Date.now()}`,
      status: "complete",
      digest_stored: true,
      digest_id: mem.id,
      insights_added: 0,
    };
  }

  private buildFilterConditions(input: SearchInput): {
    conditions: string[];
    values: unknown[];
  } {
    const conditions: string[] = ["space_id = ?"];
    const values: unknown[] = [SPACE_ID];

    if (input.source) {
      conditions.push("source = ?");
      values.push(input.source);
    }
    if (input.key) {
      conditions.push("key_name = ?");
      values.push(input.key);
    }
    if (input.tags) {
      for (const tag of input.tags
        .split(",")
        .map((t) => t.trim())
        .filter(Boolean)) {
        conditions.push("JSON_CONTAINS(tags, ?)");
        values.push(JSON.stringify(tag));
      }
    }
    return { conditions, values };
  }

  private async keywordSearch(
    input: SearchInput,
    limit: number,
    offset: number
  ): Promise<SearchResult> {
    const { conditions, values } = this.buildFilterConditions(input);

    if (input.q && this.ftsAvailable) {
      return this.ftsOnlySearch(input.q, conditions, values, limit, offset);
    }

    if (input.q) {
      conditions.push("content LIKE CONCAT('%', ?, '%')");
      values.push(input.q);
    }

    const where = conditions.join(" AND ");

    const countRows = (await this.conn.execute(
      `SELECT COUNT(*) as cnt FROM memories WHERE ${where}`,
      values
    )) as unknown as { cnt: number }[];
    const total = Number(countRows[0]?.cnt ?? 0);

    const rows = (await this.conn.execute(
      `SELECT * FROM memories WHERE ${where} ORDER BY updated_at DESC LIMIT ? OFFSET ?`,
      [...values, limit, offset]
    )) as unknown as MemoryRow[];

    return {
      data: rows.map((r) => formatMemory(r)),
      total,
      limit,
      offset,
    };
  }

  private async ftsOnlySearch(
    q: string,
    filterConditions: string[],
    filterValues: unknown[],
    limit: number,
    offset: number
  ): Promise<SearchResult> {
    try {
      const fetchLimit = limit * 3;
      const ftsRows = await this.runFTSQuery(q, filterConditions, filterValues, fetchLimit);
      const page = ftsRows.slice(offset, offset + limit);
      return {
        data: page.map((r) => formatMemory(r, parseFloat(r.fts_score ?? "0"))),
        total: ftsRows.length,
        limit,
        offset,
      };
    } catch {
      console.warn("[mnemo] keyword leg skipped (FTS error); using LIKE fallback");
      const conds = [...filterConditions, "content LIKE CONCAT('%', ?, '%')"];
      const vals = [...filterValues, q];
      const where = conds.join(" AND ");
      const rows = (await this.conn.execute(
        `SELECT * FROM memories WHERE ${where} ORDER BY updated_at DESC LIMIT ? OFFSET ?`,
        [...vals, limit, offset]
      )) as unknown as MemoryRow[];
      return {
        data: rows.map((r) => formatMemory(r)),
        total: rows.length,
        limit,
        offset,
      };
    }
  }

  private async runFTSQuery(
    q: string,
    filterConditions: string[],
    filterValues: unknown[],
    fetchLimit: number
  ): Promise<MemoryRow[]> {
    const where = filterConditions.join(" AND ");
    return (await this.conn.execute(
      `SELECT *, fts_match_word(?, content) AS fts_score
       FROM memories
       WHERE ${where} AND fts_match_word(?, content)
       ORDER BY fts_match_word(?, content) DESC
       LIMIT ?`,
      [q, ...filterValues, q, q, fetchLimit]
    )) as unknown as MemoryRow[];
  }

  private async hybridSearch(
    q: string,
    input: SearchInput,
    limit: number,
    offset: number
  ): Promise<SearchResult> {
    const { conditions: filterConditions, values: filterValues } =
      this.buildFilterConditions(input);
    const fetchLimit = limit * 3;

    let vecRows: MemoryRow[] = [];
    let vecFailed = false;
    if (this.vectorLegEnabled) {
      try {
        if (this.autoEmbedModel) {
          vecRows = (await this.conn.execute(
            `SELECT *, VEC_EMBED_COSINE_DISTANCE(embedding, ?) AS distance
             FROM memories
             WHERE ${filterConditions.join(" AND ")} AND embedding IS NOT NULL
             ORDER BY VEC_EMBED_COSINE_DISTANCE(embedding, ?)
             LIMIT ?`,
            [q, ...filterValues, q, fetchLimit]
          )) as unknown as MemoryRow[];
        } else {
          const queryVec = await this.embedder!.embed(q);
          const vecStr = vecToString(queryVec);
          vecRows = (await this.conn.execute(
            `SELECT *, VEC_COSINE_DISTANCE(embedding, ?) AS distance
             FROM memories
             WHERE ${filterConditions.join(" AND ")} AND embedding IS NOT NULL
             ORDER BY VEC_COSINE_DISTANCE(embedding, ?)
             LIMIT ?`,
            [vecStr, ...filterValues, vecStr, fetchLimit]
          )) as unknown as MemoryRow[];
        }
      } catch {
        console.warn("[mnemo] vector leg skipped");
        vecFailed = true;
      }
    }

    let ftsRows: MemoryRow[] = [];
    let kwFailed = false;
    if (this.ftsAvailable) {
      try {
        ftsRows = await this.runFTSQuery(q, filterConditions, filterValues, fetchLimit);
      } catch {
        console.warn("[mnemo] keyword leg skipped (FTS error)");
        kwFailed = true;
      }
    } else {
      try {
        ftsRows = (await this.conn.execute(
          `SELECT * FROM memories WHERE ${filterConditions.join(" AND ")} AND content LIKE CONCAT('%', ?, '%') ORDER BY updated_at DESC LIMIT ?`,
          [...filterValues, q, fetchLimit]
        )) as unknown as MemoryRow[];
      } catch {
        console.warn("[mnemo] keyword leg skipped (LIKE error)");
        kwFailed = true;
      }
    }

    if (vecFailed && kwFailed) {
      console.error("[mnemo] both search legs failed");
      return { data: [], total: 0, limit, offset };
    }

    const { scores, rows } = rrfMerge(ftsRows, vecRows);
    const sorted = Array.from(scores.entries())
      .sort((a, b) => b[1] - a[1]);
    const total = sorted.length;
    const page = sorted.slice(offset, offset + limit);

    return {
      data: page.map(([id, score]) => formatMemory(rows.get(id)!, score)),
      total,
      limit,
      offset,
    };
  }

  private validateContent(content: string): void {
    if (!content || typeof content !== "string" || !content.trim()) {
      throw new Error("content is required and must be a non-empty string");
    }
    if (content.length > MAX_CONTENT_LENGTH) {
      throw new Error(`content must be <= ${MAX_CONTENT_LENGTH} characters`);
    }
  }
}
