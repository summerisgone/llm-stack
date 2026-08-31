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

function App() {
  const [tokens, setTokens] = useState([])
  const [loading, setLoading] = useState(true)
  const [creating, setCreating] = useState(false)
  const [notice, setNotice] = useState(null)
  const [newToken, setNewToken] = useState(null)
  const [copied, setCopied] = useState(false)
  const [form, setForm] = useState({ name: '', days: 90 })

  async function load() {
    setLoading(true)
    try {
      const response = await fetch(api)
      if (!response.ok) throw new Error('Could not load tokens')
      const data = await response.json()
      setTokens(data.tokens || [])
    } catch (error) {
      setNotice({ kind: 'error', text: error.message })
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => { load() }, [])

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

  const activeCount = tokens.filter((token) => !token.revoked_at).length

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
        {loading ? <div className="empty"><span className="loader"></span>Loading tokens…</div> : tokens.length === 0 ? <div className="empty"><Icon>⌁</Icon><strong>No tokens yet</strong><span>Create one to start making API requests.</span></div> : <div className="table-wrap"><table><thead><tr><th>Name</th><th>Token</th><th>Created</th><th>Last used</th><th></th></tr></thead><tbody>{tokens.map((token) => <tr key={token.id} className={token.revoked_at ? 'revoked' : ''}><td><strong>{token.name}</strong>{token.revoked_at && <span className="revoked-label">Revoked</span>}</td><td><code>{token.prefix}…</code></td><td>{date(token.created_at)}</td><td>{date(token.last_used_at)}</td><td>{token.revoked_at ? <span className="muted">Unavailable</span> : <button className="revoke" onClick={() => revoke(token)}>Revoke</button>}</td></tr>)}</tbody></table></div>}
      </div>
    </section>
    <footer><span>Protected by Keycloak SSO</span><span>•</span><span>Tokens are validated on every request</span></footer>
  </main>
}

createRoot(document.getElementById('root')).render(<App />)
