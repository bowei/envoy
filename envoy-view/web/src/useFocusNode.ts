import { useCallback } from 'react'
import { useReactFlow } from '@xyflow/react'

/** Zoom used when jumping to a node, chosen so its text is readable. */
const FOCUS_ZOOM = 1.1

/**
 * Returns a function that pans the canvas to a node. Searching for a name in a
 * graph of hundreds is useless if you then have to find it by eye.
 *
 * Only usable inside <ReactFlow>, which provides the store.
 */
export function useFocusNode() {
  const flow = useReactFlow()

  return useCallback(
    (id: string) => {
      const node = flow.getNode(id)
      if (!node) return
      const width = node.measured?.width ?? node.width ?? 0
      const height = node.measured?.height ?? node.height ?? 0
      flow.setCenter(node.position.x + width / 2, node.position.y + height / 2, {
        // Never zoom out to reach a node: if the user is already close in, the
        // jump should keep that detail.
        zoom: Math.max(flow.getZoom(), FOCUS_ZOOM),
        duration: 350,
      })
    },
    [flow],
  )
}
