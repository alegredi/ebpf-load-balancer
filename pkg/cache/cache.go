package cache

import (
	"container/list"
	"errors"
	"net/http"
	"sync"
	"time"
)

var ErrBadStatusCode = errors.New("response not cacheable due to status code")

/* CachedResponse stores the serialized response body, headers, and TTL. */
type CachedResponse struct {
	StatusCode int
	Header     http.Header
	Body       []byte
	ExpiresAt  time.Time
}

type entry struct {
	key   string
	value *CachedResponse
}

/* LRUCache implements a concurrency-safe Least Recently Used cache with TTL.
 * It combines a doubly linked list for O(1) eviction ordering with a hash table
 * for O(1) key lookups. */
type LRUCache struct {
	capacity int
	items    map[string]*list.Element
	order    *list.List
	mu       sync.Mutex
}

func CacheKey(r *http.Request) string {
	return r.Method + ":" + r.URL.String()
}

func NewLRUCache(capacity int) *LRUCache {
	return &LRUCache{
		capacity: capacity,
		items:    make(map[string]*list.Element),
		order:    list.New(),
	}
}

func (c *LRUCache) Get(key string) (*CachedResponse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.items[key]
	if !ok {
		return nil, false
	}

	resp := el.Value.(*entry).value

	/* Lazy eviction: check expiration on access. */
	if time.Now().After(resp.ExpiresAt) {
		c.order.Remove(el)
		delete(c.items, key)
		return nil, false
	}

	c.order.MoveToFront(el)
	return resp, true
}

func (c *LRUCache) Put(key string, value *CachedResponse) error {
	if value.StatusCode < 200 || value.StatusCode > 299 {
		return ErrBadStatusCode
	}

	/* Clone body bytes to decouple cache storage from network socket buffers. */
	bodyCopy := make([]byte, len(value.Body))
	copy(bodyCopy, value.Body)
	value.Body = bodyCopy

	if value.Header == nil {
		value.Header = make(http.Header)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	/* If key exists, update value and bump to the front of LRU order. */
	if el, ok := c.items[key]; ok {
		el.Value.(*entry).value = value
		c.order.MoveToFront(el)
		return nil
	}

	/* Evict least-recently used entry if we reached capacity. */
	if c.order.Len() >= c.capacity {
		oldest := c.order.Back()
		if oldest != nil {
			ent := oldest.Value.(*entry)
			delete(c.items, ent.key)
			c.order.Remove(oldest)
		}
	}

	el := c.order.PushFront(&entry{key: key, value: value})
	c.items[key] = el
	return nil
}

func (c *LRUCache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.items[key]; ok {
		c.order.Remove(el)
		delete(c.items, key)
	}
}
