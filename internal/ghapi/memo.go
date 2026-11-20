package ghapi

import (
	"context"
	"encoding/json"
	"sync"
)

// Remaining is the X-RateLimit-Remaining c last saw, or -1 if unknown (or c
// does not track it).
func Remaining(c Client) int {
	if r, ok := c.(interface{ Remaining() int }); ok {
		return r.Remaining()
	}
	return -1
}

// Memo is a Client that reads each GET and GetPaged path at most once and
// replays the answer, error included, to every later caller. It is for one
// command invocation, where two collectors would otherwise read the same
// endpoints. GetRaw (logs) and writes pass straight through. Concurrent
// callers of one path share one request.
type Memo struct {
	c  Client
	mu sync.Mutex
	m  map[string]*memoEntry
}

type memoEntry struct {
	once  sync.Once
	body  json.RawMessage   // Get
	items []json.RawMessage // GetPaged
	err   error
}

// NewMemo wraps c. Anything whose correctness rests on reading fresh state
// (remediate's Apply) must use c, not the Memo.
func NewMemo(c Client) *Memo { return &Memo{c: c, m: map[string]*memoEntry{}} }

func (m *Memo) entry(key string) *memoEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.m[key]
	if e == nil {
		e = &memoEntry{}
		m.m[key] = e
	}
	return e
}

func (m *Memo) Get(ctx context.Context, path string, out any) error {
	e := m.entry("GET " + path)
	e.once.Do(func() { e.err = m.c.Get(ctx, path, &e.body) })
	if e.err != nil {
		return e.err
	}
	if out == nil || len(e.body) == 0 {
		return nil
	}
	return json.Unmarshal(e.body, out)
}

func (m *Memo) GetPaged(ctx context.Context, path string, each func(json.RawMessage) error) error {
	e := m.entry("PAGED " + path)
	e.once.Do(func() {
		// Collect every item regardless of what the first caller's each
		// does, so an early stop cannot truncate the memo.
		e.err = m.c.GetPaged(ctx, path, func(r json.RawMessage) error {
			e.items = append(e.items, append(json.RawMessage{}, r...))
			return nil
		})
	})
	if e.err != nil {
		return e.err
	}
	for _, it := range e.items {
		if err := each(it); err != nil {
			return err
		}
	}
	return nil
}

func (m *Memo) GetRaw(ctx context.Context, path string) ([]byte, error) {
	return m.c.GetRaw(ctx, path)
}
func (m *Memo) Patch(ctx context.Context, path string, body, out any) error {
	return m.c.Patch(ctx, path, body, out)
}
func (m *Memo) Post(ctx context.Context, path string, body, out any) error {
	return m.c.Post(ctx, path, body, out)
}

// Remaining forwards to the wrapped client.
func (m *Memo) Remaining() int { return Remaining(m.c) }
