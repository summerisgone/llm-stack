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
import { chmodSync, mkdirSync } from 'node:fs'
import { join } from 'node:path'
import { createInterface } from 'node:readline'
import { startACP } from './acp.mjs'

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
  // Readiness waits for the token too: the broker exchanges it on the first
  // web request, which on a cold start comes right after /health turns ok.
  let tokenSeen
  const tokenReady = new Promise((resolve) => { tokenSeen = resolve })
  const webArgs = ['web', '--patch', join(dshHome, 'web.patch.yml'), '--no-open']
  if (process.env.DSH_WEB_HOST) webArgs.push('--trusted-host', process.env.DSH_WEB_HOST)
  const web = spawn('dsh', webArgs, { cwd, env, stdio: ['ignore', 'pipe', 'inherit'] })
  web.on('exit', exit('dsh web'))
  createInterface({ input: web.stdout }).on('line', (line) => {
    const m = line.match(/[?&]token=([^&\s)]+)/)
    if (m) {
      webToken = decodeURIComponent(m[1])
      tokenSeen()
    }
    console.error(line.replace(/token=[^&\s)]+/g, 'token=***'))
  })

  const acp = await startACP({
    name: 'dsh acp', command: 'dsh', args: ['--profile', 'acp', '--patch', join(dshHome, 'acp.patch.yml')],
    cwd, env, mapPath: join(home, 'dsh-sessions.json'),
  })
  await tokenReady

  return {
    ...acp,
    webToken: () => webToken,
    async stop() {
      await acp.stop()
      web.kill('SIGTERM')
    },
  }
}

