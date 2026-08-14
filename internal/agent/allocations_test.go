package agent

import (
	"testing"

	"github.com/craig/composectl/internal/spec"
	"github.com/craig/composectl/internal/store"
)

func TestDesiredAllocationsDeduplicatesPinnedContainers(t *testing.T) {
	pinned := spec.Service{Swappable: false, Limits: spec.ResourceLimit{CPUMillis: 500, MemoryBytes: 1 << 30}}
	web := spec.Service{Swappable: true, Limits: spec.ResourceLimit{CPUMillis: 250, MemoryBytes: 256 << 20}}
	desired := []store.DesiredInstance{
		{Env8: "env12345", ProjectName: "r1", ServiceName: "db", Swappable: false, Service: pinned},
		{Env8: "env12345", ProjectName: "r2", ServiceName: "db", Swappable: false, Service: pinned},
		{Env8: "env12345", ProjectName: "r1", ServiceName: "web", Swappable: true, Service: web},
		{Env8: "env12345", ProjectName: "r2", ServiceName: "web", Swappable: true, Service: web},
	}

	cpu, memory := desiredAllocations(desired)
	if cpu != 1000 {
		t.Fatalf("cpu = %d, want 1000", cpu)
	}
	if memory != (1<<30)+(2*(256<<20)) {
		t.Fatalf("memory = %d, want %d", memory, (1<<30)+(2*(256<<20)))
	}
}
