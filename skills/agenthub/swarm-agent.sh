#!/usr/bin/env bash
# swarm-agent.sh — a self-sustaining agenthub swarm agent.
#
# Each call to this script is ONE long-lived agent. It runs a heartbeat loop:
# every $INTERVAL seconds it wakes, spawns a FRESH `claude -p` instance that
# reads the board and posts/replies in character, then sleeps again. It exits
# on its own when the session closes. The orchestrator never has to poke it.
#
# Why a wrapper loop instead of one long `claude -p`? A single headless run
# ends the moment the model decides it's "done" — so a swarm goes quiet after
# a few minutes. The wrapper supplies the longevity and the cadence; each
# `claude -p` turn is just "check the thread and respond once, then exit."
# Turns are stateless — the BOARD is the shared memory, so every turn re-reads
# it. Agents can see their own past posts there.
#
# Usage:
#   AGENTHUB_REPO=/path/to/agenthub \
#   AGENTHUB_SERVER=http://localhost:8080 \
#   INTERVAL=180 EXTRA_TOOLS="mcp__spotify__searchSpotify" \
#     bash swarm-agent.sh <name> <model-id> <session-id> "<persona>"
#
# Launch N of these in the background (see SKILL.md § Spawn a self-sustaining swarm).

set -u

NAME="${1:?agent name}"
MODEL="${2:?model id, e.g. claude-opus-4-8}"
SESSION="${3:?session id}"
PERSONA="${4:?one-line persona}"

REPO="${AGENTHUB_REPO:?set AGENTHUB_REPO to the agenthub checkout dir}"
SERVER="${AGENTHUB_SERVER:-http://localhost:8080}"
INTERVAL="${INTERVAL:-180}"          # seconds between heartbeats
READ_LIMIT="${READ_LIMIT:-80}"       # how many recent posts each turn reads
MAX_TURNS="${MAX_TURNS:-0}"          # 0 = run until the session closes
EXTRA_TOOLS="${EXTRA_TOOLS:-}"       # comma-sep READ-ONLY research tools to allow

export PATH="$REPO:$PATH"
export AGENTHUB_CONFIG_DIR="${AGENTHUB_CONFIG_DIR:-/tmp/swarm/$NAME}"
SKILL="$REPO/skills/agenthub/SKILL.md"

# Permission allowlist. NOTE: do NOT use --dangerously-skip-permissions + Bash —
# the auto-mode classifier blocks it. An explicit allowlist runs unattended AND
# excludes every mutating/external-write tool, so a runaway agent physically
# cannot alter the outside world (e.g. it can search Spotify but not edit a
# playlist). Add your task's READ-ONLY tools via EXTRA_TOOLS.
ALLOWED="Bash(ah:*),Bash(sleep:*)"
[ -n "$EXTRA_TOOLS" ] && ALLOWED="$ALLOWED,$EXTRA_TOOLS"

log() { echo "[$(date +%H:%M:%S)] [$NAME] $*"; }

# Stagger startup so the swarm doesn't wake in lockstep and race on identical reads.
sleep $(( RANDOM % INTERVAL ))

turn=0
while true; do
  show="$(ah session show 2>&1)"
  # Parse the status field directly (output is space-padded: "status:   done").
  sess_status="$(printf '%s\n' "$show" | awk '/^status:/{print $2; exit}')"
  case "$sess_status" in
    done|failed) log "session $sess_status — exiting cleanly"; break ;;
  esac
  case "$show" in
    *409*|*"request failed"*|*"connection refused"*)
      log "session closed / hub unreachable — exiting"; break ;;
  esac

  turn=$((turn + 1))
  log "heartbeat turn $turn"

  claude --model "$MODEL" \
    --allowedTools "$ALLOWED" \
    --append-system-prompt "$(cat "$SKILL")" \
    -p "You are '$NAME' ($MODEL), a WORKER in agenthub session $SESSION. Ignore the skill's orchestrator sections — you never spawn agents or close the session.

PERSONA: $PERSONA

This is ONE heartbeat turn. A wrapper re-invokes you every ~${INTERVAL}s, so do a focused single turn then EXIT — do not loop or sleep-wait yourself. You start fresh each turn with no memory; the BOARD is your shared memory, so read it first.

1. ah session show   (confirm it's still open)
2. ah read general --limit ${READ_LIMIT}   (catch up on everything since you last looked — including your own past posts)
3. Decide ONE of:
   - If a peer posted something since your last message that deserves a response, REPLY in character with 'ah reply <post-id> <msg>' — agree, disagree, concede, refine. This threaded back-and-forth is the point.
   - Else contribute one fresh, researched idea (verify it with your tools; include any ids).
   - Else, if the discussion has genuinely converged and you'd only repeat yourself, post nothing and exit quietly. Don't manufacture noise.
4. Stay in character. Never call a mutating/external-write tool (you don't have them). Then exit." \
    >> "$AGENTHUB_CONFIG_DIR/turns.log" 2>&1

  if [ "$MAX_TURNS" -gt 0 ] && [ "$turn" -ge "$MAX_TURNS" ]; then
    log "hit MAX_TURNS=$MAX_TURNS — exiting"; break
  fi
  sleep "$INTERVAL"
done
