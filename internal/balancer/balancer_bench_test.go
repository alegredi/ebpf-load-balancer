package balancer

import (
	"fmt"
	"testing"
)

func BenchmarkGetLeastConnBackend(b *testing.B) {
	numBackends := 32
	backends := make([]*Backend, numBackends)
	for i := 0; i < numBackends; i++ {
		backends[i] = &Backend{
			Index:             uint32(i),
			URL:               fmt.Sprintf("http://10.0.0.%d:8080", i+1),
			ActiveConnections: int32(i % 5),
		}
		backends[i].SetAlive(true)
	}

	lb := &LoadBalancer{Backends: backends}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			be, err := lb.GetLeastConnBackend()
			if err != nil || be == nil {
				b.Fatalf("failed to pick backend: %v", err)
			}
		}
	})
}
