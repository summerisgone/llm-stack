// agent-adapter puts the broker's agent contract (agent-broker/cmd/
// agent-broker/backend.go) in front of agents without an OpenAI-compatible
// server of their own: POST /v1/chat/completions (bearer API_SERVER_KEY) and
// GET /health on AGENT_PORT. AGENT_RUNTIME picks the backend (pi.mjs,
// opencode.mjs, dsh.mjs). GET /web-token hands dsh web's launch token to the
// broker's web proxy. Open WebUI resends the whole conversation on every turn;
// the agent keeps its own session instead, one per Open WebUI chat
// (X-OpenWebUI-Chat-Id, forwarded by the broker), and only the last user
// message is sent to it.
//
// Runtimes whose backend is `interactive` (dsh) also serve the broker's
// interaction protocol for the Open WebUI Pipe (docs/adr/0021). It is not
// ACP; ACP stays between this process and the agent on stdio.
//   POST /v1/agent/turns {chat_id, users: [user texts, last is the prompt]}
//     -> application/x-ndjson events, one turn: turn, notice, text,
//        reasoning, tool, permission, done | error. Closing the response
//        cancels the turn, including a pending permission request.
//   POST /v1/agent/permissions {turn, request, allow} -> 200, or 404 when
//     that request is not pending (answered, timed out, other turn).
// The chat's history must extend what the agent session has received: the
// prompts it got must be a subsequence of the earlier user messages. An
// edit, regenerate or branch breaks that and is refused rather than
// appended to the session. Prompt hashes per chat live in historyPath.
import { createHash, randomUUID, timingSafeEqual } from 'node:crypto'
import { readFileSync, renameSync, writeFileSync } from 'node:fs'
import http from 'node:http'

export function lastUserText(messages) {
  for (let i = messages.length - 1; i >= 0; i--) {
    if (messages[i].role === 'user') return contentText(messages[i].content)
  }
  return ''
}

function contentText(content) {
  if (typeof content === 'string') return content
  if (Array.isArray(content)) return content.filter((p) => p.type === 'text').map((p) => p.text).join('')
  return ''
}

// sessionKey names the agent session for a request: the Open WebUI chat id,
// else a hash of the chat's first user message (clients without the header).
export function sessionKey(headers, messages) {
  const chat = headers['x-openwebui-chat-id']
  if (chat && /^[A-Za-z0-9_-]{1,128}$/.test(chat)) return chat
  const first = messages.find((m) => m.role === 'user')
  return 'h-' + createHash('sha256').update(first ? contentText(first.content) : '').digest('hex').slice(0, 32)
}

export function chatKey(chatID, users) {
  if (typeof chatID === 'string' && /^[A-Za-z0-9_-]{1,128}$/.test(chatID)) return chatID
  return 'h-' + createHash('sha256').update(users[0] || '').digest('hex').slice(0, 32)
}

const promptHash = (text) => createHash('sha256').update(text).digest('hex').slice(0, 16)

// continues reports whether the earlier user messages contain, in order,
// every prompt the agent session has already received.
export function continues(received, earlier) {
  let i = 0
  for (const h of earlier.map(promptHash)) {
    if (i < received.length && received[i] === h) i++
  }
  return i === received.length
}

function authorized(header, key) {
  const want = Buffer.from('Bearer ' + key)
  const got = Buffer.from(header || '')
  return got.length === want.length && timingSafeEqual(got, want)
}

function sendJSON(res, status, body) {
  res.writeHead(status, { 'Content-Type': 'application/json' })
  res.end(JSON.stringify(body))
}

function openAIError(res, status, type, message) {
  sendJSON(res, status, { error: { type, message } })
}

async function readJSON(req) {
  const chunks = []
  let size = 0
  for await (const chunk of req) {
    size += chunk.length
    if (size > 32 << 20) throw new Error('body too large')
    chunks.push(chunk)
  }
  return JSON.parse(Buffer.concat(chunks).toString('utf8'))
}

// createServer serves the contract for backend: { prompt(key, text,
// { onText, onReasoning, signal }) -> Promise<final text>, webToken?() }. ready() gates
// /health and requests while the backend starts.
export function createServer({ backend, apiKey, model, ready = () => true, historyPath = null,
  permissionTimeout = 300_000 }) {
  let history = {}
  if (historyPath) {
    try {
      history = JSON.parse(readFileSync(historyPath, 'utf8'))
    } catch {}
  }
  const saveHistory = () => {
    if (!historyPath) return
    writeFileSync(historyPath + '.tmp', JSON.stringify(history))
    renameSync(historyPath + '.tmp', historyPath)
  }
  // turn/request -> resolve(decision) for permission requests awaiting the user.
  const asks = new Map()

  // One turn at a time per session; a second message waits for the first.
  const queues = new Map()
  const serialize = (key, fn) => {
    const prev = queues.get(key) || Promise.resolve()
    const run = prev.then(fn)
    const tail = run.catch(() => {}).finally(() => {
      if (queues.get(key) === tail) queues.delete(key)
    })
    queues.set(key, tail)
    return run
  }

  async function turn(req, res, key, users) {
    const id = randomUUID()
    const abort = new AbortController()
    res.on('close', () => {
      if (!res.writableFinished) abort.abort()
    })
    res.writeHead(200, { 'Content-Type': 'application/x-ndjson', 'Cache-Control': 'no-cache', 'X-Accel-Buffering': 'no' })
    const event = (e) => !res.writableEnded && !res.destroyed && res.write(JSON.stringify(e) + '\n')
    event({ type: 'turn', turn: id })
    let asked = 0
    const mine = new Set()
    abort.signal.addEventListener('abort', () => {
      for (const k of mine) asks.get(k)?.('cancel')
    })
    try {
      await serialize(key, async () => {
        if (abort.signal.aborted) return
        const received = history[key] || []
        const earlier = users.slice(0, -1)
        if (!continues(received, earlier)) {
          event({ type: 'error', message: 'This agent chat cannot edit, regenerate or branch from an earlier message: the agent has already acted on it. Continue from the latest message or start a new chat.' })
          return
        }
        if (!received.length && earlier.length && !backend.hasSession?.(key)) {
          event({ type: 'notice', text: 'The agent starts a new session here; earlier messages in this chat are not part of its context.' })
        }
        history[key] = [...received, promptHash(users.at(-1))]
        saveHistory()
        let stop = null
        await backend.prompt(key, users.at(-1), {
          signal: abort.signal,
          onText: (text) => event({ type: 'text', text }),
          onReasoning: (text) => event({ type: 'reasoning', text }),
          onTool: (t) => event({ type: 'tool', ...t }),
          onStop: (s) => { stop = s },
          onPermission: ({ title, text }) => new Promise((resolve) => {
            if (abort.signal.aborted) return resolve('cancel')
            const request = String(++asked)
            const k = `${id}/${request}`
            const timer = setTimeout(() => done('deny'), permissionTimeout)
            const done = (decision) => {
              clearTimeout(timer)
              asks.delete(k)
              mine.delete(k)
              resolve(decision)
            }
            asks.set(k, done)
            mine.add(k)
            event({ type: 'permission', turn: id, request, title, text })
          }),
        })
        event({ type: 'done', stop: abort.signal.aborted ? 'cancelled' : stop || 'end_turn' })
      })
    } catch (err) {
      if (!abort.signal.aborted) {
        console.error(JSON.stringify({ msg: 'agent turn failed', session: key, err: String(err?.message || err) }))
        event({ type: 'error', message: String(err?.message || err) })
      }
    }
    res.end()
  }

  return http.createServer(async (req, res) => {
    const path = new URL(req.url, 'http://adapter').pathname
    if (req.method === 'GET' && path === '/health') {
      return ready() ? sendJSON(res, 200, { status: 'ok' }) : sendJSON(res, 503, { status: 'starting' })
    }
    if (!authorized(req.headers.authorization, apiKey)) {
      return openAIError(res, 401, 'unauthorized', 'invalid API key')
    }
    if (req.method === 'GET' && path === '/v1/models') {
      return sendJSON(res, 200, { object: 'list', data: [{ id: model, object: 'model', owned_by: 'agent-adapter' }] })
    }
    if (req.method === 'GET' && path === '/web-token') {
      const token = backend.webToken?.()
      return token ? sendJSON(res, 200, { token }) : openAIError(res, 404, 'not_found', 'no web UI token')
    }
    if (req.method === 'POST' && path === '/v1/agent/permissions' && backend.interactive) {
      let body
      try {
        body = await readJSON(req)
      } catch {
        return openAIError(res, 400, 'invalid_request_error', 'body is not JSON')
      }
      const resolve = asks.get(`${body.turn}/${body.request}`)
      if (!resolve) return openAIError(res, 404, 'not_found', 'no such pending permission request')
      resolve(body.allow === true ? 'allow' : 'deny')
      return sendJSON(res, 200, { ok: true })
    }
    if (req.method === 'POST' && path === '/v1/agent/turns' && backend.interactive) {
      if (!ready()) return openAIError(res, 503, 'agent_unavailable', 'the agent is starting')
      let body
      try {
        body = await readJSON(req)
      } catch {
        return openAIError(res, 400, 'invalid_request_error', 'body is not JSON')
      }
      const users = Array.isArray(body.users) ? body.users.filter((u) => typeof u === 'string') : []
      if (!users.length || !users.at(-1).trim()) return openAIError(res, 400, 'invalid_request_error', 'no user message')
      return turn(req, res, chatKey(body.chat_id, users), users)
    }
    if (req.method !== 'POST' || path !== '/v1/chat/completions') {
      return openAIError(res, 404, 'not_found', 'not found')
    }
    if (!ready()) return openAIError(res, 503, 'agent_unavailable', 'the agent is starting')

    let body
    try {
      body = await readJSON(req)
    } catch {
      return openAIError(res, 400, 'invalid_request_error', 'body is not a chat completion request')
    }
    const messages = Array.isArray(body.messages) ? body.messages : []
    const text = lastUserText(messages)
    if (!text.trim()) return openAIError(res, 400, 'invalid_request_error', 'no user message')

    const id = `chatcmpl-adapter-${Date.now()}`
    const created = Math.floor(Date.now() / 1000)
    const abort = new AbortController()
    res.on('close', () => {
      if (!res.writableFinished) abort.abort()
    })

    const stream = body.stream === true
    const chunk = (delta, finish = null) => {
      res.write(`data: ${JSON.stringify({ id, object: 'chat.completion.chunk', created, model, choices: [{ index: 0, delta, finish_reason: finish }] })}\n\n`)
    }
    if (stream) {
      res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache', 'X-Accel-Buffering': 'no' })
      chunk({ role: 'assistant', content: '' })
    }
    const key = sessionKey(req.headers, messages)
    let streamed = false
    try {
      const final = await serialize(key, () => backend.prompt(key, text, {
        signal: abort.signal,
        onText: (t) => {
          streamed = true
          if (stream) chunk({ content: t })
        },
        onReasoning: (t) => stream && chunk({ reasoning_content: t }),
      }))
      if (stream) {
        // A backend that reported no deltas still has the final text.
        if (!streamed && final) chunk({ content: final })
        chunk({}, 'stop')
        res.end('data: [DONE]\n\n')
      } else {
        sendJSON(res, 200, {
          id, object: 'chat.completion', created, model,
          choices: [{ index: 0, finish_reason: 'stop', message: { role: 'assistant', content: final } }],
        })
      }
    } catch (err) {
      if (abort.signal.aborted) return
      console.error(JSON.stringify({ msg: 'agent turn failed', session: key, err: String(err?.message || err) }))
      if (stream) {
        chunk({ content: `\n\nAgent error: ${err?.message || err}` }, 'stop')
        res.end('data: [DONE]\n\n')
      } else {
        openAIError(res, 502, 'agent_error', String(err?.message || err))
      }
    }
  })
}

async function main() {
  const runtime = process.env.AGENT_RUNTIME
  const home = process.env.AGENT_HOME || '/opt/data/home'
  const apiKey = process.env.API_SERVER_KEY
  if (!apiKey || !['pi', 'opencode', 'dsh'].includes(runtime)) {
    console.error('API_SERVER_KEY and AGENT_RUNTIME=pi|opencode|dsh are required')
    process.exit(1)
  }
  let backend = null
  const server = createServer({
    apiKey,
    historyPath: `${home}/agent-turns.json`,
    permissionTimeout: Number(process.env.PERMISSION_TIMEOUT_SECONDS || 300) * 1000,
    model: process.env.AGENT_MODEL_NAME || `${runtime}-agent`,
    ready: () => backend !== null,
    backend: {
      prompt: (...args) => backend.prompt(...args),
      interactive: runtime === 'dsh',
      hasSession: (key) => backend?.hasSession?.(key),
      webToken: () => backend?.webToken?.(),
    },
  })
  server.listen(Number(process.env.AGENT_PORT || 8642), '0.0.0.0')
  const mod = await import(`./${runtime}.mjs`)
  backend = await mod.start({ home, cwd: process.env.AGENT_CWD || '/work' })
  console.log(JSON.stringify({ msg: 'agent-adapter ready', runtime }))
  const stop = async () => {
    server.close()
    await backend.stop?.()
    process.exit(0)
  }
  process.on('SIGTERM', stop)
  process.on('SIGINT', stop)
}

if (import.meta.url === `file://${process.argv[1]}`) {
  main().catch((err) => {
    console.error(err)
    process.exit(1)
  })
}
