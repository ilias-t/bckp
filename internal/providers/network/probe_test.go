package network

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPProbe(t *testing.T) {
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		switch r.URL.Path {
		case "/ntfy-down/v1/health":
			http.Error(w, "down", http.StatusBadGateway)
		case "/ntfy/v1/health":
			_, _ = w.Write([]byte(`{"healthy":true}`))
		case "/captive":
			_, _ = w.Write([]byte("<HTML><HEAD><TITLE>Success</TITLE></HEAD><BODY>Success</BODY></HTML>"))
		case "/portal":
			_, _ = w.Write([]byte("<html>Please log in</html>"))
		}
	}))
	defer srv.Close()
	ctx := context.Background()

	ok, detail := HTTPProbe(srv.Client(), ProbeTarget{URL: srv.URL + "/ntfy/v1/health"})(ctx)
	if !ok || !strings.Contains(detail, "ok") {
		t.Errorf("ntfy up: %v %q", ok, detail)
	}

	// ntfy down but the internet works: still online.
	hits = nil
	ok, detail = HTTPProbe(srv.Client(),
		ProbeTarget{URL: srv.URL + "/ntfy-down/v1/health"},
		ProbeTarget{URL: srv.URL + "/captive", Want: "Success"})(ctx)
	if !ok || !strings.Contains(detail, "HTTP 502") || len(hits) != 2 {
		t.Errorf("fallback: %v %q hits=%v", ok, detail, hits)
	}

	// A captive portal's 200 page is not online.
	ok, detail = HTTPProbe(srv.Client(), ProbeTarget{URL: srv.URL + "/portal", Want: "Success"})(ctx)
	if ok || !strings.Contains(detail, "captive portal") {
		t.Errorf("portal: %v %q", ok, detail)
	}
}

func TestHTTPProbeUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // connection refused from now on
	ok, detail := HTTPProbe(nil, ProbeTarget{URL: url + "/v1/health"})(context.Background())
	if ok || detail == "" || strings.Contains(detail, "Get ") {
		t.Errorf("unreachable: %v %q", ok, detail)
	}
}

func TestDefaultTargets(t *testing.T) {
	got := DefaultTargets("")
	if len(got) != 2 || got[0].URL != "https://ntfy.sh/v1/health" || got[1].URL != CaptiveURL || got[1].Want != "Success" {
		t.Errorf("default targets = %+v", got)
	}
	if got := DefaultTargets("https://ntfy.example.com/"); got[0].URL != "https://ntfy.example.com/v1/health" {
		t.Errorf("custom server = %+v", got[0])
	}
}
