import { useEffect, useState } from 'react'
import { base, date } from '../api.js'
import { getJSON, Loading, Failure, SourceErrors, YesNo, na } from './common.jsx'
import { TargetChain, Health } from './Inference.jsx'

export default function Routes() {
  const [data, setData] = useState(null)
  const [error, setError] = useState(null)
  useEffect(() => { getJSON(`${base}/api/admin/routes`).then(setData).catch((e) => setError(e.message)) }, [])
  return <section className="panel admin-panel">
    <div className="panel-heading"><div><p className="eyebrow">READ-ONLY</p><h2>Envoy routes</h2></div></div>
    {error ? <Failure error={error} /> : !data ? <Loading text="Loading routes…" /> : <>
      <SourceErrors errors={data.errors} />
      <p className="muted coverage">
        Observed {date(data.observed_at)}. Shows Kubernetes configuration and controller acceptance, plus Envoy traffic from the gateway Prometheus.
        Effective Envoy configuration: {data.effective_config}. Accepted does not prove Envoy loaded the route.
      </p>
      <div className="flow">
        <span className="chip">edge listener</span><span className="arrow">→</span><span className="chip">HTTPRoute</span><span className="arrow">→</span>
        <span className="chip">pat-service / Open WebUI</span><span className="arrow">→</span><span className="chip">private AI Gateway</span><span className="arrow">→</span>
        <span className="chip">AIGatewayRoute by model</span><span className="arrow">→</span><span className="chip">EPP pool or direct backend</span>
      </div>
      <h3>Gateways</h3>
      <div className="table-wrap"><table><thead><tr><th>Gateway</th><th>Listeners</th><th>Programmed</th></tr></thead><tbody>
        {data.gateways.map((g) => <tr key={g.name}><td><strong>{g.name}</strong></td><td>{g.listeners.join(', ')}</td><td><YesNo value={g.programmed} /></td></tr>)}
      </tbody></table></div>
      <h3>Edge and service routes</h3>
      {data.edge_routes.map((r) => <div key={r.name} className="route-block">
        <div className="route-head"><strong>{r.name}</strong><span className="muted">{r.parents.join(', ')}{r.hostnames.length ? ` · ${r.hostnames.join(', ')}` : ''}</span>
          {r.status.map((st) => <span key={st.parent} className="parent-status">{st.parent}: <YesNo value={st.accepted} yes="Accepted" no={`Not accepted${st.reason ? ` (${st.reason})` : ''}`} /> <YesNo value={st.resolved_refs} yes="refs resolved" no="refs unresolved" /></span>)}</div>
        <div className="table-wrap"><table><thead><tr><th>#</th><th>Match</th><th>Filters</th><th>Backend</th><th>Traffic</th></tr></thead><tbody>
          {r.rules.map((rule) => <tr key={rule.index}><td>{rule.name || rule.index}</td><td className="wrap">{rule.matches.join('; ') || 'all'}</td>
            <td className="wrap">{rule.filters.join('; ') || '—'}</td><td>{rule.backends.join(', ') || '—'}</td><td><Health health={rule.health} /></td></tr>)}
        </tbody></table></div>
      </div>)}
      <h3>Model routes (private AI Gateway)</h3>
      <div className="table-wrap"><table><thead><tr><th>Route / rule</th><th>Model match</th><th>Path</th><th>Accepted</th><th>Traffic</th></tr></thead><tbody>
        {data.model_routes.map((m) => <tr key={`${m.route}-${m.index}`}><td><strong>{m.route}</strong><span className="muted">{m.rule || `#${m.index}`} · timeout {m.timeout || 'default'}</span></td>
          <td>{m.model || <span className="muted">catch-all</span>}</td><td className="wrap">{m.targets.map((t, i) => <div key={i}><TargetChain target={t} /></div>)}</td>
          <td><YesNo value={m.route_accepted} /></td><td><Health health={m.health} /></td></tr>)}
      </tbody></table></div>
      <h3>Policies</h3>
      <div className="table-wrap"><table><thead><tr><th>Policy</th><th>Targets</th><th>Configures</th><th>Accepted</th></tr></thead><tbody>
        {data.policies.map((p) => <tr key={`${p.kind}-${p.name}`}><td><strong>{p.name}</strong><span className="muted">{p.kind}</span></td><td>{p.targets.join(', ')}</td><td>{p.features.join(', ')}</td><td><YesNo value={p.accepted} /></td></tr>)}
      </tbody></table></div>
    </>}
  </section>
}
