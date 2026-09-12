package chathub

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

type pooledConn struct {
	conn      *websocket.Conn
	created   time.Time
	handshook bool
	taken     atomic.Bool
	writeMu   sync.Mutex
	frames    chan []byte
	errs      chan error
}

const (
	maxPoolPerKey = 2
	poolConnTTL   = 300 * time.Second
)

// DialOptions are the connection-level knobs that ride on the upstream
// websocket URL. They are NOT interchangeable between connections: upstream
// resolves memory behaviour from the URL at dial time, and the chat payload has
// no equivalent — so a connection dialed with disableMemory unset runs WITH
// account memory no matter which request later reuses it.
//
// Keying the pool on oid|tid alone (the behaviour until 2026-09-11) meant every
// request served from a parked connection silently ran with the dial-time
// options: memory came back on despite M365_ENABLE_UPSTREAM_MEMORY=false and
// copilot_temp_session had no effect. A live probe returned the same stale
// account facts to three unrelated sessions and to an explicit temp-session
// control. Signature() is what makes reuse safe.
type DialOptions struct {
	ConversationID string
	SessionID      string
	LicenseType    string
	Scenario       string
	DisableMemory  bool
}

// Signature renders the options that a parked connection CANNOT be re-dialed
// for later, as a stable pool-key fragment.
//
// Only the transport-level flags join the key. Conversation and session ids are
// deliberately excluded, because they are per-request values: including them
// would make every agent turn a unique key, which both destroys the pool hit
// rate and lets parked connections accumulate without bound. Excluding them is
// safe because the gateway ships the caller's complete history inside the chat
// payload (incremental dispatch is off by default), so a connection dialed for
// one conversation still answers another correctly. The memory flag is the
// opposite case: it exists only as a dial-time URL query parameter with no
// payload equivalent, so it can never be corrected after the dial.
//
// Fields are length-prefixed so a value containing the separator cannot alias a
// different combination.
func (o DialOptions) Signature() string {
	return fmt.Sprintf("%d:%s|%d:%s|%t",
		len(o.LicenseType), o.LicenseType,
		len(o.Scenario), o.Scenario,
		o.DisableMemory)
}

// parkForwardTTL bounds how long the read pump waits for a consumer to accept a
// frame before declaring the lease dead. It is a var so tests can shorten it.
var parkForwardTTL = 30 * time.Second

// errParkConsumerStalled is reported to a lease whose consumer stopped reading
// frames, so it fails fast instead of blocking on a channel nobody feeds.
var errParkConsumerStalled = errors.New("chathub: parked connection consumer stalled")

type ConnPool struct {
	mu    sync.Mutex
	conns map[string][]*pooledConn // key = oid|tid
	// leased tracks connections handed out from the pool (i.e. ones that already
	// own a permanent reader goroutine), so Return can tell them apart from
	// freshly dialed connections. Value is the lease timestamp, used by GC as a
	// safety net against callers that never Return or Discard.
	leased map[*websocket.Conn]time.Time
	dialer *websocket.Dialer
	header http.Header
	// lastWarm throttles top-ups after a pool miss (see ShouldWarm).
	lastWarm map[string]time.Time
	stop     chan struct{}
	closed   bool
}

func NewConnPool(dialer *websocket.Dialer, header http.Header) *ConnPool {
	p := &ConnPool{
		conns:    make(map[string][]*pooledConn),
		leased:   make(map[*websocket.Conn]time.Time),
		dialer:   dialer,
		header:   header,
		lastWarm: make(map[string]time.Time),
		stop:     make(chan struct{}),
	}
	go p.gcLoop()
	return p
}

// key scopes a parked connection to one account AND one dial identity. The
// signature must be part of the key: upstream binds session, conversation and
// memory behaviour at dial time, so a connection dialed for one identity must
// never be handed to a request with another.
func (p *ConnPool) key(oid, tid, sig string) string { return oid + "|" + tid + "|" + sig }

// newPooledConn builds a parked-connection record with its frame channels
// already allocated. The channels must exist before the entry is published to
// p.conns: a concurrent Take that picks the entry reads pc.frames/pc.errs
// directly, and nil channels would leave that request waiting forever.
func newPooledConn(conn *websocket.Conn) *pooledConn {
	return &pooledConn{
		conn:    conn,
		created: time.Now(),
		frames:  make(chan []byte, 64),
		errs:    make(chan error, 1),
	}
}

// startPark keeps a parked connection alive by answering SignalR pings while
// it waits in the pool. The pump is the connection's PERMANENT single reader:
// gorilla poisons a conn after any read error (including deadline expiry), so
// ownership is never handed off. Once taken, frames are forwarded to Chat via
// channels instead.
func (p *ConnPool) startPark(key string, pc *pooledConn) {
	go func() {
		forward := time.NewTimer(parkForwardTTL)
		defer forward.Stop()
		// fail is the only way this pump terminates a lease: the consumer is
		// woken through errs and frames is closed so a blocked receive returns.
		// It runs at most once because every caller returns immediately after.
		fail := func(err error) {
			select {
			case pc.errs <- err:
			default:
			}
			close(pc.frames)
		}
		for {
			_, msg, err := pc.conn.ReadMessage()
			if err != nil {
				// Decide, under the pool lock, whether this connection still belongs
				// to the pool or has been handed to a consumer. Take now sets taken
				// atomically with removing the conn from the pool, so checking again
				// here cannot race it: taken-out conns get the consumer's error,
				// genuinely parked conns get reclaimed. This closes the window where
				// a freshly-handed-out connection was closed behind the caller's
				// back, leaving it blocking on an open frames channel.
				p.mu.Lock()
				leased := pc.taken.Load()
				if leased {
					p.mu.Unlock()
					fail(err)
				} else {
					removePooledLocked(p.conns, key, pc)
					p.mu.Unlock()
					_ = pc.conn.Close()
				}
				return
			}
			if strings.HasPrefix(string(msg), `{"type":6}`) && !pc.taken.Load() {
				pc.writeMu.Lock()
				_ = pc.conn.WriteMessage(websocket.TextMessage, []byte(`{"type":6}`+rs))
				pc.writeMu.Unlock()
				continue
			}
			if pc.taken.Load() {
				if !forward.Stop() {
					select {
					case <-forward.C:
					default:
					}
				}
				forward.Reset(parkForwardTTL)
				select {
				case pc.frames <- msg:
				case <-forward.C:
					// The consumer stopped reading. Returning bare would leave
					// it blocked on frames forever, so terminate the lease
					// explicitly and drop the connection.
					fail(errParkConsumerStalled)
					p.evict(key, pc)
					return
				}
			}
		}
	}()
}

func (p *ConnPool) evict(key string, target *pooledConn) {
	p.mu.Lock()
	removePooledLocked(p.conns, key, target)
	p.mu.Unlock()
	_ = target.conn.Close()
}

// removePooledLocked drops target from the pool slice for key. Callers must hold
// p.mu. It is the single place a connection leaves the parked set, so the park
// pump and Take agree, under the same lock, on who owns it next.
func removePooledLocked(conns map[string][]*pooledConn, key string, target *pooledConn) {
	list := conns[key]
	for i, pc := range list {
		if pc == target {
			conns[key] = append(list[:i], list[i+1:]...)
			break
		}
	}
}

// Take hands out a connection for one request. A parked connection is eligible
// only when its dial identity (DialOptions) matches the caller's exactly;
// bypassPool forces a fresh dial even when a matching connection is parked:
// after a transport-level failure the request is retried on a brand-new
// connection instead of reusing one whose parked siblings may be in the same
// bad state.
func (p *ConnPool) Take(ctx context.Context, oid, tid, wsURL string, opts DialOptions, bypassPool bool) (*websocket.Conn, *sync.Mutex, <-chan []byte, <-chan error, bool, error) {
	p.mu.Lock()
	key := p.key(oid, tid, opts.Signature())
	conns := p.conns[key]
	var picked *pooledConn
	var stale []*pooledConn
	kept := conns[:0]
	for _, pc := range conns {
		if picked == nil && !bypassPool && pc.handshook && time.Since(pc.created) < poolConnTTL {
			picked = pc
			continue
		}
		if time.Since(pc.created) >= poolConnTTL {
			stale = append(stale, pc)
			continue
		}
		kept = append(kept, pc)
	}
	if picked != nil {
		// taken.Store must happen in the same critical section that removes the
		// connection from the pool. If it happened after the unlock, the park pump
		// could observe taken==false in between and reclaim (close) a connection we
		// are about to hand out, stranding the caller on an open frames channel.
		picked.taken.Store(true)
		p.leased[picked.conn] = time.Now()
	}
	if len(kept) == 0 {
		delete(p.conns, key)
	} else {
		p.conns[key] = kept
	}
	p.mu.Unlock()

	for _, pc := range stale {
		pc.taken.Store(true)
		_ = pc.conn.Close()
	}

	if picked != nil {
		log.Printf("[connpool] hit oid=%s age_ms=%d memory=%t conv=%s", oid, time.Since(picked.created).Milliseconds(), !opts.DisableMemory, shortID(opts.ConversationID))
		return picked.conn, &picked.writeMu, picked.frames, picked.errs, true, nil
	}

	conn, resp, err := p.dialer.DialContext(ctx, wsURL, p.header.Clone())
	if err != nil {
		if resp != nil {
			log.Printf("[connpool] dial failed oid=%s status=%d", oid, resp.StatusCode)
		}
		return nil, nil, nil, nil, false, err
	}
	return conn, nil, nil, nil, false, nil
}

// warmMinInterval throttles post-miss top-ups so that a burst of parallel
// requests cannot double the upstream dial rate.
const warmMinInterval = 10 * time.Second

// ShouldWarm reports whether the pool should top a key up after a miss.
//
// Warming only when a connection was reused deadlocks the pool: parked
// connections expire after poolConnTTL, and with no hits there is no warm, so
// the pool never refills and every later request pays a fresh dial. Observed
// 2026-09-10: 137 requests after start-up, 137 with reused=false, 0 pool hits —
// the start-up warm batch had already aged past its TTL before the first
// request arrived, and nothing ever warmed again.
func (p *ConnPool) ShouldWarm(oid, tid string, opts DialOptions) bool {
	key := p.key(oid, tid, opts.Signature())
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	now := time.Now()
	if now.Sub(p.lastWarm[key]) < warmMinInterval {
		return false
	}
	p.lastWarm[key] = now
	return true
}

// Warm dials and parks one connection for the given dial identity. The wsURL
// MUST have been built with the same DialOptions, otherwise the parked entry is
// filed under a key no request will ever look up (and, before the signature was
// part of the key, would have been handed to a request that never asked for
// those options).
func (p *ConnPool) Warm(ctx context.Context, acc Account, wsURL string, opts DialOptions) {
	if wsURL == "" {
		return
	}
	key := p.key(acc.OID, acc.TID, opts.Signature())

	p.mu.Lock()
	if len(p.conns[key]) >= maxPoolPerKey {
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()

	conn, resp, err := p.dialer.DialContext(ctx, wsURL, p.header.Clone())
	if err != nil {
		if resp != nil {
			log.Printf("[connpool] warm dial failed oid=%s status=%d err=%v", acc.OID, resp.StatusCode, err)
		} else {
			log.Printf("[connpool] warm dial failed oid=%s err=%v", acc.OID, err)
		}
		return
	}

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"protocol":"json","version":1}`+"\x1e")); err != nil {
		log.Printf("[connpool] warm handshake send failed: %v", err)
		_ = conn.Close()
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, _, err = conn.ReadMessage()
	if err != nil {
		log.Printf("[connpool] warm handshake recv failed: %v", err)
		_ = conn.Close()
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	p.park(key, conn)

	log.Printf("[connpool] warmed oid=%s tid=%s memory=%t conv=%s", acc.OID, acc.TID, !opts.DisableMemory, shortID(opts.ConversationID))
}

// shortID trims an identifier for logging without losing the ability to tell
// two identities apart.
func shortID(s string) string {
	if s == "" {
		return "-"
	}
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}

// park is the ONLY way a connection enters the pool, and it is the only place
// the pump is started. It exists as a single chokepoint because the caller must
// not have read from the connection itself: startPark makes the pump the
// connection's permanent sole reader, and any second reader (a request-scoped
// one, for example) would share gorilla's frame parser with it and corrupt it.
//
// The corruption is deferred, which is what made it hard to trace: the two
// readers only tear the parser state apart, and the damage is observed by the
// NEXT request that takes the connection, as "RSV2 set, bad opcode" — long
// after the code that caused it has returned successfully.
//
// The caller must publish with no read deadline armed: startPark reads
// immediately, and a stale deadline would evict what was just parked.
func (p *ConnPool) park(key string, conn *websocket.Conn) {
	pc := newPooledConn(conn)
	pc.handshook = true
	p.mu.Lock()
	// A park that lands after Close would re-add a socket to a pool whose GC
	// loop has already exited, so nothing would ever reap it. Close the
	// connection instead of parking it.
	if p.closed || len(p.conns[key]) >= maxPoolPerKey {
		p.mu.Unlock()
		_ = conn.Close()
		return
	}
	p.conns[key] = append(p.conns[key], pc)
	p.mu.Unlock()
	p.startPark(key, pc)
}

// Discard drops a connection that must not be reused.
func (p *ConnPool) Discard(oid, tid string, conn *websocket.Conn) {
	if conn == nil {
		return
	}
	p.mu.Lock()
	delete(p.leased, conn)
	p.mu.Unlock()
	_ = conn.Close()
}

func (p *ConnPool) GC() {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for k, conns := range p.conns {
		kept := conns[:0]
		for _, pc := range conns {
			if now.Sub(pc.created) > poolConnTTL {
				pc.taken.Store(true)
				_ = pc.conn.Close()
			} else {
				kept = append(kept, pc)
			}
		}
		if len(kept) == 0 {
			delete(p.conns, k)
		} else {
			p.conns[k] = kept
		}
	}
	// Safety net: a caller that neither returns nor discards its lease would
	// otherwise pin an entry here forever.
	for conn, leasedAt := range p.leased {
		if now.Sub(leasedAt) > 2*poolConnTTL {
			delete(p.leased, conn)
		}
	}
}

func (p *ConnPool) Close() {
	// Guard against being called more than once: closing p.stop twice panics on
	// the second close, and that can happen if both a deferred shutdown and a
	// fatal handler own the pool.
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()
	close(p.stop)
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, conns := range p.conns {
		for _, pc := range conns {
			pc.taken.Store(true)
			_ = pc.conn.Close()
		}
		delete(p.conns, k)
	}
	for conn := range p.leased {
		delete(p.leased, conn)
	}
}

func (p *ConnPool) Stats() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	total := 0
	details := make([]map[string]any, 0)
	for k, conns := range p.conns {
		for _, pc := range conns {
			total++
			details = append(details, map[string]any{"key": k, "age_ms": time.Since(pc.created).Milliseconds(), "handshook": pc.handshook})
		}
	}
	return map[string]any{"mode": "connpool", "pooled_connections": total, "leased_connections": len(p.leased), "details": details}
}

func (p *ConnPool) gcLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
			p.GC()
		}
	}
}
