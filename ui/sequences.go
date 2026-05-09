package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// Sentinel errors for typed error checking in handlers
var (
	ErrAlreadyRunning = errors.New("another sequence is already running")
	ErrSeqNotFound    = errors.New("sequence not found")
	ErrNotRunning     = errors.New("no sequence is running")
)

const sequencesPath = "/config/sequences.json"

// reservedSlugs are IDs that conflict with API routes
var reservedSlugs = map[string]bool{
	"stream": true, "abort": true, "status": true,
	"start": true, "stop": true, "restart": true,
}

// Sequence is a saved ordered list of container start/stop steps
type Sequence struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Icon        string    `json:"icon,omitempty"`
	Steps       []SeqStep `json:"steps"`
	LastRun     string    `json:"lastRun,omitempty"`
	LastResult  string    `json:"lastResult,omitempty"`
	// OnRequiredFailure controls what happens when one of the Required
	// steps' container is reported by the auto-restart manager as
	// exhausted (MaxRestarts attempts in RestartCooldown, gave up).
	// "" / "none" = do nothing. "stop" = run StopSequence on this sequence.
	OnRequiredFailure string `json:"onRequiredFailure,omitempty"`
	// OnRequiredFailureDelaySeconds is how many seconds we wait after a
	// Required container goes down before firing the failure cascade.
	// Lets quick restarts (Restart button, docker restart) complete
	// without triggering the cascade. nil = use system default (30s);
	// 0 = fire immediately (useful for tightly-coupled dependencies
	// like databases where the dependents can't function for even a
	// brief outage); 1-300 = wait that many seconds.
	OnRequiredFailureDelaySeconds *int `json:"onRequiredFailureDelaySeconds,omitempty"`
	// OnRequiredRecovery controls what happens when a Required step's
	// container is reported recovered by the auto-restart manager (back
	// to healthy within budget). "" / "none" = do nothing. "restart" =
	// run RestartSequence on this sequence.
	OnRequiredRecovery string `json:"onRequiredRecovery,omitempty"`
}

// SeqStep is a single container in a sequence.
//
// Required vs SkipRequired:
//   - Required = "this step is a checkpoint AND a dependency for everything
//     after it". If it fails, subsequent non-SkipRequired steps are skipped.
//   - SkipRequired = "bypass the Required-failure rule": run this step in
//     its declared position even if a previous Required step failed.
//     Mutually exclusive with Required (a Required step IS the dependency,
//     so Required+SkipRequired is meaningless and rejected by validation).
//
// A step that is neither Required nor SkipRequired is "ordinary": it shares
// the parallel group of its preceding Required step and is skipped if
// upstream Required fails.
//
// DelaySeconds is honored on every step regardless of Required: after the
// container is started (and Wait healthy has passed if set) the executor
// waits this many seconds before reporting the step done. For Required
// steps this gates the next group; for ordinary steps it extends the
// parallel group's total runtime. Useful when a container reports running
// or healthy but still needs a few seconds to fully initialise.
type SeqStep struct {
	Container     string `json:"container"`
	Required      bool   `json:"required"`
	WaitHealthy   bool   `json:"waitHealthy"`
	DelaySeconds  int    `json:"delaySeconds,omitempty"`
	SkipRequired  bool   `json:"skipRequired,omitempty"`
}

// SeqExecution is the runtime state of a running sequence
type SeqExecution struct {
	SequenceID  string         `json:"sequenceId"`
	Mode        string         `json:"mode"` // start, stop, restart
	Phase       string         `json:"phase,omitempty"` // stopping, starting, waiting-healthy, waiting-grace-Ns
	Status      string         `json:"status"` // running, complete, failed, aborted
	CurrentStep int            `json:"currentStep"`
	TotalSteps  int            `json:"totalSteps"`
	StartedAt   time.Time      `json:"startedAt"`
	Elapsed     float64        `json:"elapsed"`
	Steps       []SeqStepState `json:"steps"`
	Error       string         `json:"error,omitempty"`
	// PhaseStartedAt records when the current Phase began. Used by the
	// frontend to compute live countdowns (e.g., the cascade-restart
	// grace-delay phase) without polling the backend each second.
	PhaseStartedAt time.Time `json:"phaseStartedAt,omitempty"`
	// RunID ties together every event emitted during this run. The same ID
	// is reused across phase transitions (restart mode's stop→start phases,
	// cascade mode's stop→restart cascade) so the UI groups all events
	// from one logical run under a single expandable parent.
	RunID string `json:"runId,omitempty"`
}

// SeqStepState is the runtime state of a single step
type SeqStepState struct {
	Container string  `json:"container"`
	Required  bool    `json:"required"`
	Status    string  `json:"status"` // waiting, starting, stopping, delaying, done, failed, skipped
	Elapsed   float64 `json:"elapsed,omitempty"`
	Log       string  `json:"log,omitempty"`
}

// SeqEvent is the SSE payload for sequence updates
type SeqEvent struct {
	Type string       `json:"type"` // seq-update, seq-complete, seq-failed, seq-aborted
	Data SeqExecution `json:"data"`
}

// SequenceExecutor manages sequences: CRUD, execution, and SSE broadcasting
type SequenceExecutor struct {
	mu          sync.RWMutex
	docker      *client.Client
	sequences   []Sequence
	execution   *SeqExecution
	cancelExec  context.CancelFunc
	subscribers map[chan SeqEvent]struct{}
	subMu       sync.Mutex
	saveCh      chan struct{} // debounced save channel
	done        chan struct{} // closed by Close(); requestSave selects on this to avoid send-on-closed-channel panic
	closeOnce   sync.Once     // guards close(saveCh) + close(done) against double-close
	// Auto-restart wiring. WireRestartManager populates these and starts
	// the event loop that drives OnRequiredFailure/OnRequiredRecovery
	// reactions. Both are nil until WireRestartManager is called, so the
	// executor keeps working in tests + headless contexts.
	restartManager *RestartManager
	// events is the shared EventBuffer the executor emits sequence-run
	// lifecycle + per-step events into. Set during WireRestartManager so
	// they appear in the same Events feed as Docker state events. Nil in
	// tests / headless contexts; emit helpers no-op when nil.
	events *EventBuffer
	// failureSeenAt[name] records when we last fired (or accepted) a
	// failure for the named container. The recovery trigger fires only
	// when a started/recovered event follows a recorded failure, so
	// stray started-events on boot don't fire the trigger.
	//
	// pendingFailure[name] is set when a state/died or state/stopped
	// event arrives. We start a debounce timer; if a state/started
	// arrives before it fires, we cancel — manual restarts (docker
	// restart, constat UI, watchtower update) cycle through stopped→
	// started inside this window and should NOT trigger the sequence.
	// Sustained failures (container is gone for >debounce) DO fire.
	failureMu       sync.Mutex
	failureSeenAt   map[string]time.Time
	pendingFailure  map[string]*time.Timer
}

// stateDebounceWindow is how long we wait after a state/died or
// state/stopped before treating it as a sustained failure. Tuned to
// cover typical manual-restart durations (UI Restart button, docker
// restart, watchtower update) so they don't cascade-stop dependents.
const stateDebounceWindow = 30 * time.Second

// NewSequenceExecutor creates and initializes a sequence executor
func NewSequenceExecutor(docker *client.Client) *SequenceExecutor {
	se := &SequenceExecutor{
		docker:         docker,
		sequences:      []Sequence{},
		subscribers:    make(map[chan SeqEvent]struct{}),
		saveCh:         make(chan struct{}, 1),
		done:           make(chan struct{}),
		failureSeenAt:  make(map[string]time.Time),
		pendingFailure: make(map[string]*time.Timer),
	}
	se.loadFromDisk()
	go se.saveLoop()
	return se
}

// saveLoop serializes all disk writes through a single goroutine
func (se *SequenceExecutor) saveLoop() {
	for range se.saveCh {
		se.doSaveToDisk()
	}
}

// requestSave signals the save loop (non-blocking, coalesces rapid saves).
// Silently no-ops after Close() — an in-flight HTTP handler calling
// requestSave between Close() and server.Shutdown would otherwise send on a
// closed channel and panic the whole process.
//
// Uses the done channel (not an atomic.Bool) so the close-vs-send check is
// race-free: `<-done` becomes observable atomically with any subsequent
// close(saveCh), so the `select` can never observe done-still-open AND
// send-on-closed-saveCh in the same scheduling slot. An atomic.Bool check
// followed by a send has a sub-microsecond window where Close() can run
// between the two instructions.
func (se *SequenceExecutor) requestSave() {
	select {
	case <-se.done:
		return // shut down
	default:
	}
	select {
	case <-se.done:
		return
	case se.saveCh <- struct{}{}:
	default:
		// save already pending
	}
}

// Close shuts down the save loop and does a final flush to disk. Idempotent
// via sync.Once — main.go calls this during shutdown; tests may call it
// again during cleanup.
//
// Order matters: close(done) FIRST so any in-flight requestSave sees it
// and bails out; close(saveCh) second to let saveLoop drain and exit.
func (se *SequenceExecutor) Close() {
	se.closeOnce.Do(func() {
		close(se.done)
		close(se.saveCh)
		se.doSaveToDisk()
		// Stop any pending-failure timers so they don't fire after
		// shutdown and reach a half-torn-down executor.
		se.failureMu.Lock()
		for name, t := range se.pendingFailure {
			t.Stop()
			delete(se.pendingFailure, name)
		}
		se.failureMu.Unlock()
	})
}

// WireRestartManager connects the auto-restart manager to this executor.
// After wiring, the executor consumes auto-restart-recovered /
// auto-restart-exhausted events from `eb` and dispatches sequence
// reactions per OnRequiredFailure / OnRequiredRecovery, and calls
// rm.Suppress before its own start/stop work to prevent self-trigger.
//
// Safe to call once during boot; further calls overwrite the previous
// wiring (the previous goroutine drains and exits when `done` closes).
func (se *SequenceExecutor) WireRestartManager(rm *RestartManager, eb *EventBuffer) {
	se.mu.Lock()
	se.restartManager = rm
	se.events = eb
	se.mu.Unlock()
	if eb == nil || rm == nil {
		return
	}
	ch := eb.Subscribe()
	go se.restartEventLoop(ch)
}

// restartEventLoop reads auto-restart + lifecycle events from the
// EventBuffer and dispatches sequence reactions. Exits when the executor
// is Closed.
//
// Recovery cascade fires on ANY down→up transition of a Required
// container, regardless of cause: docker restart, the Restart button,
// the auto-restart manager, watchtower, Unraid Apply, restart-policy
// after a graceful exit. The state path is the canonical source —
// whenever the container went through state/died, state/stopped, or
// the high-level Docker state/restarted action and then comes back
// (state/started), we fire. The 30-second debounce timer no longer
// suppresses the recovery trigger: it only delays the FAILURE trigger,
// distinguishing "sustained outage" from "quick blip".
//
// Failure cascade fires when a Required has been down continuously for
// at least 30 seconds (state path debounce timer), OR immediately when
// the auto-restart manager gives up after MaxRestarts attempts
// (auto-restart-exhausted event from the manager — already confirmed).
//
// Suppressed containers (mid-sequence run, etc.) have all triggers
// ignored entirely.
//
// Note: auto-restart-recovered events are emitted by the manager but do
// NOT directly drive a cascade — the state events that the manager's
// own ContainerRestart triggers (die, restarted, started) already fire
// the recovery cascade via the state path. Letting auto-restart-recovered
// fire it again would double-cascade if its timing slipped past the
// state-path-driven cascade.
func (se *SequenceExecutor) restartEventLoop(ch chan Event) {
	// Unsubscribe on exit so the EventBuffer drops our channel from its
	// subscriber map. Without this, every Wire-call would leak one slot
	// (the goroutine would die at Close but the channel stays registered
	// and Add() keeps trying to send into it via select-default).
	defer func() {
		if se.events != nil {
			se.events.Unsubscribe(ch)
		}
	}()
	for {
		select {
		case <-se.done:
			return
		case e := <-ch:
			switch e.Type {
			case "restart":
				if e.Action == "auto-restart-exhausted" {
					// Mark failure as well so a later manual start
					// (after the user fixes the underlying problem)
					// fires the recovery trigger via the state path.
					se.markFailure(e.Container)
					se.dispatchTrigger(e.Container, "failure")
				}
			case "state":
				if se.isSuppressed(e.Container) || !se.hasRequiredMatch(e.Container) {
					continue
				}
				switch e.Action {
				case "died", "stopped", "restarted":
					// Arm the failure debounce timer. If state/started
					// arrives within stateDebounceWindow we cancel it
					// (and fire recovery instead — see below). If not,
					// the timer fires and dispatches the failure cascade.
					// state/restarted is Docker's high-level restart
					// action — covers graceful (exit-0) restart-policy
					// recoveries that don't emit state/died.
					se.armPendingFailure(e.Container)
				case "started":
					// Recovery cascade fires whenever the container is
					// coming back up after going down. cancelPendingFailure
					// catches the quick-restart case (timer was armed,
					// we're inside the debounce window). recoveryTriggerArmed
					// catches the post-failure case (timer already fired,
					// failure cascade already dispatched, marker armed
					// for up to 30 minutes). Always evaluate both so the
					// armed marker is consumed even when the timer was
					// also active.
					cancelled := se.cancelPendingFailure(e.Container)
					armed := se.recoveryTriggerArmed(e.Container)
					if cancelled || armed {
						se.dispatchTrigger(e.Container, "recovery")
					}
				}
			}
		}
	}
}

// hasRequiredMatch reports whether `name` is a Required step in any
// saved sequence that has at least one trigger configured. Used as an
// early filter in the state-event path so we don't burn timer slots on
// containers nobody's watching.
func (se *SequenceExecutor) hasRequiredMatch(name string) bool {
	se.mu.RLock()
	defer se.mu.RUnlock()
	for _, s := range se.sequences {
		if s.OnRequiredFailure == "" && s.OnRequiredRecovery == "" {
			continue
		}
		for _, step := range s.Steps {
			if step.Container == name && step.Required {
				return true
			}
		}
	}
	return false
}

// armPendingFailure starts (or refreshes) the debounce timer for `name`.
// If the timer fires without being cancelled, the failure is treated as
// sustained: we mark it and dispatch the trigger.
//
// The debounce duration comes from the matching sequence's
// OnRequiredFailureDelaySeconds. If multiple sequences have this
// container as Required + cascade-stop configured with different
// debounce values, the SHORTEST wins — the more-aggressive sequence
// fires first, the others would fire later via their own pendingFailure
// armed-at the next state event. (For the v1 single-timer model we
// accept this minor edge case rather than per-(container, seq) timers.)
func (se *SequenceExecutor) armPendingFailure(name string) {
	delay := se.failureDebounceFor(name)
	se.failureMu.Lock()
	if existing, ok := se.pendingFailure[name]; ok {
		existing.Stop()
	}
	timer := time.AfterFunc(delay, func() {
		se.failureMu.Lock()
		delete(se.pendingFailure, name)
		se.failureSeenAt[name] = time.Now()
		se.failureMu.Unlock()
		se.dispatchTrigger(name, "failure")
	})
	se.pendingFailure[name] = timer
	se.failureMu.Unlock()
}

// failureDebounceFor returns the failure-cascade debounce window to use
// for events on `container`. Walks all sequences with this container as
// a Required step and OnRequiredFailure == "stop", returning the shortest
// configured delay. nil OnRequiredFailureDelaySeconds defaults to
// stateDebounceWindow (30s); 0 means fire immediately.
func (se *SequenceExecutor) failureDebounceFor(container string) time.Duration {
	se.mu.RLock()
	defer se.mu.RUnlock()
	shortest := stateDebounceWindow
	found := false
	for _, s := range se.sequences {
		if s.OnRequiredFailure != "stop" {
			continue
		}
		for _, step := range s.Steps {
			if step.Container == container && step.Required {
				var d time.Duration
				if s.OnRequiredFailureDelaySeconds != nil {
					d = time.Duration(*s.OnRequiredFailureDelaySeconds) * time.Second
				} else {
					d = stateDebounceWindow
				}
				if !found || d < shortest {
					shortest = d
					found = true
				}
				break
			}
		}
	}
	return shortest
}

// cancelPendingFailure stops a pending-failure timer if one is armed for
// `name`. Returns true if a timer was active and was stopped — caller
// uses that to decide whether the started-event was part of a manual
// restart (true) or a recovery from a confirmed failure (false).
func (se *SequenceExecutor) cancelPendingFailure(name string) bool {
	se.failureMu.Lock()
	defer se.failureMu.Unlock()
	timer, ok := se.pendingFailure[name]
	if !ok {
		return false
	}
	stopped := timer.Stop()
	delete(se.pendingFailure, name)
	// Stop returns false if the timer already fired; in that case the
	// failure has been dispatched and this started-event genuinely
	// represents a recovery — let the caller fall through.
	return stopped
}

// markFailure records that we accepted a failure event for this
// container. The next started/recovered event consumes the record and
// fires the recovery trigger (if armed).
func (se *SequenceExecutor) markFailure(name string) {
	se.failureMu.Lock()
	defer se.failureMu.Unlock()
	se.failureSeenAt[name] = time.Now()
}

// recoveryTriggerArmed reports whether a recent failure was recorded for
// this container. Returns true (and clears the record) if so. Stale
// failures older than 30 min are dropped — if a container has been
// failing for that long without recovery, the user is unlikely to want
// the trigger to fire when it eventually comes back.
func (se *SequenceExecutor) recoveryTriggerArmed(name string) bool {
	se.failureMu.Lock()
	defer se.failureMu.Unlock()
	t, ok := se.failureSeenAt[name]
	if !ok {
		return false
	}
	delete(se.failureSeenAt, name)
	return time.Since(t) < 30*time.Minute
}

// isSuppressed wraps the optional restartManager.IsSuppressed query so
// the dispatcher works in tests / headless contexts where no manager is
// wired.
func (se *SequenceExecutor) isSuppressed(name string) bool {
	se.mu.RLock()
	rm := se.restartManager
	se.mu.RUnlock()
	if rm == nil {
		return false
	}
	return rm.IsSuppressed(name)
}

// dispatchTrigger walks all saved sequences. For each one that lists
// `container` as a Required step AND has the relevant trigger
// configured, it fires the cascade scoped to steps that come AFTER that
// Required step. Steps before the Required are not touched — they were
// already running independently of the Required dependency, by design.
func (se *SequenceExecutor) dispatchTrigger(container, kind string) {
	se.mu.RLock()
	type match struct {
		id     string
		action string
	}
	var matches []match
	for _, s := range se.sequences {
		var reaction string
		switch kind {
		case "failure":
			reaction = s.OnRequiredFailure
		case "recovery":
			reaction = s.OnRequiredRecovery
		}
		if reaction == "" || reaction == "none" {
			continue
		}
		for _, step := range s.Steps {
			if step.Container == container && step.Required {
				matches = append(matches, match{id: s.ID, action: reaction})
				break
			}
		}
	}
	se.mu.RUnlock()

	for _, m := range matches {
		err := se.CascadeFromRequired(m.id, container, m.action)
		if err != nil {
			log.Printf("SequenceExecutor: cascade %s for sequence %q triggered by %s of %q failed: %v", m.action, m.id, kind, container, err)
		} else {
			log.Printf("SequenceExecutor: cascade %s for sequence %q triggered by %s of %q", m.action, m.id, kind, container)
		}
	}
}

// CascadeFromRequired runs the trigger reaction scoped to steps AFTER
// the matching Required step. Mode:
//   - "stop": stop the dependents in reverse order.
//   - "restart": stop the dependents (in case some are still running),
//     then if the Required step has WaitHealthy set, wait for the
//     Required container to be healthy before starting the dependents
//     in forward order. The waitHealthy gate honors the same flag the
//     user already set on the Required step — a recovery cascade
//     respects the same dependency contract a normal start does.
//
// Steps BEFORE the Required step are never touched: by definition they
// don't depend on the Required, and stopping them would surprise the
// user. Only the dependents (steps[requiredIdx+1:]) get stopped/started.
func (se *SequenceExecutor) CascadeFromRequired(seqID, containerName, mode string) error {
	se.mu.Lock()
	if se.execution != nil && se.execution.Status == "running" {
		se.mu.Unlock()
		return ErrAlreadyRunning
	}
	var seq *Sequence
	for i := range se.sequences {
		if se.sequences[i].ID == seqID {
			seq = &se.sequences[i]
			break
		}
	}
	if seq == nil {
		se.mu.Unlock()
		return ErrSeqNotFound
	}

	requiredIdx := -1
	requiredWaitHealthy := false
	requiredDelay := 0
	for i, step := range seq.Steps {
		if step.Required && step.Container == containerName {
			requiredIdx = i
			requiredWaitHealthy = step.WaitHealthy
			requiredDelay = step.DelaySeconds
			break
		}
	}
	if requiredIdx < 0 {
		se.mu.Unlock()
		return fmt.Errorf("container %q is not a Required step in sequence %q", containerName, seqID)
	}

	// Build a sub-sequence for the dependents only. SkipRequired steps
	// after the Required one are excluded — they don't depend on the
	// Required by definition, so cascading stop/restart over them would
	// surprise the user. (The help text on the editor states this
	// explicitly under "About the auto-trigger options".)
	//
	// We allocate a fresh slice for sub.Steps rather than reslicing
	// seqCopy.Steps in place, so that suppressStepsForRun(seqCopy) still
	// sees every container in the original sequence (including the
	// SkipRequired ones we want to keep suppressed during the cascade).
	seqCopy := se.deepCopySequence(*seq)
	sub := seqCopy
	sub.Steps = make([]SeqStep, 0, len(seqCopy.Steps)-requiredIdx-1)
	for _, step := range seqCopy.Steps[requiredIdx+1:] {
		if step.SkipRequired {
			continue
		}
		sub.Steps = append(sub.Steps, step)
	}

	if len(sub.Steps) == 0 {
		// Required step is the last step, OR every following step is
		// SkipRequired — nothing to cascade.
		se.mu.Unlock()
		log.Printf("SequenceExecutor: cascade %s for %q in sequence %q is a no-op (no dependent steps)", mode, containerName, seqID)
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	se.cancelExec = cancel

	phase := ""
	if mode == "restart" {
		phase = "stopping"
	}
	se.execution = se.initExecution(sub, mode, phase)
	se.mu.Unlock()

	// Suppress only the dependents (sub.Steps), NOT the Required step.
	//
	// We need the Required step's natural lifecycle events (state/started
	// after a sustained outage, manager-driven restart, manual user
	// intervention, etc.) to flow through to the state path so the
	// recovery cascade can fire. Suppressing the Required step would
	// silently drop those events for the entire suppress window and break
	// the "Required came back up → restart dependents" trigger entirely.
	//
	// The dependents DO need suppression: their own stop/start events
	// from this cascade's work shouldn't loop back through the state path
	// and trigger nested cascades. Defense in depth — most dependents are
	// not Required steps anyway, but a sequence could chain multiple
	// auto-trigger-enabled sequences via shared containers.
	//
	// Letting the Required stay un-suppressed is also useful during the
	// cascade-restart's WaitHealthy gate: if the Required goes unhealthy
	// again mid-wait, the auto-restart manager can keep trying to
	// recover it (suppress would block OnUnhealthy from acting).
	se.suppressStepsForRun(sub)

	go se.runCascade(ctx, sub, containerName, requiredWaitHealthy, requiredDelay, mode)
	return nil
}

// runCascade is the goroutine entry point for CascadeFromRequired.
// requiredDelay is the Required step's DelaySeconds — applied between
// the WaitHealthy gate and the dependent-start phase so externally-
// triggered restarts (e.g., a backup script restarting a container
// outside constat) get the same post-start grace window as a manual
// sequence Run does.
func (se *SequenceExecutor) runCascade(ctx context.Context, sub Sequence, requiredName string, waitForReady bool, requiredDelay int, mode string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("SequenceExecutor: panic in cascade: %v", r)
			se.finishExecution("failed", "seq-failed", fmt.Sprintf("internal error: %v", r))
		}
	}()
	se.emitSeqEvent("sequence", "started", sub.Name, "cascade-"+mode+" (trigger: "+requiredName+")")

	idMap, err := se.resolveAllContainerIDs(ctx)
	if err != nil {
		se.finishExecution("failed", "seq-failed", err.Error())
		return
	}

	switch mode {
	case "stop":
		if se.executeStopSteps(ctx, sub, idMap) {
			se.finishExecution("complete", "seq-complete", "")
		}
	case "restart":
		// Phase 1: stop the dependents (no-op for ones already stopped).
		if !se.executeStopSteps(ctx, sub, idMap) {
			return // already finalized as failed/aborted
		}

		// Phase 2a: wait for the Required step to be ready, if the user
		// set Wait healthy on it. Same contract as a normal sequence
		// start — Required + Wait healthy means "nothing after this
		// runs until it's healthy".
		if waitForReady {
			// Surface the wait phase to the UI — without this update,
			// the live progress bar shows the post-Phase-1 state for
			// up to 2 minutes while we silently poll Docker. Frontend
			// renders "Waiting for <required> to be healthy..." when
			// it sees this Phase value.
			se.mu.Lock()
			if se.execution != nil {
				se.execution.Phase = "waiting-healthy"
			}
			se.mu.Unlock()
			se.broadcastUpdate()
			se.emitSeqEvent("sequence", "waiting-healthy", requiredName, "")

			if requiredID, ok := idMap[requiredName]; ok {
				if err := se.waitForHealthy(ctx, requiredID, 2*time.Minute); err != nil {
					se.finishExecution("failed", "seq-failed", fmt.Sprintf("required %q never became healthy: %v", requiredName, err))
					return
				}
			} else {
				log.Printf("SequenceExecutor: cascade restart could not resolve required %q for healthy gate; proceeding without wait", requiredName)
			}
		}

		// Phase 2b: apply the Required step's configured Delay before
		// starting dependents. Mirrors the normal sequence Run contract
		// where Delay sits between the Required step's "done" state and
		// the next group's start — without this, externally-triggered
		// restarts (e.g., a backup script that docker-restarts the
		// Required outside constat) would skip the wait the user
		// configured on the Required step.
		if requiredDelay > 0 {
			// Surface the wait to the UI. The Phase value carries the
			// total duration; PhaseStartedAt anchors the live countdown.
			// Frontend uses both to render "Required step's Delay —
			// Xs remaining (of Ns)" in the live progress text.
			se.mu.Lock()
			if se.execution != nil {
				se.execution.Phase = fmt.Sprintf("waiting-required-delay-%d", requiredDelay)
				se.execution.PhaseStartedAt = time.Now().UTC()
			}
			se.mu.Unlock()
			se.broadcastUpdate()
			se.emitSeqEvent("sequence", "delaying", requiredName, fmt.Sprintf("Required step's Delay (%ds)", requiredDelay))
			log.Printf("SequenceExecutor: cascade restart applying %ds Required-step Delay on %q before starting dependents", requiredDelay, requiredName)
			select {
			case <-ctx.Done():
				se.finishExecution("aborted", "seq-aborted", "")
				return
			case <-time.After(time.Duration(requiredDelay) * time.Second):
			}
		}

		// Re-resolve IDs after the wait; some dependents may have
		// changed state during the gate.
		idMap, err = se.resolveAllContainerIDs(ctx)
		if err != nil {
			se.finishExecution("failed", "seq-failed", err.Error())
			return
		}

		// Phase 3: start the dependents in forward order.
		se.mu.Lock()
		se.execution = se.initExecution(sub, "restart", "starting")
		se.mu.Unlock()
		se.broadcastUpdate()

		if se.executeStartSteps(ctx, sub, idMap) {
			se.finishExecution("complete", "seq-complete", "")
		}
	}
}

// suppressStepsForRun mutes the auto-restart manager for every container
// in `seq` so the sequence's own stops/starts do not feed the manager
// their unhealthy/healthy churn (which would otherwise loop us back
// through restartEventLoop). Suppression length is generous (10 min);
// the manager naturally resumes once the window expires.
func (se *SequenceExecutor) suppressStepsForRun(seq Sequence) {
	se.mu.RLock()
	rm := se.restartManager
	se.mu.RUnlock()
	if rm == nil {
		return
	}
	for _, step := range seq.Steps {
		rm.Suppress(step.Container, 10*time.Minute)
	}
}

// --- CRUD ---

// List returns all saved sequences
func (se *SequenceExecutor) List() []Sequence {
	se.mu.RLock()
	defer se.mu.RUnlock()
	return se.copySequences()
}

// Get returns a sequence by ID
func (se *SequenceExecutor) Get(id string) (*Sequence, bool) {
	se.mu.RLock()
	defer se.mu.RUnlock()
	for i := range se.sequences {
		if se.sequences[i].ID == id {
			s := se.deepCopySequence(se.sequences[i])
			return &s, true
		}
	}
	return nil, false
}

// Create adds a new sequence and persists
func (se *SequenceExecutor) Create(seq Sequence) (Sequence, error) {
	if err := se.validateSequence(&seq); err != nil {
		return Sequence{}, err
	}
	se.mu.Lock()
	seq.ID = se.generateSeqID(seq.Name)
	se.sequences = append(se.sequences, seq)
	se.mu.Unlock()
	se.requestSave()
	return seq, nil
}

// Update modifies an existing sequence and persists
func (se *SequenceExecutor) Update(id string, seq Sequence) (Sequence, error) {
	if err := se.validateSequence(&seq); err != nil {
		return Sequence{}, err
	}
	se.mu.Lock()
	defer se.mu.Unlock()
	for i := range se.sequences {
		if se.sequences[i].ID == id {
			seq.ID = id
			seq.LastRun = se.sequences[i].LastRun
			seq.LastResult = se.sequences[i].LastResult
			se.sequences[i] = seq
			se.requestSave()
			return seq, nil
		}
	}
	return Sequence{}, ErrSeqNotFound
}

// Delete removes a sequence by ID
func (se *SequenceExecutor) Delete(id string) error {
	se.mu.Lock()
	defer se.mu.Unlock()
	for i := range se.sequences {
		if se.sequences[i].ID == id {
			se.sequences = append(se.sequences[:i], se.sequences[i+1:]...)
			se.requestSave()
			return nil
		}
	}
	return ErrSeqNotFound
}

// --- Validation ---

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func (se *SequenceExecutor) validateSequence(seq *Sequence) error {
	seq.Name = strings.TrimSpace(seq.Name)
	if seq.Name == "" {
		return fmt.Errorf("name is required")
	}
	if len(seq.Name) > 100 {
		return fmt.Errorf("name too long (max 100 characters)")
	}
	seq.Description = strings.TrimSpace(seq.Description)
	if len(seq.Description) > 500 {
		return fmt.Errorf("description too long (max 500 characters)")
	}
	if len(seq.Steps) == 0 {
		return fmt.Errorf("at least one step is required")
	}
	if len(seq.Steps) > 50 {
		return fmt.Errorf("maximum 50 steps allowed")
	}
	seen := make(map[string]bool, len(seq.Steps))
	for i := range seq.Steps {
		seq.Steps[i].Container = strings.TrimSpace(seq.Steps[i].Container)
		if seq.Steps[i].Container == "" {
			return fmt.Errorf("step container name cannot be empty")
		}
		if seen[seq.Steps[i].Container] {
			return fmt.Errorf("duplicate container: %s", seq.Steps[i].Container)
		}
		seen[seq.Steps[i].Container] = true
		if seq.Steps[i].DelaySeconds < 0 || seq.Steps[i].DelaySeconds > 300 {
			return fmt.Errorf("step delay must be 0-300 seconds")
		}
		if seq.Steps[i].Required && seq.Steps[i].SkipRequired {
			return fmt.Errorf("step %s cannot be both Required and Skip required", seq.Steps[i].Container)
		}
	}
	// Normalise + whitelist the new trigger fields. Empty string is the
	// canonical "do nothing" value; we accept "none" as a synonym from the
	// UI (its dropdown emits the explicit value) and persist as "".
	switch seq.OnRequiredFailure {
	case "", "none":
		seq.OnRequiredFailure = ""
		// Drop any leftover delay value if trigger is unset — the field
		// has no meaning without the failure trigger configured.
		seq.OnRequiredFailureDelaySeconds = nil
	case "stop":
		// ok
	default:
		return fmt.Errorf("onRequiredFailure must be 'none' or 'stop'")
	}
	if seq.OnRequiredFailureDelaySeconds != nil {
		v := *seq.OnRequiredFailureDelaySeconds
		if v < 0 || v > 300 {
			return fmt.Errorf("onRequiredFailureDelaySeconds must be between 0 and 300")
		}
	}
	switch seq.OnRequiredRecovery {
	case "", "none":
		seq.OnRequiredRecovery = ""
	case "restart":
		// ok
	default:
		return fmt.Errorf("onRequiredRecovery must be 'none' or 'restart'")
	}
	return nil
}

func (se *SequenceExecutor) generateSeqID(name string) string {
	slug := slugRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-")
	slug = strings.Trim(slug, "-")
	if slug == "" {
		slug = "sequence"
	}
	// Block reserved slugs that conflict with API routes
	if reservedSlugs[slug] {
		slug = slug + "-seq"
	}
	// Check for duplicates
	base := slug
	suffix := 2
	for {
		found := false
		for _, s := range se.sequences {
			if s.ID == slug {
				found = true
				break
			}
		}
		if !found {
			return slug
		}
		slug = fmt.Sprintf("%s-%d", base, suffix)
		suffix++
	}
}

// --- Deep copy helpers ---

func (se *SequenceExecutor) deepCopySequence(s Sequence) Sequence {
	c := s
	c.Steps = make([]SeqStep, len(s.Steps))
	copy(c.Steps, s.Steps)
	return c
}

func (se *SequenceExecutor) copySequences() []Sequence {
	result := make([]Sequence, len(se.sequences))
	for i, s := range se.sequences {
		result[i] = se.deepCopySequence(s)
	}
	return result
}

// --- Persistence ---

type sequencesFile struct {
	Sequences []Sequence `json:"sequences"`
}

func (se *SequenceExecutor) loadFromDisk() {
	data, err := os.ReadFile(sequencesPath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("SequenceExecutor: failed to read sequences: %v", err)
		}
		return
	}
	var file sequencesFile
	if err := json.Unmarshal(data, &file); err != nil {
		log.Printf("SequenceExecutor: failed to parse sequences: %v", err)
		return
	}
	se.mu.Lock()
	se.sequences = file.Sequences
	if se.sequences == nil {
		se.sequences = []Sequence{}
	}
	se.mu.Unlock()
	log.Printf("SequenceExecutor: loaded %d sequences from disk", len(se.sequences))
}

func (se *SequenceExecutor) doSaveToDisk() {
	// Deep-copy under lock to avoid racing with concurrent modifications
	se.mu.RLock()
	seqs := se.copySequences()
	se.mu.RUnlock()

	file := sequencesFile{Sequences: seqs}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		log.Printf("SequenceExecutor: failed to marshal sequences: %v", err)
		return
	}

	// Atomic write: write to temp file, then rename
	tmpPath := sequencesPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0664); err != nil {
		log.Printf("SequenceExecutor: failed to write temp sequences file: %v", err)
		return
	}
	if err := os.Rename(tmpPath, sequencesPath); err != nil {
		log.Printf("SequenceExecutor: failed to rename sequences file: %v", err)
		return
	}
	if err := os.Chown(sequencesPath, 99, 100); err != nil {
		log.Printf("SequenceExecutor: failed to chown sequences file: %v", err)
	}
}

// --- SSE ---

// SubscribeSeq creates a channel that receives sequence events
func (se *SequenceExecutor) SubscribeSeq() chan SeqEvent {
	ch := make(chan SeqEvent, 64)
	se.subMu.Lock()
	se.subscribers[ch] = struct{}{}
	se.subMu.Unlock()
	return ch
}

// UnsubscribeSeq removes a subscriber channel
func (se *SequenceExecutor) UnsubscribeSeq(ch chan SeqEvent) {
	se.subMu.Lock()
	delete(se.subscribers, ch)
	se.subMu.Unlock()
}

func (se *SequenceExecutor) broadcast(event SeqEvent) {
	se.subMu.Lock()
	subs := make([]chan SeqEvent, 0, len(se.subscribers))
	for ch := range se.subscribers {
		subs = append(subs, ch)
	}
	se.subMu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- event:
		default:
		}
	}
}

// GetExecution returns the current execution state (nil if none)
func (se *SequenceExecutor) GetExecution() *SeqExecution {
	se.mu.RLock()
	defer se.mu.RUnlock()
	if se.execution == nil {
		return nil
	}
	exec := se.snapshotExecution()
	return &exec
}

// snapshotExecution creates a deep copy of the current execution with computed elapsed.
// Caller must hold at least se.mu.RLock.
func (se *SequenceExecutor) snapshotExecution() SeqExecution {
	exec := *se.execution
	exec.Elapsed = time.Since(se.execution.StartedAt).Seconds()
	exec.Steps = make([]SeqStepState, len(se.execution.Steps))
	copy(exec.Steps, se.execution.Steps)
	return exec
}

func (se *SequenceExecutor) broadcastUpdate() {
	se.mu.RLock()
	if se.execution == nil {
		se.mu.RUnlock()
		return
	}
	exec := se.snapshotExecution()
	se.mu.RUnlock()
	se.broadcast(SeqEvent{Type: "seq-update", Data: exec})
}

func (se *SequenceExecutor) finishExecution(status, eventType, errMsg string) {
	se.mu.Lock()
	if se.execution == nil {
		se.mu.Unlock()
		return
	}
	se.execution.Status = status
	if errMsg != "" {
		se.execution.Error = errMsg
	}
	exec := se.snapshotExecution()
	se.cancelExec = nil

	// Update lastRun/lastResult on the saved sequence
	seqName := exec.SequenceID
	for i := range se.sequences {
		if se.sequences[i].ID == exec.SequenceID {
			se.sequences[i].LastRun = time.Now().UTC().Format(time.RFC3339)
			se.sequences[i].LastResult = status
			seqName = se.sequences[i].Name
			break
		}
	}
	se.mu.Unlock()

	// Emit lifecycle event so the events feed gets a closing event
	// matching the "started" event from runExecution / runCascade. The
	// detail field carries either elapsed time or the error message.
	var lifecycleAction string
	switch status {
	case "complete":
		lifecycleAction = "completed"
	case "aborted":
		lifecycleAction = "aborted"
	default:
		lifecycleAction = "failed"
	}
	detail := fmt.Sprintf("%.1fs", exec.Elapsed)
	if errMsg != "" {
		detail = errMsg
	}
	se.emitSeqEvent("sequence", lifecycleAction, seqName, detail)

	se.broadcast(SeqEvent{Type: eventType, Data: exec})
	se.requestSave()
}

// skipRemaining marks all waiting steps from fromIdx as skipped.
// Caller must hold se.mu.Lock().
func (se *SequenceExecutor) skipRemaining(fromIdx int) {
	for i := fromIdx; i < len(se.execution.Steps); i++ {
		if se.execution.Steps[i].Status == "waiting" {
			se.execution.Steps[i].Status = "skipped"
		}
	}
}

// --- Execution Engine ---

// StartSequence begins a start-mode execution
func (se *SequenceExecutor) StartSequence(id string) error {
	se.mu.Lock()
	if se.execution != nil && se.execution.Status == "running" {
		se.mu.Unlock()
		return ErrAlreadyRunning
	}
	var seq *Sequence
	for i := range se.sequences {
		if se.sequences[i].ID == id {
			seq = &se.sequences[i]
			break
		}
	}
	if seq == nil {
		se.mu.Unlock()
		return ErrSeqNotFound
	}
	seqCopy := se.deepCopySequence(*seq)

	ctx, cancel := context.WithCancel(context.Background())
	se.cancelExec = cancel
	se.execution = se.initExecution(seqCopy, "start", "")
	se.mu.Unlock()

	se.suppressStepsForRun(seqCopy)
	go se.runExecution(ctx, seqCopy, "start")
	return nil
}

// StopSequence begins a stop-mode execution (reverse order)
func (se *SequenceExecutor) StopSequence(id string) error {
	se.mu.Lock()
	if se.execution != nil && se.execution.Status == "running" {
		se.mu.Unlock()
		return ErrAlreadyRunning
	}
	var seq *Sequence
	for i := range se.sequences {
		if se.sequences[i].ID == id {
			seq = &se.sequences[i]
			break
		}
	}
	if seq == nil {
		se.mu.Unlock()
		return ErrSeqNotFound
	}
	seqCopy := se.deepCopySequence(*seq)

	ctx, cancel := context.WithCancel(context.Background())
	se.cancelExec = cancel
	se.execution = se.initExecution(seqCopy, "stop", "")
	se.mu.Unlock()

	se.suppressStepsForRun(seqCopy)
	go se.runExecution(ctx, seqCopy, "stop")
	return nil
}

// RestartSequence stops then starts
func (se *SequenceExecutor) RestartSequence(id string) error {
	se.mu.Lock()
	if se.execution != nil && se.execution.Status == "running" {
		se.mu.Unlock()
		return ErrAlreadyRunning
	}
	var seq *Sequence
	for i := range se.sequences {
		if se.sequences[i].ID == id {
			seq = &se.sequences[i]
			break
		}
	}
	if seq == nil {
		se.mu.Unlock()
		return ErrSeqNotFound
	}
	seqCopy := se.deepCopySequence(*seq)

	ctx, cancel := context.WithCancel(context.Background())
	se.cancelExec = cancel
	se.execution = se.initExecution(seqCopy, "restart", "stopping")
	se.mu.Unlock()

	se.suppressStepsForRun(seqCopy)
	go se.runExecution(ctx, seqCopy, "restart")
	return nil
}

// AbortExecution cancels the running execution
func (se *SequenceExecutor) AbortExecution() error {
	se.mu.Lock()
	defer se.mu.Unlock()
	if se.execution == nil || se.execution.Status != "running" {
		return ErrNotRunning
	}
	if se.cancelExec != nil {
		se.cancelExec()
	}
	return nil
}

func (se *SequenceExecutor) initExecution(seq Sequence, mode, phase string) *SeqExecution {
	steps := make([]SeqStepState, len(seq.Steps))
	for i, s := range seq.Steps {
		steps[i] = SeqStepState{
			Container: s.Container,
			Required:  s.Required,
			Status:    "waiting",
		}
	}
	// Preserve RunID across phase transitions of the same logical run
	// (restart mode's stop→start phases, cascade restart's stop→start
	// cascade) so all events emitted during one user-visible run share
	// the same group ID in the events feed. Caller must hold se.mu.Lock.
	runID := ""
	if se.execution != nil && se.execution.SequenceID == seq.ID {
		runID = se.execution.RunID
	}
	if runID == "" {
		runID = fmt.Sprintf("seqrun_%d", time.Now().UnixNano())
	}
	return &SeqExecution{
		SequenceID:  seq.ID,
		Mode:        mode,
		Phase:       phase,
		Status:      "running",
		CurrentStep: 0,
		TotalSteps:  len(seq.Steps),
		StartedAt:   time.Now().UTC(),
		Steps:       steps,
		RunID:       runID,
	}
}

// emitSeqEvent publishes a sequence-related event into the shared
// EventBuffer. The current execution's RunID is attached so the events
// feed can group all emissions from one run under a single expandable
// parent. Safe to call when events == nil (tests / headless).
func (se *SequenceExecutor) emitSeqEvent(eventType, action, container, detail string) {
	se.mu.RLock()
	eb := se.events
	runID := ""
	if se.execution != nil {
		runID = se.execution.RunID
	}
	se.mu.RUnlock()
	if eb == nil {
		return
	}
	eb.Add(Event{
		Timestamp:     time.Now(),
		Container:     container,
		Type:          eventType,
		Action:        action,
		Detail:        detail,
		SequenceRunID: runID,
	})
}

// seqDisplayName returns the user-visible name for a sequence ID. Used
// as the Container field on lifecycle events so the events feed shows
// the human-friendly sequence name rather than the slug ID.
func (se *SequenceExecutor) seqDisplayName(seqID string) string {
	se.mu.RLock()
	defer se.mu.RUnlock()
	for i := range se.sequences {
		if se.sequences[i].ID == seqID {
			return se.sequences[i].Name
		}
	}
	return seqID
}

// runExecution is the unified entry point for all execution modes
func (se *SequenceExecutor) runExecution(ctx context.Context, seq Sequence, mode string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("SequenceExecutor: panic in execution: %v", r)
			se.finishExecution("failed", "seq-failed", fmt.Sprintf("internal error: %v", r))
		}
	}()
	se.emitSeqEvent("sequence", "started", seq.Name, mode)

	// Resolve all container name→ID mappings once
	idMap, err := se.resolveAllContainerIDs(ctx)
	if err != nil {
		se.finishExecution("failed", "seq-failed", err.Error())
		return
	}

	switch mode {
	case "start":
		ok := se.executeStartSteps(ctx, seq, idMap)
		if ok {
			se.finishExecution("complete", "seq-complete", "")
		}
	case "stop":
		ok := se.executeStopSteps(ctx, seq, idMap)
		if ok {
			se.finishExecution("complete", "seq-complete", "")
		}
	case "restart":
		// Phase 1: Stop
		ok := se.executeStopSteps(ctx, seq, idMap)
		if !ok {
			return // stop failed or aborted — already finalized
		}

		// Re-resolve IDs for start phase (containers may have changed)
		idMap, err = se.resolveAllContainerIDs(ctx)
		if err != nil {
			se.finishExecution("failed", "seq-failed", err.Error())
			return
		}

		// Phase 2: Reset execution for start phase
		se.mu.Lock()
		se.execution = se.initExecution(seq, "restart", "starting")
		se.mu.Unlock()
		se.broadcastUpdate()

		ok = se.executeStartSteps(ctx, seq, idMap)
		if ok {
			se.finishExecution("complete", "seq-complete", "")
		}
	}
}

// executeStartSteps runs start-mode steps. Returns true if the run finished
// (successfully or with a Required failure that didn't abort the run), false
// if it was aborted by ctx cancellation.
//
// Chain-broken behavior: when a Required step fails, the run does NOT bail.
// Instead chainBroken flips on, and for every subsequent group the executor
// runs only SkipRequired steps; ordinary steps in those groups are marked
// skipped. This lets users mark steps that don't depend on a prior Required
// as Skip required so they keep running through partial failures (e.g. Plex
// coming after a qBittorrent Required step that's gone unhealthy).
//
// If chainBroken is set at the end, the run is finalized as failed; otherwise
// complete. The first failure's error message is preserved.
func (se *SequenceExecutor) executeStartSteps(ctx context.Context, seq Sequence, idMap map[string]string) bool {
	se.broadcastUpdate()

	groups := se.buildGroups(seq.Steps)
	stepOffset := 0
	chainBroken := false
	var firstErr error
	for _, group := range groups {
		if err := ctx.Err(); err != nil {
			se.mu.Lock()
			se.skipRemaining(stepOffset)
			se.mu.Unlock()
			se.finishExecution("aborted", "seq-aborted", "")
			return false
		}

		runGroup := group
		if chainBroken {
			// Filter to SkipRequired steps only; mark the rest skipped in
			// place so the UI shows them as "did not run because of upstream
			// failure" rather than leaving them in "waiting" forever.
			//
			// Allocate a fresh backing array (`make` instead of `group[:0]`)
			// so subsequent appends don't overwrite group's backing array
			// while we're still iterating it. The aliasing happens to be
			// safe for THIS iteration pattern (we always write at index <
			// the iteration index), but it's brittle and obscures intent.
			runGroup = make([]SeqStep, 0, len(group))
			// Collect the skipped steps so we can emit events outside the
			// mu-lock — emitSeqEvent needs RLock, which would deadlock.
			var skippedNames []string
			se.mu.Lock()
			for j, step := range group {
				if step.SkipRequired {
					runGroup = append(runGroup, step)
				} else if se.execution != nil && stepOffset+j < len(se.execution.Steps) {
					if se.execution.Steps[stepOffset+j].Status == "waiting" {
						se.execution.Steps[stepOffset+j].Status = "skipped"
						skippedNames = append(skippedNames, step.Container)
					}
				}
			}
			se.mu.Unlock()
			for _, name := range skippedNames {
				se.emitSeqEvent("sequence", "skipped", name, "upstream Required failed")
			}
		}

		if len(runGroup) > 0 {
			if err := se.executeGroupStart(ctx, runGroup, stepOffset, idMap); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				chainBroken = true
			}
		}
		stepOffset += len(group)
	}
	if chainBroken {
		msg := ""
		if firstErr != nil {
			msg = firstErr.Error()
		}
		se.finishExecution("failed", "seq-failed", msg)
		return false
	}
	return true
}

// executeStopSteps runs stop-mode steps (reverse order). Returns true if all completed.
func (se *SequenceExecutor) executeStopSteps(ctx context.Context, seq Sequence, idMap map[string]string) bool {
	se.broadcastUpdate()

	// Reverse step order for stopping
	reversed := make([]SeqStep, len(seq.Steps))
	for i, s := range seq.Steps {
		reversed[len(seq.Steps)-1-i] = s
	}

	// Also reverse the execution step states
	se.mu.Lock()
	for i, j := 0, len(se.execution.Steps)-1; i < j; i, j = i+1, j-1 {
		se.execution.Steps[i], se.execution.Steps[j] = se.execution.Steps[j], se.execution.Steps[i]
	}
	se.mu.Unlock()

	groups := se.buildGroups(reversed)
	stepOffset := 0
	for _, group := range groups {
		if err := ctx.Err(); err != nil {
			se.mu.Lock()
			se.skipRemaining(stepOffset)
			se.mu.Unlock()
			se.finishExecution("aborted", "seq-aborted", "")
			return false
		}

		if err := se.executeGroupStop(ctx, group, stepOffset, idMap); err != nil {
			se.mu.Lock()
			se.skipRemaining(stepOffset + len(group))
			se.mu.Unlock()
			se.finishExecution("failed", "seq-failed", err.Error())
			return false
		}
		stepOffset += len(group)
	}
	return true
}

// buildGroups splits steps into groups. Each group contains steps that run in parallel,
// ending at a required step (which gates the next group).
func (se *SequenceExecutor) buildGroups(steps []SeqStep) [][]SeqStep {
	var groups [][]SeqStep
	var current []SeqStep

	for _, step := range steps {
		current = append(current, step)
		if step.Required {
			groups = append(groups, current)
			current = nil
		}
	}
	if len(current) > 0 {
		groups = append(groups, current)
	}
	return groups
}

// executeGroupStart starts all containers in a group in parallel, then waits for
// the required step (if any) to be healthy before returning
func (se *SequenceExecutor) executeGroupStart(ctx context.Context, group []SeqStep, offset int, idMap map[string]string) error {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var groupErr error

	for i, step := range group {
		stepIdx := offset + i
		wg.Add(1)
		go func(step SeqStep, idx int) {
			defer wg.Done()
			err := se.startSingleContainer(ctx, step, idx, idMap)
			if err != nil && step.Required {
				mu.Lock()
				if groupErr == nil {
					groupErr = fmt.Errorf("required step %s failed: %v", step.Container, err)
				}
				mu.Unlock()
			}
		}(step, stepIdx)
	}

	wg.Wait()

	// Update CurrentStep to end of group (avoids nondeterministic parallel writes)
	se.mu.Lock()
	se.execution.CurrentStep = offset + len(group)
	se.mu.Unlock()

	return groupErr
}

// executeGroupStop stops all containers in a group in parallel
func (se *SequenceExecutor) executeGroupStop(ctx context.Context, group []SeqStep, offset int, idMap map[string]string) error {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var groupErr error

	for i, step := range group {
		stepIdx := offset + i
		wg.Add(1)
		go func(step SeqStep, idx int) {
			defer wg.Done()
			err := se.stopSingleContainer(ctx, step, idx, idMap)
			if err != nil && step.Required {
				mu.Lock()
				if groupErr == nil {
					groupErr = fmt.Errorf("required step %s failed: %v", step.Container, err)
				}
				mu.Unlock()
			}
		}(step, stepIdx)
	}

	wg.Wait()

	se.mu.Lock()
	se.execution.CurrentStep = offset + len(group)
	se.mu.Unlock()

	return groupErr
}

// startSingleContainer starts one container, optionally waits for healthy
func (se *SequenceExecutor) startSingleContainer(ctx context.Context, step SeqStep, idx int, idMap map[string]string) error {
	start := time.Now()

	// Mark as starting
	se.mu.Lock()
	se.execution.Steps[idx].Status = "starting"
	se.execution.Steps[idx].Log = "Starting container..."
	se.mu.Unlock()
	se.broadcastUpdate()
	se.emitSeqEvent("sequence", "starting", step.Container, "")

	// Look up container ID from pre-resolved map
	containerID, ok := idMap[step.Container]
	if !ok {
		if step.Required {
			se.mu.Lock()
			se.execution.Steps[idx].Status = "failed"
			se.execution.Steps[idx].Elapsed = time.Since(start).Seconds()
			se.execution.Steps[idx].Log = fmt.Sprintf("Container not found: %s", step.Container)
			se.mu.Unlock()
			se.broadcastUpdate()
			se.emitSeqEvent("sequence", "failed", step.Container, "container not found")
			return fmt.Errorf("container not found: %s", step.Container)
		}
		// Non-required: skip directly (no failed flash)
		se.mu.Lock()
		se.execution.Steps[idx].Status = "skipped"
		se.execution.Steps[idx].Elapsed = time.Since(start).Seconds()
		se.execution.Steps[idx].Log = "Skipped — container not found"
		se.mu.Unlock()
		se.broadcastUpdate()
		se.emitSeqEvent("sequence", "skipped", step.Container, "container not found")
		return nil
	}

	// Check if already running
	inspectCtx, inspectCancel := context.WithTimeout(ctx, 5*time.Second)
	inspect, err := se.docker.ContainerInspect(inspectCtx, containerID)
	inspectCancel()
	if err == nil && inspect.State.Running {
		se.mu.Lock()
		se.execution.Steps[idx].Status = "done"
		se.execution.Steps[idx].Elapsed = time.Since(start).Seconds()
		se.execution.Steps[idx].Log = "Already running"
		se.mu.Unlock()
		se.broadcastUpdate()
		se.emitSeqEvent("sequence", "done", step.Container, "already running")
		return nil
	}

	// Start container
	startCtx, startCancel := context.WithTimeout(ctx, 10*time.Second)
	err = se.docker.ContainerStart(startCtx, containerID, container.StartOptions{})
	startCancel()
	if err != nil {
		se.mu.Lock()
		se.execution.Steps[idx].Status = "failed"
		se.execution.Steps[idx].Elapsed = time.Since(start).Seconds()
		se.execution.Steps[idx].Log = fmt.Sprintf("Failed to start: %v", err)
		se.mu.Unlock()
		se.broadcastUpdate()
		se.emitSeqEvent("sequence", "failed", step.Container, err.Error())
		return err
	}

	// If waitHealthy, poll for health
	if step.Required && step.WaitHealthy {
		se.mu.Lock()
		se.execution.Steps[idx].Log = "Started — waiting for healthy..."
		se.mu.Unlock()
		se.broadcastUpdate()
		se.emitSeqEvent("sequence", "waiting-healthy", step.Container, "")

		if err := se.waitForHealthy(ctx, containerID, 30*time.Second); err != nil {
			se.mu.Lock()
			se.execution.Steps[idx].Status = "failed"
			se.execution.Steps[idx].Elapsed = time.Since(start).Seconds()
			se.execution.Steps[idx].Log = fmt.Sprintf("Health check failed: %v", err)
			se.mu.Unlock()
			se.broadcastUpdate()
			se.emitSeqEvent("sequence", "failed", step.Container, "health check failed: "+err.Error())
			return err
		}
	}

	se.mu.Lock()
	se.execution.Steps[idx].Status = "done"
	se.execution.Steps[idx].Elapsed = time.Since(start).Seconds()
	if step.WaitHealthy {
		se.execution.Steps[idx].Log = "Started — health check passed"
	} else {
		se.execution.Steps[idx].Log = "Started"
	}
	se.mu.Unlock()
	se.broadcastUpdate()
	if step.DelaySeconds == 0 {
		// Only emit "done" once per step; if a delay follows we emit it
		// after the delay completes (below).
		se.emitSeqEvent("sequence", "done", step.Container, fmt.Sprintf("%.1fs", time.Since(start).Seconds()))
	} else {
		se.emitSeqEvent("sequence", "started", step.Container, fmt.Sprintf("%.1fs", time.Since(start).Seconds()))
	}

	// Apply post-start delay if configured
	if step.DelaySeconds > 0 {
		se.mu.Lock()
		se.execution.Steps[idx].Status = "delaying"
		se.execution.Steps[idx].Log = fmt.Sprintf("Waiting %ds after start...", step.DelaySeconds)
		se.mu.Unlock()
		se.broadcastUpdate()
		se.emitSeqEvent("sequence", "delaying", step.Container, fmt.Sprintf("%ds", step.DelaySeconds))

		select {
		case <-ctx.Done():
			se.mu.Lock()
			se.execution.Steps[idx].Status = "done"
			se.execution.Steps[idx].Elapsed = time.Since(start).Seconds()
			se.execution.Steps[idx].Log = "Delay interrupted"
			se.mu.Unlock()
			se.broadcastUpdate()
			se.emitSeqEvent("sequence", "done", step.Container, "delay interrupted")
			return nil // container started OK; let outer loop detect abort via ctx
		case <-time.After(time.Duration(step.DelaySeconds) * time.Second):
		}

		se.mu.Lock()
		se.execution.Steps[idx].Status = "done"
		se.execution.Steps[idx].Elapsed = time.Since(start).Seconds()
		se.execution.Steps[idx].Log = fmt.Sprintf("Started — %ds delay complete", step.DelaySeconds)
		se.mu.Unlock()
		se.broadcastUpdate()
		se.emitSeqEvent("sequence", "done", step.Container, fmt.Sprintf("%.1fs (incl. %ds delay)", time.Since(start).Seconds(), step.DelaySeconds))
	}

	return nil
}

// stopSingleContainer stops one container
func (se *SequenceExecutor) stopSingleContainer(ctx context.Context, step SeqStep, idx int, idMap map[string]string) error {
	start := time.Now()

	se.mu.Lock()
	se.execution.Steps[idx].Status = "stopping"
	se.execution.Steps[idx].Log = "Stopping container..."
	se.mu.Unlock()
	se.broadcastUpdate()
	se.emitSeqEvent("sequence", "stopping", step.Container, "")

	containerID, ok := idMap[step.Container]
	if !ok {
		if step.Required {
			se.mu.Lock()
			se.execution.Steps[idx].Status = "failed"
			se.execution.Steps[idx].Elapsed = time.Since(start).Seconds()
			se.execution.Steps[idx].Log = fmt.Sprintf("Container not found: %s", step.Container)
			se.mu.Unlock()
			se.broadcastUpdate()
			se.emitSeqEvent("sequence", "failed", step.Container, "container not found")
			return fmt.Errorf("container not found: %s", step.Container)
		}
		se.mu.Lock()
		se.execution.Steps[idx].Status = "skipped"
		se.execution.Steps[idx].Elapsed = time.Since(start).Seconds()
		se.execution.Steps[idx].Log = "Skipped — container not found"
		se.mu.Unlock()
		se.broadcastUpdate()
		se.emitSeqEvent("sequence", "skipped", step.Container, "container not found")
		return nil
	}

	// Check if already stopped
	inspectCtx, inspectCancel := context.WithTimeout(ctx, 5*time.Second)
	inspect, err := se.docker.ContainerInspect(inspectCtx, containerID)
	inspectCancel()
	if err == nil && !inspect.State.Running {
		se.mu.Lock()
		se.execution.Steps[idx].Status = "done"
		se.execution.Steps[idx].Elapsed = time.Since(start).Seconds()
		se.execution.Steps[idx].Log = "Already stopped"
		se.mu.Unlock()
		se.broadcastUpdate()
		se.emitSeqEvent("sequence", "stopped", step.Container, "already stopped")
		return nil
	}

	timeout := 15
	stopOpts := container.StopOptions{Timeout: &timeout}
	stopCtx, stopCancel := context.WithTimeout(ctx, 20*time.Second)
	err = se.docker.ContainerStop(stopCtx, containerID, stopOpts)
	stopCancel()
	if err != nil {
		se.mu.Lock()
		se.execution.Steps[idx].Status = "failed"
		se.execution.Steps[idx].Elapsed = time.Since(start).Seconds()
		se.execution.Steps[idx].Log = fmt.Sprintf("Failed to stop: %v", err)
		se.mu.Unlock()
		se.broadcastUpdate()
		se.emitSeqEvent("sequence", "failed", step.Container, err.Error())
		return err
	}

	se.mu.Lock()
	se.execution.Steps[idx].Status = "done"
	se.execution.Steps[idx].Elapsed = time.Since(start).Seconds()
	se.execution.Steps[idx].Log = "Stopped"
	se.mu.Unlock()
	se.broadcastUpdate()
	se.emitSeqEvent("sequence", "stopped", step.Container, fmt.Sprintf("%.1fs", time.Since(start).Seconds()))
	return nil
}

// resolveAllContainerIDs builds a name→ID map with a single Docker API call
func (se *SequenceExecutor) resolveAllContainerIDs(ctx context.Context) (map[string]string, error) {
	listCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	containers, err := se.docker.ContainerList(listCtx, container.ListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %w", err)
	}

	result := make(map[string]string, len(containers))
	for _, c := range containers {
		for _, n := range c.Names {
			result[strings.TrimPrefix(n, "/")] = c.ID
		}
	}
	return result, nil
}

// waitForHealthy polls container health until healthy or timeout
func (se *SequenceExecutor) waitForHealthy(ctx context.Context, containerID string, timeout time.Duration) error {
	deadline := time.After(timeout)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("aborted")
		case <-deadline:
			return fmt.Errorf("timeout after %s waiting for healthy", timeout)
		case <-ticker.C:
			inspectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			inspect, err := se.docker.ContainerInspect(inspectCtx, containerID)
			cancel()
			if err != nil {
				continue
			}
			if !inspect.State.Running {
				return fmt.Errorf("container stopped unexpectedly")
			}
			if inspect.State.Health == nil {
				// No healthcheck defined — running is good enough
				return nil
			}
			switch inspect.State.Health.Status {
			case "healthy":
				return nil
			case "unhealthy":
				msg := "unhealthy"
				if len(inspect.State.Health.Log) > 0 {
					last := inspect.State.Health.Log[len(inspect.State.Health.Log)-1]
					msg = fmt.Sprintf("unhealthy: %s", strings.TrimSpace(last.Output))
				}
				return errors.New(msg)
			}
			// "starting" — keep waiting
		}
	}
}
