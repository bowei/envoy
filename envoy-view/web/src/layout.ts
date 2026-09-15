import dagre from '@dagrejs/dagre'
import type { Edge, Node } from '@xyflow/react'

import type { Graph, GraphNode, NodeKind } from './api'

/** The data React Flow carries on each node; the custom node renders it. */
export interface PipelineNodeData extends Record<string, unknown> {
  node: GraphNode
}

export type FlowNode = Node<PipelineNodeData, 'pipeline'>
export type FlowEdge = Edge

/**
 * Node sizes are declared rather than measured. Dagre needs them before React
 * has rendered anything, and a measure-then-relayout pass makes the canvas
 * visibly jump on every load. The CSS below keeps nodes inside these bounds.
 */
const DEFAULT_WIDTH = 220

/**
 * Nodes named after an xDS resource get a wider box: names like
 * "outbound|8080|v1|svc.ns.svc.cluster.local" are unreadable at 220px, while a
 * stage like "filter chain 0" never needs the room.
 */
const NODE_WIDTH: Partial<Record<NodeKind, number>> = {
  listener: 260,
  cluster: 280,
  endpoints: 280,
  routeConfig: 260,
  networkFilter: 250,
  httpFilter: 250,
  secret: 240,
  unresolved: 250,
}

/** Vertical padding plus borders, outside the stacked rows. */
const CHROME_HEIGHT = 18
/** One line of node text: 11px at line-height 1.33, plus the flex gap. */
const ROW_HEIGHT = 16

/** Caps matching the slice() in PipelineNode, so the box fits what it renders. */
export const MAX_DETAILS = 4
export const MAX_NOTES = 3

export function nodeSize(node: GraphNode): { width: number; height: number } {
  const rows =
    (node.sublabel ? 1 : 0) +
    Math.min(node.details?.length ?? 0, MAX_DETAILS) +
    Math.min(node.notes?.length ?? 0, MAX_NOTES)
  return {
    width: NODE_WIDTH[node.kind] ?? DEFAULT_WIDTH,
    height: CHROME_HEIGHT + ROW_HEIGHT + rows * ROW_HEIGHT,
  }
}

/**
 * Lays the graph out left to right: that matches how a request actually moves
 * through Envoy, from listener through filters to a cluster's endpoints.
 */
export function layout(graph: Graph): { nodes: FlowNode[]; edges: FlowEdge[] } {
  const g = new dagre.graphlib.Graph()
  g.setGraph({
    rankdir: 'LR',
    nodesep: 24,
    ranksep: 90,
    marginx: 32,
    marginy: 32,
  })
  g.setDefaultEdgeLabel(() => ({}))

  const byID = new Map<string, GraphNode>()
  for (const node of graph.nodes) {
    byID.set(node.id, node)
    g.setNode(node.id, nodeSize(node))
  }
  for (const edge of graph.edges) {
    // A dangling endpoint would make dagre invent a node with no size.
    if (byID.has(edge.from) && byID.has(edge.to)) {
      g.setEdge(edge.from, edge.to)
    }
  }

  dagre.layout(g)

  const nodes: FlowNode[] = graph.nodes.map((node) => {
    const { x, y } = g.node(node.id)
    const { width, height } = nodeSize(node)
    return {
      id: node.id,
      type: 'pipeline',
      // Dagre centres nodes; React Flow positions by top-left corner.
      position: { x: x - width / 2, y: y - height / 2 },
      data: { node },
      width,
      height,
    }
  })

  const edges: FlowEdge[] = graph.edges.map((edge) => ({
    id: edge.id,
    source: edge.from,
    target: edge.to,
    label: edge.label,
    // A "ref" edge crosses from the pipeline into a shared resource (a cluster,
    // a route config, a secret), so it is drawn dashed to distinguish it from
    // the chain of stages a request walks through.
    className: `edge-${edge.kind} edge-${edge.status}`,
    animated: false,
  }))

  return { nodes, edges }
}
