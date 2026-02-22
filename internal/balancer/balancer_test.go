package balancer

import (
	"sync"
	"testing"
)

func TestLeastConnBackend(t *testing.T) {
	b1 := &Backend{URL: "http://b1"}
	b1.SetAlive(true)
	b1.IncConn()
	b1.IncConn()

	b2 := &Backend{URL: "http://b2"}
	b2.SetAlive(true)
	b2.IncConn()

	lb := &LoadBalancer{
		Backends: []*Backend{b1, b2},
	}

	chosen, err := lb.GetLeastConnBackend()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chosen != b2 {
		t.Errorf("expected b2 (1 conn) to be chosen over b1 (2 conns), got %v", chosen.URL)
	}

	// Mark b2 as dead
	b2.SetAlive(false)
	chosen, err = lb.GetLeastConnBackend()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chosen != b1 {
		t.Errorf("expected b1 to be chosen when b2 is dead, got %v", chosen.URL)
	}
}

func TestDecConnNeverBelowZero(t *testing.T) {
	b := &Backend{URL: "http://b"}
	b.IncConn()
	b.DecConn()
	b.DecConn() // Extra decrement

	if b.GetActiveConnections() < 0 {
		t.Errorf("active connections went below 0: %d", b.GetActiveConnections())
	}
}

func TestConcurrentIncDec(t *testing.T) {
	b := &Backend{URL: "http://b"}
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.IncConn()
			b.DecConn()
		}()
	}

	wg.Wait()
	if b.GetActiveConnections() != 0 {
		t.Errorf("expected 0 active connections after balanced inc/dec, got %d", b.GetActiveConnections())
	}
}
