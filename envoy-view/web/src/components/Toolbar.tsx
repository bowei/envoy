import type { Config, Meta } from '../api'

interface Props {
  config: Config | null
  snapshot: Meta | null
  root: string
  loading: boolean
  onRootChange: (root: string) => void
  onRefresh: () => void
}

/**
 * A source is a file path, an admin address, or "upload". For a path the
 * filename is the identifying part, and CSS truncation would cut exactly that
 * off the end, so drop the directory here and keep it in the tooltip.
 */
function shortSource(source: string): string {
  if (!source.includes('/') || source.startsWith('http')) return source
  return source.slice(source.lastIndexOf('/') + 1)
}

export default function Toolbar({
  config,
  snapshot,
  root,
  loading,
  onRootChange,
  onRefresh,
}: Props) {
  const listeners = snapshot?.listeners ?? []

  return (
    <header className="toolbar">
      <div className="toolbar-brand">
        envoy<span>view</span>
      </div>

      <label className="toolbar-field">
        Listener
        <select
          value={root}
          disabled={!snapshot || listeners.length === 0}
          onChange={(e) => onRootChange(e.target.value)}
        >
          <option value="">All ({listeners.length})</option>
          {listeners.map((l) => (
            <option key={l.name} value={l.name}>
              {l.status !== 'ok' ? (l.status === 'error' ? '× ' : '! ') : ''}
              {l.name} — {l.address}
            </option>
          ))}
        </select>
      </label>

      <div className="toolbar-spacer" />

      {snapshot && (
        <div
          className="toolbar-meta"
          title={`${snapshot.source}\nsnapshot ${snapshot.id}`}
        >
          <span>{shortSource(snapshot.source)}</span>
          <span className="dim">
            {new Date(snapshot.createdAt).toLocaleTimeString()} ·{' '}
            {(snapshot.sizeBytes / 1024).toFixed(1)} KiB
          </span>
        </div>
      )}

      <button
        className="toolbar-button"
        onClick={onRefresh}
        disabled={loading || !config?.canFetch}
        title={
          config?.canFetch
            ? `Fetch a fresh config dump from ${config.envoyAdmin}`
            : 'No Envoy admin address is configured; start envoy-view without -dump-file to fetch'
        }
      >
        {loading ? 'Loading…' : 'Refresh'}
      </button>
    </header>
  )
}
