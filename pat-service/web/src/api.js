// Values the Go server injects into index.html <meta> tags (see main.go's
// dashboard handler); meta tags keep the page compatible with the CSP
// (script-src 'self').
const meta = (name) => document.querySelector(`meta[name="${name}"]`)?.content || ''

// Public path prefix the Gateway's URLRewrite strips before the request
// reaches the service; empty when mounted at "/".
export const base = meta('pat-base').replace(/\/$/, '')

// Public origin of Open WebUI (WEBUI_URL); falls back to the local-mac dev
// host so the development profile works without that env var.
export const webui = (meta('pat-webui') || 'http://ai.localhost:8080').replace(/\/$/, '')

// Session-bound CSRF token every state-changing request must carry.
const csrf = meta('pat-csrf')

export function send(url, { method = 'POST', json, admin = false } = {}) {
  const headers = { 'X-CSRF-Token': csrf }
  if (json !== undefined) headers['Content-Type'] = 'application/json'
  if (admin) headers['X-Admin-Source'] = 'ui'
  return fetch(url, { method, headers, body: json === undefined ? undefined : JSON.stringify(json) })
}

export function date(value) {
  return value ? new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' }).format(new Date(value)) : 'Never'
}
