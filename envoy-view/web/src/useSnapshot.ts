import { useCallback, useEffect, useRef, useState } from 'react'

import { ApiError, api, type Config, type Graph, type Meta } from './api'

export interface SnapshotState {
  config: Config | null
  snapshot: Meta | null
  graph: Graph | null
  /** Listener name the graph is scoped to; empty means the whole config. */
  root: string
  loading: boolean
  error: string | null
}

const initialState: SnapshotState = {
  config: null,
  snapshot: null,
  graph: null,
  root: '',
  loading: true,
  error: null,
}

/**
 * Owns the snapshot lifecycle: find or create one, load its graph, and let the
 * user re-fetch. The snapshot is deliberately not auto-polled — a graph that
 * relayouts underneath you while you are reading it is worse than a stale one
 * you refresh on purpose.
 */
export function useSnapshot() {
  const [state, setState] = useState<SnapshotState>(initialState)

  // Current selection, readable from callbacks without making them depend on
  // (and be recreated by) every render.
  const current = useRef({ snapshot: null as Meta | null, root: '' })

  // Guards against a slow response for an abandoned selection overwriting a
  // newer one; the canvas would silently show the wrong pipeline.
  const seq = useRef(0)

  const loadGraph = useCallback(async (snapshot: Meta, root: string) => {
    const mine = ++seq.current
    current.current = { snapshot, root }
    setState((s) => ({ ...s, snapshot, root, loading: true, error: null }))
    try {
      const graph = await api.graph(snapshot.id, root || undefined)
      if (mine !== seq.current) return
      setState((s) => ({ ...s, graph, loading: false }))
    } catch (err) {
      if (mine !== seq.current) return
      setState((s) => ({
        ...s,
        loading: false,
        // A vanished snapshot is a recoverable state, not a dead end: say what
        // to do about it rather than showing a bare 404.
        error:
          err instanceof ApiError && err.isMissingSnapshot
            ? 'This snapshot has expired. Refresh to load a new one.'
            : (err as Error).message,
      }))
    }
  }, [])

  const setRoot = useCallback(
    (root: string) => {
      const { snapshot } = current.current
      if (snapshot) void loadGraph(snapshot, root)
    },
    [loadGraph],
  )

  /** Loads a snapshot the caller already has, such as a fresh upload. */
  const open = useCallback(
    (snapshot: Meta) => {
      void loadGraph(snapshot, '')
    },
    [loadGraph],
  )

  /** Fetches a fresh dump from Envoy and switches to it. */
  const refresh = useCallback(async () => {
    setState((s) => ({ ...s, loading: true, error: null }))
    try {
      const snapshot = await api.fetchSnapshot()
      // Keep the current listener selection if the new dump still has it, so a
      // refresh does not throw the user back to the whole-config view.
      const root = current.current.root
      const keep = root && snapshot.listeners.some((l) => l.name === root)
      await loadGraph(snapshot, keep ? root : '')
    } catch (err) {
      setState((s) => ({ ...s, loading: false, error: (err as Error).message }))
    }
  }, [loadGraph])

  useEffect(() => {
    let cancelled = false
    void (async () => {
      try {
        const config = await api.config()
        if (cancelled) return
        setState((s) => ({ ...s, config }))

        // A snapshot may already exist: preloaded with -dump-file, or created
        // by an earlier page load. Reuse it rather than hitting Envoy again.
        const existing = await api.listSnapshots()
        if (cancelled) return
        if (existing.length > 0) {
          await loadGraph(existing[0], '')
        } else if (config.canFetch) {
          const snapshot = await api.fetchSnapshot()
          if (cancelled) return
          await loadGraph(snapshot, '')
        } else {
          // Upload-only mode with nothing loaded: App shows the drop target.
          setState((s) => ({ ...s, loading: false }))
        }
      } catch (err) {
        if (cancelled) return
        setState((s) => ({ ...s, loading: false, error: (err as Error).message }))
      }
    })()
    return () => {
      cancelled = true
    }
  }, [loadGraph])

  return { ...state, refresh, setRoot, open }
}
