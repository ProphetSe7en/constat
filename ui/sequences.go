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
}

// SeqStep is a single container in a sequence
type SeqStep struct {
	Container    string `json:"container"`
	Required     bool   `json:"required"`
	WaitHealthy  bool   `json:"waitHealthy"`
	DelaySeconds int    `json:"delaySeconds,omitempty"`
}

// SeqExecution is the runtime state of a running sequence
type SeqExecution struct {
	SequenceID  string         `json:"sequenceId"`
	Mode        string         `json:"mode"` // start, stop, restart
	Phase       string         `json:"phase,omitempty"` // stopping, starting (for restart mode)
	Status      string         `json:"status"` // running, complete, failed, aborted
	CurrentStep int            `json:"currentStep"`
	TotalSteps  int            `json:"totalSteps"`
	StartedAt   time.Time      `json:"startedAt"`
	Elapsed     float64        `json:"elapsed"`
	Steps       []SeqStepState `json:"steps"`
	Error       string         `json:"error,omitempty"`
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
}

// NewSequenceExecutor creates and initializes a sequence executor
func NewSequenceExecutor(docker *client.Client) *SequenceExecutor {
	se := &SequenceExecutor{
		docker:      docker,
		sequences:   []Sequence{},
		subscribers: make(map[chan SeqEvent]struct{}),
		saveCh:      make(chan struct{}, 1),
		done:        make(chan struct{}),
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
	})
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
		if !seq.Steps[i].Required && seq.Steps[i].DelaySeconds > 0 {
			return fmt.Errorf("delay is only allowed on required steps")
		}
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
	for i := range se.sequences {
		if se.sequences[i].ID == exec.SequenceID {
			se.sequences[i].LastRun = time.Now().UTC().Format(time.RFC3339)
			se.sequences[i].LastResult = status
			break
		}
	}
	se.mu.Unlock()

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
	return &SeqExecution{
		SequenceID:  seq.ID,
		Mode:        mode,
		Phase:       phase,
		Status:      "running",
		CurrentStep: 0,
		TotalSteps:  len(seq.Steps),
		StartedAt:   time.Now().UTC(),
		Steps:       steps,
	}
}

// runExecution is the unified entry point for all execution modes
func (se *SequenceExecutor) runExecution(ctx context.Context, seq Sequence, mode string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("SequenceExecutor: panic in execution: %v", r)
			se.finishExecution("failed", "seq-failed", fmt.Sprintf("internal error: %v", r))
		}
	}()

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

// executeStartSteps runs start-mode steps. Returns true if all completed, false if finalized (failed/aborted).
func (se *SequenceExecutor) executeStartSteps(ctx context.Context, seq Sequence, idMap map[string]string) bool {
	se.broadcastUpdate()

	groups := se.buildGroups(seq.Steps)
	stepOffset := 0
	for _, group := range groups {
		if err := ctx.Err(); err != nil {
			se.mu.Lock()
			se.skipRemaining(stepOffset)
			se.mu.Unlock()
			se.finishExecution("aborted", "seq-aborted", "")
			return false
		}

		if err := se.executeGroupStart(ctx, group, stepOffset, idMap); err != nil {
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
			return fmt.Errorf("container not found: %s", step.Container)
		}
		// Non-required: skip directly (no failed flash)
		se.mu.Lock()
		se.execution.Steps[idx].Status = "skipped"
		se.execution.Steps[idx].Elapsed = time.Since(start).Seconds()
		se.execution.Steps[idx].Log = "Skipped — container not found"
		se.mu.Unlock()
		se.broadcastUpdate()
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
		return err
	}

	// If waitHealthy, poll for health
	if step.Required && step.WaitHealthy {
		se.mu.Lock()
		se.execution.Steps[idx].Log = "Started — waiting for healthy..."
		se.mu.Unlock()
		se.broadcastUpdate()

		if err := se.waitForHealthy(ctx, containerID, 30*time.Second); err != nil {
			se.mu.Lock()
			se.execution.Steps[idx].Status = "failed"
			se.execution.Steps[idx].Elapsed = time.Since(start).Seconds()
			se.execution.Steps[idx].Log = fmt.Sprintf("Health check failed: %v", err)
			se.mu.Unlock()
			se.broadcastUpdate()
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

	// Apply post-start delay if configured
	if step.DelaySeconds > 0 {
		se.mu.Lock()
		se.execution.Steps[idx].Status = "delaying"
		se.execution.Steps[idx].Log = fmt.Sprintf("Waiting %ds before next group...", step.DelaySeconds)
		se.mu.Unlock()
		se.broadcastUpdate()

		select {
		case <-ctx.Done():
			se.mu.Lock()
			se.execution.Steps[idx].Status = "done"
			se.execution.Steps[idx].Elapsed = time.Since(start).Seconds()
			se.execution.Steps[idx].Log = "Delay interrupted"
			se.mu.Unlock()
			se.broadcastUpdate()
			return nil // container started OK; let outer loop detect abort via ctx
		case <-time.After(time.Duration(step.DelaySeconds) * time.Second):
		}

		se.mu.Lock()
		se.execution.Steps[idx].Status = "done"
		se.execution.Steps[idx].Elapsed = time.Since(start).Seconds()
		se.execution.Steps[idx].Log = fmt.Sprintf("Started — %ds delay complete", step.DelaySeconds)
		se.mu.Unlock()
		se.broadcastUpdate()
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

	containerID, ok := idMap[step.Container]
	if !ok {
		if step.Required {
			se.mu.Lock()
			se.execution.Steps[idx].Status = "failed"
			se.execution.Steps[idx].Elapsed = time.Since(start).Seconds()
			se.execution.Steps[idx].Log = fmt.Sprintf("Container not found: %s", step.Container)
			se.mu.Unlock()
			se.broadcastUpdate()
			return fmt.Errorf("container not found: %s", step.Container)
		}
		se.mu.Lock()
		se.execution.Steps[idx].Status = "skipped"
		se.execution.Steps[idx].Elapsed = time.Since(start).Seconds()
		se.execution.Steps[idx].Log = "Skipped — container not found"
		se.mu.Unlock()
		se.broadcastUpdate()
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
		return err
	}

	se.mu.Lock()
	se.execution.Steps[idx].Status = "done"
	se.execution.Steps[idx].Elapsed = time.Since(start).Seconds()
	se.execution.Steps[idx].Log = "Stopped"
	se.mu.Unlock()
	se.broadcastUpdate()
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
