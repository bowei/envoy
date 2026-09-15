import { useEffect, useState } from 'react'

import { api, type GraphNode, type ResourceDetail } from '../api'
import { kindStyle } from '../kinds'
import JsonTree from './JsonTree'

interface Props {
  snapshotID: string
  node: GraphNode
  onClose: () => void
}

type Load =
  | { state: 'idle' }
  | { state: 'loading' }
  | { state: 'loaded'; detail: ResourceDetail }
  | { state: 'failed'; message: string }

export default function DetailPane({ snapshotID, node, onClose }: Props) {
  const style = kindStyle(node.kind)
  const [load, setLoad] = useState<Load>({ state: 'idle' })
  const [raw, setRaw] = useState(false)

  const ref = node.resource

  // Raw JSON is fetched per node rather than shipped with the graph: a
  // production dump is tens of megabytes, and the user opens a handful of nodes.
  useEffect(() => {
    if (!ref) {
      setLoad({ state: 'idle' })
      return
    }
    let cancelled = false
    setLoad({ state: 'loading' })
    api
      .resource(snapshotID, ref.kind, ref.name)
      .then((detail) => !cancelled && setLoad({ state: 'loaded', detail }))
      .catch((err) => !cancelled && setLoad({ state: 'failed', message: err.message }))
    return () => {
      cancelled = true
    }
  }, [snapshotID, ref?.kind, ref?.name])

  const detail = load.state === 'loaded' ? load.detail : null

  return (
    <aside className="pane">
      <header className="pane-head">
        <span className="pane-badge" style={{ color: style.color }}>
          {style.badge}
        </span>
        <div className="pane-title">
          <h2>{node.label}</h2>
          <span className="dim">{style.title}</span>
        </div>
        <button className="pane-close" onClick={onClose} title="Close">
          ×
        </button>
      </header>

      <div className="pane-body">
        {node.sublabel && <p className="pane-sublabel">{node.sublabel}</p>}

        {node.notes?.map((note) => (
          <p key={note} className={`pane-note pane-note-${node.status}`}>
            {note}
          </p>
        ))}

        {node.details && node.details.length > 0 && (
          <Section title="Summary">
            <dl className="pane-fields">
              {node.details.map((d) => (
                <Field key={d.label} label={d.label} value={d.value} />
              ))}
            </dl>
          </Section>
        )}

        {detail && (
          <Section title="xDS resource">
            <dl className="pane-fields">
              <Field label="kind" value={detail.kind} />
              <Field label="name" value={detail.name} mono />
              <Field label="state" value={detail.state} />
              {detail.versionInfo && (
                <Field label="version_info" value={detail.versionInfo} mono />
              )}
              {detail.lastUpdated && (
                <Field
                  label="last updated"
                  value={new Date(detail.lastUpdated).toLocaleString()}
                />
              )}
              {detail.typeUrl && <Field label="type" value={detail.typeUrl} mono />}
            </dl>
          </Section>
        )}

        {detail?.errorState && (
          <Section title="Rejected update">
            <p className="pane-note pane-note-error">{detail.errorState.details}</p>
            <dl className="pane-fields">
              <Field
                label="failed version"
                value={detail.errorState.failed_version_info}
                mono
              />
              <Field
                label="attempted"
                value={new Date(detail.errorState.last_update_attempt).toLocaleString()}
              />
            </dl>
          </Section>
        )}

        {detail?.decodeError && (
          <Section title="Decode error">
            {/* The raw JSON below is still byte-exact, so a resource whose proto
                we cannot decode is inspectable anyway. */}
            <p className="pane-note pane-note-warning">{detail.decodeError}</p>
          </Section>
        )}

        {load.state === 'loading' && <p className="dim">Loading resource…</p>}
        {load.state === 'failed' && (
          <p className="pane-note pane-note-error">{load.message}</p>
        )}

        {detail && (
          <Section
            title="Configuration"
            action={
              <button className="pane-link" onClick={() => setRaw(!raw)}>
                {raw ? 'tree' : 'raw'}
              </button>
            }
          >
            {raw ? (
              <pre className="pane-raw">{JSON.stringify(detail.raw, null, 2)}</pre>
            ) : (
              <div className="jt">
                <JsonTree value={detail.raw} />
              </div>
            )}
          </Section>
        )}

        {!ref && (
          <p className="dim pane-hint">
            This stage is part of a listener's configuration rather than an xDS
            resource of its own. Open the listener to see its JSON.
          </p>
        )}
      </div>
    </aside>
  )
}

function Section({
  title,
  action,
  children,
}: {
  title: string
  action?: React.ReactNode
  children: React.ReactNode
}) {
  return (
    <section className="pane-section">
      <h3>
        {title}
        {action}
      </h3>
      {children}
    </section>
  )
}

function Field({
  label,
  value,
  mono,
}: {
  label: string
  value: string
  mono?: boolean
}) {
  return (
    <div>
      <dt>{label}</dt>
      <dd className={mono ? 'mono' : undefined}>{value}</dd>
    </div>
  )
}
