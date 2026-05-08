package main

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// RestartManager owns the auto-restart loop for containers labelled
// `constat.restart=true`. On each ActionHealthStatusUnhealthy from
// processDockerEvent it bumps an attempt counter inside a window of
// RestartCooldown seconds. While attempts < MaxRestarts it issues a
// ContainerRestart and writes an "auto-restart-attempted" event to the
// shared EventBuffer. If the container goes healthy before the budget is
// gone it emits "auto-restart-recovered". If the budget runs out it emits
// "auto-restart-exhausted" and stops trying until the user intervenes
// (clearing happens on a manual start, a successful manual restart, or a
// healthy event after a long quiet period).
//
// The two recovered/exhausted actions are also the trigger source for
// SequenceExecutor's OnRequiredRecovery / OnRequiredFailure reactions.
//
// Loop prevention: before SequenceExecutor itself stops or starts a
// container it calls Suppress(name, dur) so the unhealthy/healthy churn
// the sequence creates does not feed back into the manager.
type RestartManager struct {
	docker *client.Client
	events *EventBuffer

	mu     sync.Mutex
	states map[string]*restartState

	// Config (read-only after construction; live config updates call
	// SetConfig under cfgMu).
	cfgMu       sync.RWMutex
	maxRestarts int
	cooldown    time.Duration
}

type restartState struct {
	attempts        int
	windowStart     time.Time
	exhausted       bool
	suppressedUntil time.Time
}

// NewRestartManager creates a manager seeded with the current MAX_RESTARTS
// + RESTART_COOLDOWN values. SetConfig may be called later when the user
// edits Settings.
func NewRestartManager(docker *client.Client, events *EventBuffer, maxRestarts int, cooldown time.Duration) *RestartManager {
	rm := &RestartManager{
		docker:      docker,
		events:      events,
		states:      make(map[string]*restartState),
		maxRestarts: maxRestarts,
		cooldown:    cooldown,
	}
	return rm
}

// SetConfig swaps the live MaxRestarts / RestartCooldown values. Existing
// per-container windows keep their windowStart so a config change mid-flight
// only affects future attempts. Zero or negative values fall back to safe
// defaults.
func (rm *RestartManager) SetConfig(maxRestarts int, cooldown time.Duration) {
	if maxRestarts < 1 {
		maxRestarts = 3
	}
	if cooldown < time.Second {
		cooldown = 300 * time.Second
	}
	rm.cfgMu.Lock()
	rm.maxRestarts = maxRestarts
	rm.cooldown = cooldown
	rm.cfgMu.Unlock()
}

func (rm *RestartManager) snapshotConfig() (int, time.Duration) {
	rm.cfgMu.RLock()
	defer rm.cfgMu.RUnlock()
	return rm.maxRestarts, rm.cooldown
}

// Suppress mutes auto-restart tracking for `name` until now+dur. Used by
// SequenceExecutor before it stops or starts a container, so the resulting
// unhealthy/healthy churn does not feed back into the manager and trigger
// a recursive sequence run. SequenceExecutor's stopped/started state
// triggers also honor this via IsSuppressed.
func (rm *RestartManager) Suppress(name string, dur time.Duration) {
	if name == "" || dur <= 0 {
		return
	}
	until := time.Now().Add(dur)
	rm.mu.Lock()
	defer rm.mu.Unlock()
	s, ok := rm.states[name]
	if !ok {
		s = &restartState{}
		rm.states[name] = s
	}
	if until.After(s.suppressedUntil) {
		s.suppressedUntil = until
	}
}

// IsSuppressed reports whether `name` is currently within a Suppress
// window. Sequencer state-trigger code uses this to ignore stopped/started
// events that are part of an in-flight sequence run, a manual UI restart,
// or our own auto-restart attempt.
func (rm *RestartManager) IsSuppressed(name string) bool {
	if rm == nil || name == "" {
		return false
	}
	rm.mu.Lock()
	defer rm.mu.Unlock()
	s, ok := rm.states[name]
	if !ok {
		return false
	}
	return !s.suppressedUntil.IsZero() && time.Now().Before(s.suppressedUntil)
}

// Reset clears the tracked window for `name`. Called when the container
// is manually started/restarted via the API, so a previous "exhausted"
// flag does not stick forever.
func (rm *RestartManager) Reset(name string) {
	if name == "" {
		return
	}
	rm.mu.Lock()
	delete(rm.states, name)
	rm.mu.Unlock()
}

// OnUnhealthy is called from processDockerEvent when a container with the
// restart label transitions to unhealthy. Returns immediately if the
// container is currently suppressed or already exhausted. Otherwise
// performs the restart attempt asynchronously in a goroutine so the
// Docker event stream is never blocked.
func (rm *RestartManager) OnUnhealthy(name, id string) {
	if rm == nil || name == "" || id == "" {
		return
	}
	maxR, cooldown := rm.snapshotConfig()
	now := time.Now()

	rm.mu.Lock()
	s, ok := rm.states[name]
	if !ok {
		s = &restartState{windowStart: now}
		rm.states[name] = s
	}
	if !s.suppressedUntil.IsZero() && now.Before(s.suppressedUntil) {
		rm.mu.Unlock()
		return
	}
	// Window expiry: if the previous window opened more than `cooldown`
	// ago and we are not exhausted, treat this as a fresh window.
	if !s.windowStart.IsZero() && now.Sub(s.windowStart) > cooldown && !s.exhausted {
		s.attempts = 0
		s.windowStart = now
	}
	if s.exhausted {
		rm.mu.Unlock()
		return
	}
	if s.attempts >= maxR {
		s.exhausted = true
		rm.mu.Unlock()
		rm.emit(name, "auto-restart-exhausted", "")
		return
	}
	s.attempts++
	attempt := s.attempts
	rm.mu.Unlock()

	go rm.performRestart(name, id, attempt, maxR)
}

// OnHealthy is called from processDockerEvent when a container transitions
// to healthy. If we had an active window with at least one attempt and
// were not exhausted, we emit auto-restart-recovered and clear state.
// After exhaustion, recovery is silent — the user must intervene to
// re-arm any dependent sequences.
func (rm *RestartManager) OnHealthy(name string) {
	if rm == nil || name == "" {
		return
	}
	rm.mu.Lock()
	s, ok := rm.states[name]
	if !ok {
		rm.mu.Unlock()
		return
	}
	if s.attempts == 0 || s.exhausted {
		// No attempts → window opened but never fired (e.g. only
		// suppressed). Exhausted → user must manually clear. Either way,
		// drop the entry without emitting recovered.
		delete(rm.states, name)
		rm.mu.Unlock()
		return
	}
	delete(rm.states, name)
	rm.mu.Unlock()
	rm.emit(name, "auto-restart-recovered", "")
}

// performRestart issues the actual ContainerRestart call and emits an
// auto-restart-attempted event. Errors are logged + emitted with a Detail
// note so the user sees the failure in the event feed; they do NOT count
// as the cause of exhaustion (the next unhealthy event will keep counting
// until the budget is hit).
func (rm *RestartManager) performRestart(name, id string, attempt, maxR int) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	timeout := 30
	err := rm.docker.ContainerRestart(ctx, id, container.StopOptions{Timeout: &timeout})
	detail := ""
	if err != nil {
		detail = err.Error()
		log.Printf("RestartManager: %s attempt %d/%d failed: %v", name, attempt, maxR, err)
	} else {
		log.Printf("RestartManager: %s attempt %d/%d issued", name, attempt, maxR)
	}
	rm.emit(name, "auto-restart-attempted", detail)
}

func (rm *RestartManager) emit(name, action, detail string) {
	if rm.events == nil {
		return
	}
	rm.events.Add(Event{
		Timestamp: time.Now(),
		Container: name,
		Type:      "restart",
		Action:    action,
		Detail:    detail,
	})
}
