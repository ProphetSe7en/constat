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
}

// StatsBatch is the SSE payload sent to subscribers every 3s
type StatsBatch struct {
	Containers map[string]*ContainerLive `json:"containers"`
	TotalCPU   float64                   `json:"totalCpu"`
	TotalMem   uint64                    `json:"totalMemory"`
	HostMemory uint64                    `json:"hostMemory"`
	HostCPUs   int                       `json:"hostCpus"`
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

const statsHistorySize = 8640 // 72h at 30s intervals
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

// StatsCollector streams Docker stats and maintains per-container live data, averages, and history
type StatsCollector struct {
	mu          sync.RWMutex
	docker      *client.Client
	averages    map[string]*ContainerAvg    // keyed by container name
	history     map[string]*statsRingBuffer // keyed by container name
	prevNet     map[string]netSnapshot      // keyed by container name
	latest      map[string]*ContainerLive   // keyed by container name
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
		prevNet:     make(map[string]netSnapshot),
		latest:      make(map[string]*ContainerLive),
		streams:     make(map[string]context.CancelFunc),
		idToName:    make(map[string]string),
		statsCh:     make(chan streamResult, 256),
		subscribers: make(map[chan StatsBatch]struct{}),
		hostCPUs:    hostCPUs,
		hostMemory:  hostMemory,
	}
	sc.loadFromDisk()
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

	containers, err := sc.docker.ContainerList(listCtx, container.ListOptions{All: false})
	if err != nil {
		log.Printf("StatsCollector: failed to list containers: %v", err)
		return
	}

	type containerInfo struct {
		fullID string
		name   string
	}
	running := make(map[string]containerInfo, len(containers)) // name -> info
	for _, c := range containers {
		name := c.ID[:12] // fallback
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		running[name] = containerInfo{fullID: c.ID, name: name}
	}

	sc.mu.Lock()
	defer sc.mu.Unlock()

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
		batch.Containers[name] = &snapshot
		batch.TotalCPU += live.CPU
		batch.TotalMem += live.Memory
	}
	sc.mu.RUnlock()

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

// GetHistory returns all StatPoints since the given Unix timestamp
func (sc *StatsCollector) GetHistory(name string, since int64) []StatPoint {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	rb, ok := sc.history[name]
	if !ok {
		return nil
	}
	return rb.since(since)
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
	Averages map[string]*ContainerAvg `json:"averages"`
	History  map[string][]StatPoint   `json:"history"`
}

func (sc *StatsCollector) saveToDisk() {
	sc.mu.RLock()
	data := persistedData{
		Averages: sc.averages,
		History:  make(map[string][]StatPoint, len(sc.history)),
	}
	for name, rb := range sc.history {
		points := rb.all()
		if points != nil {
			data.History[name] = points
		}
	}
	sc.mu.RUnlock()

	bytes, err := json.Marshal(data)
	if err != nil {
		log.Printf("StatsCollector: failed to marshal stats: %v", err)
		return
	}

	if err := os.WriteFile(statsPersistPath, bytes, 0664); err != nil {
		log.Printf("StatsCollector: failed to save stats to disk: %v", err)
		return
	}
	os.Chown(statsPersistPath, 99, 100)
	log.Printf("StatsCollector: saved stats to disk (%d containers)", len(data.Averages))
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

	// Skip entries keyed by old-style hex container IDs (migration from ID→name keying)
	skipped := 0
	if data.Averages != nil {
		for key, avg := range data.Averages {
			if looksLikeHexID(key) {
				skipped++
				continue
			}
			sc.averages[key] = avg
		}
	}
	if data.History != nil {
		for key, points := range data.History {
			if looksLikeHexID(key) {
				continue
			}
			rb := &statsRingBuffer{}
			rb.loadFrom(points)
			sc.history[key] = rb
		}
	}

	if skipped > 0 {
		log.Printf("StatsCollector: skipped %d old ID-keyed entries during migration", skipped)
	}
	log.Printf("StatsCollector: loaded stats from disk (%d containers)", len(sc.averages))
}
