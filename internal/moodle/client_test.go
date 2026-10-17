package moodle

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(url string, concurrency, retries int) *Client {
	c := New(Options{BaseURL: url, MaxConcurrency: concurrency, MaxRetries: retries, AttemptTimeout: 2 * time.Second})
	c.backoffBase = time.Millisecond
	return c
}

func TestCallRetriesIdempotent(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, `[]`)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, 2, 3)
	if err := c.Call(context.Background(), "tok", "f", nil, true, nil); err != nil {
		t.Fatalf("esperaba éxito tras reintentos: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Fatalf("intentos = %d, quería 3", got)
	}
}

func TestCallDoesNotRetryNonIdempotent(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, 2, 3)
	err := c.Call(context.Background(), "tok", "f", nil, false, nil)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("esperaba ErrUnavailable, obtuve %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("intentos = %d, quería 1 (no idempotente)", got)
	}
}

func TestInvalidTokenMapsToSentinel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"exception":"moodle_exception","errorcode":"invalidtoken","message":"Invalid token"}`)
	}))
	defer srv.Close()

	err := newTestClient(srv.URL, 1, 0).Call(context.Background(), "bad", "f", nil, true, nil)
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("esperaba ErrInvalidToken, obtuve %v", err)
	}
}

func TestTokenGoesInBodyNotURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			t.Errorf("la URL no debería llevar parámetros: %q", r.URL.RawQuery)
		}
		_ = r.ParseForm()
		if r.PostForm.Get("wstoken") != "secret" || r.PostForm.Get("wsfunction") != "f" {
			t.Errorf("faltan wstoken/wsfunction en el cuerpo: %v", r.PostForm)
		}
		io.WriteString(w, `[]`)
	}))
	defer srv.Close()

	if err := newTestClient(srv.URL, 1, 0).Call(context.Background(), "secret", "f", nil, true, nil); err != nil {
		t.Fatal(err)
	}
}

func TestMaxConcurrencyIsRespected(t *testing.T) {
	var inflight, peak int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&inflight, 1)
		for {
			p := atomic.LoadInt32(&peak)
			if n <= p || atomic.CompareAndSwapInt32(&peak, p, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt32(&inflight, -1)
		io.WriteString(w, `[]`)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL, 5, 0)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.Call(context.Background(), "tok", "f", nil, true, nil); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	if p := atomic.LoadInt32(&peak); p > 5 {
		t.Fatalf("pico de concurrencia = %d, el máximo es 5", p)
	}
}

func TestOverloadedWhenContextExpiresWaitingForSlot(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		io.WriteString(w, `[]`)
	}))
	defer srv.Close()
	defer close(release)

	c := newTestClient(srv.URL, 1, 0)
	go c.Call(context.Background(), "tok", "f", nil, true, nil) // ocupa el único hueco
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := c.Call(ctx, "tok", "f", nil, true, nil)
	if !errors.Is(err, ErrOverloaded) {
		t.Fatalf("esperaba ErrOverloaded, obtuve %v", err)
	}
}
