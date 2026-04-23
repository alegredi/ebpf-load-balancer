package cache

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

func BenchmarkLRUCache_GetPut(b *testing.B) {
	c := NewLRUCache(1024)

	// Pre-populate cache
	for i := 0; i < 512; i++ {
		key := fmt.Sprintf("GET:/items/%d", i)
		_ = c.Put(key, &CachedResponse{
			StatusCode: http.StatusOK,
			Body:       []byte("benchmark cached response payload"),
			ExpiresAt:  time.Now().Add(10 * time.Minute),
		})
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			key := fmt.Sprintf("GET:/items/%d", i%1024)
			if _, ok := c.Get(key); !ok {
				_ = c.Put(key, &CachedResponse{
					StatusCode: http.StatusOK,
					Body:       []byte("payload"),
					ExpiresAt:  time.Now().Add(time.Minute),
				})
			}
			i++
		}
	})
}
