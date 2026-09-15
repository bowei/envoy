import { Handle, Position, type NodeProps } from '@xyflow/react'

import { kindStyle } from '../kinds'
import { MAX_DETAILS, MAX_NOTES, type FlowNode } from '../layout'

export default function PipelineNode({ data, selected }: NodeProps<FlowNode>) {
  const { node } = data
  const style = kindStyle(node.kind)

  // The whole node is truncated aggressively to fit a fixed box, so the title
  // carries everything: hovering is how you read a long xDS name without
  // opening the detail pane.
  const tooltip = [
    `${style.title}: ${node.label}`,
    node.sublabel,
    ...(node.details ?? []).map((d) => `${d.label}: ${d.value}`),
    ...(node.notes ?? []),
  ]
    .filter(Boolean)
    .join('\n')

  return (
    <div
      className={`pnode pnode-${node.status}${selected ? ' pnode-selected' : ''}`}
      // Status owns the left edge when something is wrong, because that is what
      // the reader needs to find; otherwise the kind colour identifies the stage.
      style={{ '--kind': style.color } as React.CSSProperties}
      title={tooltip}
    >
      <Handle type="target" position={Position.Left} />

      <div className="pnode-head">
        <span className="pnode-badge">{style.badge}</span>
        <span className="pnode-label">{node.label}</span>
      </div>

      {node.sublabel && <div className="pnode-sublabel">{node.sublabel}</div>}

      {node.details && node.details.length > 0 && (
        <dl className="pnode-details">
          {node.details.slice(0, MAX_DETAILS).map((d) => (
            <div key={d.label}>
              <dt>{d.label}</dt>
              <dd>{d.value}</dd>
            </div>
          ))}
        </dl>
      )}

      {node.notes?.slice(0, MAX_NOTES).map((note) => (
        <div key={note} className="pnode-note">
          {note}
        </div>
      ))}

      <Handle type="source" position={Position.Right} />
    </div>
  )
}
