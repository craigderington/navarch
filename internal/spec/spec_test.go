package spec

import "testing"

func TestPeakResourcesCountSwappableTwiceAndPinnedOnce(t *testing.T) {
	s := &DeploymentSpec{Services: map[string]Service{
		"web": {Swappable: true, Limits: ResourceLimit{CPUMillis: 250, MemoryBytes: 256 << 20}},
		"db":  {Swappable: false, Limits: ResourceLimit{CPUMillis: 500, MemoryBytes: 1 << 30}},
	}}
	if got := s.PeakCPUMillis(); got != 1000 {
		t.Fatalf("PeakCPUMillis = %d, want 1000", got)
	}
	if got := s.PeakMemoryBytes(); got != (1<<30)+(2*(256<<20)) {
		t.Fatalf("PeakMemoryBytes = %d", got)
	}
}
