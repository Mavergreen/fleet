// Package ghapi is a small GitHub REST client: bearer auth, Link-header
// paging, bounded concurrency, and backoff on rate limits.
package ghapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const maxAttempts = 5

// Client is the only way collectors talk to GitHub.
type Client interface {
	Get(ctx context.Context, path string, out any) error
	GetPaged(ctx context.Context, path string, each func(json.RawMessage) error) error
	GetRaw(ctx context.Context, path string) ([]byte, error)
	Patch(ctx context.Context, path string, body, out any) error
	Post(ctx context.Context, path string, body, out any) error
}

// Options configures New; zero values give sensible defaults.
type Options struct {
	BaseURL       string // default https://api.github.com
	MaxConcurrent int    // default 8
	Sleep         func(time.Duration)
	HTTP          *http.Client
}

// HTTPError is a non-2xx response. Body is truncated and token-scrubbed.
type HTTPError struct {
	Status int
	Path   string
	Body   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("github: HTTP %d for %s: %s", e.Status, e.Path, e.Body)
}

type client struct {
	token string
	base  string
	sem   chan struct{}
	sleep func(time.Duration)
	hc    *http.Client
}

// New returns a Client authenticating with token.
func New(token string, o Options) Client {
	if o.BaseURL == "" {
		o.BaseURL = "https://api.github.com"
	}
	if o.MaxConcurrent <= 0 {
		o.MaxConcurrent = 8
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 60 * time.Second}
	}
	if o.HTTP.CheckRedirect == nil {
		o.HTTP.CheckRedirect = noDowngrade
	}
	return &client{token: token, base: strings.TrimRight(o.BaseURL, "/"),
		sem: make(chan struct{}, o.MaxConcurrent), sleep: o.Sleep, hc: o.HTTP}
}

// noDowngrade keeps net/http's 10-redirect limit and refuses https -> http.
func noDowngrade(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if len(via) > 0 && via[len(via)-1].URL.Scheme == "https" && req.URL.Scheme != "https" {
		return errors.New("refusing redirect from https to http")
	}
	return nil
}

// wait backs off for d; with no injected Sleep it returns as soon as ctx is done.
func (c *client) wait(ctx context.Context, d time.Duration) error {
	if c.sleep != nil {
		c.sleep(d)
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *client) scrub(s string) string {
	if c.token != "" {
		s = strings.ReplaceAll(s, c.token, "***")
	}
	return s
}

func (c *client) scrubErr(err error) error {
	if err == nil || c.token == "" || !strings.Contains(err.Error(), c.token) {
		return err
	}
	return errors.New(c.scrub(err.Error()))
}

// do performs one logical request with retries, returning the body and headers.
func (c *client) do(ctx context.Context, method, fullURL, display string, body []byte) ([]byte, http.Header, error) {
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		b, h, err, wait := c.once(ctx, method, fullURL, display, body, attempt)
		if err == nil {
			return b, h, nil
		}
		lastErr = err
		if wait < 0 || attempt == maxAttempts-1 {
			break
		}
		if err := c.wait(ctx, wait); err != nil {
			return nil, nil, err
		}
	}
	return nil, nil, lastErr
}

// once makes one attempt; wait < 0 means the error is not retryable.
func (c *client) once(ctx context.Context, method, fullURL, display string, body []byte, attempt int) ([]byte, http.Header, error, time.Duration) {
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err(), -1
	}
	defer func() { <-c.sem }()

	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, rdr)
	if err != nil {
		return nil, nil, c.scrubErr(err), -1
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("github: %s %s: %s", method, display, c.scrub(errString(err))), -1
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("github: reading %s: %s", display, c.scrub(err.Error())), -1
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return data, resp.Header, nil, 0
	}
	text := string(data)
	if len(text) > 500 {
		text = text[:500]
	}
	herr := &HTTPError{Status: resp.StatusCode, Path: display, Body: c.scrub(text)}
	if !retryable(resp, text) {
		return nil, nil, herr, -1
	}
	return nil, nil, herr, backoff(resp.Header, attempt)
}

func errString(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}

func retryable(resp *http.Response, body string) bool {
	if resp.StatusCode == http.StatusTooManyRequests {
		return true
	}
	if resp.StatusCode >= 500 && resp.StatusCode <= 504 {
		return true
	}
	if resp.StatusCode != http.StatusForbidden {
		return false
	}
	h := resp.Header
	return h.Get("Retry-After") != "" ||
		strings.Contains(strings.ToLower(body), "secondary rate limit") ||
		h.Get("X-RateLimit-Remaining") == "0"
}

func backoff(h http.Header, attempt int) time.Duration {
	if v := h.Get("Retry-After"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return time.Duration(n) * time.Second
		}
	}
	if v := h.Get("X-RateLimit-Reset"); v != "" && h.Get("X-RateLimit-Remaining") == "0" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			if d := time.Until(time.Unix(n, 0)); d > 0 {
				return d + time.Second
			}
			return time.Second
		}
	}
	return time.Second << uint(attempt)
}

func (c *client) url(path string) string { return c.base + path }

func (c *client) Get(ctx context.Context, path string, out any) error {
	b, _, err := c.do(ctx, http.MethodGet, c.url(path), path, nil)
	if err != nil {
		return err
	}
	return decode(b, out, path)
}

func decode(b []byte, out any, path string) error {
	if out == nil || len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("github: decoding %s: %w", path, err)
	}
	return nil
}

func (c *client) GetRaw(ctx context.Context, path string) ([]byte, error) {
	b, _, err := c.do(ctx, http.MethodGet, c.url(path), path, nil)
	return b, err
}

func (c *client) send(ctx context.Context, method, path string, body, out any) error {
	var enc []byte
	if body != nil {
		var err error
		if enc, err = json.Marshal(body); err != nil {
			return err
		}
	}
	b, _, err := c.do(ctx, method, c.url(path), path, enc)
	if err != nil {
		return err
	}
	return decode(b, out, path)
}

func (c *client) Patch(ctx context.Context, path string, body, out any) error {
	return c.send(ctx, http.MethodPatch, path, body, out)
}

func (c *client) Post(ctx context.Context, path string, body, out any) error {
	return c.send(ctx, http.MethodPost, path, body, out)
}

var nextRe = regexp.MustCompile(`<([^>]*)>\s*;\s*rel="next"`)

func (c *client) GetPaged(ctx context.Context, path string, each func(json.RawMessage) error) error {
	u, err := url.Parse(c.url(path))
	if err != nil {
		return err
	}
	q := u.Query()
	if q.Get("per_page") == "" {
		q.Set("per_page", "100")
		u.RawQuery = q.Encode()
	}
	baseU, err := url.Parse(c.base)
	if err != nil {
		return fmt.Errorf("github: bad base URL: %s", c.scrub(errString(err)))
	}
	next := u.String()
	for next != "" {
		b, h, err := c.do(ctx, http.MethodGet, next, path, nil)
		if err != nil {
			return err
		}
		items, err := items(b)
		if err != nil {
			return fmt.Errorf("github: decoding %s: %w", path, err)
		}
		for _, it := range items {
			if err := each(it); err != nil {
				return err
			}
		}
		next = ""
		if m := nextRe.FindStringSubmatch(h.Get("Link")); m != nil {
			nu, err := url.Parse(m[1])
			if err != nil || nu.Host != baseU.Host || nu.Scheme != baseU.Scheme {
				// never send the token to a host we were not pointed at
				return fmt.Errorf("github: refusing to follow paging link for %s to another host or scheme", path)
			}
			next = nu.String()
		}
	}
	return nil
}

// items returns the elements of a bare array, or of the single array-valued
// field of an envelope object such as {"total_count":N,"jobs":[...]}.
func items(b []byte) ([]json.RawMessage, error) {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '[' {
		var arr []json.RawMessage
		return arr, json.Unmarshal(b, &arr)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil {
		return nil, err
	}
	for _, k := range []string{"workflow_runs", "jobs", "repositories"} {
		if v, ok := obj[k]; ok {
			var arr []json.RawMessage
			return arr, json.Unmarshal(v, &arr)
		}
	}
	for _, v := range obj {
		if t := bytes.TrimSpace(v); len(t) > 0 && t[0] == '[' {
			var arr []json.RawMessage
			return arr, json.Unmarshal(v, &arr)
		}
	}
	return nil, errors.New("no item array in response")
}
