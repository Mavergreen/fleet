package ghapi

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
)

// counting is a Client that counts calls per method and path.
type counting struct {
	mu    sync.Mutex
	calls map[string]int
	err   map[string]error
}

func (c *counting) hit(k string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.calls == nil {
		c.calls = map[string]int{}
	}
	c.calls[k]++
	return c.err[k]
}
func (c *counting) n(k string) int { c.mu.Lock(); defer c.mu.Unlock(); return c.calls[k] }

func (c *counting) Get(_ context.Context, p string, out any) error {
	if err := c.hit("GET " + p); err != nil {
		return err
	}
	return json.Unmarshal([]byte(`{"path":"`+p+`"}`), out)
}
func (c *counting) GetPaged(_ context.Context, p string, each func(json.RawMessage) error) error {
	if err := c.hit("PAGED " + p); err != nil {
		return err
	}
	for _, s := range []string{`1`, `2`, `3`} {
		if err := each(json.RawMessage(s)); err != nil {
			return err
		}
	}
	return nil
}
func (c *counting) GetRaw(_ context.Context, p string) ([]byte, error) {
	return []byte("log"), c.hit("RAW " + p)
}
func (c *counting) Patch(_ context.Context, p string, _, _ any) error { return c.hit("PATCH " + p) }
func (c *counting) Post(_ context.Context, p string, _, _ any) error  { return c.hit("POST " + p) }
func (c *counting) Remaining() int                                    { return 77 }

func TestMemoReadsEachPathOnce(t *testing.T) {
	under := &counting{err: map[string]error{"GET /bad": errors.New("boom")}}
	m := NewMemo(under)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var v struct{ Path string }
			if err := m.Get(ctx, "/a", &v); err != nil || v.Path != "/a" {
				t.Errorf("%v %+v", err, v)
			}
			var got []string
			if err := m.GetPaged(ctx, "/list", func(r json.RawMessage) error { got = append(got, string(r)); return nil }); err != nil || len(got) != 3 {
				t.Errorf("%v %v", err, got)
			}
			if err := m.Get(ctx, "/bad", &v); err == nil {
				t.Error("cached error lost")
			}
		}()
	}
	wg.Wait()
	for _, k := range []string{"GET /a", "PAGED /list", "GET /bad"} {
		if n := under.n(k); n != 1 {
			t.Errorf("%s read %d times", k, n)
		}
	}
	if under.n("GET /b") != 0 {
		t.Error("phantom read")
	}
	var v struct{ Path string }
	m.Get(ctx, "/b", &v)
	if under.n("GET /b") != 1 || v.Path != "/b" {
		t.Error("different paths share a memo entry")
	}
}

// A callback that stops early must not truncate what later callers see.
func TestMemoPagedEarlyStop(t *testing.T) {
	m := NewMemo(&counting{})
	stop := errors.New("stop")
	if err := m.GetPaged(context.Background(), "/l", func(json.RawMessage) error { return stop }); err != stop {
		t.Fatalf("callback error not returned: %v", err)
	}
	n := 0
	m.GetPaged(context.Background(), "/l", func(json.RawMessage) error { n++; return nil })
	if n != 3 {
		t.Fatalf("replayed %d items", n)
	}
}

func TestMemoPassesWritesAndLogsThrough(t *testing.T) {
	under := &counting{}
	m := NewMemo(under)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		m.Post(ctx, "/w", nil, nil)
		m.Patch(ctx, "/w", nil, nil)
		m.GetRaw(ctx, "/log")
	}
	for _, k := range []string{"POST /w", "PATCH /w", "RAW /log"} {
		if n := under.n(k); n != 2 {
			t.Errorf("%s: %d calls, want 2", k, n)
		}
	}
	if Remaining(m) != 77 {
		t.Error("Remaining not forwarded")
	}
}
