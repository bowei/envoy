import { useEffect, useMemo, useRef, useState } from 'react'
import { Panel } from '@xyflow/react'

import type { GraphNode } from '../api'
import { rank } from '../fuzzy'
import { kindStyle } from '../kinds'
import { useFocusNode } from '../useFocusNode'

interface Props {
  nodes: GraphNode[]
  onPick: (node: GraphNode) => void
}

export default function SearchPanel({ nodes, onPick }: Props) {
  const [query, setQuery] = useState('')
  const [cursor, setCursor] = useState(0)
  const input = useRef<HTMLInputElement>(null)
  const focus = useFocusNode()

  const results = useMemo(
    () =>
      query.trim() === ''
        ? []
        : rank(query.trim(), nodes, (n) => `${n.label} ${n.sublabel ?? ''}`),
    [query, nodes],
  )

  // Keep the highlighted row in range as the result list shrinks.
  useEffect(() => setCursor(0), [query])

  // "/" is the conventional jump-to-search key, and the one thing a reader
  // reaches for in a graph too big to scan.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const typing =
        e.target instanceof HTMLElement &&
        (e.target.tagName === 'INPUT' || e.target.tagName === 'SELECT')
      if (!typing && (e.key === '/' || (e.key === 'k' && (e.metaKey || e.ctrlKey)))) {
        e.preventDefault()
        input.current?.focus()
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [])

  const pick = (node: GraphNode) => {
    onPick(node)
    focus(node.id)
  }

  return (
    <Panel position="top-right" className="search">
      <input
        ref={input}
        className="search-input"
        value={query}
        placeholder="Search nodes  ( / )"
        spellCheck={false}
        onChange={(e) => setQuery(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Escape') {
            setQuery('')
            input.current?.blur()
          } else if (e.key === 'ArrowDown') {
            e.preventDefault()
            setCursor((c) => Math.min(c + 1, results.length - 1))
          } else if (e.key === 'ArrowUp') {
            e.preventDefault()
            setCursor((c) => Math.max(c - 1, 0))
          } else if (e.key === 'Enter' && results[cursor]) {
            pick(results[cursor].item)
          }
        }}
      />

      {query.trim() !== '' && (
        <div className="search-results">
          {results.length === 0 && <div className="search-empty">No match</div>}
          {results.map(({ item }, i) => {
            const style = kindStyle(item.kind)
            return (
              <button
                key={item.id}
                className={`search-row${i === cursor ? ' search-row-active' : ''}`}
                onMouseEnter={() => setCursor(i)}
                onClick={() => pick(item)}
              >
                <span className="search-badge" style={{ color: style.color }}>
                  {style.badge}
                </span>
                <span className="search-label">{item.label}</span>
                {item.status !== 'ok' && (
                  <span className={`search-dot search-dot-${item.status}`} />
                )}
              </button>
            )
          })}
        </div>
      )}
    </Panel>
  )
}
