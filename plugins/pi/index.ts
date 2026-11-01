// Butler relay plugin for the Pi coding agent (@earendil-works/pi-coding-agent).
// The Pi counterpart of plugins/deepseek-harness: push delivery only. MCP
// tools still come from butler-mcp.
//
// v1 is one Butler identity for the Pi process (the identity butler-mcp
// registered), injected into the current Pi session. There is no separate
// bridge session and no per-Pi-session sibling role.
//
// Verified against https://pi.dev/docs/latest/extensions and
// earendil-works/pi main (packages/coding-agent):
//   - default export is `function (pi: ExtensionAPI)`
//   - jiti loads this TypeScript; no build step
//   - do not start background work in the factory
//   - session_start begins the long-poll, session_shutdown stops it
//     (Pi reloads the extension on session switch: shutdown, then a new
//     factory, then session_start)
//   - pi.sendUserMessage(text) starts a turn when the agent is idle
//   - pi.sendUserMessage(text, { deliverAs: "followUp" }) queues while a
//     run is active (AgentSession.prompt → _queueFollowUp). steer is not
//     used: it would cut into the turn in progress
//   - the binding does not return that promise; a rejection is reported by
//     Pi as an extension error. A handed pointer that never shows up in a
//     user prompt is reclaimed once Pi is idle with an empty queue
//   - before_agent_start carries the opening user prompt; message_start
//     fires for a user message when a queued follow-up is taken (the queue
//     entry is removed just before extensions see it)
//   - ctx.isIdle() / ctx.hasPendingMessages() describe the run and the queue
//
// The file uses only erasable TypeScript plus node: builtins. The type
// import below is erased at runtime; Pi maps the package when a typechecker
// is pointed at it.

import { mkdirSync, readFileSync, renameSync, writeFileSync } from "node:fs"
import * as path from "node:path"
import * as os from "node:os"
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent"

import {
  HANDOFF_GRACE_MS,
  SWEEP_KEY,
  initialCursor,
  loadConfig,
  mentionPointer,
  note,
  planDelivery,
  settle,
  sweepPointer,
  userPromptText,
  type AgentCtx,
  type PlannedSend,
  type PluginConfig,
  type Slot,
} from "./relay.ts"

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

export interface PiSender {
  sendUserMessage(content: string, options?: { deliverAs?: "steer" | "followUp", expandPromptTemplates?: boolean }): void
}

export interface Relay {
  stop(): void
  /** before_agent_start: a run is active, so queued pointers can follow up. */
  onRunStart(ctx: AgentCtx, event?: { messageType?: string, prompt?: string }): void
  /** agent_settled: the run will not continue on its own. */
  onSettled(ctx: AgentCtx): void
  /** message_start: a user message (including a delivered follow-up) began. */
  onMessageStart(message: unknown): void
  flush(): void
}

export interface RelayOptions {
  pi: PiSender
  ctx: AgentCtx
  config?: PluginConfig
  fetch?: typeof fetch
  now?: () => number
  log?: (msg: string) => void
  warn?: (msg: string) => void
}

const sleep = (ms: number, signal: AbortSignal) => new Promise<void>(resolve => {
  const t = setTimeout(resolve, ms)
  signal.addEventListener("abort", () => { clearTimeout(t); resolve() }, { once: true })
})

function readInstance(file: string): Instance | undefined {
  try {
    const j = JSON.parse(readFileSync(file, "utf8"))
    if (j?.agent && j?.token && j?.server) {
      return { agent: String(j.agent), token: String(j.token), server: String(j.server) }
    }
  } catch { /* not written yet, or unreadable */ }
  return undefined
}

export function startRelay(opts: RelayOptions): Relay {
  const config = opts.config ?? loadConfig(os.homedir()).config
  const fetchImpl = opts.fetch ?? globalThis.fetch
  const now = opts.now ?? Date.now
  const log = opts.log ?? (msg => console.error(`[butler] ${msg}`))
  const warn = opts.warn ?? log
  const pi = opts.pi

  const controller = new AbortController()
  const signal = controller.signal
  let ctx = opts.ctx
  let streaming = false
  let arming = false
  let graceTimer: ReturnType<typeof setTimeout> | undefined
  const slots = new Map<string, Slot>()
  const titles = new Map<string, string>()

  const status = (text: string | undefined) => {
    try { ctx.ui?.setStatus("butler", text) } catch (err) { warn(`status: ${err instanceof Error ? err.message : String(err)}`) }
  }

  const clearGrace = () => {
    if (!graceTimer) return
    clearTimeout(graceTimer)
    graceTimer = undefined
  }
  const scheduleGrace = () => {
    if (graceTimer) return
    graceTimer = setTimeout(() => {
      graceTimer = undefined
      if (!signal.aborted) flush()
    }, HANDOFF_GRACE_MS)
    graceTimer.unref?.()
  }

  let inst: Instance | undefined
  const butler = async (p: string, init: RequestInit = {}): Promise<Response> => {
    for (let attempt = 0; ; attempt++) {
      if (!inst) inst = readInstance(config.stateFile)
      if (!inst) throw new Error("no butler-mcp state file yet")
      const res = await fetchImpl(inst.server.replace(/\/$/, "") + p, {
        ...init,
        signal,
        headers: {
          Authorization: `Bearer ${inst.token}`,
          "Content-Type": "application/json",
          ...(init.headers ?? {}),
        },
      })
      if (res.status !== 401 || attempt > 0) return res
      const fresh = readInstance(config.stateFile)
      if (!fresh || fresh.token === inst.token) return res
      inst = fresh
      log(`token rotated; following state file as ${inst.agent}`)
    }
  }

  let relayState: { cursor?: number } = {}
  try { relayState = JSON.parse(readFileSync(config.relayStateFile, "utf8")) } catch { /* first run */ }
  const saveRelayState = () => {
    try {
      mkdirSync(path.dirname(config.relayStateFile), { recursive: true, mode: 0o700 })
      const tmp = `${config.relayStateFile}.tmp-${process.pid}`
      writeFileSync(tmp, JSON.stringify(relayState), { mode: 0o600 })
      renameSync(tmp, config.relayStateFile)
    } catch (err) { warn(`could not save relay state: ${err instanceof Error ? err.message : String(err)}`) }
  }

  const deliver = (sends: PlannedSend[]) => {
    let nextArming = arming
    for (const send of sends) {
      try {
        if (send.deliverAs) pi.sendUserMessage(send.text, { deliverAs: send.deliverAs })
        else pi.sendUserMessage(send.text)
        log(`queued pointer for ${send.key}${send.deliverAs ? ` (${send.deliverAs})` : ""}`)
      } catch (err) {
        const slot = slots.get(send.key)
        if (slot) { slot.handed = false; slot.handedAt = 0 }
        if (!send.deliverAs) nextArming = false
        warn(`inject ${send.key}: ${err instanceof Error ? err.message : String(err)}`)
      }
    }
    arming = nextArming
    // Re-check soon. A plain send that never became a run, or a followUp Pi
    // rejected (compaction, no model), is still handed; once the session is
    // idle with an empty queue the grace window sends it again. A followUp
    // that is actually queued has hasPendingMessages() and is left alone.
    if (arming || sends.length > 0) scheduleGrace()
  }

  const flush = () => {
    // A run Pi started (the user typed, or our own turn) is busy even if
    // before_agent_start has not reached us yet. followUp waits it out;
    // a plain sendUserMessage would throw "already processing".
    if (!streaming && !ctx.isIdle()) streaming = true
    // before_agent_start sets streaming, and _startRun has already marked the
    // session busy by then. If the flag is still set once Pi is idle with
    // nothing queued, the run ended without agent_settled (prepare failed)
    // and followUp would no longer be queued behind a real run.
    if (streaming && ctx.isIdle() && !ctx.hasPendingMessages()) streaming = false
    const plan = planDelivery(slots, now(), {
      streaming,
      arming,
      idle: ctx.isIdle(),
      pendingMessages: ctx.hasPendingMessages(),
      reclaimHanded: false,
    })
    arming = plan.arming
    deliver(plan.sends)
  }

  const sessionTitle = async (sid: string): Promise<string> => {
    const cached = titles.get(sid)
    if (cached) return cached
    try {
      const res = await butler(`/api/v1/sessions/${encodeURIComponent(sid)}`)
      if (res.ok) {
        const j = await res.json() as { title?: string, session?: { title?: string } }
        const title = j?.title || j?.session?.title || sid
        titles.set(sid, title)
        return title
      }
    } catch (err) {
      if (signal.aborted) throw err
    }
    return sid
  }

  const enqueue = (key: string, text: string) => {
    if (note(slots, key, text) === "skipped") {
      log(`pointer for ${key} still queued; skipped`)
      return
    }
    flush()
  }

  const relayMessages = async (msgs: ButlerMessage[]) => {
    const me = inst!.agent
    const bySession = new Map<string, ButlerMessage[]>()
    for (const m of msgs) bySession.set(m.session_id, [...(bySession.get(m.session_id) ?? []), m])
    for (const [sid, list] of bySession) {
      const title = await sessionTitle(sid)
      enqueue(sid, mentionPointer(me, sid, title, list.map(m => m.content)))
    }
  }

  const sweep = async () => {
    const res = await butler("/api/v1/tasks?assignee=me")
    if (!res.ok) { warn(`task sweep: HTTP ${res.status}`); return }
    const j = await res.json() as { tasks?: { id?: string, status?: string, title?: string }[] }
    const open = (j?.tasks ?? []).filter(t => t && (t.status === "todo" || t.status === "in_progress") && t.id)
    if (open.length === 0) return
    enqueue(SWEEP_KEY, sweepPointer(inst!.agent, open.map(t => ({
      id: String(t.id),
      status: String(t.status),
      title: String(t.title ?? ""),
    }))))
  }

  void (async () => {
    let backoff = 0
    status("butler: waiting")
    for (;;) {
      if (signal.aborted) return
      inst = readInstance(config.stateFile)
      if (inst) break
      await sleep(2_000, signal)
    }
    if (signal.aborted || !inst) return
    log(`relaying for ${inst.agent} from ${inst.server}`)
    status("butler: live")

    let cursor = 0
    let swept = false
    for (;;) {
      if (signal.aborted) return
      try {
        const res = await butler("/api/v1/seq")
        if (!res.ok) throw new Error(`seq: HTTP ${res.status}`)
        const latest = Number((await res.json() as { latest_seq?: number })?.latest_seq ?? 0)
        const saved = relayState.cursor
        cursor = initialCursor(saved, latest)
        if (saved !== undefined && saved > latest) {
          warn(`server stream restarted (saved cursor ${saved} > seq ${latest}); replaying from 0`)
          relayState.cursor = 0
          saveRelayState()
        }
        if (config.sweep && !swept) { await sweep(); swept = true }
        break
      } catch (err) {
        if (signal.aborted) return
        backoff = Math.min(backoff ? backoff * 2 : 2_000, MAX_BACKOFF_MS)
        warn(`startup: ${err instanceof Error ? err.message : String(err)} (retry in ${backoff}ms)`)
        await sleep(backoff, signal)
      }
    }

    backoff = 0
    let idlePolls = 0
    for (;;) {
      if (signal.aborted) return
      try {
        const res = await butler(`/api/v1/wait?after=${cursor}&timeout=${config.waitTimeoutSecs}`)
        if (res.status === 401) { await sleep(2_000, signal); continue }
        if (!res.ok) throw new Error(`wait: HTTP ${res.status}`)
        const body = await res.json() as { messages?: ButlerMessage[], latest_seq?: number }
        backoff = 0
        const msgs = body?.messages ?? []
        const me = inst!.agent
        const mine = msgs.filter(m => m.sender_id !== me && m.kind !== "system" && (m.mentions ?? []).includes(me))
        if (mine.length > 0) await relayMessages(mine)
        const top = msgs.reduce((a, m) => Math.max(a, m.seq), cursor)
        cursor = Math.max(cursor, top, body?.latest_seq ?? 0)
        if (msgs.length > 0) { relayState.cursor = cursor; saveRelayState(); idlePolls = 0 }
        else if (++idlePolls >= SEQ_RECHECK_POLLS) {
          idlePolls = 0
          const seqRes = await butler("/api/v1/seq")
          if (seqRes.ok) {
            const seqNow = Number((await seqRes.json() as { latest_seq?: number })?.latest_seq ?? 0)
            if (seqNow < cursor) {
              warn(`server stream restarted (seq ${seqNow} < cursor ${cursor}); replaying from 0`)
              cursor = 0
              relayState.cursor = 0
              saveRelayState()
            }
          }
        }
        flush()
      } catch (err) {
        if (signal.aborted || (err instanceof Error && err.name === "AbortError")) return
        backoff = Math.min(backoff ? backoff * 2 : 2_000, MAX_BACKOFF_MS)
        warn(`poll error: ${err instanceof Error ? err.message : String(err)} (retry in ${backoff}ms)`)
        status("butler: retrying")
        await sleep(backoff, signal)
        if (!signal.aborted) status("butler: live")
      }
    }
  })()

  return {
    stop() {
      if (signal.aborted) return
      controller.abort()
      clearGrace()
      status(undefined)
    },
    onRunStart(next, event) {
      ctx = next
      streaming = true
      arming = false
      if (event?.messageType === "user" && event.prompt) settle(slots, event.prompt)
      flush()
    },
    onSettled(next) {
      ctx = next
      streaming = false
      arming = false
      const idle = ctx.isIdle()
      const pendingMessages = ctx.hasPendingMessages()
      const plan = planDelivery(slots, now(), {
        streaming: false,
        arming: false,
        idle,
        pendingMessages,
        reclaimHanded: idle && !pendingMessages,
      })
      arming = plan.arming
      deliver(plan.sends)
    },
    onMessageStart(message) {
      const text = userPromptText(message)
      if (text) settle(slots, text)
    },
    flush,
  }
}

const loaded = loadConfig(os.homedir())

export default function (pi: ExtensionAPI) {
  if (loaded.warning) console.error(`[butler] ${loaded.warning}`)
  let relay: Relay | undefined
  pi.on("session_start", (_event, ctx) => {
    relay?.stop()
    relay = startRelay({ pi, ctx, config: loaded.config })
  })
  pi.on("session_shutdown", () => {
    relay?.stop()
    relay = undefined
  })
  pi.on("before_agent_start", (event, ctx) => {
    const opening = event.messageType === "user" ? event.prompt : undefined
    relay?.onRunStart(ctx, opening ? { messageType: "user", prompt: opening } : undefined)
  })
  pi.on("agent_settled", (_event, ctx) => {
    relay?.onSettled(ctx)
  })
  pi.on("message_start", event => {
    relay?.onMessageStart(event.message)
  })
  // Compaction rejects prompt() until it finishes. Try the queue again once
  // it has; ctx.isIdle() / hasPendingMessages() read the live session.
  pi.on("session_compact", () => {
    relay?.flush()
  })
  pi.on("session_compact_failed", () => {
    relay?.flush()
  })
}
