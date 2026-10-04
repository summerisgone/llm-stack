import { useState } from 'react'
import { Icon } from '../components.jsx'
import { date } from '../api.js'

export async function getJSON(url) {
  const response = await fetch(url)
  const data = await response.json().catch(() => ({}))
  if (!response.ok) throw new Error(data.error?.message || `Request failed (${response.status})`)
  return data
}

export function Loading({ text }) {
  return <div className="empty"><span className="loader"></span>{text}</div>
}

export function Failure({ error }) {
  return <div className="empty"><Icon>!</Icon><strong>Could not load</strong><span>{error}</span></div>
}

// SourceErrors lists the sources a response could not read; the rest of the
// page still renders from the others.
export function SourceErrors({ errors }) {
  const entries = Object.entries(errors || {})
  if (entries.length === 0) return null
  return <div className="notice error source-errors" role="status"><Icon>!</Icon><div>
    <strong>Some sources are unavailable; their panels show N/A.</strong>
    <ul>{entries.map(([source, message]) => <li key={source}><code>{source}</code> {message}</li>)}</ul>
  </div></div>
}

export function Pager({ page, pages, onChange, disabled }) {
  if (pages <= 1) return null
  return <div className="pager">
    <button className="refresh" onClick={() => onChange(page - 1)} disabled={disabled || page === 0} aria-label="Previous page"><Icon>←</Icon></button>
    <span className="muted">{page + 1} / {pages}</span>
    <button className="refresh" onClick={() => onChange(page + 1)} disabled={disabled || page + 1 >= pages} aria-label="Next page"><Icon>→</Icon></button>
  </div>
}

export const na = <span className="muted">N/A</span>

export function compact(n) {
  if (n == null) return na
  return new Intl.NumberFormat(undefined, { notation: Math.abs(n) >= 10000 ? 'compact' : 'standard', maximumFractionDigits: 1 }).format(n)
}

export function seconds(s) {
  if (s == null) return na
  if (s < 1) return `${Math.round(s * 1000)} ms`
  if (s < 120) return `${s.toFixed(1)} s`
  const m = Math.round(s / 60)
  return m < 120 ? `${m} min` : `${Math.floor(m / 60)} h ${m % 60} min`
}

export function bytes(b) {
  if (b == null) return na
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let i = 0
  while (b >= 1024 && i < units.length - 1) { b /= 1024; i++ }
  return `${b.toFixed(i ? 1 : 0)} ${units[i]}`
}

export function pct(x) {
  return x == null ? na : `${Math.round(x * 100)}%`
}

export function Stat({ label, value, note }) {
  return <div className="stat"><span className="stat-label">{label}</span><strong>{value}</strong>{note && <span className="muted">{note}</span>}</div>
}

export function YesNo({ value, yes = 'Yes', no = 'No' }) {
  if (value == null) return na
  return value ? <span className="ok-label">✓ {yes}</span> : <span className="bad-label">✕ {no}</span>
}

// ColumnChart draws one series over time: thin columns with a rounded data
// end, a hairline baseline, clean max tick, and a hover tooltip per column.
export function ColumnChart({ title, points, value, format = compact, bucket }) {
  const [hover, setHover] = useState(null)
  const width = 640, height = 160, pad = { l: 44, r: 8, t: 8, b: 22 }
  const values = points.map(value)
  const max = Math.max(1, ...values)
  const niceMax = (() => {
    const step = 10 ** Math.floor(Math.log10(max))
    return Math.ceil(max / step) * step
  })()
  const band = (width - pad.l - pad.r) / Math.max(points.length, 1)
  const barW = Math.max(2, Math.min(24, band - 2))
  const y = (v) => pad.t + (height - pad.t - pad.b) * (1 - v / niceMax)
  const label = (at) => new Intl.DateTimeFormat(undefined, bucket === 'hour' ? { hour: '2-digit', minute: '2-digit', day: 'numeric', month: 'short' } : { day: 'numeric', month: 'short' }).format(new Date(at))
  return <figure className="chart">
    <figcaption>{title}</figcaption>
    {points.length === 0 ? <div className="empty small">No data in this range</div> :
      <div className="chart-plot">
        <svg viewBox={`0 0 ${width} ${height}`} role="img" aria-label={title} onMouseLeave={() => setHover(null)}>
          <line x1={pad.l} x2={width - pad.r} y1={y(0)} y2={y(0)} className="chart-axis" />
          <line x1={pad.l} x2={width - pad.r} y1={y(niceMax)} y2={y(niceMax)} className="chart-grid" />
          <text x={pad.l - 6} y={y(niceMax) + 4} textAnchor="end" className="chart-tick">{format(niceMax)}</text>
          <text x={pad.l - 6} y={y(0) + 4} textAnchor="end" className="chart-tick">0</text>
          {points.map((p, i) => {
            const v = value(p), x = pad.l + i * band + (band - barW) / 2, top = y(v), h = y(0) - top
            const r = Math.min(4, h, barW / 2)
            return <g key={p.at} onMouseEnter={() => setHover(i)}>
              <rect x={pad.l + i * band} y={pad.t} width={band} height={height - pad.t - pad.b} fill="transparent" />
              {h > 0 && <path className={`chart-bar${hover === i ? ' active' : ''}`}
                d={`M${x},${y(0)} V${top + r} Q${x},${top} ${x + r},${top} H${x + barW - r} Q${x + barW},${top} ${x + barW},${top + r} V${y(0)} Z`} />}
            </g>
          })}
          {points.length > 0 && <>
            <text x={pad.l} y={height - 4} className="chart-tick">{label(points[0].at)}</text>
            <text x={width - pad.r} y={height - 4} textAnchor="end" className="chart-tick">{label(points[points.length - 1].at)}</text>
          </>}
        </svg>
        {hover != null && <div className="chart-tooltip" style={{ left: `${((pad.l + (hover + 0.5) * band) / width) * 100}%` }}>
          <span>{label(points[hover].at)}</span><strong>{format(values[hover])}</strong>
        </div>}
      </div>}
  </figure>
}

export const ranges = [
  { key: '24h', label: 'Last 24 hours', hours: 24 },
  { key: '7d', label: 'Last 7 days', hours: 24 * 7 },
  { key: '30d', label: 'Last 30 days', hours: 24 * 30 },
  { key: '90d', label: 'Last 90 days', hours: 24 * 90 },
]

// coverageText says which inference paths the usage figures include
// (docs/adr/0022 section 5): Open WebUI chat is in the ledger only from the
// recorded cutover.
export function coverageText(data) {
  switch (data?.coverage) {
    case 'all_paths': return 'Usage covers all configured inference paths.'
    case 'mixed': return `Usage covers PAT and agent traffic only before ${date(data.coverage_since)}, all configured inference paths after it.`
    default: return 'Usage covers PAT and agent traffic only; Open WebUI chat is not counted yet.'
  }
}
