// opencode backend: `opencode serve` on loopback, driven over its HTTP API.
// hermes-sync has written <home>/opencode/opencode.json (provider, MCP,
// skills, AGENTS.md); sessions live in opencode's own database under
// <home>/opencode-data, and <home>/opencode-sessions.json maps chat keys to
// session ids.
import { spawn } from 'node:child_process'
import { readFileSync, writeFileSync, renameSync } from 'node:fs'
import { join } from 'node:path'

const PORT = 4096

export async function start({ home, cwd }) {
  const base = `http://127.0.0.1:${PORT}`
  const child = spawn('opencode', ['serve', '--hostname', '127.0.0.1', '--port', String(PORT)], {
    cwd,
    stdio: ['ignore', 'inherit', 'inherit'],
    env: {
      ...process.env,
      OPENCODE_CONFIG: join(home, 'opencode', 'opencode.json'),
      XDG_DATA_HOME: join(home, 'opencode-data'),
      XDG_STATE_HOME: join(home, 'opencode-data', 'state'),
      XDG_CONFIG_HOME: '/tmp/opencode-config',
      XDG_CACHE_HOME: '/tmp/opencode-cache',
      // No egress but pat-service: no plugin installs, update checks,
      // models.dev catalog, LSP downloads or sharing.
      OPENCODE_PURE: '1',
      OPENCODE_DISABLE_AUTOUPDATE: '1',
      OPENCODE_DISABLE_MODELS_FETCH: '1',
      OPENCODE_DISABLE_LSP_DOWNLOAD: '1',
      OPENCODE_DISABLE_DEFAULT_PLUGINS: '1',
      OPENCODE_DISABLE_SHARE: '1',
      OPENCODE_DISABLE_CLAUDE_CODE: '1',
      OPENCODE_DISABLE_PROJECT_CONFIG: '1',
      // opencode still tries a background npm install; fail it at once.
      npm_config_offline: 'true',
    },
  })
  child.on('exit', (code) => {
    console.error(JSON.stringify({ msg: 'opencode exited', code }))
    process.exit(1)
  })

  const call = async (method, path, body, signal) => {
    const res = await fetch(base + path, {
      method,
      signal,
      headers: body ? { 'Content-Type': 'application/json' } : {},
      body: body ? JSON.stringify(body) : undefined,
    })
    if (!res.ok) throw new Error(`opencode ${method} ${path}: ${res.status} ${(await res.text()).slice(0, 300)}`)
    const text = await res.text()
    return text ? JSON.parse(text) : null
  }

  // Per-attempt timeout: under gVisor a request that races the listener
  // coming up can hang without an answer.
  for (let i = 0; ; i++) {
    try {
      await call('GET', '/config', null, AbortSignal.timeout(2000))
      break
    } catch (err) {
      if (i > 600) throw err
      await new Promise((r) => setTimeout(r, 100))
    }
  }

  // One event stream for the process, open before the first turn; a turn
  // registers by session id.
  const listeners = new Map()
  const partTypes = new Map()
  const events = await fetch(base + '/event')
  ;(async () => {
    let buf = ''
    for await (const chunk of events.body) {
      buf += Buffer.from(chunk).toString('utf8')
      let i
      while ((i = buf.indexOf('\n')) >= 0) {
        const line = buf.slice(0, i).trim()
        buf = buf.slice(i + 1)
        if (!line.startsWith('data:')) continue
        let e
        try {
          e = JSON.parse(line.slice(5))
        } catch {
          continue
        }
        const p = e.properties || {}
        const l = listeners.get(p.sessionID)
        if (!l) continue
        switch (e.type) {
          case 'message.part.updated':
            partTypes.set(p.part.id, p.part.type)
            break
          case 'message.part.delta': {
            const type = partTypes.get(p.partID)
            if (p.field !== 'text') break
            if (type === 'text') {
              l.text += p.delta
              l.onText(p.delta)
            } else if (type === 'reasoning') {
              l.onReasoning(p.delta)
            }
            break
          }
          case 'session.error':
            l.error = p.error?.data?.message || p.error?.name || 'model error'
            break
          case 'session.idle':
            l.done()
            break
        }
      }
    }
  })().catch((err) => {
    console.error(JSON.stringify({ msg: 'opencode event stream ended', err: String(err) }))
    process.exit(1)
  })

  const mapPath = join(home, 'opencode-sessions.json')
  let map = {}
  try {
    map = JSON.parse(readFileSync(mapPath, 'utf8'))
  } catch {}
  const save = () => {
    writeFileSync(mapPath + '.tmp', JSON.stringify(map))
    renameSync(mapPath + '.tmp', mapPath)
  }

  async function sessionID(key) {
    if (map[key]) {
      try {
        await call('GET', `/session/${map[key]}`)
        return map[key]
      } catch {}
    }
    const s = await call('POST', '/session', {})
    map[key] = s.id
    save()
    return s.id
  }

  return {
    // The turn runs asynchronously and ends with session.idle on the event
    // stream: HTTP replies arrive ahead of the stream's deltas.
    async prompt(key, text, { onText, onReasoning, signal }) {
      const id = await sessionID(key)
      let done
      const idle = new Promise((resolve) => { done = resolve })
      const l = { onText, onReasoning, done, text: '', error: null }
      listeners.set(id, l)
      const onAbort = () => call('POST', `/session/${id}/abort`).catch(() => {})
      signal.addEventListener('abort', onAbort)
      try {
        await call('POST', `/session/${id}/prompt_async`, { parts: [{ type: 'text', text }] })
        await idle
        if (l.error) throw new Error(l.error)
        return l.text
      } finally {
        signal.removeEventListener('abort', onAbort)
        listeners.delete(id)
      }
    },
    async stop() {
      child.kill('SIGTERM')
    },
  }
}
