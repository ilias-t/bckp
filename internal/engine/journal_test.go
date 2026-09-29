package engine

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/ilias-t/bckp/internal/fakes"
	"github.com/ilias-t/bckp/internal/paths"
)

func TestHotspotOrder(t *testing.T) {
	tests := []struct {
		order   []string
		want    []string
		changed bool
		err     bool
	}{
		{order: []string{"Wi-Fi", "iPhone USB", "TB"}, want: []string{"iPhone USB", "Wi-Fi", "TB"}, changed: true},
		{order: []string{"Ethernet", "Wi-Fi", "TB", "iPhone USB"}, want: []string{"Ethernet", "iPhone USB", "Wi-Fi", "TB"}, changed: true},
		{order: []string{"iPhone USB", "Wi-Fi"}, want: []string{"iPhone USB", "Wi-Fi"}},
		{order: []string{"Wi-Fi", "TB"}, err: true},
	}
	for _, tc := range tests {
		got, changed, err := hotspotOrder(tc.order, "iPhone USB")
		if (err != nil) != tc.err || changed != tc.changed || (!tc.err && !slices.Equal(got, tc.want)) {
			t.Errorf("hotspotOrder(%v) = %v %v %v", tc.order, got, changed, err)
		}
	}
	// No Wi-Fi service: the hotspot goes first.
	if got, changed, _ := hotspotOrder([]string{"Ethernet", "TB", "iPhone USB"}, "iPhone USB"); !changed || got[0] != "iPhone USB" {
		t.Errorf("no wifi: %v", got)
	}
}

func TestMergeOrder(t *testing.T) {
	got := mergeOrder([]string{"Wi-Fi", "Gone", "iPhone USB", "TB"}, []string{"iPhone USB", "New", "Wi-Fi", "TB"})
	if want := []string{"Wi-Fi", "iPhone USB", "TB", "New"}; !slices.Equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestRestoreStaleJournal(t *testing.T) {
	ctx := context.Background()
	p := paths.ForHome(t.TempDir())
	n := fakes.NewNetwork(nil, "iPhone USB", "Wi-Fi", "TB")

	if restored, err := RestoreStaleJournal(ctx, n, p); restored || err != nil {
		t.Fatalf("no journal: %v %v", restored, err)
	}

	on := true
	j := Journal{Version: JournalVersion, Service: "iPhone USB", ServiceOrder: []string{"Wi-Fi", "iPhone USB", "TB"},
		WiFiPower: &on, Reordered: true, WiFiChanged: true, ServiceDisabled: true}
	_ = n.SetWiFiPower(ctx, false)
	_ = n.SetServiceEnabled(ctx, "iPhone USB", false)
	if err := writeJournal(p.RestoreJournal, j); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(p.RestoreJournal); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("journal mode: %v %v", fi, err)
	}

	// Every step is attempted even when one fails; the journal is kept.
	n.FailNext(fakes.MethodSetServiceOrder, errors.New("denied"))
	if restored, err := RestoreStaleJournal(ctx, n, p); restored || err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("failing restore: %v %v", restored, err)
	}
	if !n.WiFiOn() || !n.Enabled("iPhone USB") {
		t.Fatal("other steps must still run")
	}
	if _, err := os.Stat(p.RestoreJournal); err != nil {
		t.Fatal("journal must be kept after a failure")
	}

	if restored, err := RestoreStaleJournal(ctx, n, p); !restored || err != nil {
		t.Fatalf("retry: %v %v", restored, err)
	}
	if !slices.Equal(n.Order(), []string{"Wi-Fi", "iPhone USB", "TB"}) {
		t.Fatalf("order = %v", n.Order())
	}
	if _, err := os.Stat(p.RestoreJournal); !os.IsNotExist(err) {
		t.Fatal("journal must be removed")
	}

	// Unflagged changes are left alone.
	_ = n.SetServiceOrder(ctx, []string{"TB", "Wi-Fi", "iPhone USB"})
	if err := writeJournal(p.RestoreJournal, Journal{Version: JournalVersion, ServiceOrder: []string{"Wi-Fi", "iPhone USB", "TB"}}); err != nil {
		t.Fatal(err)
	}
	if restored, err := RestoreStaleJournal(ctx, n, p); !restored || err != nil || n.Order()[0] != "TB" {
		t.Fatalf("unflagged: %v %v %v", restored, err, n.Order())
	}

	// A corrupt journal is moved aside.
	if err := os.WriteFile(p.RestoreJournal, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreStaleJournal(ctx, n, p); err == nil || !strings.Contains(err.Error(), ".bad") {
		t.Fatalf("corrupt: %v", err)
	}
	if _, err := os.Stat(p.RestoreJournal + ".bad"); err != nil {
		t.Fatal("corrupt journal must be moved aside")
	}
	if restored, err := RestoreStaleJournal(ctx, n, p); restored || err != nil {
		t.Fatalf("after move-aside: %v %v", restored, err)
	}
}
