package balancer

import (
	"errors"
	"math"
	"net/http/httputil"
	"sync"
	"sync/atomic"
)

type Backend struct {
	URL               string
	HealthEndpoint    string
	ActiveConnections int32
	alive             int32 // 0 = false, 1 = true
	Proxy             *httputil.ReverseProxy
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

type LoadBalancer struct {
	Backends []*Backend
	mu       sync.RWMutex
}

func (l *LoadBalancer) GetLeastConnBackend() (*Backend, error) {
	l.mu.RLock()         // allow concurrent reads
	defer l.mu.RUnlock() // unlock after function

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
