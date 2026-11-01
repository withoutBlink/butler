// Butler relay plugin for DeepSeek Harness (dsh) — the DSH counterpart of
// plugins/opencode. Runs inside a long-lived dsh Host (`dsh web`).
//
// What it does (v1):
//   - Acts as the Butler identity that butler-mcp (spawned by DSH's own
//     @deepseek-ai/dsh-mcp-client) registered: it reads the live instance
//     token from butler-mcp's state file and re-reads it on 401, so the relay
//     and the MCP tools always share one live identity (registration rotates
//     the token; butler-mcp rewrites the file atomically).
//   - Long-polls GET /api/v1/wait (same endpoint as the OpenCode plugin and
//     the codex relay). Messages that @-mention this agent — including the
//     task notices the board posts on assignment/handoff/reject/comment — are
//     turned into a short POINTER prompt ("[butler] new messages in <title>
//     (<sid>)" + task ids + how to act) and queued into ONE dedicated DSH
//     Session ("Butler bridge") with agent.followup(). FYI traffic (no mention)
//     and system notices are not relayed; the agent reads them on demand.
//   - Dedupe: at most one queued pointer per Butler session. A pointer counts
//     as queued from followup() until DSH reports it left the inbox
//     (`agent/inbox/claimed` = its turn started, `agent/inbox/discarded`).
//     Fails open: anything not observed counts as not queued.
//   - Startup sweep: open tasks the agent already owns (todo/in_progress)
//     get one pointer, so work assigned while dsh was down is not lost.
//   - The wait cursor persists in a small state file and is clamped to the
//     server's latest seq (a `butler reset` restarts the stream at 0).
//
// Deferred vs the OpenCode plugin: per-DSH-session Butler roles (sibling
// registration with client_session + title renames), tool shadowing per
// session, and an "active session" target — v1 has one identity and one
// bridge Session.
//
// No DSH package imports: the plugin is loaded by absolute path from outside
// the dsh install, so it only uses the injected services and node: builtins.

import { mkdirSync, readFileSync, renameSync, writeFileSync } from 'node:fs'
import { randomUUID } from 'node:crypto'
import * as path from 'node:path'
import * as os from 'node:os'

export const name = 'butler-relay'
export const inject = ['agents', 'sessionController', 'sessionTitle', 'permissionPresets']

interface Config {
  /** butler-mcp state file (agent, token, server). */
  stateFile: string
  /** Where this plugin keeps its bridge Session id and wait cursor. */
  relayStateFile: string
  /** Working directory of the bridge Session. */
  cwd: string
  /** Title of the bridge Session in the DSH UI. */
  title: string
  /** DSH permission preset applied to the bridge Session (must not ask: nobody is there to approve). */
  permissionPreset: string
  /** Long-poll timeout in seconds. */
  waitTimeoutSecs: number
}

interface Instance { agent: string, token: string, server: string }
interface ButlerMessage {
  seq: number
  session_id: string
  sender_id: string
  kind: string
  content: string
  mentions?: string[] | null
}

const MAX_BACKOFF_MS = 60_000
const SEQ_RECHECK_POLLS = 10
const TASK_RE = /task "([^"\n]*)" \(([0-9a-f-]{36})\)/

const home = os.homedir()
const DEFAULTS: Config = {
  stateFile: path.join(home, '.config/butler/dsh/deepseek-instance.json'),
  relayStateFile: path.join(home, '.config/butler/dsh/relay-state.json'),
  cwd: '/workspace/agents/deepseek',
  title: 'Butler bridge',
  permissionPreset: 'butler-auto',
  waitTimeoutSecs: 25,
}

const sleep = (ms: number, signal: AbortSignal) => new Promise<void>(resolve => {
  const t = setTimeout(resolve, ms)
  signal.addEventListener('abort', () => { clearTimeout(t); resolve() }, { once: true })
})

function deepFreeze<T>(o: T): T {
  if (o && typeof o === 'object') {
    for (const v of Object.values(o as object)) deepFreeze(v)
    Object.freeze(o)
  }
  return o
}

/** Same shape @deepseek-ai/dsh-llm createUserMessage() produces. */
function userMessage(text: string, summary: string) {
  return deepFreeze({
    role: 'user' as const,
    content: [{ type: 'text' as const, text }],
    source: { kind: 'butler', form: 'notice' as const, summary: summary.slice(0, 120) },
    id: randomUUID(),
  })
}

export function apply(ctx: any, rawConfig?: Partial<Config>) {
  const config: Config = { ...DEFAULTS, ...(rawConfig ?? {}) }
  const log = (msg: string) => ctx.logger?.info ? ctx.logger.info(`[butler] ${msg}`) : console.error(`[butler] ${msg}`)
  const warn = (msg: string) => ctx.logger?.warn ? ctx.logger.warn(`[butler] ${msg}`) : console.error(`[butler] ${msg}`)

  const controller = new AbortController()
  const signal = controller.signal
  ctx.effect(() => () => controller.abort())

  // ---- identity (butler-mcp state file) ----
  let inst: Instance | undefined
  const readInstance = (): Instance | undefined => {
    try {
      const j = JSON.parse(readFileSync(config.stateFile, 'utf8'))
      if (j?.agent && j?.token && j?.server) return { agent: j.agent, token: j.token, server: j.server }
    } catch { /* not written yet */ }
    return undefined
  }
  const butler = async (p: string, init: RequestInit = {}): Promise<Response> => {
    for (let attempt = 0; ; attempt++) {
      if (!inst) inst = readInstance()
      if (!inst) throw new Error('no butler-mcp state file yet')
      const res = await fetch(inst.server.replace(/\/$/, '') + p, {
        ...init,
        signal,
        headers: { Authorization: `Bearer ${inst.token}`, 'Content-Type': 'application/json', ...(init.headers ?? {}) },
      })
      if (res.status !== 401 || attempt > 0) return res
      // Token rotated (butler-mcp re-registered): follow the state file.
      const fresh = readInstance()
      if (!fresh || fresh.token === inst.token) return res
      inst = fresh
      log(`token rotated; following state file as ${inst.agent}`)
    }
  }

  // ---- relay state (bridge session id, cursor) ----
  let relayState: { bridgeSessionId?: string, cursor?: number } = {}
  try { relayState = JSON.parse(readFileSync(config.relayStateFile, 'utf8')) } catch { /* first run */ }
  const saveRelayState = () => {
    try {
      mkdirSync(path.dirname(config.relayStateFile), { recursive: true, mode: 0o700 })
      const tmp = `${config.relayStateFile}.tmp-${process.pid}`
      writeFileSync(tmp, JSON.stringify(relayState, null, 2), { mode: 0o600 })
      renameSync(tmp, config.relayStateFile)
    } catch (err) { warn(`could not save relay state: ${String(err)}`) }
  }

  // ---- bridge Session ----
  let bridge: any
  const resolveBridge = async (): Promise<any> => {
    if (bridge && !bridge.disposed) {
      const live = ctx.agents.get(bridge.session.id)
      if (live) return (bridge = live)
    }
    if (relayState.bridgeSessionId) {
      const r = await ctx.sessionController.resolveAgent(relayState.bridgeSessionId)
      if (r?.agent) { bridge = r.agent; await attachToWorkspace(relayState.bridgeSessionId); return bridge }
      warn(`bridge Session ${relayState.bridgeSessionId} not resumable (${JSON.stringify(r?.error ?? null)}); creating a new one`)
    }
    const created = await ctx.sessionController.create({ cwd: config.cwd })
    const r = await ctx.sessionController.resolveAgent(created.sessionId)
    if (!r?.agent) throw new Error(`cannot resolve new bridge Session: ${JSON.stringify(r?.error ?? null)}`)
    bridge = r.agent
    relayState.bridgeSessionId = String(created.sessionId)
    saveRelayState()
    try { ctx.sessionTitle.rename(bridge.session, config.title) } catch (err) { warn(`rename failed: ${String(err)}`) }
    await attachToWorkspace(relayState.bridgeSessionId)
    log(`created bridge Session ${relayState.bridgeSessionId}`)
    return bridge
  }
  // Sessions created by a plugin are not members of any Workspace, so the web
  // UI sidebar would not list the bridge. Attach it to the Workspace for cwd.
  const attachToWorkspace = async (sessionId: string) => {
    const registry = ctx.get?.('workspaceRegistry')
    if (!registry) return
    try {
      const ws = (await registry.resolveByPath(config.cwd)) ?? (await registry.create(config.cwd))
      if (!ws.sessionIds.includes(sessionId)) {
        await ws.attachSession(sessionId)
        log(`attached bridge Session to workspace ${ws.id}`)
      }
    } catch (err) { warn(`could not attach bridge Session to a workspace: ${String(err)}`) }
  }
  const preparePermissions = (agent: any) => {
    try { ctx.permissionPresets.set(agent.session, config.permissionPreset) } catch (err) {
      warn(`permission preset ${config.permissionPreset} not applied: ${String(err)}`)
    }
  }

  // ---- dedupe: one queued pointer per key until DSH takes it out of the inbox ----
  const queued = new Map<string, string>() // key (butler sid / "sweep") -> DSH message id
  const settle = ({ message }: { message?: { id?: string } }) => {
    if (!message?.id) return
    for (const [key, id] of queued) if (id === message.id) queued.delete(key)
  }
  ctx.on('agent/inbox/claimed', settle)
  ctx.on('agent/inbox/discarded', settle)

  const rules = (me: string) => [
    `How to act (you are the Butler agent ${me} on Andrew's agent team — your current display name is whatever Butler lists for that id; use the butler MCP tools mcp__butler__*):`,
    '- If Andrew or Noodle tells you that you have a new name, call rename_self with exactly that name, then confirm in the same Butler session. Never rename anyone else.',
    '- A message that @-mentions you expects a reply: ack briefly, do it, then answer in that same Butler session with send_message (@mention whoever asked).',
    '- A task you own: update_task_status in_progress, do the work, comment_task with the result/evidence, then handoff_task with a short note. Never set a task done and never approve your own work (the reviewer decides). Only claim a task you already own.',
    '- Need a decision or approval for board work? Ask @Noodle in Butler, not Andrew. Anything that leaves the team (messages to other people, public posts, spending, deletions, Andrew\'s accounts) needs Andrew: do not do it, ask @Noodle to escalate.',
    'Relayed automatically by the Butler relay plugin; no human typed this.',
  ].join('\n')

  const inject = async (key: string, text: string, summary: string) => {
    if (queued.has(key)) { log(`pointer for ${key} still queued; skipped`); return }
    const agent = await resolveBridge()
    preparePermissions(agent)
    const msg = userMessage(text, summary)
    queued.set(key, msg.id)
    try {
      agent.followup(msg)
      log(`queued pointer for ${key} into ${agent.session.id}`)
    } catch (err) {
      queued.delete(key)
      throw err
    }
  }

  const titleCache = new Map<string, string>()
  const sessionTitle = async (sid: string): Promise<string> => {
    if (titleCache.has(sid)) return titleCache.get(sid)!
    try {
      const res = await butler(`/api/v1/sessions/${encodeURIComponent(sid)}`)
      if (res.ok) {
        const j: any = await res.json()
        const t = j?.title ?? j?.session?.title ?? sid
        titleCache.set(sid, t)
        return t
      }
    } catch { /* fall through */ }
    return sid
  }

  const relay = async (msgs: ButlerMessage[]) => {
    const me = inst!.agent
    const bySession = new Map<string, ButlerMessage[]>()
    for (const m of msgs) bySession.set(m.session_id, [...(bySession.get(m.session_id) ?? []), m])
    for (const [sid, list] of bySession) {
      const title = await sessionTitle(sid)
      const tasks = new Map<string, string>()
      for (const m of list) {
        const t = TASK_RE.exec(m.content)
        if (t) tasks.set(t[2], t[1])
      }
      const lines = [`[butler] new messages in "${title}" (${sid}): ${list.length} @-mention you.`]
      if (tasks.size > 0) {
        lines.push(`Task notices: ${[...tasks].map(([id, t]) => `${id} "${t}"`).join('; ')} — read each with get_task({task: "<id>"}).`)
      }
      lines.push(`Read the messages with read_session({session: "${sid}"}) (or check_messages for every session).`)
      lines.push(rules(me))
      await inject(sid, lines.join('\n'), `Butler: ${list.length} mention(s) in ${title}`)
    }
  }

  const sweep = async () => {
    const res = await butler('/api/v1/tasks?assignee=me')
    if (!res.ok) { warn(`task sweep: HTTP ${res.status}`); return }
    const j: any = await res.json()
    const open = (j?.tasks ?? []).filter((t: any) => t.status === 'todo' || t.status === 'in_progress')
    if (open.length === 0) return
    const text = [
      `[butler] you own ${open.length} open task(s) on the Butler board:`,
      ...open.map((t: any) => `- ${t.id} [${t.status}] "${t.title}"`),
      'Read each with get_task before acting.',
      rules(inst!.agent),
    ].join('\n')
    await inject('sweep', text, `Butler: ${open.length} open task(s)`)
  }

  // ---- main loop ----
  void (async () => {
    let backoff = 0
    // Wait for butler-mcp to register and write its state file.
    for (;;) {
      if (signal.aborted) return
      inst = readInstance()
      if (inst) break
      await sleep(2_000, signal)
    }
    log(`relaying for ${inst.agent} from ${inst.server}`)
    try { preparePermissions(await resolveBridge()) } catch (err) { warn(`bridge Session not ready yet: ${String(err)}`) }

    let cursor = 0
    let swept = false
    for (;;) {
      if (signal.aborted) return
      try {
        const res = await butler('/api/v1/seq')
        if (!res.ok) throw new Error(`seq: HTTP ${res.status}`)
        const latest = ((await res.json()) as any)?.latest_seq ?? 0
        const saved = relayState.cursor
        cursor = saved !== undefined && saved <= latest ? saved : latest
        if (!swept) { await sweep(); swept = true }
        break
      } catch (err: any) {
        if (signal.aborted) return
        backoff = Math.min(backoff ? backoff * 2 : 2_000, MAX_BACKOFF_MS)
        warn(`startup: ${err?.message ?? err} (retry in ${backoff}ms)`)
        await sleep(backoff, signal)
      }
    }

    backoff = 0
    let idle = 0
    for (;;) {
      if (signal.aborted) return
      try {
        const res = await butler(`/api/v1/wait?after=${cursor}&timeout=${config.waitTimeoutSecs}`)
        if (res.status === 401) { await sleep(2_000, signal); continue }
        if (!res.ok) throw new Error(`wait: HTTP ${res.status}`)
        const body: any = await res.json()
        backoff = 0
        const msgs: ButlerMessage[] = body?.messages ?? []
        const me = inst!.agent
        const mine = msgs.filter(m => m.sender_id !== me && m.kind !== 'system' && (m.mentions ?? []).includes(me))
        if (mine.length > 0) await relay(mine)
        const top = msgs.reduce((a, m) => Math.max(a, m.seq), cursor)
        cursor = Math.max(cursor, top, body?.latest_seq ?? 0)
        if (msgs.length > 0) { relayState.cursor = cursor; saveRelayState(); idle = 0 }
        else if (++idle >= SEQ_RECHECK_POLLS) {
          idle = 0
          const s = await butler('/api/v1/seq')
          if (s.ok) {
            const now = ((await s.json()) as any)?.latest_seq ?? 0
            if (now < cursor) { warn(`server stream restarted (seq ${now} < cursor ${cursor}); replaying from 0`); cursor = 0 }
          }
        }
      } catch (err: any) {
        if (signal.aborted) return
        backoff = Math.min(backoff ? backoff * 2 : 2_000, MAX_BACKOFF_MS)
        warn(`poll error: ${err?.message ?? err} (retry in ${backoff}ms)`)
        await sleep(backoff, signal)
      }
    }
  })()
}
