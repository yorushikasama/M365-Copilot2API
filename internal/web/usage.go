package web

import (
	"bufio"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type UsageRecord struct {
	Time         time.Time `json:"time"`
	APIKeyPrefix string    `json:"api_key_prefix"`
	ClientIP     string    `json:"client_ip,omitempty"`
	AccountEmail string    `json:"account_email"`
	Model        string    `json:"model"`
	Endpoint     string    `json:"endpoint"`
	Stream       bool      `json:"stream"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
	CacheTokens  int64     `json:"cache_tokens"`
	DurationMs   int64     `json:"duration_ms"`
	Status       int       `json:"status"`
}

const maxUsageRecords = 50000

// usageSnapshotTTL bounds how long an aggregated snapshot is reused. The console
// polls these endpoints continuously; without a cache every poll copied the whole
// record set under the same mutex the request hot path uses to append.
const usageSnapshotTTL = 3 * time.Second

// defaultUsageMaxFileBytes caps usage.jsonl. Only the last maxUsageRecords rows
// are ever served, so anything beyond this is dead weight that also slows every
// restart down.
const defaultUsageMaxFileBytes = 32 << 20

// usageRotateSlack lets the file run this many times past the retained record
// count before it is rewritten, keeping rotation amortized.
const usageRotateSlack = 2

func usageMaxFileBytes() int64 {
	if raw := strings.TrimSpace(os.Getenv("M365_USAGE_LOG_MAX_BYTES")); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return defaultUsageMaxFileBytes
}

type usageSnapshotCache struct {
	at    time.Time
	days  int
	stats map[string]any
	ips   []map[string]any
}

type usageLog struct {
	mu      sync.Mutex
	Path    string
	records []UsageRecord
	pending []UsageRecord
	persist *persistStore
	// fileRecords tracks how many rows currently sit on disk so rotation can
	// tell whether a rewrite would actually reclaim anything.
	fileRecords int
	rotations   int

	cacheMu sync.Mutex
	cache   usageSnapshotCache
}

var globalUsage = &usageLog{}

func openUsageLog() *usageLog {
	p := strings.TrimSpace(os.Getenv("M365_USAGE_LOG"))
	if p == "" {
		dir := strings.TrimSpace(os.Getenv("M365_DATA_DIR"))
		if dir == "" {
			h, _ := os.UserHomeDir()
			dir = filepath.Join(h, ".config", "m365-copilot2api")
		}
		p = filepath.Join(dir, "usage.jsonl")
	}
	s := &usageLog{Path: p}
	s.persist = &persistStore{flush: s.flush}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		log.Printf("[usage] MkdirAll failed: %v", err)
	}
	s.load()
	return s
}

// load reads the log through a ring buffer. Appending everything and trimming
// afterwards made peak memory scale with the entire retained history rather than
// with maxUsageRecords.
func (s *usageLog) load() {
	f, err := os.Open(s.Path)
	if err != nil {
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	ring := make([]UsageRecord, maxUsageRecords)
	count, next, lines := 0, 0, 0
	for scanner.Scan() {
		lines++
		var rec UsageRecord
		if json.Unmarshal(scanner.Bytes(), &rec) != nil {
			continue
		}
		ring[next] = rec
		next = (next + 1) % maxUsageRecords
		count++
	}
	// Count every physical row, not just the parseable ones: unparseable lines
	// are dropped by a rewrite too, so they count as reclaimable space.
	s.fileRecords = lines
	if count == 0 {
		return
	}
	if count < maxUsageRecords {
		s.records = append(s.records, ring[:count]...)
		return
	}
	s.records = append(s.records, ring[next:]...)
	s.records = append(s.records, ring[:next]...)
}

func (s *usageLog) trim() {
	if len(s.records) > maxUsageRecords {
		s.records = s.records[len(s.records)-maxUsageRecords:]
	}
}

func (s *usageLog) record(rec UsageRecord) {
	s.mu.Lock()
	s.records = append(s.records, rec)
	s.trim()
	s.pending = append(s.pending, rec)
	s.mu.Unlock()
	s.invalidateSnapshot()
	s.persist.markDirty()
}

func (s *usageLog) invalidateSnapshot() {
	s.cacheMu.Lock()
	s.cache = usageSnapshotCache{}
	s.cacheMu.Unlock()
}

// flush 批量追加本次累积的记录，锁外写盘。
func (s *usageLog) flush() error {
	s.mu.Lock()
	pending := s.pending
	s.pending = nil
	s.mu.Unlock()
	// appendRecords must fully return (and close its handle) before rotation:
	// Windows refuses to rename over a file that is still open.
	if err := s.appendRecords(pending); err != nil {
		return err
	}
	return s.rotateIfNeeded()
}

func (s *usageLog) appendRecords(pending []UsageRecord) error {
	if len(pending) == 0 {
		return nil
	}
	var buf []byte
	for _, rec := range pending {
		if b, err := json.Marshal(rec); err == nil {
			buf = append(buf, b...)
			buf = append(buf, '\n')
		}
	}
	f, err := os.OpenFile(s.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	n, werr := f.Write(buf)
	if werr != nil {
		// Only re-queue records whose whole line did not reach disk. Re-queuing the
		// whole batch would re-write the rows that already persisted, duplicating
		// them in the usage log on the next flush.
		s.mu.Lock()
		s.pending = append(recountPending(pending, n), s.pending...)
		s.mu.Unlock()
		return werr
	}
	s.mu.Lock()
	s.fileRecords += len(pending)
	s.mu.Unlock()
	return f.Sync()
}

// recountPending returns the tail of pending whose marshalled bytes were not
// fully written in the first n bytes. Records are newline-terminated, so counting
// the complete newlines inside the first n bytes tells exactly how many leading
// records landed on disk and must not be re-queued.
func recountPending(pending []UsageRecord, n int) []UsageRecord {
	pos := 0
	written := 0
	for i, rec := range pending {
		b, err := json.Marshal(rec)
		if err != nil {
			continue
		}
		pos += len(b) + 1
		if pos <= n {
			written = i + 1
		} else {
			break
		}
	}
	return pending[written:]
}

// rotateIfNeeded rewrites the log with just the retained records once a rewrite
// would actually reclaim space. Triggering on file size alone was wrong: a log
// that merely sits above the byte cap while the retained set already accounts for
// every row on disk would be rewritten byte-for-byte on every flush. The decision
// is therefore based on reclaimable rows, and the byte cap only brings the
// rewrite forward.
//
// It runs after pending rows were appended and clears pending in the same
// critical section as the snapshot it writes, so no row is persisted twice and
// none is lost.
func (s *usageLog) rotateIfNeeded() error {
	fi, err := os.Stat(s.Path)
	if err != nil {
		return nil
	}
	overBytes := fi.Size() > usageMaxFileBytes()

	s.mu.Lock()
	onDisk, retained := s.fileRecords, len(s.records)
	reclaimable := onDisk - retained
	// A rewrite can never shrink the file below the retained set, so require a
	// meaningful share of it to be reclaimable before paying for the rewrite.
	minReclaim := retained / usageRotateSlack
	if minReclaim < 1 {
		minReclaim = 1
	}
	overCount := onDisk > retained*usageRotateSlack
	if retained == 0 || reclaimable < minReclaim || !(overBytes || overCount) {
		s.mu.Unlock()
		return nil
	}
	recs := append([]UsageRecord(nil), s.records...)
	// Everything in recs is about to hit disk, so queued duplicates must go.
	requeue := s.pending
	s.pending = nil
	s.fileRecords = len(recs)
	s.rotations++
	s.mu.Unlock()

	var buf []byte
	for _, rec := range recs {
		if b, err := json.Marshal(rec); err == nil {
			buf = append(buf, b...)
			buf = append(buf, '\n')
		}
	}
	if err := writeFileAtomic(s.Path, buf, 0600); err != nil {
		// The file is untouched, so restore the bookkeeping and put the rows we
		// claimed back on the queue instead of dropping them.
		s.mu.Lock()
		s.fileRecords = onDisk
		s.rotations--
		s.pending = append(requeue, s.pending...)
		s.mu.Unlock()
		return err
	}
	log.Printf("[usage] rotated log: %d -> %d bytes, %d records retained, %d reclaimed", fi.Size(), len(buf), len(recs), reclaimable)
	return nil
}

func (s *usageLog) snapshot(days int) map[string]any {
	s.cacheMu.Lock()
	if s.cache.stats != nil && s.cache.days == days && time.Since(s.cache.at) < usageSnapshotTTL {
		cached := s.cache.stats
		s.cacheMu.Unlock()
		return cached
	}
	s.cacheMu.Unlock()

	stats := s.computeSnapshot(days)

	s.cacheMu.Lock()
	if s.cache.days != days {
		s.cache.ips = nil
	}
	s.cache.days = days
	s.cache.stats = stats
	s.cache.at = time.Now()
	s.cacheMu.Unlock()
	return stats
}

func (s *usageLog) computeSnapshot(days int) map[string]any {
	s.mu.Lock()
	recs := append([]UsageRecord(nil), s.records...)
	s.mu.Unlock()

	cutoff := time.Now().AddDate(0, 0, -days)
	loc := time.Now().Location()
	today := time.Now().In(loc).Truncate(24 * time.Hour)
	dayAgo := time.Now().Add(-24 * time.Hour)

	var (
		requests, in, out, cache, durationMs int64
		todayReq, todayTok                   int64
		h24Req, h24Tok                       int64
	)
	keyCounts := map[string]*usageCountStat{}
	modelCounts := map[string]*usageCountStat{}
	endpointCounts := map[string]*usageCountStat{}
	trendMap := map[string]*usageTrendPoint{}

	for _, rec := range recs {
		if rec.Time.Before(cutoff) {
			continue
		}
		requests++
		reqTok := rec.InputTokens + rec.OutputTokens + rec.CacheTokens
		in += rec.InputTokens
		out += rec.OutputTokens
		cache += rec.CacheTokens
		durationMs += rec.DurationMs
		if rec.Time.After(today) {
			todayReq++
			todayTok += reqTok
		}
		if rec.Time.After(dayAgo) {
			h24Req++
			h24Tok += reqTok
		}
		key := rec.APIKeyPrefix
		ks, ok := keyCounts[key]
		if !ok {
			ks = &usageCountStat{}
			keyCounts[key] = ks
		}
		ks.Requests++
		ks.Tokens += reqTok
		if mc, ok := modelCounts[rec.Model]; ok {
			mc.Requests++
			mc.Tokens += reqTok
		} else {
			modelCounts[rec.Model] = &usageCountStat{Requests: 1, Tokens: reqTok}
		}
		if ec, ok := endpointCounts[rec.Endpoint]; ok {
			ec.Requests++
			ec.Tokens += reqTok
		} else {
			endpointCounts[rec.Endpoint] = &usageCountStat{Requests: 1, Tokens: reqTok}
		}
		date := rec.Time.In(loc).Format("01-02")
		if tp, ok := trendMap[date]; ok {
			tp.Requests++
			tp.Tokens += reqTok
		} else {
			trendMap[date] = &usageTrendPoint{Date: date, Requests: 1, Tokens: reqTok}
		}
	}

	avgMs := int64(0)
	if requests > 0 {
		avgMs = durationMs / requests
	}

	model := make([]map[string]any, 0, len(modelCounts))
	for name, c := range modelCounts {
		model = append(model, map[string]any{"name": name, "requests": c.Requests, "tokens": c.Tokens})
	}
	sort.Slice(model, func(i, j int) bool { return model[i]["tokens"].(int64) > model[j]["tokens"].(int64) })

	ep := make([]map[string]any, 0, len(endpointCounts))
	for k, c := range endpointCounts {
		ep = append(ep, map[string]any{"endpoint": k, "requests": c.Requests, "tokens": c.Tokens})
	}
	sort.Slice(ep, func(i, j int) bool { return ep[i]["tokens"].(int64) > ep[j]["tokens"].(int64) })

	keys := make([]map[string]any, 0, len(keyCounts))
	for k, c := range keyCounts {
		keys = append(keys, map[string]any{"api_key_prefix": k, "requests": c.Requests, "tokens": c.Tokens})
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i]["requests"].(int64) > keys[j]["requests"].(int64) })

	trend := make([]map[string]any, 0, len(trendMap))
	for _, t := range trendMap {
		trend = append(trend, map[string]any{"date": t.Date, "requests": t.Requests, "tokens": t.Tokens})
	}
	sort.Slice(trend, func(i, j int) bool { return trend[i]["date"].(string) < trend[j]["date"].(string) })

	return map[string]any{
		"summary": map[string]any{
			"requests":         requests,
			"tokens":           in + out + cache,
			"input":            in,
			"output":           out,
			"cache":            cache,
			"avg_ms":           avgMs,
			"today_requests":   todayReq,
			"today_tokens":     todayTok,
			"last24h_requests": h24Req,
			"last24h_tokens":   h24Tok,
		},
		"models":    model,
		"endpoints": ep,
		"keys":      keys,
		"trend":     trend,
	}
}

func (s *usageLog) ipSnapshot(days int) []map[string]any {
	if days <= 0 {
		days = 30
	}
	s.cacheMu.Lock()
	if s.cache.ips != nil && s.cache.days == days && time.Since(s.cache.at) < usageSnapshotTTL {
		cached := s.cache.ips
		s.cacheMu.Unlock()
		return cached
	}
	s.cacheMu.Unlock()

	out := s.computeIPSnapshot(days)

	s.cacheMu.Lock()
	if s.cache.days != days {
		s.cache.stats = nil
		s.cache.days = days
		s.cache.at = time.Now()
	}
	s.cache.ips = out
	s.cacheMu.Unlock()
	return out
}

func (s *usageLog) computeIPSnapshot(days int) []map[string]any {
	s.mu.Lock()
	recs := append([]UsageRecord(nil), s.records...)
	s.mu.Unlock()
	cutoff := time.Now().AddDate(0, 0, -days)
	type stat struct {
		Requests, Tokens int64
		Last             time.Time
	}
	counts := map[string]*stat{}
	for _, rec := range recs {
		if rec.ClientIP == "" || rec.Time.Before(cutoff) {
			continue
		}
		v := counts[rec.ClientIP]
		if v == nil {
			v = &stat{}
			counts[rec.ClientIP] = v
		}
		v.Requests++
		v.Tokens += rec.InputTokens + rec.OutputTokens + rec.CacheTokens
		if rec.Time.After(v.Last) {
			v.Last = rec.Time
		}
	}
	out := make([]map[string]any, 0, len(counts))
	for ip, v := range counts {
		out = append(out, map[string]any{"ip": ip, "requests": v.Requests, "tokens": v.Tokens, "lastSeen": v.Last})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["requests"].(int64) > out[j]["requests"].(int64) })
	return out
}

// logs returns one page. It used to copy every retained record just to slice a
// handful out, holding the same mutex the request hot path appends under.
func (s *usageLog) logs(limit, offset int) map[string]any {
	s.mu.Lock()
	total := len(s.records)
	if offset > total {
		offset = total
	}
	start := total - offset - limit
	if start < 0 {
		start = 0
	}
	end := total - offset
	if end < 0 {
		end = 0
	}
	if start >= end {
		s.mu.Unlock()
		return map[string]any{"logs": []UsageRecord{}, "total": total}
	}
	out := make([]UsageRecord, end-start)
	copy(out, s.records[start:end])
	s.mu.Unlock()

	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return map[string]any{"logs": out, "total": total}
}

type usageCountStat struct {
	Requests int64
	Tokens   int64
}

type usageTrendPoint struct {
	Date     string `json:"date"`
	Requests int64  `json:"requests"`
	Tokens   int64  `json:"tokens"`
}
