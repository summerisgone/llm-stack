import { useEffect, useState } from 'react'
import { base, date } from '../api.js'
import { Icon, Topbar } from '../components.jsx'
import { getJSON, Loading, Failure, Pager, coverageText } from './common.jsx'
import Overview from './Overview.jsx'
import UserDetail from './UserDetail.jsx'
import Inference from './Inference.jsx'
import Nodes from './Nodes.jsx'
import Routes from './Routes.jsx'

// Administration console (docs/adr/0022). Every call is authorized by the
// server; this component only decides what to render.

const sections = [
  { key: 'overview', label: 'Overview' },
  { key: 'users', label: 'Users' },
  { key: 'inference', label: 'Inference' },
  { key: 'nodes', label: 'Nodes' },
  { key: 'routes', label: 'Envoy routes' },
  { key: 'audit', label: 'Audit log' },
]

const usersPageSize = 25

function Users() {
  const [search, setSearch] = useState('')
  const [query, setQuery] = useState('')
  const [days, setDays] = useState(30)
  const [page, setPage] = useState(0)
  const [data, setData] = useState(null)
  const [error, setError] = useState(null)
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    const params = new URLSearchParams({ page, size: usersPageSize, days })
    if (query) params.set('search', query)
    setLoading(true)
    setError(null)
    getJSON(`${base}/api/admin/users?${params}`).then(setData).catch((e) => setError(e.message)).finally(() => setLoading(false))
  }, [query, days, page])

  function submit(event) {
    event.preventDefault()
    setPage(0)
    setQuery(search.trim())
  }

  const pages = data ? Math.max(1, Math.ceil(data.total / usersPageSize)) : 1
  return <section className="panel admin-panel">
    <div className="panel-heading">
      <div><p className="eyebrow">KEYCLOAK DIRECTORY</p><h2>Users {data && <span className="count">{data.total}</span>}</h2></div>
      <Pager page={page} pages={pages} onChange={setPage} disabled={loading} />
    </div>
    <form className="admin-filters" onSubmit={submit}>
      <input placeholder="Search username, email or name" value={search} onChange={(e) => setSearch(e.target.value)} />
      <select value={days} onChange={(e) => { setPage(0); setDays(Number(e.target.value)) }} aria-label="Usage period">
        <option value="7">Last 7 days</option><option value="30">Last 30 days</option><option value="90">Last 90 days</option>
      </select>
      <button className="primary">Search</button>
    </form>
    {data && <p className="muted coverage">{coverageText(data)}</p>}
    {loading ? <Loading text="Loading users…" /> : error ? <Failure error={error} /> : data.users.length === 0
      ? <div className="empty"><Icon>⌁</Icon><strong>No users found</strong></div>
      : <div className="table-wrap"><table><thead><tr><th>User</th><th>Status</th><th>Roles</th><th>Last activity</th><th>Requests</th><th>Tokens</th><th>Cost</th></tr></thead><tbody>
        {data.users.map((u) => <tr key={u.subject} className={u.enabled ? '' : 'revoked'}>
          <td><a className="row-link" href={`${base}/admin/users/${encodeURIComponent(u.subject)}`}><strong>{u.username}</strong></a><span className="muted">{u.email || u.name || u.subject}</span></td>
          <td>{u.enabled ? 'Enabled' : <span className="revoked-label">Disabled</span>}</td>
          <td>{u.roles.length ? u.roles.join(', ') : <span className="muted">none</span>}</td>
          <td>{u.last_activity_at ? date(u.last_activity_at) : <span className="muted">No requests</span>}</td>
          <td>{u.requests.toLocaleString()}</td>
          <td>{u.tokens.toLocaleString()}</td>
          <td>{u.cost_amount ? `${u.cost_amount.toFixed(2)} ${data.currency}` : '—'}</td>
        </tr>)}
      </tbody></table></div>}
  </section>
}

const outcomes = ['requested', 'denied', 'started', 'succeeded', 'failed', 'cancelled', 'rolled_back', 'manual_intervention_required']

function AuditDetail({ event }) {
  const show = (value) => (value == null ? '—' : JSON.stringify(value, null, 2))
  return <tr className="audit-detail"><td colSpan="7">
    <dl>
      <dt>Request</dt><dd><code>{event.request_id || '—'}</code></dd>
      <dt>Operation</dt><dd><code>{event.operation_id || '—'}</code>{event.parent_operation_id && <> parent <code>{event.parent_operation_id}</code></>}</dd>
      <dt>Actor</dt><dd><code>{event.actor.issuer || 'unknown'} / {event.actor.subject || 'unknown'}</code>{event.service_identity && <> via <code>{event.service_identity}</code></>}</dd>
      {event.reason && <><dt>Reason</dt><dd>{event.reason}</dd></>}
      {(event.expected_revision != null || event.applied_revision != null) && <><dt>Revision</dt><dd>expected {event.expected_revision ?? '—'}, applied {event.applied_revision ?? '—'}</dd></>}
    </dl>
    <div className="audit-diff"><div><p className="eyebrow">BEFORE</p><pre>{show(event.before)}</pre></div><div><p className="eyebrow">AFTER</p><pre>{show(event.after)}</pre></div></div>
  </td></tr>
}

function Audit() {
  const empty = { actor: '', action: '', target_type: '', target_id: '', outcome: '', operation_id: '', from: '', to: '' }
  const [form, setForm] = useState(empty)
  const [filters, setFilters] = useState(empty)
  const [events, setEvents] = useState([])
  const [next, setNext] = useState('')
  const [open, setOpen] = useState(null)
  const [error, setError] = useState(null)
  const [loading, setLoading] = useState(true)

  async function load(before) {
    const params = new URLSearchParams({ limit: 50 })
    for (const [key, value] of Object.entries(filters)) {
      if (!value) continue
      params.set(key, key === 'from' || key === 'to' ? new Date(value).toISOString() : value)
    }
    if (before) params.set('before_id', before)
    setLoading(true)
    setError(null)
    try {
      const data = await getJSON(`${base}/api/admin/audit?${params}`)
      setEvents((current) => (before ? [...current, ...data.events] : data.events))
      setNext(data.next_before_id)
    } catch (e) {
      setError(e.message)
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => { load('') }, [filters])

  const field = (key, placeholder) => <input placeholder={placeholder} value={form[key]} onChange={(e) => setForm({ ...form, [key]: e.target.value })} />
  return <section className="panel admin-panel">
    <div className="panel-heading"><div><p className="eyebrow">READ-ONLY JOURNAL</p><h2>Audit log</h2></div></div>
    <form className="admin-filters audit-filters" onSubmit={(e) => { e.preventDefault(); setOpen(null); setFilters(form) }}>
      {field('actor', 'Actor subject')}
      {field('action', 'Action')}
      {field('target_type', 'Target type')}
      {field('target_id', 'Target ID')}
      {field('operation_id', 'Operation ID')}
      <select value={form.outcome} onChange={(e) => setForm({ ...form, outcome: e.target.value })} aria-label="Outcome">
        <option value="">Any outcome</option>{outcomes.map((o) => <option key={o} value={o}>{o}</option>)}
      </select>
      <label>From<input type="datetime-local" value={form.from} onChange={(e) => setForm({ ...form, from: e.target.value })} /></label>
      <label>To<input type="datetime-local" value={form.to} onChange={(e) => setForm({ ...form, to: e.target.value })} /></label>
      <button className="primary">Apply</button>
    </form>
    <p className="muted coverage">Without a time range the last 30 days are shown. Times are in your browser's time zone.</p>
    {error ? <Failure error={error} /> : events.length === 0 && !loading
      ? <div className="empty"><Icon>⌁</Icon><strong>No events</strong><span>Nothing matches these filters.</span></div>
      : <div className="table-wrap"><table><thead><tr><th>Time</th><th>Actor</th><th>Action</th><th>Target</th><th>Source</th><th>Outcome</th><th>Error</th></tr></thead><tbody>
        {events.map((e) => [
          <tr key={e.id} className="audit-row" onClick={() => setOpen(open === e.id ? null : e.id)}>
            <td>{date(e.occurred_at)}</td>
            <td><strong>{e.actor.name || e.actor.subject || 'unknown'}</strong></td>
            <td><code>{e.action}</code></td>
            <td>{e.target_type ? `${e.target_type} ${e.target_id}` : '—'}</td>
            <td>{e.source}</td>
            <td><span className={`outcome outcome-${e.outcome}`}>{e.outcome}</span></td>
            <td>{e.error_code ? <span className="muted">{e.error_code}: {e.error_message}</span> : ''}</td>
          </tr>,
          open === e.id && <AuditDetail key={`${e.id}-detail`} event={e} />,
        ])}
      </tbody></table></div>}
    {loading ? <Loading text="Loading events…" /> : next && <button className="text-button" onClick={() => load(next)}>Load older events</button>}
  </section>
}

const screens = { overview: Overview, users: Users, inference: Inference, nodes: Nodes, routes: Routes, audit: Audit }

export default function AdminApp({ capabilities, path }) {
  const parts = path.split('/').filter(Boolean) // ["admin", section, ...]
  const current = screens[parts[1]] ? parts[1] : 'overview'
  const subject = current === 'users' && parts[2] ? decodeURIComponent(parts[2]) : null
  const Screen = screens[current]
  return <main className="shell">
    <Topbar capabilities={capabilities || []} admin />
    {capabilities === null ? <Loading text="Checking access…" />
      : !capabilities.includes('admin')
        ? <div className="empty"><Icon>!</Icon><strong>Administrator role required</strong><span>Ask an administrator to assign the ai-admin role in Keycloak, then sign in again.</span></div>
        : <>
          <nav className="admin-nav">{sections.map((s) => <a key={s.key} href={`${base}/admin/${s.key}`} className={s.key === current ? 'active' : ''}>{s.label}</a>)}</nav>
          {subject ? <UserDetail subject={subject} /> : <Screen />}
        </>}
  </main>
}
