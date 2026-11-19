package ghapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// A long primary-rate-limit wait says so, once per reset time, however many
// requests are waiting for it.
func TestLongBackoffNoticeOncePerReset(t *testing.T) {
	reset := time.Now().Add(10 * time.Minute).Unix()
	var hits sync.Map
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, again := hits.LoadOrStore(r.URL.Path, true); !again {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("X-RateLimit-Remaining", "4321")
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	var notice lockedBuf
	var s sleeps
	c := New("tok", Options{BaseURL: srv.URL, Sleep: s.sleep, Notice: &notice})
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.Get(context.Background(), "/p"+strconv.Itoa(i), nil); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got := notice.String()
	hhmm := time.Unix(reset, 0).Format("15:04")
	re := regexp.MustCompile(`^rate limited; waiting (\d+)m until ` + hhmm + ` \(shared GitHub quota\)\n$`)
	m := re.FindStringSubmatch(got)
	if m == nil {
		t.Fatalf("notice %q", got)
	}
	if n, _ := strconv.Atoi(m[1]); n < 9 || n > 11 {
		t.Fatalf("minutes %s", m[1])
	}
	if r := Remaining(c); r != 4321 {
		t.Fatalf("remaining %d", r)
	}
}

// Short waits are not worth a line.
func TestShortBackoffIsQuiet(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "10")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	var notice lockedBuf
	var s sleeps
	c := New("tok", Options{BaseURL: srv.URL, Sleep: s.sleep, Notice: &notice})
	if err := c.Get(context.Background(), "/x", nil); err != nil {
		t.Fatal(err)
	}
	if notice.String() != "" {
		t.Fatalf("notice %q", notice.String())
	}
}

func TestRemainingUnknownIsMinusOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) }))
	defer srv.Close()
	c := New("tok", Options{BaseURL: srv.URL})
	if r := Remaining(c); r != -1 {
		t.Fatalf("before any request: %d", r)
	}
	c.Get(context.Background(), "/x", nil)
	if r := Remaining(c); r != -1 {
		t.Fatalf("no header: %d", r)
	}
}
