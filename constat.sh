#!/bin/bash
# constat.sh — Docker Container Monitor
# Listens to Docker events, sends batched Discord notifications,
# and auto-restarts unhealthy containers (label-gated).

#=============================================================================
# LOAD CONFIGURATION
#=============================================================================
# Supports three modes:
#   1. Standalone: loads constat.conf from script directory
#   2. Container with mounted config: loads /config/constat.conf
#   3. Container with env vars only: no config file needed
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONFIG_FILE="${CONFIG_FILE:-$SCRIPT_DIR/constat.conf}"
[ -f "$CONFIG_FILE" ] || CONFIG_FILE="/config/constat.conf"
[ -f "$CONFIG_FILE" ] && source "$CONFIG_FILE"

# Copy sample config to /config for user reference
[ -d "/config" ] && [ -f "/constat.conf.sample" ] && cp -f /constat.conf.sample /config/constat.conf.sample

# Defaults — env vars and config values take precedence
ENABLE_DISCORD="${ENABLE_DISCORD:-true}"
DISCORD_WEBHOOK_STATE="${DISCORD_WEBHOOK_STATE:-}"
DISCORD_WEBHOOK_HEALTH="${DISCORD_WEBHOOK_HEALTH:-}"
DISCORD_WEBHOOK_MAINTENANCE="${DISCORD_WEBHOOK_MAINTENANCE:-}"
BOT_NAME="${BOT_NAME:-Constat}"
CONSTAT_VERSION="0.9.7"
SERVER_LABEL="${SERVER_LABEL:-Unraid}"
COLOR_STARTED="${COLOR_STARTED:-2ecc71}"
COLOR_STOPPED="${COLOR_STOPPED:-95a5a6}"
COLOR_DIED="${COLOR_DIED:-ed4245}"
COLOR_UNHEALTHY="${COLOR_UNHEALTHY:-ed4245}"
COLOR_RECOVERED="${COLOR_RECOVERED:-2ecc71}"
COLOR_RESTARTING="${COLOR_RESTARTING:-e67e22}"
BATCH_WINDOW="${BATCH_WINDOW:-5}"
EXCLUDE_CONTAINERS="${EXCLUDE_CONTAINERS:-constat}"
RESTART_LABEL="${RESTART_LABEL:-constat.restart}"
RESTART_COOLDOWN="${RESTART_COOLDOWN:-300}"
MAX_RESTARTS="${MAX_RESTARTS:-3}"
SUMMARY_INTERVAL="${SUMMARY_INTERVAL:-21600}"  # 6 hours

# Memory monitoring defaults
MEMORY_PAUSED="${MEMORY_PAUSED:-false}"
MEMORY_POLL_INTERVAL="${MEMORY_POLL_INTERVAL:-30}"
MEMORY_DEFAULT_DURATION="${MEMORY_DEFAULT_DURATION:-300}"
if [ -z "${MEMORY_WATCH+x}" ]; then MEMORY_WATCH=(); fi
COLOR_MEMORY_WARN="${COLOR_MEMORY_WARN:-e67e22}"
COLOR_MEMORY_CRIT="${COLOR_MEMORY_CRIT:-ed4245}"

#=============================================================================
# LOCKFILE
#=============================================================================
LOCKFILE="/tmp/constat.lock"
exec 9>"$LOCKFILE"
if ! flock -n 9; then
    echo "Another instance is already running. Exiting."
    exit 0
fi

#=============================================================================
# GLOBALS
#=============================================================================
declare -a EVENT_BUFFER=()
declare -A RESTART_TIMES=()
declare -A RESTART_COUNTS=()
declare -A UNHEALTHY_CONTAINERS=()
declare -A RECENTLY_STARTED=()
declare -a MEM_RULE_NAME=()
declare -a MEM_RULE_LIMIT=()
declare -a MEM_RULE_ACTION=()
declare -a MEM_RULE_DURATION=()
declare -A MEM_EXCEEDED_SINCE=()
declare -A MEM_LAST_ACTION=()
declare -A MEMORY_RESTARTING=()
LAST_MEM_POLL=0
MEM_CONFIG_MTIME=""
TIMER_PID=""
EVENTS_PID=""
RUNNING=true

#=============================================================================
# HELPER FUNCTIONS
#=============================================================================
log() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*"
}

log_summary() {
    local hlist=() rlist=()
    while read -r cname; do
        [ -z "$cname" ] && continue
        should_exclude "$cname" && continue
        local has_health
        has_health=$(docker inspect --format '{{if .State.Health}}true{{end}}' "$cname" 2>/dev/null)
        [ "$has_health" != "true" ] && continue
        hlist+=("$cname")
        local label
        label=$(docker inspect --format '{{index .Config.Labels "'"$RESTART_LABEL"'"}}' "$cname" 2>/dev/null)
        [ "$label" = "true" ] && rlist+=("$cname")
    done < <(docker ps --format '{{.Names}}')

    log "SUMMARY: Healthcheck active (${#hlist[@]}): ${hlist[*]:-none}"
    log "SUMMARY: Auto-restart enabled (${#rlist[@]}): ${rlist[*]:-none}"
}

should_exclude() {
    local name="${1,,}"  # lowercase for case-insensitive match
    IFS=',' read -ra EXCLUDES <<< "${EXCLUDE_CONTAINERS,,}"
    for ex in "${EXCLUDES[@]}"; do
        ex=$(echo "$ex" | xargs)  # trim whitespace
        [ "$name" = "$ex" ] && return 0
    done
    return 1
}

check_restart_cooldown() {
    local name="$1"
    local now
    now=$(date +%s)

    local last=${RESTART_TIMES[$name]:-0}
    if [ $((now - last)) -lt "$RESTART_COOLDOWN" ]; then
        local count=${RESTART_COUNTS[$name]:-0}
        if [ "$count" -ge "$MAX_RESTARTS" ]; then
            log "WARN: $name hit max restarts ($MAX_RESTARTS) within cooldown"
            return 1
        fi
    else
        # Cooldown expired — reset counter
        RESTART_COUNTS[$name]=0
    fi

    return 0
}

can_restart() {
    local name="$1"
    # Check exclude list
    should_exclude "$name" && return 1
    # Check label
    local label
    label=$(docker inspect --format '{{index .Config.Labels "'"$RESTART_LABEL"'"}}' "$name" 2>/dev/null)
    [ "$label" != "true" ] && return 1

    # Check UI restart override
    local disabled_file="/config/restart_disabled.json"
    if [ -f "$disabled_file" ] && command -v jq > /dev/null 2>&1; then
        if jq -e --arg n "$name" 'index($n) != null' "$disabled_file" > /dev/null 2>&1; then
            log "$name restart disabled via UI override"
            return 1
        fi
    fi

    check_restart_cooldown "$name"
}

do_restart() {
    local name="$1"
    local now
    now=$(date +%s)

    RESTART_TIMES[$name]=$now
    RESTART_COUNTS[$name]=$(( ${RESTART_COUNTS[$name]:-0} + 1 ))

    log "Restarting $name (attempt ${RESTART_COUNTS[$name]}/$MAX_RESTARTS)"
    docker restart "$name" > /dev/null 2>&1 &
}

#=============================================================================
# MEMORY MONITORING
#=============================================================================
parse_mem_limit() {
    local val="${1,,}"  # lowercase
    local num="${val%[gmk]}"
    case "$val" in
        *g) echo $(( ${num%.*} * 1073741824 )) ;;
        *m) echo $(( ${num%.*} * 1048576 )) ;;
        *k) echo $(( ${num%.*} * 1024 )) ;;
        *)  echo "$num" ;;
    esac
}

parse_docker_mem() {
    local val="$1"
    local num unit
    num=$(echo "$val" | sed 's/[^0-9.]//g')
    unit=$(echo "$val" | sed 's/[0-9.]//g')
    case "$unit" in
        GiB) awk "BEGIN {printf \"%.0f\", $num * 1073741824}" ;;
        MiB) awk "BEGIN {printf \"%.0f\", $num * 1048576}" ;;
        KiB) awk "BEGIN {printf \"%.0f\", $num * 1024}" ;;
        B)   echo "${num%.*}" ;;
        *)   echo "0" ;;
    esac
}

format_mem() {
    local bytes="$1"
    if [ -z "$bytes" ] || [ "$bytes" -eq 0 ] 2>/dev/null; then
        echo "0 B"
    elif [ "$bytes" -ge 1073741824 ]; then
        awk "BEGIN {printf \"%.2f GiB\", $bytes/1073741824}"
    elif [ "$bytes" -ge 1048576 ]; then
        awk "BEGIN {printf \"%.1f MiB\", $bytes/1048576}"
    else
        awk "BEGIN {printf \"%.0f KiB\", $bytes/1024}"
    fi
}

format_duration() {
    local secs="$1"
    if [ "$secs" -ge 3600 ]; then
        local h=$((secs / 3600))
        local m=$(( (secs % 3600) / 60 ))
        echo "${h}h ${m}m"
    elif [ "$secs" -ge 60 ]; then
        local m=$((secs / 60))
        local s=$((secs % 60))
        echo "${m}m ${s}s"
    else
        echo "${secs}s"
    fi
}

reset_mem_timers_for() {
    local target_name="$1"
    local idx
    for idx in "${!MEM_RULE_NAME[@]}"; do
        if [ "${MEM_RULE_NAME[$idx]}" = "$target_name" ]; then
            unset 'MEM_EXCEEDED_SINCE[rule_'"$idx"']'
            unset 'MEM_LAST_ACTION[rule_'"$idx"']'
        fi
    done
}

parse_memory_watch() {
    MEM_RULE_NAME=()
    MEM_RULE_LIMIT=()
    MEM_RULE_ACTION=()
    MEM_RULE_DURATION=()

    [ ${#MEMORY_WATCH[@]} -eq 0 ] && return 0

    local idx=0
    for entry in "${MEMORY_WATCH[@]}"; do
        IFS=':' read -r name limit action duration <<< "$entry"
        if [ -z "$name" ] || [ -z "$limit" ] || [ -z "$action" ]; then
            log "WARN: Invalid MEMORY_WATCH entry: $entry"
            continue
        fi
        if [ "$action" != "notify" ] && [ "$action" != "restart" ]; then
            log "WARN: Invalid action '$action' for $name (must be notify or restart), defaulting to notify"
            action="notify"
        fi
        local limit_bytes
        limit_bytes=$(parse_mem_limit "$limit")
        MEM_RULE_NAME[$idx]="$name"
        MEM_RULE_LIMIT[$idx]=$limit_bytes
        MEM_RULE_ACTION[$idx]="$action"
        MEM_RULE_DURATION[$idx]=${duration:-$MEMORY_DEFAULT_DURATION}
        log "MEMORY: Rule $idx — $name, limit $(format_mem "$limit_bytes"), action $action, duration ${MEM_RULE_DURATION[$idx]}s"
        idx=$((idx + 1))
    done
}

reload_memory_config() {
    [ ! -f "$CONFIG_FILE" ] && return 0
    local current_mtime
    current_mtime=$(stat -c %Y "$CONFIG_FILE" 2>/dev/null) || return 0
    [ "$current_mtime" = "$MEM_CONFIG_MTIME" ] && return 0
    MEM_CONFIG_MTIME="$current_mtime"

    # Save old rules for timer preservation
    local -a OLD_RULE_NAME=("${MEM_RULE_NAME[@]}")
    local -a OLD_RULE_LIMIT=("${MEM_RULE_LIMIT[@]}")
    local -a OLD_RULE_ACTION=("${MEM_RULE_ACTION[@]}")
    local -a OLD_RULE_DURATION=("${MEM_RULE_DURATION[@]}")

    # Re-source config
    source "$CONFIG_FILE"
    MEMORY_PAUSED="${MEMORY_PAUSED:-false}"
    MEMORY_POLL_INTERVAL="${MEMORY_POLL_INTERVAL:-30}"
    MEMORY_DEFAULT_DURATION="${MEMORY_DEFAULT_DURATION:-300}"
    if [ -z "${MEMORY_WATCH+x}" ]; then MEMORY_WATCH=(); fi

    # Re-parse rules
    parse_memory_watch

    # Preserve timers for rules that haven't changed
    local new_idx
    for new_idx in "${!MEM_RULE_NAME[@]}"; do
        local old_idx
        for old_idx in "${!OLD_RULE_NAME[@]}"; do
            if [ "${MEM_RULE_NAME[$new_idx]}" = "${OLD_RULE_NAME[$old_idx]}" ] &&
               [ "${MEM_RULE_LIMIT[$new_idx]}" = "${OLD_RULE_LIMIT[$old_idx]}" ] &&
               [ "${MEM_RULE_ACTION[$new_idx]}" = "${OLD_RULE_ACTION[$old_idx]}" ] &&
               [ "${MEM_RULE_DURATION[$new_idx]}" = "${OLD_RULE_DURATION[$old_idx]}" ]; then
                # Rule unchanged — migrate timers from old index to new index
                local old_key="rule_${old_idx}" new_key="rule_${new_idx}"
                if [ -n "${MEM_EXCEEDED_SINCE[$old_key]+x}" ]; then
                    MEM_EXCEEDED_SINCE[$new_key]="${MEM_EXCEEDED_SINCE[$old_key]}"
                fi
                if [ -n "${MEM_LAST_ACTION[$old_key]+x}" ]; then
                    MEM_LAST_ACTION[$new_key]="${MEM_LAST_ACTION[$old_key]}"
                fi
                break
            fi
        done
    done

    # Clean up timers for rules that no longer exist
    local key
    for key in "${!MEM_EXCEEDED_SINCE[@]}"; do
        local idx="${key#rule_}"
        [ -z "${MEM_RULE_NAME[$idx]+x}" ] && unset "MEM_EXCEEDED_SINCE[$key]"
    done
    for key in "${!MEM_LAST_ACTION[@]}"; do
        local idx="${key#rule_}"
        [ -z "${MEM_RULE_NAME[$idx]+x}" ] && unset "MEM_LAST_ACTION[$key]"
    done

    log "MEMORY: Config reloaded — ${#MEM_RULE_NAME[@]} rules, paused=$MEMORY_PAUSED, poll=${MEMORY_POLL_INTERVAL}s"
}

memory_check() {
    reload_memory_config
    [ "$MEMORY_PAUSED" = "true" ] && return 0
    [ ${#MEM_RULE_NAME[@]} -eq 0 ] && return 0

    # Self-rate-limit
    local now
    now=$(date +%s)
    if [ $((now - LAST_MEM_POLL)) -lt "$MEMORY_POLL_INTERVAL" ]; then
        return 0
    fi
    LAST_MEM_POLL=$now

    # Single docker stats call for all containers (5s timeout)
    local stats_output
    stats_output=$(timeout 5 docker stats --no-stream --format '{{.Name}}\t{{.MemUsage}}' 2>/dev/null) || {
        log "WARN: docker stats timed out or failed"
        return 0
    }

    # Build usage map: container name → bytes
    local -A usage_map=()
    while IFS=$'\t' read -r cname memusage; do
        [ -z "$cname" ] && continue
        local usage_str="${memusage%% / *}"
        usage_map[$cname]=$(parse_docker_mem "$usage_str")
    done <<< "$stats_output"

    # Iterate all rules by index
    local idx
    for idx in "${!MEM_RULE_NAME[@]}"; do
        local name="${MEM_RULE_NAME[$idx]}"
        local limit_bytes=${MEM_RULE_LIMIT[$idx]}
        local duration=${MEM_RULE_DURATION[$idx]}
        local rule_key="rule_${idx}"

        # Check if container exists in stats
        if [ -z "${usage_map[$name]+x}" ]; then
            # Container not running — reset timer if set
            if [ -n "${MEM_EXCEEDED_SINCE[$rule_key]}" ]; then
                log "MEMORY: Rule $idx ($name) — container absent from stats (stopped?), timer reset"
                unset 'MEM_EXCEEDED_SINCE[$rule_key]'
            fi
            continue
        fi

        local usage_bytes=${usage_map[$name]}

        if [ "$usage_bytes" -ge "$limit_bytes" ]; then
            # Over threshold — mark first time if not already marked
            if [ -z "${MEM_EXCEEDED_SINCE[$rule_key]}" ]; then
                MEM_EXCEEDED_SINCE[$rule_key]=$now
                log "MEMORY: Rule $idx ($name) exceeded threshold — $(format_mem "$usage_bytes") / $(format_mem "$limit_bytes")"
            fi

            local exceeded_since=${MEM_EXCEEDED_SINCE[$rule_key]}
            local elapsed=$((now - exceeded_since))

            if [ "$elapsed" -ge "$duration" ]; then
                # Gate repeat actions: only fire if duration has passed since last action
                local last_action=${MEM_LAST_ACTION[$rule_key]:-0}
                local since_last=$((now - last_action))
                if [ "$last_action" -eq 0 ] || [ "$since_last" -ge "$duration" ]; then
                    memory_action_rule "$idx" "$usage_bytes" "$limit_bytes" "$elapsed"
                fi
            fi
        else
            # Below threshold — reset timers and notify recovery
            if [ -n "${MEM_EXCEEDED_SINCE[$rule_key]}" ]; then
                local exceeded_since=${MEM_EXCEEDED_SINCE[$rule_key]}
                local elapsed=$((now - exceeded_since))
                local usage_fmt limit_fmt
                usage_fmt=$(format_mem "$usage_bytes")
                limit_fmt=$(format_mem "$limit_bytes")
                log "MEMORY: Rule $idx ($name) dropped below threshold — $usage_fmt / $limit_fmt after $(format_duration "$elapsed"), timer reset"
                # Only send recovery if we actually fired at least one action
                if [ -n "${MEM_LAST_ACTION[$rule_key]}" ]; then
                    send_memory_discord "$name" "$usage_fmt" "$limit_fmt" "$elapsed" "recovered" "Memory returned to normal"
                    emit_ui_event "$name" "memory" "recovered" "$usage_fmt / $limit_fmt — after $(format_duration "$elapsed")"
                fi
                unset 'MEM_EXCEEDED_SINCE[$rule_key]'
                unset 'MEM_LAST_ACTION[$rule_key]'
            fi
        fi
    done
}

memory_action_rule() {
    local idx="$1" usage="$2" limit="$3" elapsed="$4"
    local name="${MEM_RULE_NAME[$idx]}"
    local action="${MEM_RULE_ACTION[$idx]}"
    local usage_fmt limit_fmt
    usage_fmt=$(format_mem "$usage")
    limit_fmt=$(format_mem "$limit")

    if [ "$action" = "restart" ]; then
        if check_restart_cooldown "$name"; then
            log "MEMORY: Rule $idx — restarting $name — $usage_fmt / $limit_fmt (exceeded for $(format_duration "$elapsed"))"
            MEMORY_RESTARTING[$name]=1
            do_restart "$name"
            send_memory_discord "$name" "$usage_fmt" "$limit_fmt" "$elapsed" "restart" "Container restarted (exceeded $limit_fmt for $(format_duration "$elapsed"))"
        else
            log "MEMORY: Rule $idx — restart blocked for $name — cooldown/max attempts"
            send_memory_discord "$name" "$usage_fmt" "$limit_fmt" "$elapsed" "blocked" "Restart blocked (max attempts reached)"
        fi
    else
        log "MEMORY: Rule $idx — alert for $name — $usage_fmt / $limit_fmt (exceeded for $(format_duration "$elapsed"))"
        send_memory_discord "$name" "$usage_fmt" "$limit_fmt" "$elapsed" "notify" "Notification sent (exceeded $limit_fmt for $(format_duration "$elapsed"))"
    fi

    MEM_LAST_ACTION["rule_${idx}"]=$(date +%s)

    # Emit event to web UI
    emit_ui_event "$name" "memory" "$action" "$usage_fmt / $limit_fmt — $(format_duration "$elapsed")"
}

emit_ui_event() {
    local container="$1" type="$2" action="$3" detail="$4"
    curl -sS -X POST "http://localhost:7890/api/events" \
        -H "Content-Type: application/json" \
        -d "$(jq -n --arg c "$container" --arg t "$type" --arg a "$action" --arg d "$detail" \
            '{container:$c, type:$t, action:$a, detail:$d}')" \
        > /dev/null 2>&1 || true
}

send_memory_discord() {
    [ "$ENABLE_DISCORD" != "true" ] && return 0
    [ -z "$DISCORD_WEBHOOK_HEALTH" ] && return 0

    local name="$1" usage="$2" limit="$3" elapsed="$4" severity="$5" status_msg="$6"

    local color_hex title
    case "$severity" in
        notify)    color_hex="$COLOR_MEMORY_WARN"; title="Memory Warning" ;;
        restart)   color_hex="$COLOR_MEMORY_CRIT"; title="Memory Restart" ;;
        blocked)   color_hex="$COLOR_MEMORY_CRIT"; title="Memory Alert — restart blocked" ;;
        recovered) color_hex="$COLOR_RECOVERED";    title="Memory Recovered" ;;
    esac

    local color_dec=$((16#$color_hex))
    local ts
    ts=$(date -u +%Y-%m-%dT%H:%M:%S.000Z)
    local elapsed_fmt
    elapsed_fmt=$(format_duration "$elapsed")

    local payload
    payload=$(jq -n \
        --arg bot "$BOT_NAME" \
        --arg title "$title" \
        --arg name "$name" \
        --arg usage "$usage" \
        --arg limit "$limit" \
        --arg elapsed "$elapsed_fmt" \
        --arg status "$status_msg" \
        --arg footer "Constat v${CONSTAT_VERSION} by ProphetSe7en" \
        --arg ts "$ts" \
        --argjson color "$color_dec" \
        '{
            username: $bot,
            embeds: [{
                author: {name: ("⚠ " + $bot + ": " + $title)},
                color: $color,
                fields: [
                    {name: "Container", value: $name, inline: true},
                    {name: "Usage / Limit", value: ($usage + " / " + $limit), inline: true},
                    {name: "Exceeded for", value: $elapsed, inline: true},
                    {name: "Action", value: $status, inline: false}
                ],
                footer: {text: $footer},
                timestamp: $ts
            }]
        }')

    curl -sS -X POST "$DISCORD_WEBHOOK_HEALTH" -H "Content-Type: application/json" -d "$payload" > /dev/null 2>&1 || true
    log "Discord: Health — $title ($name)"
}

#=============================================================================
# DISCORD NOTIFICATION
#=============================================================================
send_discord() {
    [ "$ENABLE_DISCORD" != "true" ] && return 0

    local webhook="$1"
    local author="$2"
    local field_name="$3"
    local field_value="$4"
    local color_hex="$5"

    local color_dec=$((16#$color_hex))
    local ts
    ts=$(date -u +%Y-%m-%dT%H:%M:%S.000Z)

    local payload
    payload=$(jq -n \
        --arg bot "$BOT_NAME" \
        --arg author "$author" \
        --arg fname "$field_name" \
        --arg fval "$field_value" \
        --arg footer "Constat v${CONSTAT_VERSION} by ProphetSe7en" \
        --arg ts "$ts" \
        --argjson color "$color_dec" \
        '{
            username: $bot,
            embeds: [{
                author: {name: $author},
                color: $color,
                fields: [{name: $fname, value: $fval, inline: false}],
                footer: {text: $footer},
                timestamp: $ts
            }]
        }')

    curl -sS -X POST "$webhook" -H "Content-Type: application/json" -d "$payload" > /dev/null 2>&1 || true
}

#=============================================================================
# BATCH FLUSH
#=============================================================================
flush_batch() {
    [ ${#EVENT_BUFFER[@]} -eq 0 ] && return 0

    # Categorize events
    local -A state_lines=()
    local -A health_lines=()
    local -A state_counts=()
    local -A health_counts=()
    local -A state_colors=()
    local -A health_colors=()

    for entry in "${EVENT_BUFFER[@]}"; do
        # Format: "type|category|color|line"
        local type category color line
        IFS='|' read -r type category color line <<< "$entry"

        if [ "$type" = "state" ]; then
            state_lines[$category]+="$line"$'\n'
            state_counts[$category]=$(( ${state_counts[$category]:-0} + 1 ))
            state_colors[$category]="$color"
        elif [ "$type" = "health" ]; then
            health_lines[$category]+="$line"$'\n'
            health_counts[$category]=$(( ${health_counts[$category]:-0} + 1 ))
            health_colors[$category]="$color"
        fi
    done

    # Send state change embeds in logical order
    local ordered_states="Stopped Died Killed Paused Restarted Unpaused Started"
    for category in $ordered_states; do
        [ -z "${state_lines[$category]}" ] && continue
        local count=${state_counts[$category]}
        local lines="${state_lines[$category]}"
        local color="${state_colors[$category]}"
        lines="${lines%$'\n'}"  # trim trailing newline

        send_discord "$DISCORD_WEBHOOK_STATE" \
            "🔄 $BOT_NAME: State Change" \
            "$category ($count)" \
            '```'"$lines"'```' \
            "$color"

        log "Discord: State Change — $category ($count)"
    done

    # Send health embeds
    for category in "${!health_lines[@]}"; do
        local count=${health_counts[$category]}
        local lines="${health_lines[$category]}"
        local color="${health_colors[$category]}"
        lines="${lines%$'\n'}"

        send_discord "$DISCORD_WEBHOOK_HEALTH" \
            "⚕ $BOT_NAME: Health Issue" \
            "$category ($count)" \
            '```'"$lines"'```' \
            "$color"

        log "Discord: Health Issue — $category ($count)"
    done

    # Clear buffer
    EVENT_BUFFER=()
}

reset_timer() {
    # Kill previous timer
    if [ -n "$TIMER_PID" ]; then
        kill "$TIMER_PID" 2>/dev/null
        wait "$TIMER_PID" 2>/dev/null
    fi

    # Start new timer — sends USR1 after BATCH_WINDOW seconds
    (sleep "$BATCH_WINDOW" && kill -USR1 $$ 2>/dev/null) &
    TIMER_PID=$!
}

#=============================================================================
# SIGNAL HANDLERS
#=============================================================================
handle_flush() {
    flush_batch
}

handle_shutdown() {
    log "Shutting down..."
    RUNNING=false
    flush_batch

    # Cleanup
    [ -n "$TIMER_PID" ] && kill "$TIMER_PID" 2>/dev/null
    [ -n "$EVENTS_PID" ] && kill "$EVENTS_PID" 2>/dev/null

    log "Constat stopped."
    exit 0
}

trap 'handle_flush' USR1
trap 'handle_shutdown' SIGTERM SIGINT

#=============================================================================
# MAIN
#=============================================================================
log "Constat starting..."
log "Batch window: ${BATCH_WINDOW}s | Exclude: ${EXCLUDE_CONTAINERS}"
log "Restart label: ${RESTART_LABEL} | Cooldown: ${RESTART_COOLDOWN}s | Max: ${MAX_RESTARTS}"

# Parse memory watch config
MEM_CONFIG_MTIME=$(stat -c %Y "$CONFIG_FILE" 2>/dev/null || echo "")
parse_memory_watch
if [ ${#MEM_RULE_NAME[@]} -gt 0 ]; then
    if [ "$MEMORY_PAUSED" = "true" ]; then
        log "Memory monitoring: PAUSED (${#MEM_RULE_NAME[@]} rules defined, actions suspended)"
    else
        log "Memory monitoring: ON (poll ${MEMORY_POLL_INTERVAL}s, default duration ${MEMORY_DEFAULT_DURATION}s, ${#MEM_RULE_NAME[@]} rules)"
    fi
else
    log "Memory monitoring: no rules defined"
fi

# Startup scan
log_summary
LAST_SUMMARY=$(date +%s)

# Main event loop — reconnects automatically
while $RUNNING; do
    log "Connecting to Docker event stream..."

    # Read timeout: use shorter of SUMMARY_INTERVAL and MEMORY_POLL_INTERVAL
    READ_TIMEOUT="$SUMMARY_INTERVAL"
    if [ ${#MEM_RULE_NAME[@]} -gt 0 ] && [ "$MEMORY_PAUSED" != "true" ] && [ "$MEMORY_POLL_INTERVAL" -lt "$READ_TIMEOUT" ]; then
        READ_TIMEOUT="$MEMORY_POLL_INTERVAL"
    fi

    while read -r -t "$READ_TIMEOUT" event || {
        # read timed out — check periodic summary + memory
        now_s=$(date +%s)
        if [ $((now_s - LAST_SUMMARY)) -ge "$SUMMARY_INTERVAL" ]; then
            log_summary
            LAST_SUMMARY=$now_s
        fi
        memory_check
        continue
    }; do
        # Periodic summary check (in case events keep flowing)
        now_s=$(date +%s)
        if [ $((now_s - LAST_SUMMARY)) -ge "$SUMMARY_INTERVAL" ]; then
            log_summary
            LAST_SUMMARY=$now_s
        fi
        memory_check

        # Parse event
        action=$(echo "$event" | jq -r '.Action // empty')
        name=$(echo "$event" | jq -r '.Actor.Attributes.name // empty')
        image=$(echo "$event" | jq -r '.Actor.Attributes.image // empty')
        exit_code=$(echo "$event" | jq -r '.Actor.Attributes.exitCode // empty')

        [ -z "$action" ] || [ -z "$name" ] && continue

        # Skip excluded containers
        should_exclude "$name" && continue

        # Categorize action
        # Note: docker stop generates kill → die(0) → stop. We only report:
        #   - stop: graceful stop (covers docker stop)
        #   - die with non-zero exit: unexpected crash
        #   - start: container started
        # kill events are always followed by die/stop, so we skip them.
        # die with exit 0 is part of graceful stop, so we skip it.
        case "$action" in
            start)
                RECENTLY_STARTED[$name]=$(date +%s)
                EVENT_BUFFER+=("state|Started|$COLOR_STARTED|$name exited → running")
                log "EVENT: $name started ($image)"
                reset_timer
                ;;
            stop)
                # Docker sends die → stop. Remove any die event for this container
                # from the buffer — signal deaths (137/143) are part of graceful stop.
                cleaned=()
                for entry in "${EVENT_BUFFER[@]}"; do
                    case "$entry" in
                        "state|Died|"*"|$name died"*) ;;
                        *) cleaned+=("$entry") ;;
                    esac
                done
                EVENT_BUFFER=("${cleaned[@]}")
                EVENT_BUFFER+=("state|Stopped|$COLOR_STOPPED|$name running → exited")
                reset_mem_timers_for "$name"
                log "EVENT: $name stopped ($image)"
                reset_timer
                ;;
            die)
                # Only report truly unexpected deaths:
                # - exit 0 = graceful stop, always skip
                # - exit 137 (SIGKILL) / 143 (SIGTERM) = signal death during stop/restart
                #   If there's already a stop for this container in the buffer, skip it
                if [ -n "$exit_code" ] && [ "$exit_code" != "0" ]; then
                    # Check if this is a signal death alongside a stop event
                    has_stop=false
                    for entry in "${EVENT_BUFFER[@]}"; do
                        case "$entry" in
                            "state|Stopped|"*"|$name running → exited") has_stop=true; break ;;
                            "state|Restarted|"*"|$name restarted") has_stop=true; break ;;
                        esac
                    done
                    if ! $has_stop; then
                        EVENT_BUFFER+=("state|Died|$COLOR_DIED|$name died (exit $exit_code)")
                        log "EVENT: $name died (exit $exit_code) ($image)"
                        reset_timer
                    fi
                fi
                ;;
            restart)
                # Docker sends stop → start → restart. Remove the redundant
                # stop and start events for this container from the buffer.
                cleaned=()
                for entry in "${EVENT_BUFFER[@]}"; do
                    case "$entry" in
                        "state|Stopped|"*"|$name running → exited") ;;
                        "state|Started|"*"|$name exited → running") ;;
                        *) cleaned+=("$entry") ;;
                    esac
                done
                EVENT_BUFFER=("${cleaned[@]}")
                reset_mem_timers_for "$name"
                # Suppress state notification if this was a memory-triggered restart
                if [ "${MEMORY_RESTARTING[$name]}" = "1" ]; then
                    unset 'MEMORY_RESTARTING[$name]'
                    log "EVENT: $name restarted (memory-triggered, state notification suppressed)"
                else
                    EVENT_BUFFER+=("state|Restarted|$COLOR_RESTARTING|$name restarted")
                    log "EVENT: $name restarted ($image)"
                fi
                reset_timer
                ;;
            pause)
                EVENT_BUFFER+=("state|Paused|$COLOR_STOPPED|$name running → paused")
                log "EVENT: $name paused ($image)"
                reset_timer
                ;;
            unpause)
                EVENT_BUFFER+=("state|Unpaused|$COLOR_STARTED|$name paused → running")
                log "EVENT: $name unpaused ($image)"
                reset_timer
                ;;
            "health_status: unhealthy")
                UNHEALTHY_CONTAINERS[$name]=1
                restart_info=""
                if can_restart "$name"; then
                    do_restart "$name"
                    restart_info=" (restarting)"
                fi
                EVENT_BUFFER+=("health|Unhealthy|$COLOR_UNHEALTHY|$name healthy → unhealthy${restart_info}")
                log "EVENT: $name unhealthy${restart_info} ($image)"
                reset_timer
                ;;
            "health_status: healthy")
                if [ "${UNHEALTHY_CONTAINERS[$name]}" = "1" ]; then
                    # Was unhealthy — this is a real recovery
                    unset 'UNHEALTHY_CONTAINERS[$name]'
                    EVENT_BUFFER+=("health|Recovered|$COLOR_RECOVERED|$name unhealthy → healthy")
                    log "EVENT: $name recovered ($image)"
                elif [ -n "${RECENTLY_STARTED[$name]}" ]; then
                    # Just started — startup health confirmation
                    unset 'RECENTLY_STARTED[$name]'
                    log "EVENT: $name startup healthy (skipped)"
                else
                    # Periodic healthy after being healthy — skip
                    :
                fi
                ;;
            *)
                # Skip exec_*, create, destroy, attach, detach, top, resize, etc.
                ;;
        esac
    done < <(docker events --filter type=container --format '{{json .}}')

    # docker events exited — reconnect after delay
    if $RUNNING; then
        log "Docker event stream disconnected. Reconnecting in 5s..."
        sleep 5
    fi
done
