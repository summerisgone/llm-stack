import { useEffect, useState } from 'react'
import { base, date } from '../api.js'
import { getJSON, Loading, Failure, SourceErrors, YesNo, bytes, pct, na } from './common.jsx'

function used(total, avail) {
  if (total == null || avail == null) return na
  return <>{bytes(total - avail)} of {bytes(total)} <span className="muted">({pct((total - avail) / total)})</span></>
}

function num(v, digits = 2) {
  return v == null ? na : v.toFixed(digits)
}

export default function Nodes() {
  const [data, setData] = useState(null)
  const [error, setError] = useState(null)
  useEffect(() => { getJSON(`${base}/api/admin/nodes`).then(setData).catch((e) => setError(e.message)) }, [])
  return <section className="panel admin-panel">
    <div className="panel-heading"><div><p className="eyebrow">READ-ONLY TOPOLOGY</p><h2>Nodes</h2></div></div>
    {error ? <Failure error={error} /> : !data ? <Loading text="Loading nodes…" /> : <>
      <SourceErrors errors={data.errors} />
      <p className="muted coverage">Observed {date(data.observed_at)}. Readings older than {data.stale_after_seconds} s are marked stale. Missing sensors show N/A, never zero. GPU requests count pods in the stack namespace.</p>
      <div className="node-grid">{data.nodes.map((n) => <NodeCard key={n.name} node={n} />)}</div>
    </>}
  </section>
}

function NodeCard({ node }) {
  const h = node.hardware
  return <article className="node-card">
    <header>
      <div><strong>{node.name}</strong><span className="muted">{node.roles.join(', ') || 'worker'} · kubelet {node.kubelet_version}</span></div>
      <YesNo value={node.ready && node.problems.length === 0} yes="Ready" no={node.ready ? node.problems.join(', ') : 'Not ready'} />
    </header>
    {node.taints.length > 0 && <p className="muted">Taints: {node.taints.join(', ')}</p>}
    {h?.shares_host_with.length > 0 && <p className="muted">Same physical host as {h.shares_host_with.join(', ')}: host totals are not additive.</p>}
    <dl className="kv">
      <dt>GPUs</dt><dd>{node.gpus_requested} requested of {node.gpus_allocatable} allocatable</dd>
      <dt>Load 1/5/15</dt><dd>{h ? <>{num(h.load1)} / {num(h.load5)} / {num(h.load15)} <span className="muted">· {pct(h.normalized_load1)} of {h.cpus ?? '?'} CPUs</span></> : na}</dd>
      <dt>CPU busy</dt><dd>{h ? pct(h.cpu_busy) : na}</dd>
      <dt>Memory</dt><dd>{h ? used(h.mem_total_bytes, h.mem_available_bytes) : na}</dd>
      <dt>Disk</dt><dd>{h ? used(h.disk_size_bytes, h.disk_available_bytes) : na}</dd>
      <dt>Temperatures</dt><dd>{h?.sensors.length ? h.sensors.map((s) => <span key={s.label} className="chip">{s.label} {Math.round(s.celsius)} °C</span>) : na}</dd>
      {h?.stale && <><dt>Metrics</dt><dd className="bad-label">stale since {date(h.observed_at)}</dd></>}
    </dl>
    {h?.gpus.length > 0 && <div className="table-wrap"><table><thead><tr><th>GPU</th><th>Util</th><th>VRAM</th><th>Temp</th><th>Power</th></tr></thead><tbody>
      {h.gpus.map((g) => <tr key={g.index}>
        <td><strong>{g.index}: {g.name}</strong>{g.shared_with_node && <span className="muted">same device as on {g.shared_with_node}</span>}{g.stale && <span className="bad-label">stale</span>}</td>
        <td>{g.util_percent == null ? na : `${Math.round(g.util_percent)}%`}</td>
        <td>{g.mem_used_bytes == null ? na : <>{bytes(g.mem_used_bytes)} / {bytes(g.mem_total_bytes)}</>}</td>
        <td>{g.temp_celsius == null ? na : `${Math.round(g.temp_celsius)} °C`}</td>
        <td>{g.power_watts == null ? na : `${Math.round(g.power_watts)} W`}</td>
      </tr>)}
    </tbody></table></div>}
    {node.engines.length > 0 && <div className="table-wrap"><table><thead><tr><th>Engine pod</th><th>GPUs</th><th>TP group</th><th>Model cache</th></tr></thead><tbody>
      {node.engines.map((e) => <tr key={e.pod}><td><strong>{e.engine}</strong><span className="muted">{e.pod}</span></td><td>{e.gpus}</td><td>{e.tensor_parallel}</td>
        <td>cache {e.model_cache} · pod <YesNo value={e.ready} yes="ready" no="not ready" /></td></tr>)}
    </tbody></table></div>}
  </article>
}
