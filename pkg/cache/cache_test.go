package cache

import (
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestLRUCache_BasicPutGet(t *testing.T) {
	c := NewLRUCache(2)

	resp1 := &CachedResponse{
		StatusCode: http.StatusOK,
		Body:       []byte("response 1"),
		ExpiresAt:  time.Now().Add(1 * time.Minute),
	}
	if err := c.Put("key1", resp1); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, ok := c.Get("key1")
	if !ok {
		t.Fatalf("expected key1 to be present")
	}
	if string(got.Body) != "response 1" {
		t.Fatalf("got body %q, expected 'response 1'", string(got.Body))
	}
}

func TestLRUCache_Eviction(t *testing.T) {
	c := NewLRUCache(2)

	_ = c.Put("k1", &CachedResponse{StatusCode: 200, Body: []byte("v1"), ExpiresAt: time.Now().Add(time.Minute)})
	_ = c.Put("k2", &CachedResponse{StatusCode: 200, Body: []byte("v2"), ExpiresAt: time.Now().Add(time.Minute)})

	// Access k1 to make k2 the least recently used
	_, _ = c.Get("k1")

	// Put k3, which should evict k2
	_ = c.Put("k3", &CachedResponse{StatusCode: 200, Body: []byte("v3"), ExpiresAt: time.Now().Add(time.Minute)})

	if _, ok := c.Get("k2"); ok {
		t.Errorf("expected k2 to be evicted")
	}
	if _, ok := c.Get("k1"); !ok {
		t.Errorf("expected k1 to be present")
	}
	if _, ok := c.Get("k3"); !ok {
		t.Errorf("expected k3 to be present")
	}
}

func TestLRUCache_Expiration(t *testing.T) {
	c := NewLRUCache(2)

	_ = c.Put("k1", &CachedResponse{
		StatusCode: 200,
		Body:       []byte("v1"),
		ExpiresAt:  time.Now().Add(20 * time.Millisecond),
	})

	time.Sleep(30 * time.Millisecond)

	if _, ok := c.Get("k1"); ok {
		t.Errorf("expected expired entry to return false")
	}
}

func TestLRUCache_Concurrency(t *testing.T) {
	c := NewLRUCache(10)
	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			key := "key"
			_ = c.Put(key, &CachedResponse{
				StatusCode: 200,
				Body:       []byte("data"),
				ExpiresAt:  time.Now().Add(time.Minute),
			})
			_, _ = c.Get(key)
		}(i)
	}

	wg.Wait()
}
