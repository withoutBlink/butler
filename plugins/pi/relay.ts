// Pure delivery logic for the Pi relay. No Pi imports: the extension loader
// (jiti) and `node --test` can both run this file, and the decisions are
// unit-tested without a Pi process.
//
// Pointers are deduped per Butler session until Pi has started handling
// them. "Started" is a user prompt whose text contains that pointer's tag
// (`before_agent_start` for the run that opens on it, `message_start` when
// a queued follow-up is actually taken). While a run is active the next
// pointer is `deliverAs: "followUp"`, which waits until the run finishes
// instead of steering over the work in progress. An idle session gets one
// plain sendUserMessage, which starts a turn; further pointers wait until
// that run is streaming so a second call cannot race the first into
// "already processing".

import { readFileSync } from "node:fs"
import * as path from "node:path"

export const NOTIFY_TAG = "butler-notify:"
export const SWEEP_KEY = "sweep"
export const HANDOFF_GRACE_MS = 2_000
export const STUCK_MS = 5 * 60 * 1000

const TASK_RE = /task "([^"\n]*)" \(([0-9a-f-]{36})\)/g

export interface Slot {
  text: string
  /** sendUserMessage has been called; waiting until a user prompt shows the tag. */
  handed: boolean
  handedAt: number
}

export interface AgentCtx {
  isIdle(): boolean
  hasPendingMessages(): boolean
  ui?: { setStatus(key: string, text: string | undefined): void }
}

export interface PluginConfig {
  /** butler-mcp state file (agent, token, server). */
  stateFile: string
  /** Where this plugin keeps its wait cursor. */
  relayStateFile: string
  /** Long-poll timeout in seconds (butler clamps to 1..60). */
  waitTimeoutSecs: number
  /** One startup pointer for open tasks assigned to this agent. */
  sweep: boolean
}

export interface DeliveryState {
  streaming: boolean
  arming: boolean
  idle: boolean
  pendingMessages: boolean
  /** Drop every handed slot first. Used when a run settled and the queue is empty,
   * so a follow-up Pi cleared on abort is sent again. */
  reclaimHanded: boolean
}

export interface PlannedSend {
  key: string
  text: string
  deliverAs?: "followUp"
}

export function pointerTag(key: string): string {
  return `${NOTIFY_TAG}${key}`
}

export function promptHasTag(prompt: string, key: string): boolean {
  return prompt.split(/\s+/).includes(pointerTag(key))
}

export function expandHome(p: string, home: string): string {
  if (p === "~") return home
  if (p.startsWith("~/")) return path.join(home, p.slice(2))
  return p
}

export function defaultConfig(home: string): PluginConfig {
  const dir = path.join(home, ".config", "butler", "pi")
  return {
    stateFile: path.join(dir, "instance.json"),
    relayStateFile: path.join(dir, "relay-state.json"),
    waitTimeoutSecs: 25,
    sweep: true,
  }
}

function clampTimeout(n: number): number {
  if (!Number.isFinite(n)) return 25
  return Math.min(60, Math.max(1, Math.floor(n)))
}

function applyFile(cfg: PluginConfig, raw: unknown, home: string) {
  if (!raw || typeof raw !== "object") return
  const j = raw as Record<string, unknown>
  if (typeof j.stateFile === "string" && j.stateFile.trim()) cfg.stateFile = expandHome(j.stateFile.trim(), home)
  if (typeof j.relayStateFile === "string" && j.relayStateFile.trim()) cfg.relayStateFile = expandHome(j.relayStateFile.trim(), home)
  if (typeof j.waitTimeoutSecs === "number") cfg.waitTimeoutSecs = clampTimeout(j.waitTimeoutSecs)
  if (typeof j.sweep === "boolean") cfg.sweep = j.sweep
}

function readOptional(file: string): string | undefined {
  try {
    return readFileSync(file, "utf8")
  } catch {
    return undefined
  }
}

/** Defaults, then ~/.config/butler/pi/plugin.json (or BUTLER_PI_CONFIG), then env. */
export function loadConfig(home: string, env: NodeJS.ProcessEnv = process.env): { config: PluginConfig, warning?: string } {
  const config = defaultConfig(home)
  const explicit = env.BUTLER_PI_CONFIG?.trim()
  const file = explicit ? expandHome(explicit, home) : path.join(home, ".config", "butler", "pi", "plugin.json")
  let warning: string | undefined
  const text = readOptional(file)
  if (text === undefined) {
    if (explicit) warning = `config ${file} not found; using defaults`
  } else {
    try {
      applyFile(config, JSON.parse(text), home)
    } catch (err) {
      warning = `config ${file} is not valid JSON (${err instanceof Error ? err.message : String(err)}); using defaults`
    }
  }
  if (env.BUTLER_STATE_FILE?.trim()) config.stateFile = expandHome(env.BUTLER_STATE_FILE.trim(), home)
  if (env.BUTLER_RELAY_STATE_FILE?.trim()) config.relayStateFile = expandHome(env.BUTLER_RELAY_STATE_FILE.trim(), home)
  if (env.BUTLER_WAIT_TIMEOUT?.trim()) config.waitTimeoutSecs = clampTimeout(Number(env.BUTLER_WAIT_TIMEOUT))
  if (env.BUTLER_PI_SWEEP?.trim()) config.sweep = !/^(0|false|no|off)$/i.test(env.BUTLER_PI_SWEEP.trim())
  return { config, warning }
}

/**
 * First run (no saved cursor) starts at the server's latest seq so history
 * is not replayed. A saved cursor still inside the stream is kept. A saved
 * cursor ahead of the stream means the DB was recreated (`butler reset`
 * restarts seq at 0) and the fresh stream is replayed from the start.
 */
export function initialCursor(saved: number | undefined, latest: number): number {
  if (saved === undefined || Number.isNaN(saved)) return latest
  if (saved > latest) return 0
  return saved
}

export function oneLine(s: string): string {
  return s.replace(/[\r\n]+/g, " ").trim()
}

export function houseRules(me: string): string {
  return [
    `How to act (you are the Butler agent ${me} on Andrew's agent team — your current display name is whatever Butler lists for that id; use the butler MCP tools, which Pi registers as mcp__butler__<tool>, for example mcp__butler__read_session, mcp__butler__send_message, mcp__butler__check_messages, mcp__butler__get_task, mcp__butler__update_task_status, mcp__butler__comment_task, mcp__butler__handoff_task, and mcp__butler__rename_self. Pi's default exposure is codemode, so call them from a codemode script in the butler namespace; with exposure "direct" they are ordinary tools):`,
    "- If Andrew or Noodle tells you that you have a new name, call rename_self with exactly that name, then confirm in the same Butler session. Never rename anyone else.",
    "- A message that @-mentions you expects a reply: ack briefly, do it, then answer in that same Butler session with send_message (@mention whoever asked).",
    "- A task you own: update_task_status in_progress, do the work, comment_task with the result/evidence, then handoff_task with a short note. Never set a task done and never approve your own work (the reviewer decides). Only claim a task you already own.",
    "- Need a decision or approval for board work? Ask @Noodle in Butler, not Andrew. Anything that leaves the team (messages to other people, public posts, spending, deletions, Andrew's accounts) needs Andrew: do not do it, ask @Noodle to escalate.",
    "Relayed automatically by the Butler Pi plugin; no human typed this.",
  ].join("\n")
}

export function tasksFrom(contents: string[]): Map<string, string> {
  const tasks = new Map<string, string>()
  for (const content of contents) {
    for (const m of content.matchAll(TASK_RE)) tasks.set(m[2], m[1])
  }
  return tasks
}

/** One short pointer. The message bodies stay on the server; the agent reads them. */
export function mentionPointer(me: string, sid: string, title: string, contents: string[]): string {
  const tasks = tasksFrom(contents)
  const lines = [`[butler] new messages in "${oneLine(title) || sid}" (${sid}).`]
  if (tasks.size > 0) {
    const listed = [...tasks].map(([id, taskTitle]) => `${id} "${oneLine(taskTitle)}"`).join("; ")
    lines.push(`Task notices: ${listed} — read each with get_task.`)
  }
  lines.push(`Read the messages with read_session (session "${sid}"), or check_messages for every session.`)
  lines.push(houseRules(me))
  lines.push(pointerTag(sid))
  return lines.join("\n")
}

export interface OpenTask {
  id: string
  status: string
  title: string
}

export function sweepPointer(me: string, tasks: OpenTask[]): string {
  return [
    `[butler] you own ${tasks.length} open task(s) on the Butler board:`,
    ...tasks.map(t => `- ${t.id} [${t.status}] "${oneLine(t.title)}"`),
    "Read each with get_task before acting.",
    houseRules(me),
    pointerTag(SWEEP_KEY),
  ].join("\n")
}

/** Record a pointer. A slot Pi has already accepted is left alone (the queued
 * text tells the agent to read the session, which includes anything newer). */
export function note(slots: Map<string, Slot>, key: string, text: string): "skipped" | "updated" {
  const cur = slots.get(key)
  if (cur?.handed) return "skipped"
  slots.set(key, { text, handed: false, handedAt: 0 })
  return "updated"
}

export function settle(slots: Map<string, Slot>, prompt: string): string[] {
  const cleared: string[] = []
  if (!prompt) return cleared
  for (const key of [...slots.keys()]) {
    if (!promptHasTag(prompt, key)) continue
    slots.delete(key)
    cleared.push(key)
  }
  return cleared
}

export function userPromptText(message: unknown): string {
  if (!message || typeof message !== "object") return ""
  const m = message as { role?: unknown, content?: unknown }
  if (m.role !== "user") return ""
  if (typeof m.content === "string") return m.content
  if (!Array.isArray(m.content)) return ""
  const parts: string[] = []
  for (const part of m.content) {
    if (!part || typeof part !== "object") continue
    const p = part as { type?: unknown, text?: unknown }
    if (p.type === "text" && typeof p.text === "string") parts.push(p.text)
  }
  return parts.join("\n")
}

/**
 * Decide which slots to hand to sendUserMessage. Mutates `handed` on the
 * slots it selects. At most one plain (turn-starting) send when Pi is idle;
 * every other send while a run is active is followUp.
 */
export function planDelivery(slots: Map<string, Slot>, now: number, state: DeliveryState): { sends: PlannedSend[], arming: boolean } {
  let arming = state.arming
  const unhand = (slot: Slot) => {
    slot.handed = false
    slot.handedAt = 0
  }
  if (state.reclaimHanded) {
    arming = false
    for (const slot of slots.values()) if (slot.handed) unhand(slot)
  } else {
    for (const slot of slots.values()) {
      if (!slot.handed) continue
      const age = now - slot.handedAt
      const stuck = !state.pendingMessages && age >= STUCK_MS
      const grace = !state.streaming && state.idle && !state.pendingMessages && age >= HANDOFF_GRACE_MS
      if (stuck || grace) {
        unhand(slot)
        if (!state.streaming) arming = false
      }
    }
  }

  const sends: PlannedSend[] = []
  const hand = (key: string, slot: Slot, deliverAs?: "followUp") => {
    slot.handed = true
    slot.handedAt = now
    const send: PlannedSend = { key, text: slot.text }
    if (deliverAs) send.deliverAs = deliverAs
    sends.push(send)
  }

  if (state.streaming) {
    for (const [key, slot] of slots) {
      if (slot.handed) continue
      hand(key, slot, "followUp")
    }
    return { sends, arming: false }
  }
  if (arming || state.pendingMessages || !state.idle) return { sends, arming }
  for (const [key, slot] of slots) {
    if (slot.handed) continue
    hand(key, slot)
    return { sends, arming: true }
  }
  return { sends, arming: false }
}
