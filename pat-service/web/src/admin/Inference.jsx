import { useEffect, useState } from 'react'
import { base, date } from '../api.js'
import { getJSON, Loading, Failure, SourceErrors, YesNo, na } from './common.jsx'

export function TargetChain({ target }) {
  return <span className="chain">
    <span className="chip">{target.backend}</span>
    {target.model_override && <span className="chip muted-chip">as {target.model_override}</span>}
    <span className="arrow">→</span>
    {target.kind === 'pool' && <><span className="chip">pool {target.pool}</span><span className="muted">EPP fairness</span></>}
    {target.kind === 'engine' && <span className="chip">direct, no EPP</span>}
    {target.kind === 'external' && <span className="chip">external {target.endpoints.join(', ')}</span>}
    {target.kind === 'service' && <span className="chip">service {target.endpoints.join(', ')}</span>}
    {target.kind === 'unresolved' && <span className="chip bad-chip">unresolved backend</span>}
    {target.engines.length > 0 && <><span className="arrow">→</span>{target.engines.map((e) => <span className="chip" key={e}>{e}</span>)}</>}
    {target.ready_endpoints != null && <span className={target.ready_endpoints > 0 ? 'ok-label' : 'bad-label'}>{target.ready_endpoints} ready</span>}
  </span>
}

export function Health({ health }) {
  if (!health) return na
  return <span className={health.stale ? 'muted' : ''}>
    {health.request_rate != null ? `${health.request_rate.toFixed(2)} rq/s` : na}
    {health.error_5xx_rate ? ` · ${health.error_5xx_rate.toFixed(2)} 5xx/s` : ''}
    {health.endpoints != null ? ` · ${health.healthy_endpoints ?? 0}/${health.endpoints} healthy` : ''}
    {health.stale ? ' (stale)' : ''}
  </span>
}

export default function Inference() {
  const [data, setData] = useState(null)
  const [error, setError] = useState(null)
  useEffect(() => { getJSON(`${base}/api/admin/inference`).then(setData).catch((e) => setError(e.message)) }, [])
  return <section className="panel admin-panel">
    <div className="panel-heading"><div><p className="eyebrow">READ-ONLY INVENTORY</p><h2>Inference</h2></div></div>
    {error ? <Failure error={error} /> : !data ? <Loading text="Loading inventory…" /> : <>
      <SourceErrors errors={data.errors} />
      <p className="muted coverage">Observed {date(data.observed_at)}. Kubernetes desired state and readiness; switching engines and models arrives with managed operations.</p>
      <h3>Models</h3>
      <div className="table-wrap"><table><thead><tr><th>Public model</th><th>Route rule</th><th>Serves</th><th>Gateway traffic</th></tr></thead><tbody>
        {data.models.map((m) => <tr key={`${m.route}-${m.index}`}>
          <td><strong>{m.model || 'any other name'}</strong>{!m.model && <span className="muted">catch-all</span>}</td>
          <td>{m.route} / {m.rule || `#${m.index}`}<span className="muted">timeout {m.timeout || 'default'}</span></td>
          <td className="wrap">{m.targets.map((t, i) => <div key={i}><TargetChain target={t} /></div>)}</td>
          <td><Health health={m.health} /></td>
        </tr>)}
      </tbody></table></div>
      <h3>Engines</h3>
      <div className="table-wrap"><table><thead><tr><th>Release</th><th>Engine</th><th>Served model</th><th>Checkpoint</th><th>GPUs</th><th>Replicas</th><th>State</th><th>Pods</th></tr></thead><tbody>
        {data.engines.map((e) => <tr key={e.name}>
          <td><strong>{e.name}</strong><span className="muted">{e.release || 'not Helm'}{e.pools.length ? ` · pool ${e.pools.join(', ')}` : ''}</span></td>
          <td>{e.engine} <span className="muted">{e.engine_version}{e.managed ? '' : ' · read-only'}</span></td>
          <td>{e.served_model || na}</td>
          <td>{e.checkpoint || na}</td>
          <td>{e.gpus_per_replica} per replica<span className="muted">TP {e.tensor_parallel}</span></td>
          <td>{e.ready_replicas}/{e.desired_replicas}</td>
          <td><span className={`state state-${e.state}`}>{e.state}</span></td>
          <td className="wrap">{e.pods.length === 0 ? <span className="muted">none</span> : e.pods.map((p) => <div key={p.name}>{p.node || 'unscheduled'} · <YesNo value={p.ready} yes="ready" no={p.phase} /><span className="muted">model cache {p.model_cache}{p.restarts ? ` · ${p.restarts} restarts` : ''}</span></div>)}</td>
        </tr>)}
      </tbody></table></div>
      <h3>Pools</h3>
      <div className="table-wrap"><table><thead><tr><th>Pool</th><th>Selector</th><th>Endpoint picker</th><th>Engines</th><th>Ready endpoints</th></tr></thead><tbody>
        {data.pools.map((p) => <tr key={p.name}>
          <td><strong>{p.name}</strong><span className="muted">port {p.target_ports.join(', ')}</span></td>
          <td><code>{Object.entries(p.selector).map(([k, v]) => `${k}=${v}`).join(', ')}</code></td>
          <td>{p.endpoint_picker} <YesNo value={p.endpoint_picker_ready > 0} yes="ready" no="not ready" /><span className="muted">failure mode {p.failure_mode || 'default'}</span></td>
          <td>{p.engines.join(', ') || na}</td>
          <td>{p.endpoints.filter((e) => e.ready).length} of {p.endpoints.length}</td>
        </tr>)}
      </tbody></table></div>
    </>}
  </section>
}
