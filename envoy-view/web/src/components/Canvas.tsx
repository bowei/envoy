import { useMemo } from 'react'
import {
  Background,
  BackgroundVariant,
  Controls,
  MiniMap,
  ReactFlow,
  type NodeTypes,
} from '@xyflow/react'

import type { Graph, GraphNode } from '../api'
import { layout, type FlowNode, type PipelineNodeData } from '../layout'
import Legend from './Legend'
import PipelineNode from './PipelineNode'
import ProblemsPanel from './ProblemsPanel'
import SearchPanel from './SearchPanel'

const nodeTypes: NodeTypes = { pipeline: PipelineNode }

// Dagre has already placed everything, so the canvas is a viewer: dragging a
// node or drawing an edge would only desynchronise it from the config.
const STATIC_FLOW = {
  nodesDraggable: false,
  nodesConnectable: false,
  elementsSelectable: true,
  edgesFocusable: false,
} as const

const MINIMAP_COLORS: Record<string, string> = {
  ok: '#3f4a5a',
  warning: '#8a6d1f',
  error: '#8a2f2f',
}

interface Props {
  graph: Graph
  selectedID: string | null
  onSelect: (node: GraphNode | null) => void
}

export default function Canvas({ graph, selectedID, onSelect }: Props) {
  // Laying out is the expensive part, and it only depends on the graph, not on
  // selection or hover.
  const { nodes, edges } = useMemo(() => layout(graph), [graph])

  // Selection is owned by App so the search panel can drive it too, so it is
  // applied to the laid-out nodes rather than left to React Flow's own state.
  const selected = useMemo(
    () => nodes.map((n) => (n.selected === (n.id === selectedID) ? n : { ...n, selected: n.id === selectedID })),
    [nodes, selectedID],
  )

  return (
    <ReactFlow
      nodes={selected}
      edges={edges}
      onNodeClick={(_, n) => onSelect((n as FlowNode).data.node)}
      onPaneClick={() => onSelect(null)}
      nodeTypes={nodeTypes}
      fitView
      // React Flow's default fitView never magnifies, which leaves a short
      // pipeline as a thin unreadable band in the middle of the viewport.
      fitViewOptions={{ padding: 0.15, maxZoom: 1.6 }}
      minZoom={0.05}
      proOptions={{ hideAttribution: true }}
      {...STATIC_FLOW}
    >
      <Background variant={BackgroundVariant.Dots} gap={18} size={1} />
      {/* Panels live inside <ReactFlow> so they can pan the viewport. */}
      <Legend />
      <SearchPanel nodes={graph.nodes} onPick={onSelect} />
      <ProblemsPanel graph={graph} onPick={onSelect} />
      <Controls showInteractive={false} />
      <MiniMap
        pannable
        zoomable
        nodeColor={(n) => {
          const status = (n.data as PipelineNodeData | undefined)?.node?.status
          return MINIMAP_COLORS[status ?? 'ok'] ?? MINIMAP_COLORS.ok
        }}
      />
    </ReactFlow>
  )
}
