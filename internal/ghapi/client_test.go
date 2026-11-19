package ghapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type sleeps struct {
	mu sync.Mutex
	d  []time.Duration
}

func (s *sleeps) sleep(d time.Duration) { s.mu.Lock(); s.d = append(s.d, d); s.mu.Unlock() }

func newTest(t *testing.T, h http.Handler, s *sleeps) Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New("sekrit-token", Options{BaseURL: srv.URL, Sleep: s.sleep})
}

func TestGetPagedFollowsLinkHeader(t *testing.T) {
	var base string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sekrit-token" ||
			r.Header.Get("Accept") != "application/vnd.github+json" ||
			r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
			t.Errorf("bad headers: %v", r.Header)
		}
		page := r.URL.Query().Get("page")
		if page == "" {
			if r.URL.Query().Get("per_page") != "100" {
				t.Errorf("per_page not added: %s", r.URL.RawQuery)
			}
			page = "1"
		}
		switch page {
		case "1":
			w.Header().Set("Link", fmt.Sprintf(`<%s/x?page=2&per_page=100>; rel="next", <%s/x?page=3>; rel="last"`, base, base))
			fmt.Fprint(w, `[1,2]`)
		case "2":
			w.Header().Set("Link", fmt.Sprintf(`<%s/x?page=3&per_page=100>; rel="next"`, base))
			fmt.Fprint(w, `[3]`)
		default:
			fmt.Fprint(w, `[4,5]`)
		}
	}))
	defer srv.Close()
	base = srv.URL
	c := New("sekrit-token", Options{BaseURL: srv.URL})
	var got []string
	err := c.GetPaged(context.Background(), "/x", func(m json.RawMessage) error {
		got = append(got, string(m))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "1,2,3,4,5" {
		t.Fatalf("got %v", got)
	}
}

func TestGetPagedUnwrapsItemsKey(t *testing.T) {
	for _, body := range []string{
		`{"total_count":2,"workflow_runs":[{"id":1},{"id":2}]}`,
		`{"total_count":2,"jobs":[{"id":1},{"id":2}]}`,
		`{"total_count":2,"repositories":[{"id":1},{"id":2}]}`,
		`[{"id":1},{"id":2}]`,
	} {
		c := newTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }), &sleeps{})
		n := 0
		if err := c.GetPaged(context.Background(), "/x", func(m json.RawMessage) error {
			n++
			if !strings.HasPrefix(string(m), `{"id":`) {
				t.Errorf("%s: item %s", body, m)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if n != 2 {
			t.Errorf("%s: n=%d", body, n)
		}
	}
}

func TestClientBacksOffOnSecondaryRateLimit(t *testing.T) {
	var n int32
	s := &sleeps{}
	c := newTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(403)
			fmt.Fprint(w, `{"message":"You have exceeded a secondary rate limit"}`)
			return
		}
		fmt.Fprint(w, `{"ok":true}`)
	}), s)
	var out struct{ OK bool }
	if err := c.Get(context.Background(), "/x", &out); err != nil || !out.OK {
		t.Fatalf("err=%v out=%v", err, out)
	}
	if len(s.d) != 1 || s.d[0] != 2*time.Second {
		t.Fatalf("sleeps %v", s.d)
	}
}

func TestClientBacksOffOn429(t *testing.T) {
	var n int32
	s := &sleeps{}
	c := newTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) <= 2 {
			w.WriteHeader(429)
			return
		}
		fmt.Fprint(w, `{}`)
	}), s)
	if err := c.Get(context.Background(), "/x", nil); err != nil {
		t.Fatal(err)
	}
	if len(s.d) != 2 || s.d[0] != time.Second || s.d[1] != 2*time.Second {
		t.Fatalf("sleeps %v", s.d)
	}
}

func TestClientGivesUpAfter5Attempts(t *testing.T) {
	var n int32
	s := &sleeps{}
	c := newTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(403)
		fmt.Fprint(w, `{"message":"secondary rate limit"}`)
	}), s)
	err := c.Get(context.Background(), "/x", nil)
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 403 {
		t.Fatalf("err=%v", err)
	}
	if n != 5 {
		t.Fatalf("attempts=%d", n)
	}
}

func TestPlain403NotRetried(t *testing.T) {
	var n int32
	s := &sleeps{}
	c := newTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.Header().Set("X-RateLimit-Remaining", "4000")
		w.WriteHeader(403)
		fmt.Fprint(w, `{"message":"Resource not accessible by personal access token"}`)
	}), s)
	err := c.Get(context.Background(), "/x", nil)
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 403 {
		t.Fatalf("err=%v", err)
	}
	if n != 1 || len(s.d) != 0 {
		t.Fatalf("attempts=%d sleeps=%v", n, s.d)
	}
}

func TestGetRawFollowsRedirect(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/actions/jobs/1/logs", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/storage/log.txt", http.StatusFound)
	})
	mux.HandleFunc("/storage/log.txt", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "line1\nline2\n") })
	c := newTest(t, mux, &sleeps{})
	b, err := c.GetRaw(context.Background(), "/repos/o/r/actions/jobs/1/logs")
	if err != nil || string(b) != "line1\nline2\n" {
		t.Fatalf("b=%q err=%v", b, err)
	}
}

func TestConcurrencyIsBounded(t *testing.T) {
	var cur, max int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := atomic.AddInt32(&cur, 1)
		for {
			m := atomic.LoadInt32(&max)
			if v <= m || atomic.CompareAndSwapInt32(&max, m, v) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt32(&cur, -1)
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()
	c := New("t", Options{BaseURL: srv.URL, MaxConcurrent: 3})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.Get(context.Background(), "/x", nil); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if max > 3 || max < 1 {
		t.Fatalf("max in flight %d", max)
	}
}

func TestTokenNeverInError(t *testing.T) {
	// server echoes the token in its error body; the error must not carry it
	c := newTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		fmt.Fprint(w, r.Header.Get("Authorization"))
	}), &sleeps{})
	err := c.Get(context.Background(), "/x", nil)
	if err == nil || strings.Contains(err.Error(), "sekrit-token") {
		t.Fatalf("err=%v", err)
	}
	// connection failure
	c2 := New("sekrit-token", Options{BaseURL: "http://127.0.0.1:1"})
	err = c2.Get(context.Background(), "/x", nil)
	if err == nil || strings.Contains(err.Error(), "sekrit-token") {
		t.Fatalf("err=%v", err)
	}
}

func TestPatchSendsBody(t *testing.T) {
	c := newTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		if r.Method != "PATCH" || in["a"] != "b" {
			t.Errorf("%s %v", r.Method, in)
		}
		fmt.Fprint(w, `{"ok":true}`)
	}), &sleeps{})
	var out struct{ OK bool }
	if err := c.Patch(context.Background(), "/x", map[string]string{"a": "b"}, &out); err != nil || !out.OK {
		t.Fatal(err, out)
	}
}

func TestRetries5xxThenSucceeds(t *testing.T) {
	var n int32
	s := &sleeps{}
	c := newTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			w.WriteHeader(502)
			return
		}
		fmt.Fprint(w, `{"ok":true}`)
	}), s)
	var out struct{ OK bool }
	if err := c.Get(context.Background(), "/x", &out); err != nil || !out.OK {
		t.Fatalf("err=%v out=%v", err, out)
	}
	if len(s.d) != 1 || s.d[0] != time.Second {
		t.Fatalf("sleeps %v", s.d)
	}
}

func TestGivesUp5xxAfter5Attempts(t *testing.T) {
	var n int32
	c := newTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.WriteHeader(503)
	}), &sleeps{})
	err := c.Get(context.Background(), "/x", nil)
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 503 || n != 5 {
		t.Fatalf("err=%v attempts=%d", err, n)
	}
}

func TestResetHeaderIgnoredWhenRemainingNonZero(t *testing.T) {
	var n int32
	s := &sleeps{}
	c := newTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			w.Header().Set("X-RateLimit-Remaining", "4000")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
			w.WriteHeader(502)
			return
		}
		fmt.Fprint(w, `{}`)
	}), s)
	if err := c.Get(context.Background(), "/x", nil); err != nil {
		t.Fatal(err)
	}
	if len(s.d) != 1 || s.d[0] != time.Second {
		t.Fatalf("sleeps %v", s.d)
	}
}

func TestLinkSchemeDowngradeRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		w.Header().Set("Link", fmt.Sprintf(`<https://%s/x?page=2>; rel="next"`, host))
		fmt.Fprint(w, `[1]`)
	}))
	defer srv.Close()
	c := New("t", Options{BaseURL: srv.URL})
	err := c.GetPaged(context.Background(), "/x", func(json.RawMessage) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("err=%v", err)
	}
}

func TestCrossHostRedirectDropsAuthorization(t *testing.T) {
	var gotAuth atomic.Value
	gotAuth.Store("unset")
	dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		fmt.Fprint(w, "logdata")
	}))
	defer dst.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(dst.URL, "http://"))
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			t.Error("first hop lacks Authorization")
		}
		http.Redirect(w, r, "http://localhost:"+port+"/log", http.StatusFound)
	}))
	defer src.Close()
	c := New("sekrit-token", Options{BaseURL: src.URL})
	b, err := c.GetRaw(context.Background(), "/logs")
	if err != nil || string(b) != "logdata" {
		t.Fatalf("b=%q err=%v", b, err)
	}
	if a := gotAuth.Load().(string); a != "" {
		t.Fatalf("redirect target saw Authorization %q", a)
	}
}

func TestContextCancelledDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.WriteHeader(503)
	}))
	defer srv.Close()
	c := New("t", Options{BaseURL: srv.URL, Sleep: func(time.Duration) { cancel() }})
	err := c.Get(ctx, "/x", nil)
	if !errors.Is(err, context.Canceled) || n != 1 {
		t.Fatalf("err=%v attempts=%d", err, n)
	}
}

func TestDefaultBackoffReturnsPromptlyOnCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(429)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	c := New("t", Options{BaseURL: srv.URL})
	start := time.Now()
	err := c.Get(ctx, "/x", nil)
	if !errors.Is(err, context.Canceled) || time.Since(start) > time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
}

func TestRedirectDowngradeRefused(t *testing.T) {
	var hit int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hit, 1)
	}))
	defer plain.Close()
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/log", http.StatusFound)
	}))
	defer tls.Close()
	c := New("tok-xyz", Options{BaseURL: tls.URL, HTTP: tls.Client()})
	_, err := c.GetRaw(context.Background(), "/logs")
	if err == nil || !strings.Contains(err.Error(), "https to http") || hit != 0 {
		t.Fatalf("err=%v hit=%d", err, hit)
	}
}

// A 5xx on a write may still have taken effect (a 502 after the comment was
// created), so writes are never retried on 5xx.
func TestPostNotRetriedOn5xx(t *testing.T) {
	for _, status := range []int{500, 502, 503, 504} {
		var n int32
		c := newTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&n, 1)
			w.WriteHeader(status)
		}), &sleeps{})
		err := c.Post(context.Background(), "/x", map[string]string{"body": "b"}, nil)
		var he *HTTPError
		if !errors.As(err, &he) || he.Status != status || n != 1 {
			t.Errorf("POST %d: err=%v attempts=%d", status, err, n)
		}
		n = 0
		err = c.Patch(context.Background(), "/x", map[string]string{"body": "b"}, nil)
		if !errors.As(err, &he) || he.Status != status || n != 1 {
			t.Errorf("PATCH %d: err=%v attempts=%d", status, err, n)
		}
	}
}

// 429 and secondary-rate-limit 403 are refused before any effect, so a write
// may be retried.
func TestPostRetriedOn429(t *testing.T) {
	for _, refuse := range []func(w http.ResponseWriter){
		func(w http.ResponseWriter) { w.WriteHeader(429) },
		func(w http.ResponseWriter) {
			w.WriteHeader(403)
			fmt.Fprint(w, `{"message":"You have exceeded a secondary rate limit"}`)
		},
	} {
		var n int32
		s := &sleeps{}
		c := newTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if atomic.AddInt32(&n, 1) == 1 {
				refuse(w)
				return
			}
			fmt.Fprint(w, `{}`)
		}), s)
		if err := c.Post(context.Background(), "/x", map[string]string{"body": "b"}, nil); err != nil || n != 2 || len(s.d) != 1 {
			t.Fatalf("err=%v attempts=%d sleeps=%v", err, n, s.d)
		}
	}
}
