package chathub

import (
	"context"
	"errors"
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
	stop   chan struct{}
	closed bool
}

func NewConnPool(dialer *websocket.Dialer, header http.Header) *ConnPool {
	p := &ConnPool{
		conns:  make(map[string][]*pooledConn),
		leased: make(map[*websocket.Conn]time.Time),
		dialer: dialer,
		header: header,
		stop:   make(chan struct{}),
	}
	go p.gcLoop()
	return p
}

func (p *ConnPool) key(oid, tid string) string { return oid + "|" + tid }

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
					pc.conn.Close()
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
	target.conn.Close()
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

func (p *ConnPool) Take(ctx context.Context, oid, tid string, wsURL string) (*websocket.Conn, *sync.Mutex, <-chan []byte, <-chan error, bool, error) {
	p.mu.Lock()
	key := p.key(oid, tid)
	conns := p.conns[key]
	var picked *pooledConn
	var stale []*pooledConn
	kept := conns[:0]
	for _, pc := range conns {
		if picked == nil && pc.handshook && time.Since(pc.created) < poolConnTTL {
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
		pc.conn.Close()
	}

	if picked != nil {
		log.Printf("[connpool] hit oid=%s age_ms=%d", oid, time.Since(picked.created).Milliseconds())
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

func (p *ConnPool) Warm(ctx context.Context, acc Account, wsURL string) {
	if wsURL == "" {
		return
	}
	key := p.key(acc.OID, acc.TID)

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
		conn.Close()
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, _, err = conn.ReadMessage()
	if err != nil {
		log.Printf("[connpool] warm handshake recv failed: %v", err)
		conn.Close()
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	pc := newPooledConn(conn)
	pc.handshook = true
	p.mu.Lock()
	if len(p.conns[key]) >= maxPoolPerKey {
		p.mu.Unlock()
		conn.Close()
		return
	}
	p.conns[key] = append(p.conns[key], pc)
	p.mu.Unlock()
	p.startPark(key, pc)

	log.Printf("[connpool] warmed connection oid=%s tid=%s", acc.OID, acc.TID)
}

func (p *ConnPool) WarmWithProbe(ctx context.Context, acc Account, wsURL string) {
	p.Warm(ctx, acc, wsURL)
}

// Return re-parks a healthy connection so the next request for the same account
// can skip the WebSocket handshake.
//
// Only freshly dialed connections are re-parked. A connection taken from the
// pool already owns a permanent reader goroutine, and gorilla forbids concurrent
// readers, so parking it again would either race that reader or risk handing a
// frame from one request to another. Those are closed instead.
func (p *ConnPool) Return(oid, tid string, conn *websocket.Conn) {
	if conn == nil {
		return
	}
	key := p.key(oid, tid)

	p.mu.Lock()
	_, wasPooled := p.leased[conn]
	delete(p.leased, conn)
	full := len(p.conns[key]) >= maxPoolPerKey
	p.mu.Unlock()

	if wasPooled || full || oid == "" {
		conn.Close()
		return
	}

	// Callers arm short deadlines before handing the connection back; a parked
	// connection must have none, otherwise the read pump trips immediately and
	// evicts what we just parked. Clear them BEFORE publishing: a concurrent
	// Take could otherwise hand out a connection whose pump dies on the stale
	// deadline.
	_ = conn.SetReadDeadline(time.Time{})
	_ = conn.SetWriteDeadline(time.Time{})

	pc := newPooledConn(conn)
	pc.handshook = true
	p.mu.Lock()
	if len(p.conns[key]) >= maxPoolPerKey {
		p.mu.Unlock()
		conn.Close()
		return
	}
	p.conns[key] = append(p.conns[key], pc)
	p.mu.Unlock()
	p.startPark(key, pc)
	log.Printf("[connpool] parked returned connection oid=%s", oid)
}

// Discard drops a connection that must not be reused.
func (p *ConnPool) Discard(oid, tid string, conn *websocket.Conn) {
	if conn == nil {
		return
	}
	p.mu.Lock()
	delete(p.leased, conn)
	p.mu.Unlock()
	conn.Close()
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
				pc.conn.Close()
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
			pc.conn.Close()
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
