import type { NodeKind } from './api'

export interface KindStyle {
  /** Corner badge. The full kind name would dominate a 220px box. */
  badge: string
  /** Human name, used in the legend and in tooltips. */
  title: string
  /** Accent colour, applied to the badge and to the node's left edge. */
  color: string
}

/**
 * Colour groups the pipeline into the four things a reader is actually
 * separating: where traffic enters (blue), what processes it (violet), how it
 * is routed (green), and where it goes (pink). Warning amber and error red are
 * deliberately absent so status can never be confused with kind.
 */
export const KIND_STYLES: Record<NodeKind, KindStyle> = {
  listener: { badge: 'LSTN', title: 'Listener', color: '#5aa0f2' },
  filterChain: { badge: 'CHAIN', title: 'Filter chain', color: '#4bb3c4' },
  networkFilter: { badge: 'NET', title: 'Network filter', color: '#9a8ce8' },
  httpFilter: { badge: 'HTTP', title: 'HTTP filter', color: '#b7a4f0' },
  routeConfig: { badge: 'RDS', title: 'Route configuration', color: '#57b877' },
  virtualHost: { badge: 'VHOST', title: 'Virtual host', color: '#6cc78a' },
  route: { badge: 'ROUTE', title: 'Route', color: '#86d3a0' },
  cluster: { badge: 'CDS', title: 'Cluster', color: '#e07aa8' },
  endpoints: { badge: 'EDS', title: 'Endpoints', color: '#eb9ec0' },
  secret: { badge: 'SDS', title: 'Secret', color: '#8f9bb0' },
  unresolved: { badge: '??', title: 'Unresolved reference', color: '#e05a5a' },
}

/** Legend order follows the left-to-right order of a real pipeline. */
export const KIND_ORDER: NodeKind[] = [
  'listener',
  'filterChain',
  'networkFilter',
  'httpFilter',
  'routeConfig',
  'virtualHost',
  'route',
  'cluster',
  'endpoints',
  'secret',
  'unresolved',
]

const FALLBACK: KindStyle = { badge: '?', title: 'Unknown', color: '#8f9bb0' }

/** Tolerates a kind added to the Go side before the UI knows about it. */
export function kindStyle(kind: NodeKind): KindStyle {
  return KIND_STYLES[kind] ?? FALLBACK
}
