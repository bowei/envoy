import { useState } from 'react'

/** Depth to expand on first render: enough to see shape, not enough to flood. */
const AUTO_EXPAND_DEPTH = 2

/** Containers this small are expanded regardless of depth — collapsing a
 *  one-key object hides more than it saves. */
const ALWAYS_EXPAND_SIZE = 3

interface Props {
  value: unknown
  /** Key this value sits under in its parent, if any. */
  name?: string
  depth?: number
  /** Set by search: subtrees containing a match start expanded. */
  forceOpen?: boolean
}

export default function JsonTree({ value, name, depth = 0, forceOpen }: Props) {
  if (value === null || typeof value !== 'object') {
    return (
      <div className="jt-row" style={{ paddingLeft: depth * 12 }}>
        {name !== undefined && <span className="jt-key">{name}:</span>}
        <Scalar value={value} />
      </div>
    )
  }
  return <Branch value={value} name={name} depth={depth} forceOpen={forceOpen} />
}

function Branch({ value, name, depth = 0, forceOpen }: Props) {
  const isArray = Array.isArray(value)
  const entries = isArray
    ? (value as unknown[]).map((v, i) => [String(i), v] as const)
    : Object.entries(value as Record<string, unknown>)

  const [open, setOpen] = useState(
    forceOpen ?? (depth < AUTO_EXPAND_DEPTH || entries.length <= ALWAYS_EXPAND_SIZE),
  )

  const brackets = isArray ? '[]' : '{}'
  const summary = entries.length === 0 ? brackets : `${brackets[0]}…${brackets[1]}`

  return (
    <div>
      <div
        className="jt-row jt-branch"
        style={{ paddingLeft: depth * 12 }}
        onClick={() => setOpen(!open)}
      >
        <span className="jt-caret">{entries.length === 0 ? ' ' : open ? '▾' : '▸'}</span>
        {name !== undefined && <span className="jt-key">{name}:</span>}
        <span className="jt-punct">{open ? brackets[0] : summary}</span>
        {!open && entries.length > 0 && (
          <span className="jt-count">
            {entries.length} {isArray ? 'items' : 'keys'}
          </span>
        )}
      </div>

      {open &&
        entries.map(([k, v]) => (
          <JsonTree key={k} name={k} value={v} depth={depth + 1} forceOpen={forceOpen} />
        ))}

      {open && entries.length > 0 && (
        <div className="jt-row jt-punct" style={{ paddingLeft: depth * 12 }}>
          <span className="jt-caret" />
          {brackets[1]}
        </div>
      )}
    </div>
  )
}

function Scalar({ value }: { value: unknown }) {
  if (typeof value === 'string') {
    // A type URL is the single most useful string in an Envoy config, so it is
    // worth picking out from the surrounding values.
    const cls = value.startsWith('type.googleapis.com/') ? 'jt-type' : 'jt-string'
    return <span className={cls}>"{value}"</span>
  }
  if (value === null) return <span className="jt-null">null</span>
  if (typeof value === 'boolean') return <span className="jt-bool">{String(value)}</span>
  return <span className="jt-number">{String(value)}</span>
}
