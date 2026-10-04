import { base, webui, send } from './api.js'

export function Icon({ children, className = '' }) {
  return <span aria-hidden="true" className={`icon ${className}`}>{children}</span>
}

export async function logout() {
  await send(`${base}/auth/logout`)
  window.location.assign(`${base}/`)
}

// Topbar is shared by the personal dashboard and the admin console. The
// Administration link only follows /api/session; the server authorizes.
export function Topbar({ capabilities, admin }) {
  return <nav className="topbar">
    <a className="brand" href={`${base}/`} aria-label="AI Stack PAT dashboard">
      <span className="brand-mark"><span></span><span></span><span></span></span>
      <span>AI Stack <em>{admin ? 'Administration' : 'Console'}</em></span>
    </a>
    <div className="topbar-actions">
      {admin
        ? <a className="openwebui-link" href={`${base}/`}>My tokens</a>
        : capabilities.includes('admin') && <a className="openwebui-link" href={`${base}/admin`}>Administration</a>}
      <a className="openwebui-link" href={webui}>Open WebUI <Icon>↗</Icon></a>
      <button className="signout" onClick={logout}><Icon>↗</Icon> Sign out</button>
    </div>
  </nav>
}
