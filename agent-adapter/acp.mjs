// ACP (Agent Client Protocol) client for agents that run as an ACP server on
// stdio: newline-delimited JSON-RPC (docs/adr/0021). startACP drives one
// agent process; start() picks the command for opencode, pi and hermes when
// AGENT_PROTOCOL=acp (dsh.mjs uses startACP for dsh, always). The client
// advertises no fs or terminal capabilities, so agents use their own tools
// inside the pod. mapPath maps chat keys to ACP session ids; a known session
// is reattached with session/resume or session/load, whichever the agent
// accepts, before a new one is created.
import { spawn } from 'node:child_process'
import { mkdirSync, readdirSync, readFileSync, renameSync, statSync, symlinkSync, writeFileSync } from 'node:fs'
import { homedir } from 'node:os'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { createInterface } from 'node:readline'

export async function startACP({ name, command, args = [], cwd, env, mapPath }) {
  const child = spawn(command, args, { cwd, env, stdio: ['pipe', 'pipe', 'inherit'] })
  let stopping = false
  child.on('exit', (code) => {
    if (stopping) return
    console.error(JSON.stringify({ msg: `${name} exited`, code }))
    process.exit(1)
  })
  let nextId = 0
  const pending = new Map()
  const updates = new Map()
  const permissions = new Map()
  // toolCallId -> the latest fields of that call, for permission requests.
  const calls = new Map()
  const send = (msg) => child.stdin.write(JSON.stringify({ jsonrpc: '2.0', ...msg }) + '\n')
  const request = (method, params) => new Promise((resolve, reject) => {
    const id = ++nextId
    pending.set(id, { resolve, reject })
    send({ id, method, params })
  })
  createInterface({ input: child.stdout }).on('line', (line) => {
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
      if (m.error) p.reject(new Error(`${name}: ${m.error.message}`))
      else p.resolve(m.result)
    } else if (m.method === 'session/update') {
      const u = m.params.update
      if (u?.toolCallId && (u.sessionUpdate === 'tool_call' || u.sessionUpdate === 'tool_call_update')) {
        calls.set(u.toolCallId, { ...calls.get(u.toolCallId), ...Object.fromEntries(Object.entries(u).filter(([, v]) => v !== undefined)) })
      }
      updates.get(m.params.sessionId)?.(u)
    } else if (m.method === 'session/request_permission') {
      const ask = permissions.get(m.params.sessionId)
      if (!ask) {
        // Chat-completions path: the pod is the sandbox (gVisor,
        // NetworkPolicy), as before the interaction path existed.
        const allow = pick(m.params.options, ['allow_once', 'allow_always'])
        send({ id: m.id, result: { outcome: allow ? { outcome: 'selected', optionId: allow.optionId } : { outcome: 'cancelled' } } })
        return
      }
      // Interaction path: the user decides; anything but an explicit
      // approval is a rejection or a cancellation.
      // Some agents (dsh) send a bare toolCallId; the earlier tool_call
      // update carries the title and input.
      const tc = { ...calls.get(m.params.toolCall?.toolCallId), ...m.params.toolCall }
      ask({ title: tc.title || 'Tool call', text: toolText(tc) || inputText(tc.rawInput) }).then((decision) => {
        const o = decision === 'allow' ? pick(m.params.options, ['allow_once', 'allow_always'])
          : decision === 'deny' ? pick(m.params.options, ['reject_once', 'reject_always']) : null
        send({ id: m.id, result: { outcome: o ? { outcome: 'selected', optionId: o.optionId } : { outcome: 'cancelled' } } })
      })
    } else if (m.id !== undefined) {
      send({ id: m.id, error: { code: -32601, message: 'method not found' } })
    }
  })
  const init = await request('initialize', { protocolVersion: 1, clientCapabilities: {} })
  const caps = init?.agentCapabilities || {}

  let map = {}
  try {
    map = JSON.parse(readFileSync(mapPath, 'utf8'))
  } catch {}
  const save = () => {
    writeFileSync(mapPath + '.tmp', JSON.stringify(map))
    renameSync(mapPath + '.tmp', mapPath)
  }

  // Sessions opened by this connection; others are reattached from disk.
  const open = new Set()
  async function sessionID(key) {
    const id = map[key]
    if (id && open.has(id)) return id
    if (id) {
      // Advertised methods first; an agent advertising neither (dsh) is
      // still tried with session/resume.
      const reattach = []
      if (caps.sessionCapabilities?.resume) reattach.push('session/resume')
      if (caps.loadSession) reattach.push('session/load')
      if (!reattach.length) reattach.push('session/resume')
      for (const method of reattach) {
        try {
          await request(method, { sessionId: id, cwd, mcpServers: [] })
          open.add(id)
          return id
        } catch {}
      }
    }
    const s = await request('session/new', { cwd, mcpServers: [] })
    map[key] = s.sessionId
    save()
    open.add(s.sessionId)
    return s.sessionId
  }

  return {
    // Consecutive messages of one turn (between tool calls) are separated
    // by a blank line when the agent tags them with message ids.
    // onTool and onPermission are the interaction path; without
    // onPermission requests are approved.
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
        calls.clear()
      }
    },
    interactive: true,
    hasSession: (key) => key in map,
    async stop() {
      stopping = true
      child.kill('SIGTERM')
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

// inputText shows a tool call's raw input: the command if it has one.
function inputText(input) {
  if (!input || typeof input !== 'object') return ''
  if (typeof input.command === 'string') return '$ ' + input.command
  return JSON.stringify(input).slice(0, 2000)
}

// Asks before running shell commands and editing files; the rest of
// opencode.json's permissions (web tools denied) stay as rendered.
const OPENCODE_PERMISSION = JSON.stringify({ bash: 'ask', edit: 'ask' })

export async function start({ home, cwd, runtime }) {
  mkdirSync(cwd, { recursive: true })
  switch (runtime) {
    case 'opencode':
      // Same profile layout as opencode.mjs; sessions live in opencode's
      // database, so the chat map is shared with the native path.
      return startACP({
        name: 'opencode acp', command: 'opencode', args: ['acp'], cwd,
        mapPath: join(home, 'opencode-sessions.json'),
        env: {
          ...process.env,
          OPENCODE_CONFIG: join(home, 'opencode', 'opencode.json'),
          OPENCODE_PERMISSION,
          XDG_DATA_HOME: join(home, 'opencode-data'),
          XDG_STATE_HOME: join(home, 'opencode-data', 'state'),
          XDG_CONFIG_HOME: '/tmp/opencode-config',
          XDG_CACHE_HOME: '/tmp/opencode-cache',
          OPENCODE_PURE: '1',
          OPENCODE_DISABLE_AUTOUPDATE: '1',
          OPENCODE_DISABLE_MODELS_FETCH: '1',
          OPENCODE_DISABLE_LSP_DOWNLOAD: '1',
          OPENCODE_DISABLE_DEFAULT_PLUGINS: '1',
          OPENCODE_DISABLE_SHARE: '1',
          OPENCODE_DISABLE_CLAUDE_CODE: '1',
          OPENCODE_DISABLE_PROJECT_CONFIG: '1',
          npm_config_offline: 'true',
        },
      })
    case 'pi': {
      // pi-acp runs `pi --mode rpc` through pi-rpc (this directory), which
      // loads pi-mcp-adapter. pi-acp keeps its session map under
      // ~/.pi/pi-acp; HOME is the /work emptyDir, so link it to the profile.
      const piAcpDir = join(home, 'pi-acp')
      mkdirSync(piAcpDir, { recursive: true })
      mkdirSync(join(homedir(), '.pi'), { recursive: true })
      try {
        symlinkSync(piAcpDir, join(homedir(), '.pi', 'pi-acp'))
      } catch {}
      migratePiSessions(home, cwd, piAcpDir)
      const here = dirname(fileURLToPath(import.meta.url))
      return startACP({
        name: 'pi-acp', command: join(here, 'node_modules', '.bin', 'pi-acp'), cwd,
        mapPath: join(home, 'pi-acp-sessions.json'),
        env: {
          ...process.env,
          PI_CODING_AGENT_DIR: join(home, 'pi'),
          PI_ACP_PI_COMMAND: join(here, 'pi-rpc'),
          npm_config_offline: 'true',
        },
      })
    }
    case 'hermes':
      // Same profile as `hermes gateway run`; umask 002 keeps personal
      // files group-writable for the catalog sync uid (pods.go).
      process.umask(0o002)
      return startACP({
        name: 'hermes acp', command: '/opt/hermes/.venv/bin/hermes', args: ['acp'], cwd,
        mapPath: join(home, 'hermes-acp-sessions.json'),
        env: { ...process.env, HERMES_HOME: home },
      })
    default:
      throw new Error(`no ACP command for runtime ${runtime}`)
  }
}

// migratePiSessions hands chats of the native pi path (pi.mjs: the newest
// session file in <home>/pi-sessions/<key>/) to pi-acp, so they continue with
// their history: an entry in pi-acp's session map plus the chat map entry
// that session/load then reattaches. Chats already mapped are left alone.
function migratePiSessions(home, cwd, piAcpDir) {
  const mapPath = join(home, 'pi-acp-sessions.json')
  const storePath = join(piAcpDir, 'session-map.json')
  let map = {}
  let store = { version: 1, sessions: {} }
  try {
    map = JSON.parse(readFileSync(mapPath, 'utf8'))
  } catch {}
  try {
    store = JSON.parse(readFileSync(storePath, 'utf8'))
  } catch {}
  let keys = []
  try {
    keys = readdirSync(join(home, 'pi-sessions'))
  } catch {
    return
  }
  let changed = false
  for (const key of keys) {
    if (map[key]) continue
    const dir = join(home, 'pi-sessions', key)
    let files = []
    try {
      files = readdirSync(dir).filter((f) => f.endsWith('.jsonl'))
    } catch {
      continue
    }
    const newest = files.map((f) => join(dir, f)).sort((a, b) => statSync(b).mtimeMs - statSync(a).mtimeMs)[0]
    if (!newest) continue
    const sessionId = `native-${key}`
    store.sessions[sessionId] = { sessionId, cwd, sessionFile: newest, updatedAt: new Date().toISOString() }
    map[key] = sessionId
    changed = true
  }
  if (!changed) return
  writeFileSync(storePath, JSON.stringify(store, null, 2) + '\n')
  writeFileSync(mapPath + '.tmp', JSON.stringify(map))
  renameSync(mapPath + '.tmp', mapPath)
}
