package network

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Prober checks end-to-end internet reachability. detail is a short
// human-readable summary for diagnostics.
type Prober func(ctx context.Context) (ok bool, detail string)

// ProbeTarget is one URL to try. Want, when set, must appear in the body
// (so a captive portal's 200 page does not count as online).
type ProbeTarget struct {
	URL  string
	Want string
}

// Probe defaults.
const (
	// ProbeTimeout bounds each probe request.
	ProbeTimeout = 4 * time.Second
	// CaptiveURL is Apple's captive-portal check, which answers "Success".
	CaptiveURL = "http://captive.apple.com/hotspot-detect.html"
	// DefaultNtfyServer is used when Options.NtfyServer is empty.
	DefaultNtfyServer = "https://ntfy.sh"
)

// DefaultTargets are ntfy's health endpoint first, then
// captive.apple.com, so an ntfy outage alone is not reported as offline.
func DefaultTargets(ntfyServer string) []ProbeTarget {
	if ntfyServer == "" {
		ntfyServer = DefaultNtfyServer
	}
	return []ProbeTarget{
		{URL: strings.TrimRight(ntfyServer, "/") + "/v1/health"},
		{URL: CaptiveURL, Want: "Success"},
	}
}

// HTTPProbe returns a Prober that GETs each target in order and succeeds on
// the first 2xx answer (containing Want, if set). A nil client gets a
// fresh transport without keep-alives, so every probe opens a new
// connection instead of trusting a pooled one.
func HTTPProbe(client *http.Client, targets ...ProbeTarget) Prober {
	if client == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.DisableKeepAlives = true
		client = &http.Client{Transport: tr, Timeout: ProbeTimeout}
	}
	return func(ctx context.Context) (bool, string) {
		var fails []string
		for _, t := range targets {
			err := probeOne(ctx, client, t)
			if err == nil {
				fails = append(fails, hostOf(t.URL)+" ok")
				return true, strings.Join(fails, "; ")
			}
			fails = append(fails, fmt.Sprintf("%s: %v", hostOf(t.URL), err))
			if ctx.Err() != nil {
				break
			}
		}
		return false, strings.Join(fails, "; ")
	}
}

func probeOne(ctx context.Context, client *http.Client, t ProbeTarget) error {
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.URL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return shortErr(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if t.Want != "" && !strings.Contains(string(body), t.Want) {
		return fmt.Errorf("HTTP %d without %q (captive portal?)", resp.StatusCode, t.Want)
	}
	return nil
}

// shortErr drops the "Get <url>:" prefix of *url.Error.
func shortErr(err error) error {
	if ue, ok := err.(*url.Error); ok {
		return ue.Err
	}
	return err
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}
