// dsh (DeepSeek Harness) backend: two dsh processes on one DSH_HOME
// (<home>/dsh), where agent-sync has rendered cordis.patch.yml (provider =
// pat-service, MCP, skills), acp.patch.yml, web.patch.yml and AGENTS.md.
// `dsh --profile acp` serves the chats over ACP (newline-delimited JSON-RPC
// on stdio); `dsh web` serves the user's browser UI on :3080, reached only
// through agent-broker's web proxy. <home>/dsh-sessions.json maps chat keys
// to ACP session ids; sessions live under <home>/dsh/sessions. Tool calls
// and permission requests reach the user only on the interaction path
// (server.mjs /v1/agent/turns, docs/adr/0021).
import { spawn } from 'node:child_process'
import { chmodSync, mkdirSync, readFileSync, writeFileSync, renameSync } from 'node:fs'
import { join } from 'node:path'
import { createInterface } from 'node:readline'

export async function start({ home, cwd }) {
  const dshHome = join(home, 'dsh')
  mkdirSync(cwd, { recursive: true })
  // fsGroup makes kubelet add group rw to the profile volume at every mount;
  // dsh refuses a credentials file readable beyond its owner.
  try {
    chmodSync(join(dshHome, '.credentials.yaml'), 0o600)
  } catch {}
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
  const permissions = new Map()
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
      const ask = permissions.get(m.params.sessionId)
      if (!ask) {
        // Chat-completions path (ADR 0020): the pod is the sandbox (gVisor,
        // NetworkPolicy), as for the other runtimes.
        const allow = pick(m.params.options, ['allow_once', 'allow_always'])
        send({ id: m.id, result: { outcome: allow ? { outcome: 'selected', optionId: allow.optionId } : { outcome: 'cancelled' } } })
        return
      }
      // Interaction path (ADR 0021): the user decides; anything but an
      // explicit approval is a rejection or a cancellation.
      const tc = m.params.toolCall || {}
      ask({ title: tc.title || 'Tool call', text: toolText(tc) }).then((decision) => {
        const o = decision === 'allow' ? pick(m.params.options, ['allow_once', 'allow_always'])
          : decision === 'deny' ? pick(m.params.options, ['reject_once', 'reject_always']) : null
        send({ id: m.id, result: { outcome: o ? { outcome: 'selected', optionId: o.optionId } : { outcome: 'cancelled' } } })
      })
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
    // onTool and onPermission are the interaction path (ADR 0021); without
    // onPermission requests are approved as before.
    async prompt(key, text, { onText, onReasoning, onTool, onPermission, onStop, signal }) {
      const id = await sessionID(key)
      let out = ''
      let lastMessage = null
      updates.set(id, (u) => {
        if (u.sessionUpdate === 'tool_call' || u.sessionUpdate === 'tool_call_update') {
          return onTool?.({ id: u.toolCallId, title: u.title, kind: u.kind, status: u.status, text: toolText(u) })
        }
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
      if (onPermission) permissions.set(id, onPermission)
      try {
        const r = await request('session/prompt', { sessionId: id, prompt: [{ type: 'text', text }] })
        onStop?.(r?.stopReason)
        return out
      } finally {
        signal.removeEventListener('abort', onAbort)
        updates.delete(id)
        permissions.delete(id)
      }
    },
    interactive: true,
    hasSession: (key) => key in map,
    webToken: () => webToken,
    async stop() {
      acp.kill('SIGTERM')
      web.kill('SIGTERM')
    },
  }
}

function pick(options, kinds) {
  for (const k of kinds) {
    const o = options.find((o) => o.kind === k)
    if (o) return o
  }
  return null
}

// toolText summarizes an ACP tool call's content for display: text blocks,
// diff paths and terminal ids.
function toolText(tc) {
  const parts = []
  for (const c of tc.content || []) {
    if (c.type === 'content' && c.content?.type === 'text') parts.push(c.content.text)
    else if (c.type === 'diff') parts.push(`diff: ${c.path}`)
    else if (c.type === 'terminal') parts.push(`terminal: ${c.terminalId}`)
  }
  return parts.join('\n')
}
