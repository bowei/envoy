import { useState } from 'react'
import { Panel } from '@xyflow/react'

import type { Graph, GraphNode } from '../api'
import { useFocusNode } from '../useFocusNode'

interface Props {
  graph: Graph
  onPick: (node: GraphNode) => void
}

/**
 * Everything the builder flagged, in one list. This is the reason the tool
 * exists: a dangling cluster reference or a rejected listener update is
 * invisible in a raw config dump and easy to miss on a large canvas, so it
 * gets a permanent, countable home rather than only a coloured border.
 */
export default function ProblemsPanel({ graph, onPick }: Props) {
  const [open, setOpen] = useState(false)
  const focus = useFocusNode()

  if (graph.problems.length === 0) return null

  const errors = graph.problems.filter((p) => p.status === 'error').length
  const byID = new Map(graph.nodes.map((n) => [n.id, n]))

  return (
    // bottom-left is the zoom controls and bottom-right the minimap.
    <Panel position="bottom-center" className="problems">
      {open && (
        <div className="problems-body">
          {graph.problems.map((p, i) => {
            const node = byID.get(p.nodeId)
            return (
              <button
                key={`${p.nodeId}:${i}`}
                className={`problems-row problems-row-${p.status}`}
                disabled={!node}
                onClick={() => {
                  if (!node) return
                  onPick(node)
                  focus(node.id)
                }}
              >
                <span className="problems-mark">{p.status === 'error' ? '×' : '!'}</span>
                <span>
                  {node && <strong>{node.label}</strong>} {p.message}
                </span>
              </button>
            )
          })}
        </div>
      )}

      <button
        className={`problems-toggle${errors > 0 ? ' problems-toggle-error' : ''}`}
        onClick={() => setOpen(!open)}
      >
        {graph.problems.length} problem{graph.problems.length === 1 ? '' : 's'}
        {open ? ' ▾' : ' ▴'}
      </button>
    </Panel>
  )
}
