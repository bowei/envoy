// Types mirroring internal/api and internal/graph. Keep them in sync by hand:
// the schema is small and stable enough that codegen would cost more than it
// saves.

export type Status = 'ok' | 'warning' | 'error'

export type NodeKind =
  | 'listener'
  | 'filterChain'
  | 'networkFilter'
  | 'httpFilter'
  | 'routeConfig'
  | 'virtualHost'
  | 'route'
  | 'cluster'
  | 'endpoints'
  | 'secret'
  | 'unresolved'

export type ResourceKind =
  | 'listener'
  | 'cluster'
  | 'route'
  | 'endpoint'
  | 'secret'

export interface Detail {
  label: string
  value: string
}

export interface ResourceRef {
  kind: ResourceKind
  name: string
}

export interface GraphNode {
  id: string
  kind: NodeKind
  label: string
  sublabel?: string
  status: Status
  details?: Detail[]
  notes?: string[]
  resource?: ResourceRef
  collapsed?: boolean
}

export interface GraphEdge {
  id: string
  from: string
  to: string
  kind: 'chain' | 'ref'
  label?: string
  status: Status
}

export interface Problem {
  nodeId: string
  message: string
  status: Status
}

export interface Graph {
  nodes: GraphNode[]
  edges: GraphEdge[]
  roots: string[]
  problems: Problem[]
}

export interface ListenerSummary {
  name: string
  address: string
  state: string
  status: Status
}

export interface ServerInfo {
  version: string
  state: string
  node: { id: string; cluster: string }
  uptime_current_epoch: string
}

export interface Meta {
  id: string
  source: string
  createdAt: string
  sizeBytes: number
  counts: Record<string, number>
  warnings: string[]
  listeners: ListenerSummary[]
  serverInfo?: ServerInfo
}

export interface Config {
  envoyAdmin: string
  canFetch: boolean
  includeEds: boolean
}

export interface IndexEntry {
  kind: ResourceKind
  name: string
  state: string
}

export interface ResourceDetail {
  kind: ResourceKind
  name: string
  state: string
  versionInfo?: string
  lastUpdated?: string
  typeUrl?: string
  decodeError?: string
  errorState?: {
    details: string
    failed_version_info: string
    last_update_attempt: string
  }
  raw: unknown
}

/** ApiError carries the server's explanation, which is written for a human. */
export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message)
  }

  /** An expired or deleted snapshot, which the UI recovers from by re-fetching. */
  get isMissingSnapshot() {
    return this.status === 404
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  let res: Response
  try {
    res = await fetch(path, init)
  } catch (err) {
    // A network-level failure usually means the Go server went away, which is
    // worth saying plainly rather than showing "Failed to fetch".
    throw new ApiError(0, `cannot reach envoy-view: ${(err as Error).message}`)
  }

  if (!res.ok) {
    let message = `${res.status} ${res.statusText}`
    try {
      const body = await res.json()
      if (body?.error) message = body.error
    } catch {
      // Non-JSON error body; the status line is all we have.
    }
    throw new ApiError(res.status, message)
  }
  if (res.status === 204) return undefined as T
  return (await res.json()) as T
}

export const api = {
  config: () => request<Config>('/api/config'),

  listSnapshots: () =>
    request<{ snapshots: Meta[] }>('/api/snapshots').then((r) => r.snapshots),

  /** Fetches a fresh dump from Envoy. */
  fetchSnapshot: () => request<Meta>('/api/snapshots', { method: 'POST' }),

  /** Parses a dump the user supplied, with no Envoy involved. */
  uploadSnapshot: (dump: string) =>
    request<Meta>('/api/snapshots', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: dump,
    }),

  snapshot: (id: string) => request<Meta>(`/api/snapshots/${id}`),

  graph: (id: string, root?: string) => {
    const q = new URLSearchParams()
    if (root) q.set('root', root)
    const qs = q.toString()
    return request<Graph>(`/api/snapshots/${id}/graph${qs ? `?${qs}` : ''}`)
  },

  index: (id: string) =>
    request<{ resources: IndexEntry[] }>(`/api/snapshots/${id}/index`).then(
      (r) => r.resources,
    ),

  resource: (id: string, kind: string, name: string) => {
    const q = new URLSearchParams({ kind, name })
    return request<ResourceDetail>(`/api/snapshots/${id}/resource?${q}`)
  },
}
