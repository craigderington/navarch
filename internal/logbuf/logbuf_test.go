package logbuf

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestWriteAndReadByCursor(t *testing.T) {
	b := New()
	id := uuid.New()
	b.Write(id, "first\n")
	b.Write(id, "second\n")

	chunks, next, dropped := b.Read(id, 0)
	if len(chunks) != 2 || chunks[0].Data != "first\n" || chunks[1].Data != "second\n" {
		t.Fatalf("read from 0 got %+v", chunks)
	}
	if dropped {
		t.Fatal("nothing should have been dropped")
	}

	// A cursor is how a follow avoids reprinting: reading again from the
	// returned cursor must yield nothing until more arrives.
	again, next2, _ := b.Read(id, next)
	if len(again) != 0 {
		t.Fatalf("re-reading at the cursor must return nothing, got %+v", again)
	}
	b.Write(id, "third\n")
	got, _, _ := b.Read(id, next2)
	if len(got) != 1 || got[0].Data != "third\n" {
		t.Fatalf("expected only the new chunk, got %+v", got)
	}
}

// Reading does not consume. A requester whose connection drops mid-page has to
// be able to ask again, or a tail would lose output to its own transport.
func TestReadIsNotDestructive(t *testing.T) {
	b := New()
	id := uuid.New()
	b.Write(id, "line\n")
	first, _, _ := b.Read(id, 0)
	second, _, _ := b.Read(id, 0)
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("both reads should see the chunk: %d then %d", len(first), len(second))
	}
}

// The cap must drop the OLDEST output. Refusing new writes instead would freeze
// a tail at the moment a container got chatty, which reads as a container that
// stopped — the single most misleading thing this could do.
func TestOverflowDropsOldestAndSaysSo(t *testing.T) {
	b := New()
	b.maxBytes = 32
	id := uuid.New()
	b.Write(id, strings.Repeat("a", 20))
	b.Write(id, strings.Repeat("b", 20))
	b.Write(id, strings.Repeat("c", 20))

	chunks, _, dropped := b.Read(id, 0)
	if !dropped {
		t.Fatal("dropping content must be reported, not silent")
	}
	joined := ""
	for _, c := range chunks {
		joined += c.Data
	}
	if strings.Contains(joined, "a") {
		t.Fatalf("oldest content should have gone first, got %q", joined)
	}
	if !strings.Contains(joined, "c") {
		t.Fatalf("newest content must survive, got %q", joined)
	}
}

// Memory is bounded across requests too, or a caller opening tails in a loop
// exhausts the control plane.
func TestRequestSlotsAreCapped(t *testing.T) {
	b := New()
	b.maxRequests = 2
	if !b.Write(uuid.New(), "a") || !b.Write(uuid.New(), "b") {
		t.Fatal("writes within the cap must be accepted")
	}
	if b.Write(uuid.New(), "c") {
		t.Fatal("a write beyond the request cap must be refused, not silently accepted")
	}
	if b.Len() != 2 {
		t.Fatalf("expected 2 buffered requests, got %d", b.Len())
	}
}

// A requester that walks away leaves no row transition behind, so idleness is
// the only signal its buffer — which may hold secrets — is dead weight.
func TestExpireFreesIdleBuffers(t *testing.T) {
	b := New()
	now := time.Now()
	b.nowFn = func() time.Time { return now }
	idle, fresh := uuid.New(), uuid.New()
	b.Write(idle, "old\n")

	now = now.Add(time.Hour)
	b.Write(fresh, "new\n")

	if n := b.Expire(30 * time.Minute); n != 1 {
		t.Fatalf("expected 1 idle buffer freed, got %d", n)
	}
	if chunks, _, _ := b.Read(idle, 0); len(chunks) != 0 {
		t.Fatal("the idle buffer's content must be gone")
	}
	if chunks, _, _ := b.Read(fresh, 0); len(chunks) != 1 {
		t.Fatal("the recently used buffer must survive")
	}
}

func TestDropFreesImmediately(t *testing.T) {
	b := New()
	id := uuid.New()
	b.Write(id, "secret-ish\n")
	b.Drop(id)
	if chunks, _, _ := b.Read(id, 0); len(chunks) != 0 {
		t.Fatal("Drop must free the content, not just forget the cursor")
	}
}

// The agent writes on its poll while the requester reads on theirs; -race would
// catch a missing lock here and nowhere else.
func TestConcurrentWriteAndRead(t *testing.T) {
	b := New()
	id := uuid.New()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			b.Write(id, "x")
		}
	}()
	go func() {
		defer wg.Done()
		var cursor int64
		for i := 0; i < 200; i++ {
			_, cursor, _ = b.Read(id, cursor)
		}
	}()
	wg.Wait()
}
