// dsh (DeepSeek Harness) backend: two dsh processes on one DSH_HOME
// (<home>/dsh), where agent-sync has rendered cordis.patch.yml (provider =
// pat-service, MCP, skills), acp.patch.yml, web.patch.yml and AGENTS.md.
// `dsh --profile acp` serves the chats over ACP (newline-delimited JSON-RPC
// on stdio); `dsh web` serves the user's browser UI on :3080, reached only
// through agent-broker's web proxy. <home>/dsh-sessions.json maps chat keys
// to ACP session ids; sessions live under <home>/dsh/sessions.
import { spawn } from 'node:child_process'
import { mkdirSync, readFileSync, writeFileSync, renameSync } from 'node:fs'
import { join } from 'node:path'
import { createInterface } from 'node:readline'

export async function start({ home, cwd }) {
  const dshHome = join(home, 'dsh')
  mkdirSync(cwd, { recursive: true })
  const env = { ...process.env, DSH_HOME: dshHome }
  const exit = (name) => (code) => {
    console.error(JSON.stringify({ msg: `${name} exited`, code }))
    process.exit(1)
  }

  // The launch token dsh web prints is handed only to the broker
  // (GET /web-token), which exchanges it for the browser's session cookie.
  let webToken = null
  const webArgs = ['web', '--patch', join(dshHome, 'web.patch.yml'), '--no-open']
  if (process.env.DSH_WEB_HOST) webArgs.push('--trusted-host', process.env.DSH_WEB_HOST)
  const web = spawn('dsh', webArgs, { cwd, env, stdio: ['ignore', 'pipe', 'inherit'] })
  web.on('exit', exit('dsh web'))
  createInterface({ input: web.stdout }).on('line', (line) => {
    const m = line.match(/[?&]token=([^&\s)]+)/)
    if (m) webToken = decodeURIComponent(m[1])
    console.error(line.replace(/token=[^&\s)]+/g, 'token=***'))
  })

  const acp = spawn('dsh', ['--profile', 'acp', '--patch', join(dshHome, 'acp.patch.yml')], {
    cwd, env, stdio: ['pipe', 'pipe', 'inherit'],
  })
  acp.on('exit', exit('dsh acp'))
  let nextId = 0
  const pending = new Map()
  const updates = new Map()
  const send = (msg) => acp.stdin.write(JSON.stringify({ jsonrpc: '2.0', ...msg }) + '\n')
  const request = (method, params) => new Promise((resolve, reject) => {
    const id = ++nextId
    pending.set(id, { resolve, reject })
    send({ id, method, params })
  })
  createInterface({ input: acp.stdout }).on('line', (line) => {
    let m
    try {
      m = JSON.parse(line)
    } catch {
      return
    }
    if (m.method === undefined) {
      const p = pending.get(m.id)
      if (!p) return
      pending.delete(m.id)
      if (m.error) p.reject(new Error(`dsh: ${m.error.message}`))
      else p.resolve(m.result)
    } else if (m.method === 'session/update') {
      updates.get(m.params.sessionId)?.(m.params.update)
    } else if (m.method === 'session/request_permission') {
      // The pod is the sandbox (gVisor, NetworkPolicy), as for the other runtimes.
      const allow = m.params.options.find((o) => o.kind === 'allow_once' || o.kind === 'allow_always')
      send({ id: m.id, result: { outcome: allow ? { outcome: 'selected', optionId: allow.optionId } : { outcome: 'cancelled' } } })
    } else if (m.id !== undefined) {
      send({ id: m.id, error: { code: -32601, message: 'method not found' } })
    }
  })
  await request('initialize', { protocolVersion: 1, clientCapabilities: {} })

  const mapPath = join(home, 'dsh-sessions.json')
  let map = {}
  try {
    map = JSON.parse(readFileSync(mapPath, 'utf8'))
  } catch {}
  const save = () => {
    writeFileSync(mapPath + '.tmp', JSON.stringify(map))
    renameSync(mapPath + '.tmp', mapPath)
  }

  // Sessions opened by this ACP connection; others are resumed from disk.
  const open = new Set()
  async function sessionID(key) {
    const id = map[key]
    if (id && open.has(id)) return id
    if (id) {
      try {
        await request('session/resume', { sessionId: id, cwd, mcpServers: [] })
        open.add(id)
        return id
      } catch {}
    }
    const s = await request('session/new', { cwd, mcpServers: [] })
    map[key] = s.sessionId
    save()
    open.add(s.sessionId)
    return s.sessionId
  }

  return {
    // dsh sends whole committed messages, not token deltas; consecutive
    // messages of one turn (between tool calls) are separated by a blank line.
    async prompt(key, text, { onText, onReasoning, signal }) {
      const id = await sessionID(key)
      let out = ''
      let lastMessage = null
      updates.set(id, (u) => {
        if (u.content?.type !== 'text') return
        if (u.sessionUpdate === 'agent_thought_chunk') return onReasoning(u.content.text)
        if (u.sessionUpdate !== 'agent_message_chunk') return
        const t = out && u.messageId !== lastMessage ? '\n\n' + u.content.text : u.content.text
        lastMessage = u.messageId
        out += t
        onText(t)
      })
      const onAbort = () => send({ method: 'session/cancel', params: { sessionId: id } })
      signal.addEventListener('abort', onAbort)
      try {
        await request('session/prompt', { sessionId: id, prompt: [{ type: 'text', text }] })
        return out
      } finally {
        signal.removeEventListener('abort', onAbort)
        updates.delete(id)
      }
    },
    webToken: () => webToken,
    async stop() {
      acp.kill('SIGTERM')
      web.kill('SIGTERM')
    },
  }
}
