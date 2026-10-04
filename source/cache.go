package main

import (
	"container/list"
	"encoding/binary"
	"hash/maphash"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// The response cache.  A pool answers a repeated query from memory instead of asking an upstream server
// again, for as long as the records' TTLs allow (RFC 1035 and RFC 2308 for negative answers).  It lives in the
// pool, so a changed pool (servers added, paused or removed) starts with an empty cache.
//
// What is cached: an answer to a plain query (opcode QUERY, one question, class IN or any) that came back
// NOERROR (with data or without: "no data") or NXDOMAIN, not truncated.  A negative answer needs the zone's SOA in
// the authority section, whose MINIMUM sets how long it may be kept.  Nothing else is cached: SERVFAIL, REFUSED, truncated
// answers, zone transfers, anything signed with TSIG, and queries carrying EDNS options other than a client cookie.
//
// The cache key is the lower-cased name, type and class, the RD, AD and CD flags, whether the query had EDNS and its DO
// bit, the UDP size the client allows (UDP answers are limited to it), the transport, and — when ECS is on — the client's
// network, because the upstream may answer each network differently.  An entry lives for the smallest TTL among its
// records (the SOA minimum for a negative answer), at most cache_max_ttl; the TTLs a client sees count down with the entry's
// age and are capped to cache_max_ttl.  The ID and the case of the name (0x20 randomisation) are taken from the client's query.
// EDNS options in the stored answer (cookie, padding, NSID) are dropped.

const (
	cacheDefaultEntries = 10000
	cacheDefaultMaxTTL  = 3600
	cacheMinEntries     = 100
	cacheMaxEntries     = 1000000
	cacheMaxTTLLimit    = 7 * 24 * 3600
	optCookie           = 10
	optNSID             = 3
	optPadding          = 12
	typeTSIG            = 250
	typeTKEY            = 249
)

type cacheEntry struct {
	key  string
	msg  []byte
	ttls []int // offsets of the TTL fields that count down
	at   time.Time
	life time.Duration
	elem *list.Element
	sh   *cacheShard
}

// cacheShard is one slice of the cache with its own lock and its own LRU order.  At 100k queries a second one
// lock around the whole cache was the largest lock wait on a production node.
type cacheShard struct {
	mu  sync.Mutex
	max int
	m   map[string]*cacheEntry
	lru *list.List // front = most recently used
}

// cacheShardMin: a cache smaller than this stays in one shard, so its LRU order is exact.
const (
	cacheShards   = 16
	cacheShardMin = 4096
)

type respCache struct {
	max    int
	maxTTL uint32
	shards []*cacheShard
	seed   maphash.Seed
	now    func() time.Time

	Hits     atomic.Uint64 // answered from the cache
	Misses   atomic.Uint64 // cacheable queries that had to go upstream
	Bypassed atomic.Uint64 // queries that are never cached
	Evicted  atomic.Uint64 // entries pushed out because the cache was full
}

func newRespCache(max, maxTTL int) *respCache {
	if max < 1 {
		max = cacheDefaultEntries
	}
	if maxTTL < 1 {
		maxTTL = cacheDefaultMaxTTL
	}
	n := 1
	if max >= cacheShardMin {
		n = cacheShards
	}
	c := &respCache{max: max, maxTTL: uint32(maxTTL), seed: maphash.MakeSeed(), now: time.Now}
	for i := 0; i < n; i++ {
		c.shards = append(c.shards, &cacheShard{max: (max + n - 1) / n, m: map[string]*cacheEntry{}, lru: list.New()})
	}
	return c
}

func (c *respCache) shardFor(key string) *cacheShard {
	if len(c.shards) == 1 {
		return c.shards[0]
	}
	return c.shards[maphash.String(c.seed, key)%uint64(len(c.shards))]
}

func (c *respCache) Len() int {
	n := 0
	for _, sh := range c.shards {
		sh.mu.Lock()
		n += len(sh.m)
		sh.mu.Unlock()
	}
	return n
}

// keyFor returns the cache key of a client query, or ok=false when the query is not one to cache.
// ecs is the pool's ECS setting: with it the client's network is part of the key.
func (c *respCache) keyFor(q []byte, tcp bool, client netip.Addr, ecs bool, p4, p6 int) (string, bool) {
	if len(q) < 12 || q[2]&0x80 != 0 || (q[2]>>3)&0x0F != 0 || binary.BigEndian.Uint16(q[4:]) != 1 ||
		binary.BigEndian.Uint16(q[6:]) != 0 || binary.BigEndian.Uint16(q[8:]) != 0 {
		return "", false
	}
	name, qt, ok := questionOf(q)
	if !ok || qt == 251 || qt == 252 { // IXFR, AXFR (ANY is cached like any other type: a benchmark that asks it, and Technitium, serve it from memory)
		return "", false
	}
	qEnd, ok := questionEnd(q)
	if !ok {
		return "", false
	}
	qclass := binary.BigEndian.Uint16(q[qEnd-2:])
	flags := int(q[2] & 0x01)    // RD
	flags |= int(q[3]&0x30) >> 3 // AD (bit 1), CD (bit 2)
	size := 0
	if !tcp {
		size = 512
	}
	rrs, _, ok := recordsAfterQuestion(q)
	if !ok || len(rrs) > 1 {
		return "", false
	}
	if len(rrs) == 1 {
		rr := rrs[0]
		if rr.typ != typeOPT {
			return "", false
		}
		if !optionsOnly(q[rr.rdataOff:rr.end], optCookie) {
			return "", false
		}
		flags |= 1 << 4
		if ttl := binary.BigEndian.Uint32(q[rr.rdataOff-6:]); ttl&0x8000 != 0 { // DO
			flags |= 1 << 3
		}
		if !tcp {
			if size = int(rr.class); size < 512 {
				size = 512
			}
		}
	}
	if tcp {
		flags |= 1 << 5
	}
	nw := ""
	if ecs && ecsUsable(client) {
		a := client.Unmap()
		bits := p6
		if a.Is4() {
			bits = p4
		}
		if pf, err := a.Prefix(bits); err == nil {
			nw = pf.String()
		}
	}
	return name + "|" + strconv.Itoa(int(qt)) + "|" + strconv.Itoa(int(qclass)) + "|" + strconv.Itoa(flags) + "|" + strconv.Itoa(size) + "|" + nw, true
}

// questionEnd is the offset just after the (single) question of a message.
func questionEnd(m []byte) (int, bool) {
	o, ok := skipName(m, 12)
	if !ok || o+4 > len(m) {
		return 0, false
	}
	return o + 4, true
}

// optionsOnly reports whether every EDNS option in rdata has one of the given codes.
func optionsOnly(rdata []byte, codes ...uint16) bool {
	for i := 0; i < len(rdata); {
		if i+4 > len(rdata) {
			return false
		}
		code, l := binary.BigEndian.Uint16(rdata[i:]), int(binary.BigEndian.Uint16(rdata[i+2:]))
		if i+4+l > len(rdata) {
			return false
		}
		found := false
		for _, c := range codes {
			if c == code {
				found = true
			}
		}
		if !found {
			return false
		}
		i += 4 + l
	}
	return true
}

// get returns the cached answer for the query, with its ID, question and TTLs made right, or nil.
func (c *respCache) get(key string, q []byte) []byte {
	sh := c.shardFor(key)
	sh.mu.Lock()
	e := sh.m[key]
	if e == nil {
		sh.mu.Unlock()
		return nil
	}
	age := c.now().Sub(e.at)
	if age >= e.life || age < 0 {
		sh.drop(e)
		sh.mu.Unlock()
		return nil
	}
	sh.lru.MoveToFront(e.elem)
	sh.mu.Unlock()
	// an entry's message is never changed after it is stored, so it is copied outside the lock
	out := append([]byte(nil), e.msg...)
	ttls := e.ttls

	qEnd, ok := questionEnd(q)
	if !ok || qEnd != e.qEnd() {
		return nil
	}
	copy(out[0:2], q[0:2])
	copy(out[12:qEnd], q[12:qEnd]) // the client's own spelling of the name
	secs := uint32(age / time.Second)
	for _, off := range ttls {
		v := binary.BigEndian.Uint32(out[off:])
		if v > secs {
			v -= secs
		} else {
			v = 0
		}
		binary.BigEndian.PutUint32(out[off:], v)
	}
	return out
}

func (e *cacheEntry) qEnd() int {
	o, _ := questionEnd(e.msg)
	return o
}

func (sh *cacheShard) drop(e *cacheEntry) {
	sh.lru.Remove(e.elem)
	delete(sh.m, e.key)
}

// put stores a response to a query whose key is key, when it is one worth keeping.
func (c *respCache) put(key string, resp []byte) {
	msg, ttls, life, ok := c.prepare(resp)
	if !ok {
		return
	}
	sh := c.shardFor(key)
	e := &cacheEntry{key: key, msg: msg, ttls: ttls, at: c.now(), life: life, sh: sh}
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if old := sh.m[key]; old != nil {
		sh.drop(old)
	}
	e.elem = sh.lru.PushFront(e)
	sh.m[key] = e
	for len(sh.m) > sh.max {
		sh.drop(sh.lru.Back().Value.(*cacheEntry))
		c.Evicted.Add(1)
	}
}

// prepare checks a response and makes the copy to keep: TTLs capped, options dropped.  ok is false for
// one that must not be cached.  ttls are the TTL field offsets, life how long the copy may be served.
func (c *respCache) prepare(resp []byte) (msg []byte, ttls []int, life time.Duration, ok bool) {
	h, hok := parseHeader(resp)
	if !hok || !h.qr || h.tc || h.qdcount != 1 || (resp[2]>>3)&0x0F != 0 || (h.rcode != rcodeNoError && h.rcode != rcodeNXDomain) {
		return nil, nil, 0, false
	}
	rrs, _, rok := recordsAfterQuestion(resp)
	if !rok {
		return nil, nil, 0, false
	}
	an := int(binary.BigEndian.Uint16(resp[6:]))
	ns := int(binary.BigEndian.Uint16(resp[8:]))
	msg = append([]byte(nil), resp...)
	// an OPT record may carry a cookie, padding or NSID, none of which belongs to another client: drop them (anything else: no caching)
	for i, rr := range rrs {
		if rr.typ != typeOPT {
			continue
		}
		if i != len(rrs)-1 || rr.end != len(msg) || !optionsOnly(msg[rr.rdataOff:rr.end], optCookie, optNSID, optPadding) {
			return nil, nil, 0, false
		}
		binary.BigEndian.PutUint16(msg[rr.rdataOff-2:], 0)
		msg = msg[:rr.rdataOff]
		rrs[i].end = rr.rdataOff
		rrs[i].rdlen = 0
	}
	min := uint32(c.maxTTL)
	negative := h.rcode == rcodeNXDomain || an == 0
	soaMin := uint32(0xFFFFFFFF)
	for i, rr := range rrs {
		if rr.typ == typeOPT {
			continue
		}
		if rr.typ == typeTSIG || rr.typ == typeTKEY {
			return nil, nil, 0, false
		}
		off := rr.rdataOff - 6
		ttl := binary.BigEndian.Uint32(msg[off:])
		if ttl > c.maxTTL {
			ttl = c.maxTTL
		}
		if rr.typ == typeSOA && i >= an && i < an+ns { // the SOA of a negative answer
			if _, n, err := readName(msg, rr.rdataOff); err == nil {
				if _, n, err = readName(msg, n); err == nil && n+20 <= rr.end {
					if m := binary.BigEndian.Uint32(msg[n+16:]); m < soaMin {
						soaMin = m
					}
					if m := binary.BigEndian.Uint32(msg[n+16:]); negative && m < ttl {
						ttl = m
					}
				}
			}
		}
		binary.BigEndian.PutUint32(msg[off:], ttl)
		if ttl < min {
			min = ttl
		}
		ttls = append(ttls, off)
	}
	if negative {
		if soaMin == 0xFFFFFFFF { // no SOA: no way to know for how long
			return nil, nil, 0, false
		}
	} else if len(ttls) == 0 {
		return nil, nil, 0, false
	}
	if min == 0 {
		return nil, nil, 0, false
	}
	return msg, ttls, time.Duration(min) * time.Second, true
}

// cacheStats is what the DNS views show.
type cacheStats struct {
	On       bool   `json:"on"`
	Entries  int    `json:"entries"`
	Max      int    `json:"max"`
	MaxTTL   int    `json:"max_ttl"`
	Hits     uint64 `json:"hits"`
	Misses   uint64 `json:"misses"`
	Bypassed uint64 `json:"bypassed"`
	Evicted  uint64 `json:"evicted"`
}

func (c *respCache) stats() cacheStats {
	if c == nil {
		return cacheStats{}
	}
	return cacheStats{On: true, Entries: c.Len(), Max: c.max, MaxTTL: int(c.maxTTL), Hits: c.Hits.Load(), Misses: c.Misses.Load(), Bypassed: c.Bypassed.Load(), Evicted: c.Evicted.Load()}
}
