import { useCallback, useState } from 'react'
import '@xyflow/react/dist/style.css'

import { api, type GraphNode } from './api'
import Canvas from './components/Canvas'
import DetailPane from './components/DetailPane'
import Toolbar from './components/Toolbar'
import { useSnapshot } from './useSnapshot'

export default function App() {
  const { config, snapshot, graph, root, loading, error, refresh, setRoot, open } =
    useSnapshot()
  const [uploadError, setUploadError] = useState<string | null>(null)
  const [selected, setSelected] = useState<GraphNode | null>(null)

  // A node from the previous graph would show stale JSON against the new
  // snapshot, or a resource that no longer exists.
  const selectedNode =
    selected && graph?.nodes.some((n) => n.id === selected.id) ? selected : null

  const onUpload = useCallback(
    async (file: File) => {
      setUploadError(null)
      try {
        open(await api.uploadSnapshot(await file.text()))
      } catch (err) {
        setUploadError((err as Error).message)
      }
    },
    [open],
  )

  return (
    <div className="app">
      <Toolbar
        config={config}
        snapshot={snapshot}
        root={root}
        loading={loading}
        onRootChange={setRoot}
        onRefresh={refresh}
      />

      <div className="workspace">
        <main className="canvas">
          {graph ? (
            // Remounting on listener change resets the viewport, so fitView
            // frames the new pipeline instead of leaving it off-screen.
            <Canvas
              key={`${snapshot?.id}:${root}`}
              graph={graph}
              selectedID={selectedNode?.id ?? null}
              onSelect={setSelected}
            />
          ) : (
            <Placeholder
              loading={loading}
              message={error ?? uploadError}
              canFetch={config?.canFetch ?? false}
              onUpload={onUpload}
            />
          )}
        </main>

        {snapshot && selectedNode && (
          <DetailPane
            snapshotID={snapshot.id}
            node={selectedNode}
            onClose={() => setSelected(null)}
          />
        )}
      </div>

      {graph && (error || uploadError) && (
        <div className="banner banner-error">{error ?? uploadError}</div>
      )}
    </div>
  )
}

function Placeholder({
  loading,
  message,
  canFetch,
  onUpload,
}: {
  loading: boolean
  message: string | null
  canFetch: boolean
  onUpload: (file: File) => void
}) {
  const [dragging, setDragging] = useState(false)

  if (loading && !message) {
    return <div className="placeholder">Loading configuration…</div>
  }

  return (
    <div
      className={`placeholder${dragging ? ' placeholder-drag' : ''}`}
      onDragOver={(e) => {
        e.preventDefault()
        setDragging(true)
      }}
      onDragLeave={() => setDragging(false)}
      onDrop={(e) => {
        e.preventDefault()
        setDragging(false)
        const file = e.dataTransfer.files[0]
        if (file) onUpload(file)
      }}
    >
      {message && <p className="placeholder-error">{message}</p>}
      <p>
        {canFetch
          ? 'No configuration loaded. Refresh to fetch one from Envoy, or drop a config_dump JSON file here.'
          : 'Drop a config_dump JSON file here to explore it.'}
      </p>
      <label className="toolbar-button">
        Choose file…
        <input
          type="file"
          accept="application/json,.json"
          hidden
          onChange={(e) => {
            const file = e.target.files?.[0]
            if (file) onUpload(file)
            // Reset so choosing the same file twice fires onChange again.
            e.target.value = ''
          }}
        />
      </label>
    </div>
  )
}
