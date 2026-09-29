package ntfy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ilias-t/bckp/internal/contracts"
)

func TestSendJSON(t *testing.T) {
	var got map[string]any
	var ctype, path, method string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, ctype = r.Method, r.URL.Path, r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &got); err != nil {
			t.Errorf("body %q: %v", b, err)
		}
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	defer srv.Close()

	n := New(srv.URL+"/", "secret-topic", srv.Client())
	err := n.Send(context.Background(), contracts.Notification{
		Priority: 4, Title: "Mac is HOT (serious)", Body: "CPU 92°C GPU 80°C — open the bag", Tags: []string{"fire", "warning"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || path != "/" || ctype != "application/json" {
		t.Errorf("request %s %s %q", method, path, ctype)
	}
	want := map[string]any{
		"topic": "secret-topic", "title": "Mac is HOT (serious)", "message": "CPU 92°C GPU 80°C — open the bag",
		"priority": float64(4), "tags": []any{"fire", "warning"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %v\nwant %v", got, want)
	}
}

func TestSendOmitsDefaults(t *testing.T) {
	var raw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		raw = string(b)
	}))
	defer srv.Close()
	if err := New(srv.URL, "t", nil).Send(context.Background(), contracts.Notification{Body: "hi"}); err != nil {
		t.Fatal(err)
	}
	if raw != `{"topic":"t","message":"hi"}` {
		t.Errorf("body = %s", raw)
	}
}

func TestSendNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"code":42901,"http":429,"error":"limit reached"}`))
	}))
	defer srv.Close()
	err := New(srv.URL, "secret-topic", nil).Send(context.Background(), contracts.Notification{Title: "x"})
	if err == nil || !strings.Contains(err.Error(), "HTTP 429") || !strings.Contains(err.Error(), "limit reached") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "secret-topic") {
		t.Errorf("error leaks the topic: %v", err)
	}
}

func TestSendInvalid(t *testing.T) {
	n := New("http://127.0.0.1:1", "t", nil)
	if err := n.Send(context.Background(), contracts.Notification{Priority: 9, Title: "x"}); err == nil {
		t.Error("priority 9 accepted")
	}
	if err := New("http://127.0.0.1:1", "", nil).Send(context.Background(), contracts.Notification{Title: "x"}); err == nil {
		t.Error("empty topic accepted")
	}
}

func TestSendHonorsContext(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := New(srv.URL, "t", nil).Send(ctx, contracts.Notification{Title: "x"}); err == nil {
		t.Fatal("want timeout error")
	}
	if time.Since(start) > 5*time.Second {
		t.Error("Send ignored the context deadline")
	}
}
