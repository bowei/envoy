import { useState } from 'react'
import { Panel } from '@xyflow/react'

import { KIND_ORDER, kindStyle } from '../kinds'

/**
 * Envoy's stage names are not self-evident from a four-letter badge, so the
 * legend spells them out. It starts collapsed: it is useful once and then only
 * takes up canvas.
 */
export default function Legend() {
  const [open, setOpen] = useState(false)

  return (
    <Panel position="top-left" className="legend">
      <button className="legend-toggle" onClick={() => setOpen(!open)}>
        {open ? '× Legend' : '? Legend'}
      </button>

      {open && (
        <div className="legend-body">
          {KIND_ORDER.map((kind) => {
            const style = kindStyle(kind)
            return (
              <div key={kind} className="legend-row">
                <span className="legend-swatch" style={{ background: style.color }} />
                <span className="legend-badge">{style.badge}</span>
                <span>{style.title}</span>
              </div>
            )
          })}

          <hr />
          <div className="legend-row">
            <svg width="26" height="8">
              <line x1="0" y1="4" x2="26" y2="4" stroke="var(--border-strong)" strokeWidth="1.5" />
            </svg>
            <span>Stage a request flows through</span>
          </div>
          <div className="legend-row">
            <svg width="26" height="8">
              <line
                x1="0"
                y1="4"
                x2="26"
                y2="4"
                stroke="var(--border-strong)"
                strokeWidth="1.5"
                strokeDasharray="4 3"
              />
            </svg>
            <span>Reference to a shared resource</span>
          </div>
        </div>
      )}
    </Panel>
  )
}
