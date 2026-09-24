import { useEffect, useState } from 'react'
import { createRoot } from 'react-dom/client'
import './styles.css'

// The public path prefix is injected by the Go server into the
// <meta name="pat-base"> tag in index.html from the URL_PREFIX env var.
// Reading it from a meta tag rather than an inline <script> keeps the page
// compatible with the service's own CSP header (script-src 'self'). Empty
// string when the dashboard is mounted at "/", otherwise the public path
// prefix that the Gateway's URLRewrite filter strips before the request
// reaches this service.
const base = (document.querySelector('meta[name="pat-base"]')?.content || '').replace(/\/$/, '')
const api = `${base}/api/tokens`

// Public origin of Open WebUI, injected by the Go server from the WEBUI_URL
// env var (set on the remote profile). Falls back to the local-mac dev host
// so the development profile keeps working without that env var.
const webui = (document.querySelector('meta[name="pat-webui"]')?.content || 'http://ai.localhost:8080').replace(/\/$/, '')

function date(value) {
  return value ? new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' }).format(new Date(value)) : 'Never'
}

function Icon({ children, className = '' }) {
  return <span aria-hidden="true" className={`icon ${className}`}>{children}</span>
}

// heatmapDays is how far back the calendar heatmap reaches -- 182 days is
// about 26 weeks, the same span GitHub's own contribution graph shows by
// default, and matches the /api/usage/daily?days= default's spirit without
// asking the API for a full year on every dashboard load.
const heatmapDays = 182

const sessionsPageSize = 20

// buildHeatmapWeeks turns the sparse `daily` rows (only days with activity)
// into a dense grid of every day in the window, grouped into Sunday-start
// weeks so the grid always renders complete columns -- same layout GitHub's
// heatmap uses. Returns the grid plus the window's max day total, which the
// caller buckets colors against.
function buildHeatmapWeeks(daily) {
  const byDate = new Map(daily.map((d) => [d.date, d]))
  const today = new Date()
  today.setUTCHours(0, 0, 0, 0)
  const start = new Date(today)
  start.setUTCDate(start.getUTCDate() - (heatmapDays - 1))
  start.setUTCDate(start.getUTCDate() - start.getUTCDay())

  const cells = []
  let max = 0
  for (const d = new Date(start); d <= today; d.setUTCDate(d.getUTCDate() + 1)) {
    const key = d.toISOString().slice(0, 10)
    const entry = byDate.get(key)
    const tokens = entry ? entry.prompt_tokens + entry.completion_tokens : 0
    max = Math.max(max, tokens)
    cells.push({ date: key, tokens, cost: entry?.cost_amount || 0 })
  }
  const weeks = []
  for (let i = 0; i < cells.length; i += 7) weeks.push(cells.slice(i, i + 7))
  return { weeks, max }
}

function heatmapLevel(tokens, max) {
  if (tokens <= 0 || max <= 0) return 0
  const ratio = tokens / max
  if (ratio > 0.75) return 4
  if (ratio > 0.5) return 3
  if (ratio > 0.25) return 2
  return 1
}

function Heatmap({ daily }) {
  const { weeks, max } = buildHeatmapWeeks(daily)
  return <div className="heatmap" role="img" aria-label={`Daily token usage over the last ${heatmapDays} days`}>
    {weeks.map((week, i) => <div className="heatmap-week" key={i}>
      {week.map((day) => <div key={day.date} className="heatmap-day" data-level={heatmapLevel(day.tokens, max)}
        title={`${day.date}: ${day.tokens.toLocaleString()} tokens`} />)}
    </div>)}
  </div>
}

function formatDuration(startedAt, endedAt) {
  const ms = new Date(endedAt) - new Date(startedAt)
  if (ms < 60000) return '<1m'
  const minutes = Math.round(ms / 60000)
  return minutes < 60 ? `${minutes}m` : `${Math.floor(minutes / 60)}h ${minutes % 60}m`
}

function SessionsTable({ sessions, currency }) {
  if (sessions.length === 0) {
    return <div className="empty"><Icon>⌁</Icon><strong>No sessions yet</strong><span>Agent sessions show up here once you send some traffic.</span></div>
  }
  return <div className="table-wrap"><table><thead><tr><th>Model</th><th>Key</th><th>Started</th><th>Duration</th><th>Steps</th><th>Tokens</th><th>Cost</th></tr></thead><tbody>
    {sessions.map((s) => <tr key={s.session_id}>
      <td><strong>{s.model}</strong></td>
      <td>{s.token_name || <span className="muted">—</span>}</td>
      <td>{date(s.started_at)}</td>
      <td>{formatDuration(s.started_at, s.ended_at)}</td>
      <td>{s.steps}</td>
      <td>{(s.prompt_tokens + s.completion_tokens).toLocaleString()}</td>
      <td>{s.cost_amount ? `${s.cost_amount.toFixed(2)} ${currency}` : '—'}</td>
    </tr>)}
  </tbody></table></div>
}

const usageWindowLabels = { hour: 'Last hour', day: 'Last 24 hours', week: 'Last 7 days', month: 'This month' }

function LimitWidget({ limit }) {
  if (!limit) return null
  const hasLimit = limit.limit > 0
  const pct = hasLimit ? Math.min(100, (limit.spent / limit.limit) * 100) : 0
  const over = hasLimit && limit.spent > limit.limit
  return <div className="panel limit-panel">
    <div className="panel-heading"><div><p className="eyebrow">USAGE</p><h2>Spend and tokens</h2></div></div>
    <div className="usage-windows">
      {(limit.windows || []).map((w) => <div className="usage-window" key={w.key}>
        <span>{usageWindowLabels[w.key] || w.key}</span>
        <span className="muted">{w.tokens.toLocaleString()} tokens</span>
        <strong>{w.spent.toFixed(2)} {limit.currency}</strong>
      </div>)}
    </div>
    {hasLimit
      ? <>
        <div className="limit-bar-track"><div className={`limit-bar-fill${over ? ' over' : ''}`} style={{ width: `${pct}%` }} /></div>
        <div className="limit-meta"><span>{limit.spent.toFixed(2)} {limit.currency} this month</span><span>limit {limit.limit.toFixed(2)} {limit.currency}</span></div>
      </>
      : <div className="limit-meta"><span className="muted">No monthly limit configured yet</span></div>}
  </div>
}

// HermesPanel issues the Hermes agent's inference key (POST /api/hermes-token).
// The key goes straight into the agent's credential Secret and is never
// shown; issuing again replaces it and restarts a running agent.
function HermesPanel({ current, onIssued, setNotice }) {
  const [issuing, setIssuing] = useState(false)

  async function issue() {
    if (current && !window.confirm('Replace the current Hermes agent key? The old key is revoked and a running agent restarts.')) return
    setIssuing(true)
    setNotice(null)
    try {
      const response = await fetch(`${base}/api/hermes-token`, { method: 'POST' })
      if (!response.ok) throw new Error((await response.text()).trim() || 'Could not issue Hermes key')
      const data = await response.json()
      setNotice({ kind: 'success', text: `Hermes agent key issued, valid until ${date(data.expires_at)}.${data.agent_restarted ? ' Your running agent was restarted.' : ''}` })
      await onIssued()
    } catch (error) {
      setNotice({ kind: 'error', text: error.message })
    } finally {
      setIssuing(false)
    }
  }

  return <div className="panel hermes-panel">
    <div className="panel-heading"><div><p className="eyebrow">HERMES AGENT</p><h2>Agent inference key</h2></div></div>
    <p className="muted">{current
      ? <>Active key <code>{current.prefix}…</code>, expires {date(current.expires_at)}.</>
      : 'No active key. Your Hermes agent cannot call models until you issue one.'}</p>
    <button className="primary" onClick={issue} disabled={issuing}>{issuing ? 'Issuing key…' : <>{current ? 'Rotate Hermes key' : 'Issue Hermes key'} <Icon>→</Icon></>}</button>
  </div>
}

function App() {
  const [tokens, setTokens] = useState([])
  const [tokensCurrency, setTokensCurrency] = useState('')
  const [loading, setLoading] = useState(true)
  const [creating, setCreating] = useState(false)
  const [notice, setNotice] = useState(null)
  const [newToken, setNewToken] = useState(null)
  const [copied, setCopied] = useState(false)
  const [form, setForm] = useState({ name: '', days: 90 })
  const [daily, setDaily] = useState([])
  const [sessions, setSessions] = useState([])
  const [sessionsTotal, setSessionsTotal] = useState(0)
  const [sessionsPage, setSessionsPage] = useState(0)
  const [sessionsLoading, setSessionsLoading] = useState(true)
  const [limit, setLimit] = useState(null)
  const [usageLoading, setUsageLoading] = useState(true)

  async function load() {
    setLoading(true)
    try {
      const response = await fetch(api)
      if (!response.ok) throw new Error('Could not load tokens')
      const data = await response.json()
      setTokens(data.tokens || [])
      setTokensCurrency(data.currency || '')
    } catch (error) {
      setNotice({ kind: 'error', text: error.message })
    } finally {
      setLoading(false)
    }
  }

  async function loadUsage() {
    setUsageLoading(true)
    try {
      const [dailyRes, limitRes] = await Promise.all([
        fetch(`${base}/api/usage/daily?days=${heatmapDays}`),
        fetch(`${base}/api/usage/limit`),
      ])
      if (dailyRes.ok) setDaily((await dailyRes.json()).days || [])
      if (limitRes.ok) setLimit(await limitRes.json())
    } catch {
      // Usage is a secondary panel; a failed fetch just leaves it empty
      // rather than surfacing a notice over the primary token workflow.
    } finally {
      setUsageLoading(false)
    }
  }

  async function loadSessions(page) {
    setSessionsLoading(true)
    try {
      const response = await fetch(`${base}/api/usage/sessions?limit=${sessionsPageSize}&offset=${page * sessionsPageSize}`)
      if (response.ok) {
        const data = await response.json()
        setSessions(data.sessions || [])
        setSessionsTotal(data.total || 0)
      }
    } catch {
      // Same as loadUsage: a secondary panel stays empty on failure.
    } finally {
      setSessionsLoading(false)
    }
  }

  useEffect(() => { load(); loadUsage() }, [])
  useEffect(() => { loadSessions(sessionsPage) }, [sessionsPage])

  async function create(event) {
    event.preventDefault()
    setCreating(true)
    setNotice(null)
    try {
      const response = await fetch(api, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: form.name, expires_in_days: Number(form.days) }),
      })
      const data = await response.json()
      if (!response.ok) throw new Error(data.error?.message || 'Could not create token')
      setNewToken(data)
      setCopied(false)
      setForm({ name: '', days: 90 })
      setNotice({ kind: 'success', text: 'Personal access token created.' })
      await load()
    } catch (error) {
      setNotice({ kind: 'error', text: error.message })
    } finally {
      setCreating(false)
    }
  }

  async function copyToken() {
    try {
      await navigator.clipboard.writeText(newToken.token)
      setCopied(true)
    } catch {
      setNotice({ kind: 'error', text: 'Clipboard access was blocked. Copy the token manually.' })
    }
  }

  async function revoke(token) {
    if (!window.confirm(`Revoke “${token.name}”? This cannot be undone.`)) return
    setNotice(null)
    try {
      const response = await fetch(`${api}/${token.id}`, { method: 'POST' })
      if (!response.ok) throw new Error('Could not revoke token')
      setNotice({ kind: 'success', text: `“${token.name}” was revoked. It can no longer access inference.` })
      await load()
    } catch (error) {
      setNotice({ kind: 'error', text: error.message })
    }
  }

  async function logout() {
    await fetch(`${base}/auth/logout`, { method: 'POST' })
    window.location.assign(`${base}/`)
  }

  const sessionsPages = Math.max(1, Math.ceil(sessionsTotal / sessionsPageSize))
  const activeCount = tokens.filter((token) => !token.revoked_at).length
  const hermesToken = tokens.find((token) => token.issued_by === 'hermes' && !token.revoked_at && new Date(token.expires_at) > new Date())

  return <main className="shell">
    <nav className="topbar">
      <a className="brand" href={`${base}/`} aria-label="AI Stack PAT dashboard">
        <span className="brand-mark"><span></span><span></span><span></span></span>
        <span>AI Stack <em>Console</em></span>
      </a>
      <div className="topbar-actions">
        <a className="openwebui-link" href={webui}>Open WebUI <Icon>↗</Icon></a>
        <button className="signout" onClick={logout}><Icon>↗</Icon> Sign out</button>
      </div>
    </nav>

    <section className="hero">
      <div>
        <p className="eyebrow">DEVELOPER ACCESS</p>
        <h1>Personal access<br /><span>tokens.</span></h1>
        <p className="lede">Create scoped credentials for the OpenAI-compatible API. Your secret is displayed only once; we retain only its cryptographic hash.</p>
      </div>
      <aside className="endpoint-card">
        <span className="status-dot"></span><span className="endpoint-label">Inference gateway</span>
        <code>POST /v1/chat/completions</code>
        <small>Authenticate with <strong>Bearer sk-…</strong></small>
      </aside>
    </section>

    {notice && <div className={`notice ${notice.kind}`} role="status"><Icon>{notice.kind === 'success' ? '✓' : '!'}</Icon>{notice.text}<button onClick={() => setNotice(null)} aria-label="Dismiss">×</button></div>}

    {newToken && <section className="secret-card" aria-labelledby="secret-title">
      <div className="secret-title"><Icon>⌁</Icon><div><p className="eyebrow">COPY NOW</p><h2 id="secret-title">Your new token</h2></div></div>
      <p>This value will not be shown again. Store it in your password manager or secret store.</p>
      <div className="secret-value"><code>{newToken.token}</code><button className="copy" onClick={copyToken}>{copied ? 'Copied' : 'Copy token'}</button></div>
      <button className="text-button" onClick={() => setNewToken(null)}>I’ve stored it safely</button>
    </section>}

    <section className="workspace">
      <div className="panel create-panel">
        <div className="panel-heading"><div><p className="eyebrow">NEW CREDENTIAL</p><h2>Create a token</h2></div><span className="step">01</span></div>
        <form onSubmit={create}>
          <label>Token name<input required maxLength="100" placeholder="e.g. Local development" value={form.name} onChange={(event) => setForm({ ...form, name: event.target.value })} /></label>
          <label>Expires after<select value={form.days} onChange={(event) => setForm({ ...form, days: event.target.value })}><option value="30">30 days</option><option value="90">90 days</option><option value="180">180 days</option><option value="365">1 year</option></select></label>
          <button className="primary" disabled={creating}>{creating ? 'Creating token…' : <>Create personal token <Icon>→</Icon></>}</button>
        </form>
      </div>

      <div className="panel token-panel">
        <div className="panel-heading"><div><p className="eyebrow">ACTIVE INVENTORY</p><h2>Your tokens <span className="count">{activeCount}</span></h2></div><button className="refresh" onClick={load} disabled={loading} aria-label="Refresh tokens"><Icon className={loading ? 'spin' : ''}>↻</Icon></button></div>
        {loading ? <div className="empty"><span className="loader"></span>Loading tokens…</div> : tokens.length === 0 ? <div className="empty"><Icon>⌁</Icon><strong>No tokens yet</strong><span>Create one to start making API requests.</span></div> : <div className="table-wrap"><table><thead><tr><th>Name</th><th>Token</th><th>Created</th><th>Last used</th><th>Usage</th><th></th></tr></thead><tbody>{tokens.map((token) => <tr key={token.id} className={token.revoked_at ? 'revoked' : ''}><td><strong>{token.name}</strong>{token.issued_by === 'hermes' && <span className="hermes-label">Hermes</span>}{token.revoked_at && <span className="revoked-label">Revoked</span>}</td><td><code>{token.prefix}…</code></td><td>{date(token.created_at)}</td><td>{date(token.last_used_at)}</td><td>{token.cost_amount ? `${token.cost_amount.toFixed(2)} ${tokensCurrency}` : '—'}</td><td>{token.revoked_at ? <span className="muted">Unavailable</span> : <button className="revoke" onClick={() => revoke(token)}>Revoke</button>}</td></tr>)}</tbody></table></div>}
      </div>
    </section>

    <HermesPanel current={hermesToken} onIssued={load} setNotice={setNotice} />

    <section className="workspace usage-section">
      <LimitWidget limit={limit} />
      <div className="panel heatmap-panel">
        <div className="panel-heading"><div><p className="eyebrow">LAST {heatmapDays} DAYS</p><h2>Daily tokens</h2></div></div>
        {usageLoading ? <div className="empty"><span className="loader"></span>Loading usage…</div> : <>
          <Heatmap daily={daily} />
          <div className="heatmap-legend"><span>Less</span>{[0, 1, 2, 3, 4].map((level) => <span key={level} className="heatmap-day" data-level={level} />)}<span>More</span></div>
        </>}
      </div>
    </section>

    <section className="panel sessions-panel">
      <div className="panel-heading"><div><p className="eyebrow">AGENT ACTIVITY</p><h2>Recent sessions <span className="count">{sessionsTotal}</span></h2></div>
        {sessionsPages > 1 && <div className="pager">
          <button className="refresh" onClick={() => setSessionsPage(sessionsPage - 1)} disabled={sessionsLoading || sessionsPage === 0} aria-label="Previous page"><Icon>←</Icon></button>
          <span className="muted">{sessionsPage + 1} / {sessionsPages}</span>
          <button className="refresh" onClick={() => setSessionsPage(sessionsPage + 1)} disabled={sessionsLoading || sessionsPage + 1 >= sessionsPages} aria-label="Next page"><Icon>→</Icon></button>
        </div>}
      </div>
      {sessionsLoading ? <div className="empty"><span className="loader"></span>Loading sessions…</div> : <SessionsTable sessions={sessions} currency={limit?.currency || ''} />}
    </section>

    <footer><span>Protected by Keycloak SSO</span><span>•</span><span>Tokens are validated on every request</span></footer>
  </main>
}

createRoot(document.getElementById('root')).render(<App />)
