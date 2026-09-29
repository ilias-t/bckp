package outbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ilias-t/bckp/internal/contracts"
	"github.com/ilias-t/bckp/internal/fakes"
)

var (
	start   = time.Date(2026, 9, 28, 20, 14, 0, 0, time.UTC)
	errDown = errors.New("ntfy: publish: dial tcp: no route to host")
)

func msg(i int) contracts.Notification {
	return contracts.Notification{Priority: 3, Title: fmt.Sprintf("m%d", i), Body: fmt.Sprintf("body %d", i), Tags: []string{"t"}}
}

func setup(t *testing.T) (*Outbox, *fakes.Notifier, *fakes.Clock, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "outbox")
	inner := fakes.NewNotifier()
	clock := fakes.NewClock(start)
	return New(dir, inner, clock), inner, clock, dir
}

func titles(ns []contracts.Notification) []string {
	var out []string
	for _, n := range ns {
		out = append(out, n.Title)
	}
	return out
}

func TestSendSuccessPassesThrough(t *testing.T) {
	o, inner, _, dir := setup(t)
	if err := o.Send(context.Background(), msg(1)); err != nil {
		t.Fatal(err)
	}
	if got := titles(inner.Sent()); strings.Join(got, ",") != "m1" {
		t.Errorf("sent %v", got)
	}
	if o.Pending() != 0 {
		t.Error("pending after success")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("dir created without need: %v", err)
	}
}

func TestSendFailureQueues(t *testing.T) {
	o, inner, _, dir := setup(t)
	inner.SetErr(errDown)
	err := o.Send(context.Background(), msg(1))
	if !errors.Is(err, contracts.ErrQueued) || !errors.Is(err, errDown) {
		t.Fatalf("err = %v, want contracts.ErrQueued wrapping the inner error", err)
	}
	if o.Pending() != 1 {
		t.Fatalf("pending = %d", o.Pending())
	}
	fi, err := os.Stat(dir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v, %v", fi.Mode(), err)
	}
	names, _ := o.list()
	fi, err = os.Stat(filepath.Join(dir, names[0]))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v, %v", fi.Mode(), err)
	}
	// Survives a restart.
	o2 := New(dir, inner, fakes.NewClock(start))
	if o2.Pending() != 1 {
		t.Errorf("new instance sees %d pending", o2.Pending())
	}
}

func TestFlushInOrderWithNote(t *testing.T) {
	o, inner, clock, _ := setup(t)
	inner.SetErr(errDown)
	for i := 1; i <= 3; i++ {
		_ = o.Send(context.Background(), msg(i)) // same fake time: seq orders them
	}
	clock.Advance(time.Hour)
	inner.SetErr(nil)
	inner.Reset()
	if err := o.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	sent := inner.Sent()
	if got := strings.Join(titles(sent), ","); got != "m1,m2,m3" {
		t.Fatalf("order = %s", got)
	}
	if sent[0].Body != "body 1\n\n(queued 20:14)" || sent[0].Priority != 3 || sent[0].Tags[0] != "t" {
		t.Errorf("delivered = %+v", sent[0])
	}
	if o.Pending() != 0 {
		t.Errorf("pending = %d", o.Pending())
	}
}

func TestFlushStopsAtFirstFailure(t *testing.T) {
	o, inner, _, _ := setup(t)
	inner.SetErr(errDown)
	for i := 1; i <= 3; i++ {
		_ = o.Send(context.Background(), msg(i))
	}
	inner.SetErr(nil)
	inner.Reset()
	flaky := &flakyNotifier{inner: inner, failOn: 2} // m1 ok, m2 fails
	o.inner = flaky
	err := o.Flush(context.Background())
	if !errors.Is(err, errDown) || !strings.Contains(err.Error(), "2 pending") {
		t.Fatalf("err = %v", err)
	}
	if got := strings.Join(titles(inner.Sent()), ","); got != "m1" {
		t.Errorf("sent = %s", got)
	}
	if o.Pending() != 2 {
		t.Errorf("pending = %d", o.Pending())
	}
	// The rest go out next time, in order.
	flaky.failOn = 0
	if err := o.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(titles(inner.Sent()), ","); got != "m1,m2,m3" {
		t.Errorf("sent = %s", got)
	}
}

// flakyNotifier fails its failOn-th call (1-based) and delegates the rest.
type flakyNotifier struct {
	inner  contracts.Notifier
	failOn int
	calls  int
}

func (f *flakyNotifier) Send(ctx context.Context, n contracts.Notification) error {
	f.calls++
	if f.calls == f.failOn {
		return errDown
	}
	return f.inner.Send(ctx, n)
}

func TestOpportunisticFlushOnSuccess(t *testing.T) {
	o, inner, _, _ := setup(t)
	inner.SetErr(errDown)
	_ = o.Send(context.Background(), msg(1))
	_ = o.Send(context.Background(), msg(2))
	inner.SetErr(nil)
	if err := o.Send(context.Background(), msg(3)); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(titles(inner.Sent()), ","); got != "m3,m1,m2" {
		t.Errorf("sent = %s", got)
	}
	if o.Pending() != 0 {
		t.Errorf("pending = %d", o.Pending())
	}
}

func TestCapDropsOldest(t *testing.T) {
	o, inner, clock, _ := setup(t)
	o.max = 5
	inner.SetErr(errDown)
	for i := 1; i <= 8; i++ {
		_ = o.Send(context.Background(), msg(i))
		clock.Advance(time.Second)
	}
	if o.Pending() != 5 {
		t.Fatalf("pending = %d", o.Pending())
	}
	inner.SetErr(nil)
	if err := o.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(titles(inner.Sent()), ","); got != "m4,m5,m6,m7,m8" {
		t.Errorf("kept = %s", got)
	}
}

func TestDefaultCap(t *testing.T) {
	o, inner, _, _ := setup(t)
	inner.SetErr(errDown)
	for i := range DefaultMax + 3 {
		_ = o.Send(context.Background(), msg(i))
	}
	if o.Pending() != DefaultMax {
		t.Errorf("pending = %d, want %d", o.Pending(), DefaultMax)
	}
}

func TestCorruptAndTempFiles(t *testing.T) {
	o, inner, _, dir := setup(t)
	inner.SetErr(errDown)
	_ = o.Send(context.Background(), msg(1))
	if err := os.WriteFile(filepath.Join(dir, "00000000000000000000-000000.json"), []byte("{garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, tmpPrefix+"123"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if o.Pending() != 2 {
		t.Fatalf("pending = %d (temp files must be ignored)", o.Pending())
	}
	// A new instance cleans temp files.
	o = New(dir, inner, fakes.NewClock(start))
	if _, err := os.Stat(filepath.Join(dir, tmpPrefix+"123")); !os.IsNotExist(err) {
		t.Errorf("temp file not cleaned: %v", err)
	}
	inner.SetErr(nil)
	if err := o.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(titles(inner.Sent()), ","); got != "m1" {
		t.Errorf("sent = %s", got)
	}
	if o.Pending() != 0 {
		t.Errorf("corrupt file kept")
	}
}

func TestQueueFailure(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	inner := fakes.NewNotifier()
	inner.SetErr(errDown)
	err := New(file, inner, fakes.NewClock(start)).Send(context.Background(), msg(1))
	if err == nil || errors.Is(err, contracts.ErrQueued) || !errors.Is(err, errDown) {
		t.Fatalf("err = %v", err)
	}
}

func TestInvalidNotQueued(t *testing.T) {
	o, inner, _, _ := setup(t)
	inner.SetErr(errDown)
	if err := o.Send(context.Background(), contracts.Notification{}); err == nil || errors.Is(err, contracts.ErrQueued) {
		t.Fatalf("err = %v", err)
	}
	if o.Pending() != 0 || len(inner.Attempts()) != 0 {
		t.Error("invalid notification was sent or queued")
	}
}

func TestFlushContextCanceled(t *testing.T) {
	o, inner, _, _ := setup(t)
	inner.SetErr(errDown)
	_ = o.Send(context.Background(), msg(1))
	inner.SetErr(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := o.Flush(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if o.Pending() != 1 {
		t.Error("message lost on canceled flush")
	}
}

func TestConcurrentUse(t *testing.T) {
	o, inner, _, _ := setup(t)
	inner.SetErr(errDown)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() { _ = o.Send(context.Background(), msg(i)) })
	}
	wg.Go(func() { _ = o.Flush(context.Background()) })
	wg.Wait()
	if o.Pending() != 20 {
		t.Fatalf("pending = %d", o.Pending())
	}
	inner.SetErr(nil)
	for range 3 {
		wg.Go(func() { _ = o.Flush(context.Background()) })
	}
	wg.Wait()
	if o.Pending() != 0 {
		t.Errorf("pending = %d", o.Pending())
	}
}

func TestMulti(t *testing.T) {
	a, b := fakes.NewNotifier(), fakes.NewNotifier()
	m := Multi(a, nil, b)
	if err := m.Send(context.Background(), msg(1)); err != nil {
		t.Fatal(err)
	}
	if len(a.Sent()) != 1 || len(b.Sent()) != 1 {
		t.Errorf("fan-out: a=%d b=%d", len(a.Sent()), len(b.Sent()))
	}

	// One failure does not stop the others; errors are joined and
	// contracts.ErrQueued stays detectable through Multi.
	o, inner, _, _ := setup(t)
	inner.SetErr(errDown)
	boom := errors.New("osascript boom")
	b.SetErr(boom)
	err := Multi(o, a, b).Send(context.Background(), msg(2))
	if !errors.Is(err, contracts.ErrQueued) || !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if len(a.Sent()) != 2 {
		t.Error("healthy notifier skipped")
	}

	if Multi(a) != contracts.Notifier(a) {
		t.Error("single notifier should be returned as-is")
	}
	if err := Multi().Send(context.Background(), msg(3)); err != nil {
		t.Errorf("empty multi: %v", err)
	}
	if err := Multi().Send(context.Background(), contracts.Notification{}); err == nil {
		t.Error("empty multi accepted an invalid notification")
	}
}
