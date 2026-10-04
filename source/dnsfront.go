package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	ipFreebind   = 15   // IP_FREEBIND
	ipv6Freebind = 78   // IPV6_FREEBIND
	maxInflight  = 8192 // queries being worked on at once; a slow upstream must not make the proxy drop at 1024
	udpReaders   = 4    // goroutines reading the UDP socket
	udpWorkers   = 64   // goroutines that stay alive to answer UDP queries (see readUDP)
	tcpIdle      = 10 * time.Second
)

// DNSFrontend serves DNS on one VIP (UDP + TCP) and relays every query
// through the pool's fastest healthy upstream.
type DNSFrontend struct {
	addr netip.Addr
	port int
	pool func() *Pool
	gw   *srvSeries // the gateway's history (srvhist.go): client latency, errors, cache hits; nil when not set

	mu      sync.Mutex
	udp     net.PacketConn
	tcp     net.Listener
	dot     net.Listener // DNS over TLS, when dotPort is set
	dotPort int
	doh     *http.Server // DNS over HTTPS, when dohPort is set
	dohPort int
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	sem     chan struct{}

	fmu    sync.Mutex
	flight map[string]chan struct{} // cache keys being fetched from an upstream right now

	upd updateState
}

func NewDNSFrontend(addr netip.Addr, port int, pool func() *Pool) *DNSFrontend {
	return &DNSFrontend{addr: addr, port: port, pool: pool}
}

func (f *DNSFrontend) listenAddr() string {
	return net.JoinHostPort(f.addr.String(), strconv.Itoa(f.port))
}

// freebind lets us bind the VIP before the kernel has it configured
// (e.g. an AFN whose address is added a moment later).
func freebind(network string, _ string, c syscall.RawConn) error {
	var serr error
	err := c.Control(func(fd uintptr) {
		if network == "udp6" || network == "tcp6" {
			serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, ipv6Freebind, 1)
		} else {
			serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, ipFreebind, 1)
		}
	})
	if err != nil {
		return err
	}
	return serr
}

func (f *DNSFrontend) Start() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.udp != nil {
		return nil
	}
	lc := net.ListenConfig{Control: freebind}
	netw := "4"
	if f.addr.Is6() {
		netw = "6"
	}
	ctx := context.Background()
	udp, err := lc.ListenPacket(ctx, "udp"+netw, f.listenAddr())
	if err != nil {
		return err
	}
	tcp, err := lc.Listen(ctx, "tcp"+netw, f.listenAddr())
	if err != nil {
		udp.Close()
		return err
	}
	f.udp, f.tcp = udp, tcp
	f.ctx, f.cancel = context.WithCancel(context.Background())
	f.sem = make(chan struct{}, maxInflight)
	f.wg.Add(2)
	go f.serveUDP(udp)
	go f.serveTCP(tcp)
	infof("dns: proxy listening on %s (udp+tcp)", f.listenAddr())
	f.startDoTLocked(lc, netw)
	f.startDoHLocked(lc, netw)
	return nil
}

func (f *DNSFrontend) Stop() {
	f.mu.Lock()
	if f.udp == nil {
		f.mu.Unlock()
		return
	}
	f.cancel()
	f.udp.Close()
	f.tcp.Close()
	if f.dot != nil {
		f.dot.Close()
		f.dot = nil
	}
	if f.doh != nil {
		f.doh.Close()
		f.doh = nil
	}
	f.udp, f.tcp = nil, nil
	f.mu.Unlock()
	f.wg.Wait()
	infof("dns: proxy on %s stopped", f.listenAddr())
}

func (f *DNSFrontend) Listening() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.udp != nil
}

// resolve forwards one query; on failure it answers SERVFAIL so clients fail
// fast instead of timing out.
func (f *DNSFrontend) resolve(query []byte, tcp bool, client netip.Addr) []byte {
	// who may ask, and how often, comes first: before the cache and before dynamic updates are relayed
	if p := f.pool(); p != nil {
		if v := p.admit(client); v != admitOK {
			return p.turnedAway(query, tcp, v)
		}
	}
	if isUpdateMsg(query) {
		return f.handleUpdate(query, tcp, client)
	}
	var t0 time.Time
	timed := f.gw.sampled()
	if timed {
		t0 = time.Now()
	}
	resp := f.resolveRaw(query, tcp, client)
	if timed && f.ctx.Err() == nil {
		f.gw.lat(time.Since(t0))
	}
	if f.ctx.Err() == nil {
		if name, qt, ok := questionOf(query); ok {
			rc := rcodeServFail
			if h, hok := parseHeader(resp); hok {
				rc = h.rcode
			}
			qstats.Record(client, name, qt, tcp, rc)
			if rc == rcodeServFail || rc == rcodeRefused {
				f.gw.addFail()
			} else {
				f.gw.addOK()
			}
		}
	}
	return resp
}

func (f *DNSFrontend) resolveRaw(query []byte, tcp bool, client netip.Addr) []byte {
	p := f.pool()
	if p == nil {
		return errorResponse(query, rcodeServFail)
	}
	var ckey string
	cacheable := false
	if c := p.cache; c != nil {
		if ckey, cacheable = c.keyFor(query, tcp, client, p.cfg.ECS, p.cfg.ECSPrefix4, p.cfg.ECSPrefix6); cacheable {
			if r := c.get(ckey, query); r != nil {
				c.Hits.Add(1)
				qstats.RecordCache(true)
				f.gw.addHit()
				p.Queries.Add(1)
				p.Answered.Add(1)
				return r
			}
			// the same question already on its way to an upstream: wait for that answer instead of sending another
			done, leader := f.joinFlight(ckey)
			if !leader {
				select {
				case <-done:
				case <-f.ctx.Done():
				}
				if r := c.get(ckey, query); r != nil { // served from the leader's answer: a hit
					c.Hits.Add(1)
					qstats.RecordCache(true)
					f.gw.addHit()
					p.Queries.Add(1)
					p.Answered.Add(1)
					return r
				}
			} else {
				defer f.endFlight(ckey, done)
			}
			c.Misses.Add(1)
			qstats.RecordCache(false)
		} else {
			c.Bypassed.Add(1)
		}
	}
	resp, err := p.ForwardFrom(f.ctx, query, tcp, client)
	if err == nil && cacheable {
		p.cache.put(ckey, resp)
	}
	if err != nil {
		if f.ctx.Err() == nil {
			p.Failed.Add(1)
			debugf("dns: %v — answering SERVFAIL", err)
		}
		return errorResponse(query, rcodeServFail)
	}
	return resp
}

// bigRcvBuf asks for a large receive buffer on the listening socket: a burst of queries that arrives while every
// reader is busy waits there instead of being dropped by the kernel (the client sees that as a timeout).  The
// FORCE variant ignores net.core.rmem_max and needs privilege, so the plain one is the fallback.
func bigRcvBuf(pc net.PacketConn) {
	uc, ok := pc.(*net.UDPConn)
	if !ok {
		return
	}
	const want = 8 << 20
	if rc, err := uc.SyscallConn(); err == nil {
		forced := false
		rc.Control(func(fd uintptr) {
			forced = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUFFORCE, want) == nil
		})
		if forced {
			return
		}
	}
	uc.SetReadBuffer(want)
}

// joinFlight registers a fetch of key.  The first caller is the leader (done is its channel, to be passed to
// endFlight); the others get the leader's channel to wait on.
func (f *DNSFrontend) joinFlight(key string) (done chan struct{}, leader bool) {
	f.fmu.Lock()
	defer f.fmu.Unlock()
	if ch, ok := f.flight[key]; ok {
		return ch, false
	}
	if f.flight == nil {
		f.flight = map[string]chan struct{}{}
	}
	ch := make(chan struct{})
	f.flight[key] = ch
	return ch, true
}

func (f *DNSFrontend) endFlight(key string, done chan struct{}) {
	f.fmu.Lock()
	delete(f.flight, key)
	f.fmu.Unlock()
	close(done)
}

func (f *DNSFrontend) serveUDP(pc net.PacketConn) {
	defer f.wg.Done()
	bigRcvBuf(pc)
	rep := newUDPReplier(pc)
	defer rep.close() // after the readers are gone; the Stop that closed pc also waits for the workers
	var rw sync.WaitGroup
	// Each reader has its own small set of workers and its own hand-off channel.  One channel shared by all 64
	// workers (each also waiting on the shutdown channel) was the biggest lock wait left at 98k queries a second:
	// every hand-off and every wake-up took the same two channel locks.
	for i := 0; i < udpReaders; i++ {
		jobs := make(chan udpJob) // unbuffered: a query goes only to a worker that is idle right now
		for w := 0; w < udpWorkers/udpReaders; w++ {
			f.wg.Add(1)
			go f.udpWorker(jobs)
		}
		rw.Add(1)
		go func() {
			defer rw.Done()
			defer close(jobs) // the workers end with their reader
			f.readUDP(pc, rep, jobs)
		}()
	}
	rw.Wait()
}

type udpJob struct {
	rep    *udpReplier
	q      []byte
	client net.Addr
}

func (f *DNSFrontend) answerUDP(j udpJob) {
	if resp := f.resolve(j.q, false, addrOf(j.client)); resp != nil {
		j.rep.WriteTo(resp, j.client)
	}
}

// udpWorker answers queries for as long as the frontend runs.  A goroutine per query costs a new stack that has
// to grow to the depth of the resolver for every one of them (a tenth of the CPU at 90k queries a second), so a
// set of workers stays alive and keeps its stack; when they are all busy (an upstream is slow) the reader falls
// back to a goroutine per query, up to maxInflight of them.
func (f *DNSFrontend) udpWorker(jobs <-chan udpJob) {
	defer f.wg.Done()
	for j := range jobs {
		f.answerUDP(j)
	}
}

func (f *DNSFrontend) readUDP(pc net.PacketConn, rep *udpReplier, jobs chan<- udpJob) {
	buf := make([]byte, 65535)
	for {
		n, client, err := pc.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if n < 12 {
			continue
		}
		j := udpJob{rep: rep, q: append([]byte(nil), buf[:n]...), client: client}
		select {
		case jobs <- j:
			continue
		default:
		}
		select {
		case f.sem <- struct{}{}:
		default:
			continue // overloaded: drop, client will retry
		}
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			defer func() { <-f.sem }()
			f.answerUDP(j)
		}()
	}
}

func (f *DNSFrontend) serveTCP(l net.Listener) {
	defer f.wg.Done()
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			f.handleTCP(c)
		}()
	}
}

func (f *DNSFrontend) handleTCP(c net.Conn) {
	defer c.Close()
	stop := context.AfterFunc(f.ctx, func() { c.Close() })
	defer stop()
	for {
		c.SetReadDeadline(time.Now().Add(tcpIdle))
		var lb [2]byte
		if _, err := io.ReadFull(c, lb[:]); err != nil {
			return
		}
		q := make([]byte, binary.BigEndian.Uint16(lb[:]))
		if _, err := io.ReadFull(c, q); err != nil {
			return
		}
		resp := f.resolve(q, true, addrOf(c.RemoteAddr()))
		if resp == nil {
			return
		}
		out := make([]byte, 2+len(resp))
		binary.BigEndian.PutUint16(out, uint16(len(resp)))
		copy(out[2:], resp)
		c.SetWriteDeadline(time.Now().Add(tcpIdle))
		if _, err := c.Write(out); err != nil {
			return
		}
	}
}

// dotCertificate hands out the certificate for DNS over TLS: the one the web GUI serves (set in main).
var dotCertificate func(*tls.ClientHelloInfo) (*tls.Certificate, error)

// startDoTLocked opens the DNS-over-TLS listener (RFC 7858) when a port is set. A failure is logged and
// leaves plain DNS running. Clients are served exactly as over TCP: length-prefixed messages on one
// connection, the same idle limit.
func (f *DNSFrontend) startDoTLocked(lc net.ListenConfig, netw string) {
	if f.dotPort == 0 {
		return
	}
	addr := net.JoinHostPort(f.addr.String(), strconv.Itoa(f.dotPort))
	if dotCertificate == nil {
		errorf("dns: cannot serve DNS over TLS on %s: no certificate is available", addr)
		return
	}
	l, err := lc.Listen(context.Background(), "tcp"+netw, addr)
	if err != nil {
		errorf("dns: cannot listen for DNS over TLS on %s: %v", addr, err)
		return
	}
	f.dot = tls.NewListener(l, &tls.Config{GetCertificate: dotCertificate, MinVersion: tls.VersionTLS12, NextProtos: []string{"dot"}})
	f.wg.Add(1)
	go f.serveTCP(f.dot)
	infof("dns: DNS over TLS listening on %s", addr)
}

// dohPath is where DNS over HTTPS is served (RFC 8484's usual path).
const dohPath = "/dns-query"

// startDoHLocked opens the DNS-over-HTTPS listener when a port is set (HTTP/2 and HTTP/1.1, TLS 1.2+, the
// web GUI's certificate). A failure is logged and leaves the other listeners running.
func (f *DNSFrontend) startDoHLocked(lc net.ListenConfig, netw string) {
	if f.dohPort == 0 {
		return
	}
	addr := net.JoinHostPort(f.addr.String(), strconv.Itoa(f.dohPort))
	if dotCertificate == nil {
		errorf("dns: cannot serve DNS over HTTPS on %s: no certificate is available", addr)
		return
	}
	l, err := lc.Listen(context.Background(), "tcp"+netw, addr)
	if err != nil {
		errorf("dns: cannot listen for DNS over HTTPS on %s: %v", addr, err)
		return
	}
	srv := &http.Server{
		Handler:           http.HandlerFunc(f.serveDoH),
		ReadHeaderTimeout: tcpIdle,
		ReadTimeout:       tcpIdle,
		WriteTimeout:      tcpIdle,
		IdleTimeout:       tcpIdle,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	f.doh = srv
	tl := tls.NewListener(l, &tls.Config{GetCertificate: dotCertificate, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1"}})
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		srv.Serve(tl)
	}()
	infof("dns: DNS over HTTPS listening on %s%s", addr, dohPath)
}

// serveDoH answers one DNS-over-HTTPS request: a POST whose body is the DNS message, or a GET with the
// message as the base64url "dns" parameter (RFC 8484). It goes through the same path as a TCP query.
func (f *DNSFrontend) serveDoH(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != dohPath {
		http.NotFound(w, r)
		return
	}
	var q []byte
	switch r.Method {
	case http.MethodPost:
		if ct := strings.ToLower(r.Header.Get("Content-Type")); !strings.HasPrefix(ct, "application/dns-message") {
			http.Error(w, "Content-Type must be application/dns-message", http.StatusUnsupportedMediaType)
			return
		}
		b, err := io.ReadAll(io.LimitReader(r.Body, dohMaxAnswer+1))
		if err != nil || len(b) > dohMaxAnswer {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		q = b
	case http.MethodGet:
		b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(r.URL.Query().Get("dns"), "="))
		if err != nil || len(b) == 0 || len(b) > dohMaxAnswer {
			http.Error(w, "missing or bad dns parameter", http.StatusBadRequest)
			return
		}
		q = b
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
		return
	}
	if len(q) < 12 {
		http.Error(w, "not a DNS message", http.StatusBadRequest)
		return
	}
	if f.ctx == nil || f.ctx.Err() != nil {
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	resp := f.resolve(q, true, httpClientAddr(r))
	if resp == nil {
		http.Error(w, "no answer", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/dns-message")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(resp)
}

// httpClientAddr is the address a request came from (the zero Addr when it cannot be read).
func httpClientAddr(r *http.Request) netip.Addr {
	if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		return ap.Addr().Unmap()
	}
	return netip.Addr{}
}
