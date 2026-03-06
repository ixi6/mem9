import type { MemoryBackend } from "./backend.js";
import type {
  Memory,
  SearchResult,
  CreateMemoryInput,
  UpdateMemoryInput,
  SearchInput,
  IngestInput,
  IngestResult,
} from "./types.js";

type TenantRegisterResponse = {
  ok: boolean;
  tenant_id: string;
  token: string;
  claim_url?: string;
  status: string;
};

export class ServerBackend implements MemoryBackend {
  private baseUrl: string;
  private token: string;
  private agentName: string;

  constructor(apiUrl: string, apiToken: string, agentName: string) {
    this.baseUrl = apiUrl.replace(/\/+$/, "");
    this.token = apiToken;
    this.agentName = agentName;
  }

  async register(tenantName?: string): Promise<TenantRegisterResponse> {
    const name = tenantName ?? `${this.agentName}-tenant`;
    const resp = await fetch(this.baseUrl + "/api/tenants/register", {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
      },
      body: JSON.stringify({
        name,
        agent_name: this.agentName,
        agent_type: "openclaw",
      }),
    });

    if (!resp.ok) {
      const body = await resp.text();
      throw new Error(`tenant registration failed (${resp.status}): ${body}`);
    }

    const data = (await resp.json()) as TenantRegisterResponse;
    if (!data?.token) {
      throw new Error("tenant registration did not return a token");
    }

    this.token = data.token;
    return data;
  }

  async store(input: CreateMemoryInput): Promise<Memory> {
    return this.request("POST", "/api/memories", input);
  }

  async search(input: SearchInput): Promise<SearchResult> {
    const params = new URLSearchParams();
    if (input.q) params.set("q", input.q);
    if (input.tags) params.set("tags", input.tags);
    if (input.source) params.set("source", input.source);
    if (input.limit != null) params.set("limit", String(input.limit));
    if (input.offset != null) params.set("offset", String(input.offset));

    const qs = params.toString();
    const raw = await this.request<{
      memories: Memory[];
      total: number;
      limit: number;
      offset: number;
    }>("GET", `/api/memories${qs ? "?" + qs : ""}`);
    return {
      data: raw.memories ?? [],
      total: raw.total,
      limit: raw.limit,
      offset: raw.offset,
    };
  }

  async get(id: string): Promise<Memory | null> {
    try {
      return await this.request<Memory>("GET", `/api/memories/${id}`);
    } catch {
      return null;
    }
  }

  async update(id: string, input: UpdateMemoryInput): Promise<Memory | null> {
    try {
      return await this.request<Memory>("PUT", `/api/memories/${id}`, input);
    } catch {
      return null;
    }
  }

  async remove(id: string): Promise<boolean> {
    try {
      await this.request("DELETE", `/api/memories/${id}`);
      return true;
    } catch {
      return false;
    }
  }

  async ingest(input: IngestInput): Promise<IngestResult> {
    return this.request<IngestResult>("POST", "/api/memories/ingest", input);
  }

  private async requestRaw(
    method: string,
    path: string,
    body?: unknown
  ): Promise<Response> {
    const url = this.baseUrl + path;
    const headers: Record<string, string> = {
      Authorization: `Bearer ${this.token}`,
      "Content-Type": "application/json",
      "X-Mnemo-Agent-Id": this.agentName,
    };
    return fetch(url, {
      method,
      headers,
      body: body != null ? JSON.stringify(body) : undefined,
      signal: AbortSignal.timeout(8_000),
    });
  }

  private async request<T>(
    method: string,
    path: string,
    body?: unknown
  ): Promise<T> {
    const resp = await this.requestRaw(method, path, body);

    if (resp.status === 204) {
      return undefined as T;
    }

    const data = await resp.json();
    if (!resp.ok) {
      throw new Error((data as { error?: string }).error || `HTTP ${resp.status}`);
    }
    return data as T;
  }
}
