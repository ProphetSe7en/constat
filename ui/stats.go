package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// ContainerAvg tracks running average CPU/Memory for a container
type ContainerAvg struct {
	StartedAt string  `json:"startedAt"`
	Samples   int     `json:"samples"`
	CPUSum    float64 `json:"cpuSum"`
	MemorySum uint64  `json:"memorySum"`
	AvgCPU    float64 `json:"avgCpu"`
	AvgMemory uint64  `json:"avgMemory"`
}

// ContainerLive holds the latest live stats for a container
type ContainerLive struct {
	CPU         float64 `json:"cpu"`
	Memory      uint64  `json:"memory"`
	MemoryLimit uint64  `json:"memoryLimit"`
	NetRxRate   float64 `json:"netRxRate"`
	NetTxRate   float64 `json:"netTxRate"`
	State       string  `json:"state,omitempty"`
	Health      string  `json:"health,omitempty"`
	StartedAt   string  `json:"startedAt,omitempty"`
	Status      string  `json:"status,omitempty"` // transient status: stopped-health, stopped-mem
}

// MemTriggerCount tracks restart count vs max for a memory rule
type MemTriggerCount struct {
	Count int    `json:"count"`
	Max   int    `json:"max"`
	Name  string `json:"name"`
}

// StatsBatch is the SSE payload sent to subscribers every 3s
type StatsBatch struct {
	Containers      map[string]*ContainerLive   `json:"containers"`
	TotalCPU        float64                     `json:"totalCpu"`
	TotalMem        uint64                      `json:"totalMemory"`
	HostMemory      uint64                      `json:"hostMemory"`
	HostCPUs        int                         `json:"hostCpus"`
	MemTriggers     map[string]*MemTriggerCount `json:"memTriggers,omitempty"`
}

// streamResult is sent from per-container stream goroutines to main loop
type streamResult struct {
	name        string // container name (used as map key for stats persistence)
	id          string // 12-char container ID (used for Docker API calls)
	startedAt   string
	cpu         float64
	memory      uint64
	memoryLimit uint64
	rxTotal     uint64
	txTotal     uint64
}

// StatPoint is a single time-series data point for CPU/Memory/Network
type StatPoint struct {
	Time   int64   `json:"t"`            // Unix timestamp
	CPU    float64 `json:"c"`            // CPU %
	Memory uint64  `json:"m"`            // Memory bytes
	NetRx  float64 `json:"rx,omitempty"` // Network RX bytes/sec
	NetTx  float64 `json:"tx,omitempty"` // Network TX bytes/sec
}

const statsHistorySize = 2880          // 24h at 30s intervals (recent high-resolution data)
const statsAggregateInterval int64 = 300 // 5 minutes — older data aggregated to this interval
const statsMaxAgeDays = 7               // keep up to 7 days of aggregated history
const statsPersistPath = "/config/stats-history.json"

// statsRingBuffer is a fixed-size ring buffer for StatPoint time series
type statsRingBuffer struct {
	points [statsHistorySize]StatPoint
	head   int // next write position
	count  int // valid entries (max statsHistorySize)
}

func (rb *statsRingBuffer) add(p StatPoint) {
	rb.points[rb.head] = p
	rb.head = (rb.head + 1) % statsHistorySize
	if rb.count < statsHistorySize {
		rb.count++
	}
}

func (rb *statsRingBuffer) reset() {
	rb.head = 0
	rb.count = 0
}

// recent returns the last n points in chronological order
func (rb *statsRingBuffer) recent(n int) []StatPoint {
	if n <= 0 || rb.count == 0 {
		return nil
	}
	if n > rb.count {
		n = rb.count
	}
	result := make([]StatPoint, n)
	start := (rb.head - n + statsHistorySize) % statsHistorySize
	for i := 0; i < n; i++ {
		result[i] = rb.points[(start+i)%statsHistorySize]
	}
	return result
}

// since returns all points with Time >= sinceUnix in chronological order
func (rb *statsRingBuffer) since(sinceUnix int64) []StatPoint {
	if rb.count == 0 {
		return nil
	}
	var result []StatPoint
	start := (rb.head - rb.count + statsHistorySize) % statsHistorySize
	for i := 0; i < rb.count; i++ {
		p := rb.points[(start+i)%statsHistorySize]
		if p.Time >= sinceUnix {
			result = append(result, p)
		}
	}
	return result
}

// all returns all valid points in chronological order
func (rb *statsRingBuffer) all() []StatPoint {
	if rb.count == 0 {
		return nil
	}
	result := make([]StatPoint, rb.count)
	start := (rb.head - rb.count + statsHistorySize) % statsHistorySize
	for i := 0; i < rb.count; i++ {
		result[i] = rb.points[(start+i)%statsHistorySize]
	}
	return result
}

// loadFrom rebuilds the ring buffer from a slice of points
func (rb *statsRingBuffer) loadFrom(points []StatPoint) {
	rb.reset()
	for _, p := range points {
		rb.add(p)
	}
}

// looksLikeHexID returns true if the key looks like a 12-char hex container ID
// Used to skip old ID-keyed entries when migrating to name-keyed stats
func looksLikeHexID(s string) bool {
	if len(s) != 12 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// netSnapshot stores previous network bytes for rate calculation
type netSnapshot struct {
	timeMs int64 // Unix milliseconds for sub-second rate precision
	rx     uint64
	tx   uint64
}

// containerMeta holds state/health info from Docker, updated via syncStreams
type containerMeta struct {
	State     string
	Health    string
	StartedAt string
}

// StatsCollector streams Docker stats and maintains per-container live data, averages, and history
type StatsCollector struct {
	mu          sync.RWMutex
	docker      *client.Client
	averages    map[string]*ContainerAvg    // keyed by container name
	history     map[string]*statsRingBuffer // keyed by container name — recent 24h at 30s
	aggregated  map[string][]StatPoint      // keyed by container name — older data at 5min intervals
	prevNet     map[string]netSnapshot      // keyed by container name
	latest      map[string]*ContainerLive   // keyed by container name
	meta        map[string]*containerMeta   // keyed by container name — state/health from Docker
	status      map[string]string           // keyed by container name — transient status (restarting-health, stopped-health, etc.)
	streams     map[string]context.CancelFunc // keyed by container name
	idToName    map[string]string           // 12-char ID → container name (for API lookups)
	statsCh     chan streamResult
	subscribers map[chan StatsBatch]struct{}
	subMu       sync.Mutex
	hostCPUs    int
	hostMemory  uint64
}

// NewStatsCollector creates a new stats collector
func NewStatsCollector(docker *client.Client, hostCPUs int, hostMemory uint64) *StatsCollector {
	sc := &StatsCollector{
		docker:      docker,
		averages:    make(map[string]*ContainerAvg),
		history:     make(map[string]*statsRingBuffer),
		aggregated:  make(map[string][]StatPoint),
		prevNet:     make(map[string]netSnapshot),
		latest:      make(map[string]*ContainerLive),
		meta:        make(map[string]*containerMeta),
		status:      make(map[string]string),
		streams:     make(map[string]context.CancelFunc),
		idToName:    make(map[string]string),
		statsCh:     make(chan streamResult, 256),
		subscribers: make(map[chan StatsBatch]struct{}),
		hostCPUs:    hostCPUs,
		hostMemory:  hostMemory,
	}
	sc.loadFromDisk()
	sc.loadContainerStatus()
	return sc
}

// Run starts the streaming stats loop (call as goroutine)
func (sc *StatsCollector) Run(ctx context.Context) {
	sc.syncStreams(ctx)

	syncTicker := time.NewTicker(10 * time.Second)
	defer syncTicker.Stop()

	broadcastTicker := time.NewTicker(3 * time.Second)
	defer broadcastTicker.Stop()

	historyTicker := time.NewTicker(30 * time.Second)
	defer historyTicker.Stop()

	persistTicker := time.NewTicker(5 * time.Minute)
	defer persistTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			sc.stopAllStreams()
			sc.saveToDisk()
			return
		case r := <-sc.statsCh:
			sc.processStreamResult(r)
		case <-syncTicker.C:
			sc.syncStreams(ctx)
		case <-broadcastTicker.C:
			sc.broadcastLatest()
		case <-historyTicker.C:
			sc.appendHistory()
		case <-persistTicker.C:
			sc.saveToDisk()
		}
	}
}

// syncStreams starts/stops per-container streaming goroutines based on running containers
func (sc *StatsCollector) syncStreams(ctx context.Context) {
	listCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	containers, err := sc.docker.ContainerList(listCtx, container.ListOptions{All: true})
	if err != nil {
		log.Printf("StatsCollector: failed to list containers: %v", err)
		return
	}

	type containerInfo struct {
		fullID string
		name   string
	}
	running := make(map[string]containerInfo) // name -> info (running only)
	newMeta := make(map[string]*containerMeta, len(containers))
	for _, c := range containers {
		name := c.ID[:12] // fallback
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		if c.State == "running" {
			running[name] = containerInfo{fullID: c.ID, name: name}
		}
		// Health and startedAt are set by inspect below for running containers
		newMeta[name] = &containerMeta{State: c.State, Health: "none"}
	}

	// Refresh startedAt for running containers via inspect (cheap on local socket)
	for name, info := range running {
		inspCtx, inspCancel := context.WithTimeout(ctx, 2*time.Second)
		insp, err := sc.docker.ContainerInspect(inspCtx, info.fullID)
		inspCancel()
		if err == nil && insp.State.StartedAt != "" {
			if m, ok := newMeta[name]; ok {
				m.StartedAt = insp.State.StartedAt
			}
			// Also update health from inspect (more precise than parsing Status string)
			if insp.State.Health != nil {
				if m, ok := newMeta[name]; ok {
					m.Health = string(insp.State.Health.Status)
				}
			}
		}
	}

	sc.mu.Lock()
	defer sc.mu.Unlock()

	// Update container metadata (state/health)
	sc.meta = newMeta

	// Rebuild ID→name mapping
	sc.idToName = make(map[string]string, len(running))
	for name, info := range running {
		sc.idToName[info.fullID[:12]] = name
	}

	// Start streams for new containers
	for name, info := range running {
		if _, exists := sc.streams[name]; !exists {
			streamCtx, streamCancel := context.WithCancel(ctx)
			sc.streams[name] = streamCancel
			go sc.streamContainer(streamCtx, info.fullID, name)
		}
	}

	// Stop streams for containers that are no longer running
	for name, cancelFn := range sc.streams {
		if _, stillRunning := running[name]; !stillRunning {
			cancelFn()
			delete(sc.streams, name)
			delete(sc.latest, name)
			delete(sc.prevNet, name)
			// Keep history and averages — they survive container stops for charts
		}
	}
}

// streamContainer opens a streaming stats connection for a single container
func (sc *StatsCollector) streamContainer(ctx context.Context, fullID, name string) {
	// Clean up streams map on exit so syncStreams can start a new stream after restart
	defer func() {
		sc.mu.Lock()
		delete(sc.streams, name)
		sc.mu.Unlock()
	}()

	// Fetch startedAt once via inspect
	inspectCtx, inspectCancel := context.WithTimeout(ctx, 5*time.Second)
	defer inspectCancel()
	inspect, err := sc.docker.ContainerInspect(inspectCtx, fullID)
	if err != nil {
		log.Printf("StatsCollector: inspect failed for %s: %v", name, err)
		return
	}
	startedAt := inspect.State.StartedAt

	resp, err := sc.docker.ContainerStats(ctx, fullID, true) // stream=true
	if err != nil {
		log.Printf("StatsCollector: stream open failed for %s: %v", name, err)
		return
	}
	defer resp.Body.Close()

	decoder := json.NewDecoder(resp.Body)
	for {
		var stats container.StatsResponse
		if err := decoder.Decode(&stats); err != nil {
			// EOF = container stopped, or context cancelled
			return
		}

		var rxTotal, txTotal uint64
		for _, net := range stats.Networks {
			rxTotal += net.RxBytes
			txTotal += net.TxBytes
		}

		select {
		case sc.statsCh <- streamResult{
			name:        name,
			id:          fullID[:12],
			startedAt:   startedAt,
			cpu:         calculateCPUPercent(&stats),
			memory:      calculateMemUsage(&stats),
			memoryLimit: stats.MemoryStats.Limit,
			rxTotal:     rxTotal,
			txTotal:     txTotal,
		}:
		case <-ctx.Done():
			return
		}
	}
}

// processStreamResult updates latest stats, averages, and network rates from a stream result
func (sc *StatsCollector) processStreamResult(r streamResult) {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	nowMs := time.Now().UnixMilli()

	// Update latest live stats (keyed by container name)
	live, exists := sc.latest[r.name]
	if !exists {
		live = &ContainerLive{}
		sc.latest[r.name] = live
	}
	live.CPU = r.cpu
	live.Memory = r.memory
	live.MemoryLimit = r.memoryLimit

	// Update startedAt in meta (precise value from container inspect)
	if m, ok := sc.meta[r.name]; ok {
		m.StartedAt = r.startedAt
	}

	// Detect restart before computing network rates (startedAt changes on restart)
	avg, avgExists := sc.averages[r.name]
	restarted := !avgExists || avg.StartedAt != r.startedAt

	if restarted {
		// New container or restarted — reset averages and network baseline
		sc.averages[r.name] = &ContainerAvg{
			StartedAt: r.startedAt,
			Samples:   1,
			CPUSum:    r.cpu,
			MemorySum: r.memory,
			AvgCPU:    r.cpu,
			AvgMemory: r.memory,
		}
		delete(sc.prevNet, r.name)
		live.NetRxRate = 0
		live.NetTxRate = 0
	} else {
		avg.Samples++
		avg.CPUSum += r.cpu
		avg.MemorySum += r.memory
		avg.AvgCPU = avg.CPUSum / float64(avg.Samples)
		avg.AvgMemory = avg.MemorySum / uint64(avg.Samples)

		// Compute network rate from previous snapshot (only if not restarted)
		prev, hasPrev := sc.prevNet[r.name]
		if hasPrev && nowMs > prev.timeMs {
			elapsed := float64(nowMs-prev.timeMs) / 1000.0 // milliseconds → seconds
			// Guard before subtraction to prevent uint64 underflow
			if r.rxTotal >= prev.rx {
				live.NetRxRate = float64(r.rxTotal-prev.rx) / elapsed
			} else {
				live.NetRxRate = 0
			}
			if r.txTotal >= prev.tx {
				live.NetTxRate = float64(r.txTotal-prev.tx) / elapsed
			} else {
				live.NetTxRate = 0
			}
		}
	}
	sc.prevNet[r.name] = netSnapshot{timeMs: nowMs, rx: r.rxTotal, tx: r.txTotal}
}

// broadcastLatest snapshots all latest values and pushes to SSE subscribers
func (sc *StatsCollector) broadcastLatest() {
	sc.mu.RLock()
	batch := StatsBatch{
		Containers: make(map[string]*ContainerLive, len(sc.latest)),
		HostMemory: sc.hostMemory,
		HostCPUs:   sc.hostCPUs,
	}
	for name, live := range sc.latest {
		snapshot := *live // copy
		if m, ok := sc.meta[name]; ok {
			snapshot.State = m.State
			snapshot.Health = m.Health
			snapshot.StartedAt = m.StartedAt
		}
		if s, ok := sc.status[name]; ok {
			snapshot.Status = s
		}
		batch.Containers[name] = &snapshot
		batch.TotalCPU += live.CPU
		batch.TotalMem += live.Memory
	}
	// Include stopped containers (no live stats, but state/health matter)
	for name, m := range sc.meta {
		if _, hasLive := sc.latest[name]; !hasLive {
			cl := &ContainerLive{
				State:     m.State,
				Health:    m.Health,
				StartedAt: m.StartedAt,
			}
			if s, ok := sc.status[name]; ok {
				cl.Status = s
			}
			batch.Containers[name] = cl
		}
	}
	sc.mu.RUnlock()

	// Read memory trigger counts from bash (best-effort, non-blocking)
	if raw, err := os.ReadFile("/config/mem_trigger_counts.json"); err == nil {
		var triggers map[string]*MemTriggerCount
		if json.Unmarshal(raw, &triggers) == nil && len(triggers) > 0 {
			batch.MemTriggers = triggers
		}
	}

	sc.subMu.Lock()
	for ch := range sc.subscribers {
		select {
		case ch <- batch:
		default:
			// Slow subscriber, skip this batch
		}
	}
	sc.subMu.Unlock()
}

// appendHistory adds current latest values to the ring buffers (every 30s for chart data density)
func (sc *StatsCollector) appendHistory() {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	now := time.Now().Unix()
	for name, live := range sc.latest {
		rb, ok := sc.history[name]
		if !ok {
			rb = &statsRingBuffer{}
			sc.history[name] = rb
		}
		rb.add(StatPoint{Time: now, CPU: live.CPU, Memory: live.Memory, NetRx: live.NetRxRate, NetTx: live.NetTxRate})
	}
}

// stopAllStreams cancels all streaming goroutines
func (sc *StatsCollector) stopAllStreams() {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	for name, cancelFn := range sc.streams {
		cancelFn()
		delete(sc.streams, name)
	}
}

// SetContainerStatus sets a transient status for a container (e.g. "stopped-health")
// Persists to /config/container_status.json so it survives restarts.
func (sc *StatsCollector) SetContainerStatus(name, status string) {
	sc.mu.Lock()
	if status == "" {
		delete(sc.status, name)
	} else {
		sc.status[name] = status
	}
	// Copy for persistence outside lock
	snapshot := make(map[string]string, len(sc.status))
	for k, v := range sc.status {
		snapshot[k] = v
	}
	sc.mu.Unlock()

	// Persist to disk (best-effort, atomic — power-loss mid-write shouldn't
	// leave a truncated status file that poisons the next boot's stats).
	data, err := json.Marshal(snapshot)
	if err == nil {
		_ = atomicWriteFile("/config/container_status.json", data, 0644)
	}
}

// GetContainerStatus returns the transient status for a container
func (sc *StatsCollector) GetContainerStatus(name string) string {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	return sc.status[name]
}

// RemoveContainer drops all in-memory state for a container name. Called
// when Docker emits a `destroy` event (container fully removed: `docker
// rm`, or `--rm` auto-cleanup for one-shots) and from PruneStale as a
// safety net. Without this, every transient Docker one-shot with an
// auto-generated name (`admiring_hofstadter` etc.) leaked ~112 KiB of
// ring-buffer plus map entries forever — ~62% of prod's stats-history
// was phantoms at the time this was added.
//
// Removing a name that isn't in the maps is a no-op.
func (sc *StatsCollector) RemoveContainer(name string) {
	if name == "" {
		return
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()

	// Stop any active stats stream first so the goroutine can exit cleanly
	// before we drop the map it writes into.
	if cancel, ok := sc.streams[name]; ok {
		cancel()
		delete(sc.streams, name)
	}

	delete(sc.history, name)
	delete(sc.aggregated, name)
	delete(sc.averages, name)
	delete(sc.latest, name)
	delete(sc.meta, name)
	delete(sc.status, name)
	delete(sc.prevNet, name)

	// idToName is ID → name; drop any reverse entries that pointed here.
	for id, n := range sc.idToName {
		if n == name {
			delete(sc.idToName, id)
		}
	}
}

// PruneStale removes in-memory state for any tracked container name that
// no longer appears in `docker ps -a`. Runs on startup (to flush
// accumulated phantom entries from prior constat versions that didn't
// listen for destroy events) and every STATS_CLEANUP_INTERVAL
// thereafter as a safety net for events the stream might miss
// (reconnect gaps, docker daemon restarts, container destroys that
// arrive during constat's own startup window).
//
// Returns the number of entries pruned so the caller can log it.
func (sc *StatsCollector) PruneStale(ctx context.Context) int {
	listCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	containers, err := sc.docker.ContainerList(listCtx, container.ListOptions{All: true})
	if err != nil {
		log.Printf("StatsCollector: PruneStale list failed: %v", err)
		return 0
	}

	// Build the set of names Docker still knows about.
	alive := make(map[string]struct{}, len(containers))
	for _, c := range containers {
		name := c.ID[:12]
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		alive[name] = struct{}{}
	}

	// Snapshot the tracked names under read lock. Don't mutate during
	// iteration — RemoveContainer takes the write lock. Collect from each
	// map since a partial-write or transient rename could leave a name in
	// one map but not another.
	sc.mu.RLock()
	tracked := make([]string, 0, len(sc.history)+len(sc.aggregated)+len(sc.averages))
	seen := make(map[string]struct{})
	for name := range sc.history {
		if _, ok := seen[name]; !ok {
			seen[name] = struct{}{}
			tracked = append(tracked, name)
		}
	}
	for name := range sc.aggregated {
		if _, ok := seen[name]; !ok {
			seen[name] = struct{}{}
			tracked = append(tracked, name)
		}
	}
	for name := range sc.averages {
		if _, ok := seen[name]; !ok {
			seen[name] = struct{}{}
			tracked = append(tracked, name)
		}
	}
	for name := range sc.latest {
		if _, ok := seen[name]; !ok {
			seen[name] = struct{}{}
			tracked = append(tracked, name)
		}
	}
	for name := range sc.meta {
		if _, ok := seen[name]; !ok {
			seen[name] = struct{}{}
			tracked = append(tracked, name)
		}
	}
	sc.mu.RUnlock()

	pruned := 0
	for _, name := range tracked {
		if _, ok := alive[name]; ok {
			continue
		}
		sc.RemoveContainer(name)
		pruned++
	}
	return pruned
}

// loadContainerStatus restores persisted container status from disk
func (sc *StatsCollector) loadContainerStatus() {
	data, err := os.ReadFile("/config/container_status.json")
	if err != nil {
		return
	}
	var status map[string]string
	if err := json.Unmarshal(data, &status); err == nil {
		sc.status = status
	}
}

// NameForID resolves a 12-char container ID to a container name
func (sc *StatsCollector) NameForID(id string) (string, bool) {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	name, ok := sc.idToName[id]
	return name, ok
}

// GetLatest returns the latest live stats for a container (by name)
func (sc *StatsCollector) GetLatest(name string) (*ContainerLive, bool) {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	live, exists := sc.latest[name]
	if !exists {
		return nil, false
	}
	snapshot := *live // copy to avoid races
	return &snapshot, true
}

// GetAverage returns the average stats for a container (by name)
func (sc *StatsCollector) GetAverage(name string) (avgCPU float64, avgMemory uint64, ok bool) {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	avg, exists := sc.averages[name]
	if !exists {
		return 0, 0, false
	}
	return avg.AvgCPU, avg.AvgMemory, true
}

// GetNetRate returns the network rx/tx rates in bytes/sec for a container
func (sc *StatsCollector) GetNetRate(name string) (rxRate, txRate float64, ok bool) {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	live, exists := sc.latest[name]
	if !exists {
		return 0, 0, false
	}
	return live.NetRxRate, live.NetTxRate, true
}

// GetRecentStats returns the last n StatPoints for sparklines
func (sc *StatsCollector) GetRecentStats(name string, n int) []StatPoint {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	rb, ok := sc.history[name]
	if !ok {
		return nil
	}
	return rb.recent(n)
}

// GetHistory returns all StatPoints since the given Unix timestamp,
// combining aggregated (older, 5min intervals) and recent (ring buffer, 30s) data.
func (sc *StatsCollector) GetHistory(name string, since int64) []StatPoint {
	sc.mu.RLock()
	defer sc.mu.RUnlock()

	var result []StatPoint

	// 1. Add aggregated points that fall within range
	if agg, ok := sc.aggregated[name]; ok {
		for _, p := range agg {
			if p.Time >= since {
				result = append(result, p)
			}
		}
	}

	// 2. Add recent ring buffer points
	rb, ok := sc.history[name]
	if !ok {
		return result
	}
	recent := rb.since(since)

	// 3. Merge — skip aggregated points that overlap with ring buffer
	if len(result) > 0 && len(recent) > 0 {
		cutoff := recent[0].Time
		filtered := result[:0]
		for _, p := range result {
			if p.Time < cutoff {
				filtered = append(filtered, p)
			}
		}
		result = append(filtered, recent...)
	} else {
		result = append(result, recent...)
	}

	return result
}

// SubscribeStats creates a channel that receives StatsBatch updates
func (sc *StatsCollector) SubscribeStats() chan StatsBatch {
	ch := make(chan StatsBatch, 4)
	sc.subMu.Lock()
	sc.subscribers[ch] = struct{}{}
	sc.subMu.Unlock()
	return ch
}

// UnsubscribeStats removes a subscriber channel
func (sc *StatsCollector) UnsubscribeStats(ch chan StatsBatch) {
	sc.subMu.Lock()
	delete(sc.subscribers, ch)
	sc.subMu.Unlock()
	// Don't close the channel — broadcastLatest() may still reference it.
	// Let GC handle cleanup.
}

// --- Persistence ---

// persistedData is the JSON structure for saving/loading stats
type persistedData struct {
	Averages   map[string]*ContainerAvg `json:"averages"`
	History    map[string][]StatPoint   `json:"history"`    // recent 24h at 30s
	Aggregated map[string][]StatPoint   `json:"aggregated"` // older data at 5min intervals
}

// aggregatePoints downsamples points to statsAggregateInterval (5min) buckets.
// Each bucket averages CPU/NetRx/NetTx and takes max Memory.
func aggregatePoints(points []StatPoint) []StatPoint {
	if len(points) == 0 {
		return nil
	}
	var result []StatPoint
	bucketStart := (points[0].Time / statsAggregateInterval) * statsAggregateInterval
	var cpuSum, rxSum, txSum float64
	var memMax uint64
	var count int
	var lastTime int64

	flush := func() {
		if count > 0 {
			result = append(result, StatPoint{
				Time:   lastTime,
				CPU:    cpuSum / float64(count),
				Memory: memMax,
				NetRx:  rxSum / float64(count),
				NetTx:  txSum / float64(count),
			})
		}
	}

	for _, p := range points {
		bucket := (p.Time / statsAggregateInterval) * statsAggregateInterval
		if bucket != bucketStart {
			flush()
			bucketStart = bucket
			cpuSum, rxSum, txSum = 0, 0, 0
			memMax = 0
			count = 0
		}
		cpuSum += p.CPU
		rxSum += p.NetRx
		txSum += p.NetTx
		if p.Memory > memMax {
			memMax = p.Memory
		}
		lastTime = p.Time
		count++
	}
	flush()
	return result
}

func (sc *StatsCollector) saveToDisk() {
	sc.mu.RLock()
	data := persistedData{
		Averages:   sc.averages,
		History:    make(map[string][]StatPoint, len(sc.history)),
		Aggregated: make(map[string][]StatPoint, len(sc.aggregated)),
	}

	now := time.Now().Unix()
	recentCutoff := now - 86400 // 24h ago
	maxAge := now - int64(statsMaxAgeDays)*86400

	for name, rb := range sc.history {
		points := rb.all()
		if points != nil {
			data.History[name] = points
		}
	}

	// Build aggregated: existing aggregated + ring buffer points older than 24h
	for name := range sc.history {
		var oldPoints []StatPoint

		// Keep existing aggregated points within max age
		if existing, ok := sc.aggregated[name]; ok {
			for _, p := range existing {
				if p.Time >= maxAge && p.Time < recentCutoff {
					oldPoints = append(oldPoints, p)
				}
			}
		}

		// Add ring buffer points that are older than 24h (they'll be aggregated)
		if rb, ok := sc.history[name]; ok {
			for _, p := range rb.all() {
				if p.Time < recentCutoff && p.Time >= maxAge {
					oldPoints = append(oldPoints, p)
				}
			}
		}

		if len(oldPoints) > 0 {
			data.Aggregated[name] = aggregatePoints(oldPoints)
		}
	}
	sc.mu.RUnlock()

	bytes, err := json.Marshal(data)
	if err != nil {
		log.Printf("StatsCollector: failed to marshal stats: %v", err)
		return
	}

	if err := atomicWriteFile(statsPersistPath, bytes, 0664); err != nil {
		log.Printf("StatsCollector: failed to save stats to disk: %v", err)
		return
	}
	os.Chown(statsPersistPath, 99, 100)
	log.Printf("StatsCollector: saved stats to disk (%d containers, history %d KB)", len(data.Averages), len(bytes)/1024)
}

func (sc *StatsCollector) loadFromDisk() {
	bytes, err := os.ReadFile(statsPersistPath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("StatsCollector: failed to read stats from disk: %v", err)
		}
		return
	}

	var data persistedData
	if err := json.Unmarshal(bytes, &data); err != nil {
		log.Printf("StatsCollector: failed to parse stats from disk: %v", err)
		return
	}

	sc.mu.Lock()
	defer sc.mu.Unlock()

	now := time.Now().Unix()
	maxAge := now - int64(statsMaxAgeDays)*86400

	// Skip entries keyed by old-style hex container IDs (migration from ID→name keying)
	skipped := 0
	if data.Averages != nil {
		for key, avg := range data.Averages {
			if looksLikeHexID(key) {
				skipped++
				continue
			}
			// Reset corrupted averages (bogus CPU values from Docker stats glitches)
			if avg.AvgCPU > 10000 || avg.CPUSum > 10000*float64(avg.Samples+1) {
				avg.CPUSum = 0
				avg.Samples = 0
				avg.AvgCPU = 0
				avg.MemorySum = 0
				avg.AvgMemory = 0
			}
			sc.averages[key] = avg
		}
	}

	// Load aggregated data (older, 5min intervals)
	if data.Aggregated != nil {
		for key, points := range data.Aggregated {
			if looksLikeHexID(key) {
				continue
			}
			// Filter out points older than max age
			var valid []StatPoint
			for _, p := range points {
				if p.Time >= maxAge {
					valid = append(valid, p)
				}
			}
			if len(valid) > 0 {
				sc.aggregated[key] = valid
			}
		}
	}

	// Load recent history into ring buffer
	if data.History != nil {
		for key, points := range data.History {
			if looksLikeHexID(key) {
				continue
			}
			// Only load points that fit in the ring buffer (recent 24h)
			recentCutoff := now - 86400
			rb := &statsRingBuffer{}
			for _, p := range points {
				if p.Time >= recentCutoff {
					rb.add(p)
				}
			}
			sc.history[key] = rb
		}
	}

	if skipped > 0 {
		log.Printf("StatsCollector: skipped %d old ID-keyed entries during migration", skipped)
	}
	log.Printf("StatsCollector: loaded stats from disk (%d containers, %d with aggregated history)", len(sc.averages), len(sc.aggregated))
}
