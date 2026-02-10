package cache

import (
	"container/list"
	"errors"
	"net/http"
	"sync"
	"time"
)

var ErrBadStatusCode = errors.New("response not cacheable due to status code")

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

	if el, ok := c.items[key]; ok {
		resp := el.Value.(*entry).value
		// delete cached response if expired
		if time.Now().After(resp.ExpiresAt) {
			c.order.Remove(el)
			delete(c.items, key)
			return nil, false
		}
		c.order.MoveToFront(el)
		return resp, true
	}
	return nil, false
}

func (c *LRUCache) Put(key string, value *CachedResponse) error {
	if value.StatusCode < 200 || value.StatusCode > 299 {
		return ErrBadStatusCode
	}

	// copia sicura del body
	bodyCopy := append([]byte(nil), value.Body...)
	value.Body = bodyCopy

	// check for nil header
	if value.Header == nil {
		value.Header = make(http.Header)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// update existing
	if el, ok := c.items[key]; ok {
		el.Value.(*entry).value = value
		c.order.MoveToFront(el)
		return nil
	}

	// capacity check
	if c.order.Len() >= c.capacity {
		oldest := c.order.Back()
		if oldest != nil {
			ent := oldest.Value.(*entry)
			delete(c.items, ent.key)
			c.order.Remove(oldest)
		}
	}

	// add
	el := c.order.PushFront(&entry{key, value})
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
