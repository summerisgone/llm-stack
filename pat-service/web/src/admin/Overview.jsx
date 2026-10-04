import { useEffect, useState } from 'react'
import { base } from '../api.js'
import { getJSON, Loading, Failure, SourceErrors, Stat, ColumnChart, compact, pct, ranges, na, coverageText } from './common.jsx'

export default function Overview() {
  const [range, setRange] = useState('24h')
  const [data, setData] = useState(null)
  const [error, setError] = useState(null)

  useEffect(() => {
    setData(null)
    setError(null)
    const hours = ranges.find((r) => r.key === range).hours
    getJSON(`${base}/api/admin/overview?hours=${hours}`).then(setData).catch((e) => setError(e.message))
  }, [range])

  return <>
    <section className="panel admin-panel">
      <div className="panel-heading">
        <div><p className="eyebrow">ALL USERS</p><h2>Overview</h2></div>
        <select value={range} onChange={(e) => setRange(e.target.value)} aria-label="Period">
          {ranges.slice(0, 3).map((r) => <option key={r.key} value={r.key}>{r.label}</option>)}
        </select>
      </div>
      {error ? <Failure error={error} /> : !data ? <Loading text="Loading overview…" /> : <OverviewBody data={data} />}
    </section>
  </>
}

function OverviewBody({ data }) {
  const t = data.totals
  const n = data.nodes || {}
  const inference = data.inference
  return <>
    <SourceErrors errors={data.errors} />
    <p className="muted coverage">{coverageText(data)} Token quotas are not enforced yet.</p>
    {t && <div className="stats">
      <Stat label="Requests" value={compact(t.requests)} note={`${compact(t.requests - (t.outcomes.ok || 0))} not ok`} />
      <Stat label="Tokens" value={compact(t.prompt_tokens + t.completion_tokens)} note={`${compact(t.cached_tokens)} cached prompt`} />
      <Stat label="Active users" value={compact(t.active_users)} />
      <Stat label="Gateway 429s" value={compact(t.outcomes.rejected || 0)} note="Quota rejections: not enforced" />
      <Stat label="Nodes not ready" value={n.total == null ? na : `${n.not_ready} of ${n.total}`} />
      <Stat label="Highest load" value={n.max_normalized_load == null ? na : pct(n.max_normalized_load)} note={n.max_load_node ? `${n.max_load_node}, load1 per CPU` : ''} />
      <Stat label="Hottest sensor" value={n.hottest ? `${Math.round(n.hottest.celsius)} °C` : na} note={n.hottest ? `${n.hottest.label} on ${n.hottest.node}` : ''} />
    </div>}
    {data.timeline && <div className="chart-row">
      <ColumnChart title="Requests" points={data.timeline} value={(p) => p.requests} bucket={data.bucket} />
      <ColumnChart title="Tokens (prompt + output)" points={data.timeline} value={(p) => p.prompt_tokens + p.completion_tokens} bucket={data.bucket} />
    </div>}
    <div className="split">
      <div>
        <h3>Top users by tokens</h3>
        {data.top_users.length === 0 ? <p className="muted">No requests in this period.</p> :
          <div className="table-wrap"><table><thead><tr><th>User</th><th>Requests</th><th>Tokens</th><th>Cost</th></tr></thead><tbody>
            {data.top_users.map((u) => <tr key={u.subject}>
              <td><a className="row-link" href={`${base}/admin/users/${encodeURIComponent(u.subject)}`}>{u.name || u.subject}</a></td>
              <td>{compact(u.requests)}</td><td>{compact(u.tokens)}</td><td>{u.cost_amount ? `${u.cost_amount.toFixed(2)} ${data.currency}` : '—'}</td>
            </tr>)}
          </tbody></table></div>}
      </div>
      <div>
        <h3>Model pools and engines</h3>
        {!inference ? <p className="muted">Kubernetes topology unavailable.</p> : <>
          <p className="muted">Public models: {inference.models.join(', ') || 'none'}</p>
          <div className="table-wrap"><table><thead><tr><th>Engine</th><th>Model</th><th>Replicas</th><th>State</th></tr></thead><tbody>
            {inference.engines.map((e) => <tr key={e.name}><td><strong>{e.name}</strong><span className="muted">{e.engine} {e.engine_version}</span></td>
              <td>{e.served_model || na}</td><td>{e.ready_replicas}/{e.desired_replicas}</td><td><span className={`state state-${e.state}`}>{e.state}</span></td></tr>)}
          </tbody></table></div>
        </>}
      </div>
    </div>
  </>
}
