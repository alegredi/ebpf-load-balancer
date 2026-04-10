package balancer

import (
	"errors"
	"math"
	"net/http/httputil"
	"sync"
	"sync/atomic"
)

// Backend represents a registered upstream server.
type Backend struct {
	Index                uint32
	URL                  string
	HealthEndpoint       string
	Weight               uint32
	ActiveConnections    int32
	alive                int32 // 0 = false, 1 = true
	consecutiveFails     int32
	consecutiveSuccesses int32
	Proxy                *httputil.ReverseProxy
}

func (b *Backend) IncConn() {
	atomic.AddInt32(&b.ActiveConnections, 1)
}

func (b *Backend) DecConn() {
	for {
		current := atomic.LoadInt32(&b.ActiveConnections)
		if current <= 0 {
			return
		}
		if atomic.CompareAndSwapInt32(&b.ActiveConnections, current, current-1) {
			return
		}
	}
}

func (b *Backend) GetActiveConnections() int32 {
	return atomic.LoadInt32(&b.ActiveConnections)
}

func (b *Backend) SetAlive(alive bool) {
	if alive {
		atomic.StoreInt32(&b.alive, 1)
	} else {
		atomic.StoreInt32(&b.alive, 0)
	}
}

func (b *Backend) IsAlive() bool {
	return atomic.LoadInt32(&b.alive) == 1
}

// MarkSuccess records a successful health probe and applies hysteresis.
// Returns true if the backend transitioned from down to up.
func (b *Backend) MarkSuccess(threshold int32) bool {
	atomic.StoreInt32(&b.consecutiveFails, 0)
	succ := atomic.AddInt32(&b.consecutiveSuccesses, 1)
	if succ >= threshold && !b.IsAlive() {
		b.SetAlive(true)
		return true
	}
	return false
}

// MarkFailure records a failed health probe and applies hysteresis.
// Returns true if the backend transitioned from up to down.
func (b *Backend) MarkFailure(threshold int32) bool {
	atomic.StoreInt32(&b.consecutiveSuccesses, 0)
	fails := atomic.AddInt32(&b.consecutiveFails, 1)
	if fails >= threshold && b.IsAlive() {
		b.SetAlive(false)
		return true
	}
	return false
}

// LoadBalancer coordinates routing across backends.
type LoadBalancer struct {
	Backends []*Backend
	mu       sync.RWMutex
}

// GetLeastConnBackend selects the alive backend with the fewest active connections.
func (l *LoadBalancer) GetLeastConnBackend() (*Backend, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	if len(l.Backends) == 0 {
		return nil, errors.New("no backends available")
	}

	var chosen *Backend
	least := int32(math.MaxInt32)

	for _, b := range l.Backends {
		if !b.IsAlive() {
			continue
		}
		conn := b.GetActiveConnections()
		if chosen == nil || conn < least {
			chosen = b
			least = conn
		}
	}

	if chosen == nil {
		return nil, errors.New("no alive backends available")
	}

	return chosen, nil
}

func (l *LoadBalancer) ReleaseBackend(b *Backend) {
	b.DecConn()
}
