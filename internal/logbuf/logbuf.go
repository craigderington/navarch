// Package logbuf holds delivered container output in memory, briefly.
//
// It exists so that log content never reaches Postgres. Container stdout
// routinely carries secrets — an application logging its own DATABASE_URL, a
// debug dump of the environment, a stack trace with a token — and the platform
// otherwise goes to real trouble to keep secret plaintext out of the control
// plane and its database. Persisting stdout would put that plaintext at rest, in
// every backup, readable by anyone with database access, in exchange for a
// durability nobody asked for: an operator running `navarch logs` wants to read
// output now, not to consult it next quarter.
//
// Content still passes through control-plane memory, which is unavoidable while
// the agent has no inbound server and cannot be read from directly. That
// exposure is deliberately bounded: capped per request and in total, dropped as
// soon as the requester has read it or the request expires, and never written to
// a log line — see the guidance on Write.
package logbuf

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	// DefaultMaxBytesPerRequest caps one request's unread backlog. A container
	// writing faster than its reader drains would otherwise grow this without
	// limit; the oldest bytes are dropped instead, because for a tail the newest
	// output is the output somebody is waiting for.
	DefaultMaxBytesPerRequest = 1 << 20 // 1 MiB
	// DefaultMaxRequests caps how many requests may hold a buffer at once, so a
	// caller opening tails in a loop cannot exhaust the control plane's memory.
	DefaultMaxRequests = 64
)

// Chunk is one delivery from an agent. Seq is assigned by the buffer and is
// what a reader's cursor refers to; the agent does not number its own chunks,
// because two agents delivering for one request would then collide.
type Chunk struct {
	Seq  int64  `json:"seq"`
	Data string `json:"data"`
}

type entry struct {
	chunks   []Chunk
	bytes    int
	nextSeq  int64
	dropped  bool // oldest content was discarded to stay under the cap
	lastSeen time.Time
}

// Buffer is a bounded, in-memory store of delivered chunks keyed by request.
// Safe for concurrent use: the agent writes on its poll while the requester
// reads on theirs.
type Buffer struct {
	mu          sync.Mutex
	entries     map[uuid.UUID]*entry
	maxBytes    int
	maxRequests int
	nowFn       func() time.Time
}

func New() *Buffer {
	return &Buffer{
		entries:     map[uuid.UUID]*entry{},
		maxBytes:    DefaultMaxBytesPerRequest,
		maxRequests: DefaultMaxRequests,
		nowFn:       time.Now,
	}
}

// Write records a chunk delivered by an agent.
//
// Never log the data it carries. Every caller of this package is handling
// output that may contain a tenant's secrets, and a debug line printing a chunk
// would write to the control plane's own log the exact plaintext this package
// exists to keep out of its database.
//
// Returns false when the request is unknown to the buffer and no slot could be
// taken for it, which is how a caller learns that a delivery arrived for a
// request nobody is reading any more.
func (b *Buffer) Write(id uuid.UUID, data string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.entries[id]
	if !ok {
		if len(b.entries) >= b.maxRequests {
			return false
		}
		e = &entry{}
		b.entries[id] = e
	}
	e.lastSeen = b.nowFn()
	if data == "" {
		return true
	}
	e.chunks = append(e.chunks, Chunk{Seq: e.nextSeq, Data: data})
	e.nextSeq++
	e.bytes += len(data)
	// Drop from the front rather than refusing the write: a tail that stops
	// updating looks like a silent container, which is the single most
	// misleading thing this could do.
	for e.bytes > b.maxBytes && len(e.chunks) > 1 {
		e.bytes -= len(e.chunks[0].Data)
		e.chunks = e.chunks[1:]
		e.dropped = true
	}
	return true
}

// Read returns chunks with Seq >= cursor, the next cursor to use, and whether
// any content was dropped to stay under the cap. Chunks are not consumed by
// reading — a reader that crashes mid-page can ask again — they are freed by
// Drop or Expire.
func (b *Buffer) Read(id uuid.UUID, cursor int64) (chunks []Chunk, next int64, dropped bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.entries[id]
	if !ok {
		return nil, cursor, false
	}
	e.lastSeen = b.nowFn()
	for _, c := range e.chunks {
		if c.Seq >= cursor {
			chunks = append(chunks, c)
		}
	}
	return chunks, e.nextSeq, e.dropped
}

// Drop frees a request's buffer. Called when its row reaches a terminal state
// or is swept, so memory tracks the request table rather than outliving it.
func (b *Buffer) Drop(id uuid.UUID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.entries, id)
}

// Expire frees buffers untouched for longer than maxAge and reports how many
// went. A requester that walks away mid-tail leaves no row transition behind,
// so age is the only signal that its buffer is dead weight.
func (b *Buffer) Expire(maxAge time.Duration) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	cutoff := b.nowFn().Add(-maxAge)
	n := 0
	for id, e := range b.entries {
		if e.lastSeen.Before(cutoff) {
			delete(b.entries, id)
			n++
		}
	}
	return n
}

// Len reports how many requests currently hold a buffer.
func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.entries)
}
