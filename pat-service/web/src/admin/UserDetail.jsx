import { useEffect, useMemo, useState } from 'react'
import { base, date } from '../api.js'
import { Icon } from '../components.jsx'
import { getJSON, Loading, Failure, Stat, ColumnChart, Pager, compact, seconds, ranges, na, coverageText } from './common.jsx'

const sessionsPageSize = 20

export default function UserDetail({ subject }) {
  const [range, setRange] = useState('7d')
  const [model, setModel] = useState('')
  const [source, setSource] = useState('')
  const [data, setData] = useState(null)
  const [error, setError] = useState(null)

  // The window is fixed when a filter changes, so the summary, sessions and
  // requests all describe the same period.
  const query = useMemo(() => {
    const to = new Date()
    const from = new Date(to.getTime() - ranges.find((r) => r.key === range).hours * 3600e3)
    const p = new URLSearchParams({ from: from.toISOString(), to: to.toISOString() })
    if (model) p.set('model', model)
    if (source) p.set('source', source)
    return p.toString()
  }, [subject, range, model, source])
  const userURL = `${base}/api/admin/users/${encodeURIComponent(subject)}`

  useEffect(() => {
    setData(null)
    setError(null)
    getJSON(`${userURL}?${query}`).then(setData).catch((e) => setError(e.message))
  }, [query])

  return <section className="panel admin-panel">
    <a className="text-button back" href={`${base}/admin/users`}>← All users</a>
    {error ? <Failure error={error} /> : !data ? <Loading text="Loading user…" /> : <>
      <div className="panel-heading">
        <div>
          <p className="eyebrow">{data.user.deleted ? 'HISTORICAL SUBJECT' : 'USER'}</p>
          <h2>{data.user.username || data.user.subject}</h2>
          <p className="muted">{data.user.deleted ? 'No Keycloak account has this subject any more.' : `${data.user.email || ''} · ${data.user.enabled ? 'enabled' : 'disabled'} · roles: ${data.user.roles.join(', ') || 'none'}`}</p>
          <p className="muted"><code>{data.user.subject}</code></p>
        </div>
      </div>
      <div className="admin-filters">
        <select value={range} onChange={(e) => setRange(e.target.value)} aria-label="Period">{ranges.map((r) => <option key={r.key} value={r.key}>{r.label}</option>)}</select>
        <select value={model} onChange={(e) => setModel(e.target.value)} aria-label="Model"><option value="">All models</option>{data.models.map((m) => <option key={m}>{m}</option>)}</select>
        <select value={source} onChange={(e) => setSource(e.target.value)} aria-label="Source"><option value="">All sources</option>{data.sources.map((s) => <option key={s}>{s}</option>)}</select>
      </div>
      <p className="muted coverage">
        {coverageText(data)} History from {data.history_from ? date(data.history_from) : 'no requests'};
        timing recorded from {data.timing_from ? date(data.timing_from) : 'no timed requests'}. Times shown in your browser's time zone.
      </p>
      <Stats data={data} />
      <div className="chart-row">
        <ColumnChart title={`Requests per ${data.bucket}`} points={data.timeline} value={(p) => p.requests} bucket={data.bucket} />
        <ColumnChart title={`Tokens per ${data.bucket} (prompt + output)`} points={data.timeline} value={(p) => p.prompt_tokens + p.completion_tokens} bucket={data.bucket} />
      </div>
      <Sessions url={`${userURL}/sessions?${query}`} currency={data.currency} />
      <Requests url={`${userURL}/requests?${query}`} currency={data.currency} />
    </>}
  </section>
}

function Stats({ data }) {
  const t = data.totals, l = data.latency, u = data.usage_time
  return <>
    <div className="stats">
      <Stat label="Requests" value={compact(t.requests)} note={Object.entries(t.outcomes).map(([k, v]) => `${k} ${v}`).join(' · ')} />
      <Stat label="Prompt tokens" value={compact(t.prompt_tokens)} note={`${compact(t.cached_tokens)} cached`} />
      <Stat label="Output tokens" value={compact(t.completion_tokens)} note={t.requests_without_usage ? `${t.requests_without_usage} requests without usage` : 'all with reported usage'} />
      <Stat label="Cost" value={t.cost_amount ? `${t.cost_amount.toFixed(2)} ${data.currency}` : '—'} note="informational, at configured prices" />
      <Stat label="Sessions" value={compact(t.sessions)} note={`${t.requests_without_session} requests outside a session`} />
      <Stat label="Active time" value={seconds(u.active_seconds)} note={`union of request intervals; summed ${seconds(u.summed_request_seconds)}`} />
    </div>
    <div className="stats">
      <Stat label="TTFT p50 / p95" value={l.ttft_samples ? <>{seconds(l.ttft_p50_seconds)} / {seconds(l.ttft_p95_seconds)}</> : na} note={`${l.ttft_samples} samples`} />
      <Stat label="TPOT p50 / p95" value={l.tpot_samples ? <>{seconds(l.tpot_p50_seconds)} / {seconds(l.tpot_p95_seconds)}</> : na} note={`${l.tpot_samples} complete streams`} />
      <Stat label="Output rate" value={l.output_tokens_per_second == null ? na : `${l.output_tokens_per_second.toFixed(1)} tok/s`} note={`end to end, ${l.rate_samples} requests`} />
      <Stat label="Session span" value={seconds(u.session_span_seconds)} note="wall clock, summed over sessions" />
    </div>
    <p className="muted coverage">
      TTFT: receipt at pat-service to first text, reasoning or tool-call output. TPOT is measured at the proxy and is approximate.
      Not measurable: {l.not_measurable.recorded_before_timing} recorded before timing, {l.not_measurable.not_streaming} not streamed, {l.not_measurable.no_output} streams without output.
    </p>
  </>
}

function Sessions({ url, currency }) {
  const [page, setPage] = useState(0)
  const [data, setData] = useState(null)
  useEffect(() => { setPage(0) }, [url])
  useEffect(() => {
    getJSON(`${url}&limit=${sessionsPageSize}&offset=${page * sessionsPageSize}`).then(setData).catch(() => setData({ sessions: [], total: 0 }))
  }, [url, page])
  const pages = data ? Math.max(1, Math.ceil(data.total / sessionsPageSize)) : 1
  return <div className="subsection">
    <div className="panel-heading"><h3>Sessions <span className="count">{data?.total ?? 0}</span></h3><Pager page={page} pages={pages} onChange={setPage} disabled={!data} /></div>
    {!data ? <Loading text="Loading sessions…" /> : data.sessions.length === 0 ? <p className="muted">No sessions in this period.</p> :
      <div className="table-wrap"><table><thead><tr><th>Model</th><th>Key</th><th>Started</th><th>Ended</th><th>Requests</th><th>Tokens</th><th>Cost</th></tr></thead><tbody>
        {data.sessions.map((s) => <tr key={s.session_id}><td><strong>{s.model}</strong></td><td>{s.token_name || '—'}</td><td>{date(s.started_at)}</td><td>{date(s.ended_at)}</td>
          <td>{s.steps}</td><td>{compact(s.prompt_tokens + s.completion_tokens)}</td><td>{s.cost_amount ? `${s.cost_amount.toFixed(2)} ${currency}` : '—'}</td></tr>)}
      </tbody></table></div>}
  </div>
}

function Requests({ url, currency }) {
  const [rows, setRows] = useState(null)
  const [next, setNext] = useState('')
  const load = async (before) => {
    const data = await getJSON(`${url}&limit=50${before ? `&before_id=${before}` : ''}`).catch(() => ({ requests: [], next_before_id: '' }))
    setRows((current) => (before ? [...current, ...data.requests] : data.requests))
    setNext(data.next_before_id)
  }
  useEffect(() => { setRows(null); load('') }, [url])
  return <div className="subsection">
    <h3>Requests</h3>
    {!rows ? <Loading text="Loading requests…" /> : rows.length === 0 ? <p className="muted">No requests in this period.</p> : <>
      <div className="table-wrap"><table><thead><tr><th>Finished</th><th>Model</th><th>Source</th><th>Outcome</th><th>Prompt / cached / output</th><th>TTFT</th><th>Duration</th><th>Cost</th></tr></thead><tbody>
        {rows.map((q) => <tr key={q.id}>
          <td>{date(q.recorded_at)}</td><td><strong>{q.model}</strong><span className="muted">{q.token_name}</span></td><td>{q.source}{q.streaming ? ' · stream' : ''}</td>
          <td><span className={`outcome outcome-${q.outcome === 'ok' ? 'succeeded' : 'failed'}`}>{q.outcome}{q.http_status ? ` ${q.http_status}` : ''}</span>{q.usage_quality !== 'actual' && <span className="muted">usage {q.usage_quality}</span>}</td>
          <td>{compact(q.prompt_tokens)} / {compact(q.cached_tokens)} / {compact(q.completion_tokens)}</td>
          <td>{seconds(q.ttft_seconds)}</td><td>{seconds(q.duration_seconds)}</td><td>{q.cost_amount ? `${q.cost_amount.toFixed(4)} ${currency}` : '—'}</td>
        </tr>)}
      </tbody></table></div>
      {next && <button className="text-button" onClick={() => load(next)}>Load older requests <Icon>↓</Icon></button>}
    </>}
  </div>
}
