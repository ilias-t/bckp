package proto

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ilias-t/bckp/internal/contracts"
)

func TestLineReader(t *testing.T) {
	in := "{\"op\":\"status\"}\n\r\n{\"op\":\"renew\",\"lease\":\"a\"}\r\ntail"
	lr := NewLineReader(strings.NewReader(in), 64)
	want := []string{`{"op":"status"}`, ``, `{"op":"renew","lease":"a"}`, `tail`}
	for _, w := range want {
		got, err := lr.ReadLine()
		if err != nil || string(got) != w {
			t.Fatalf("ReadLine = %q, %v; want %q", got, err, w)
		}
	}
	if _, err := lr.ReadLine(); err != io.EOF {
		t.Fatalf("want io.EOF, got %v", err)
	}
}

func TestLineReaderLimit(t *testing.T) {
	exact := strings.Repeat("a", MaxDaemonRequestBytes)
	lr := NewLineReader(strings.NewReader(exact+"\n"), MaxDaemonRequestBytes)
	if got, err := lr.ReadLine(); err != nil || len(got) != MaxDaemonRequestBytes {
		t.Fatalf("line at limit: len %d, %v", len(got), err)
	}

	// A never-ending line must fail without reading everything.
	src := &countingReader{r: strings.NewReader(strings.Repeat("b", 1<<20))}
	lr = NewLineReader(src, MaxDaemonRequestBytes)
	if _, err := lr.ReadLine(); !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("err = %v, want ErrLineTooLong", err)
	}
	if src.n > 3*MaxDaemonRequestBytes {
		t.Fatalf("read %d bytes before rejecting", src.n)
	}
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func TestDecodeStrict(t *testing.T) {
	var req DaemonRequest
	lr := NewLineReader(strings.NewReader(`{"op":"status","evil":1}`+"\n"), MaxDaemonRequestBytes)
	var de *DecodeError
	if err := lr.Decode(&req, true); !errors.As(err, &de) {
		t.Fatalf("strict decode of unknown field: %v", err)
	}
	if err := Unmarshal([]byte(`{"op":"status"} {"op":"status"}`), &req, true); !errors.As(err, &de) {
		t.Fatalf("strict decode of trailing data: %v", err)
	}
	var resp EngineResponse
	if err := Unmarshal([]byte(`{"ok":true,"future_field":1}`), &resp, false); err != nil || !resp.OK {
		t.Fatalf("lenient decode: %v", err)
	}
}

func TestWriteLine(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteLine(&buf, DaemonRequest{Op: OpAcquire, TTLS: 90, Reason: "backpack"}); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != `{"op":"acquire","ttl_s":90,"reason":"backpack"}`+"\n" {
		t.Fatalf("got %q", got)
	}
}

func TestDaemonRequestValidate(t *testing.T) {
	cases := []struct {
		req     DaemonRequest
		wantErr bool
		wantTTL int
	}{
		{DaemonRequest{Op: OpAcquire}, false, 90},
		{DaemonRequest{Op: OpAcquire, TTLS: 5}, false, 30},
		{DaemonRequest{Op: OpAcquire, TTLS: 9999}, false, 300},
		{DaemonRequest{Op: OpAcquire, TTLS: 120}, false, 120},
		{DaemonRequest{Op: OpAcquire, TTLS: -1}, true, 0},
		{DaemonRequest{Op: OpAcquire, Lease: "x"}, true, 0},
		{DaemonRequest{Op: OpRenew, Lease: "x"}, false, 0},
		{DaemonRequest{Op: OpRenew}, true, 0},
		{DaemonRequest{Op: OpRelease, Lease: "x", Reason: "r"}, true, 0},
		{DaemonRequest{Op: OpSleep, Reason: "thermal critical"}, false, 0},
		{DaemonRequest{Op: OpSleep, Lease: "x", Reason: "thermal critical"}, false, 0},
		{DaemonRequest{Op: OpSleep, TTLS: 30}, true, 0},
		{DaemonRequest{Op: OpStatus}, false, 0},
		{DaemonRequest{Op: OpStatus, Lease: "x"}, true, 0},
		{DaemonRequest{Op: "shell"}, true, 0},
		{DaemonRequest{}, true, 0},
	}
	for _, c := range cases {
		req := c.req
		err := req.Validate()
		if (err != nil) != c.wantErr {
			t.Errorf("%+v: err = %v, wantErr %v", c.req, err, c.wantErr)
			continue
		}
		if err != nil && CodeOf(err) != CodeBadRequest {
			t.Errorf("%+v: code = %q", c.req, CodeOf(err))
		}
		if err == nil && req.TTLS != c.wantTTL {
			t.Errorf("%+v: ttl = %d, want %d", c.req, req.TTLS, c.wantTTL)
		}
	}
}

func TestSanitizeReason(t *testing.T) {
	if got := SanitizeReason("  hot\nline\x00two  "); got != "hot line two" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("é", 200)
	if got := SanitizeReason(long); len([]rune(got)) != MaxReasonLen {
		t.Errorf("len = %d runes", len([]rune(got)))
	}
	if got := SanitizeReason("bad\xffutf8"); got != "bad utf8" {
		t.Errorf("got %q", got)
	}
}

func TestDaemonResponsesWireShape(t *testing.T) {
	exp := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	b, _ := json.Marshal(DaemonLeaseResponse{Result: OKResult(), Lease: "id1", Expires: exp})
	if string(b) != `{"ok":true,"lease":"id1","expires":"2026-09-28T12:00:00Z"}` {
		t.Errorf("lease response: %s", b)
	}
	b, _ = json.Marshal(DaemonLeaseResponse{Result: Fail(CodeBusy, "lease active")})
	if string(b) != `{"ok":false,"error":"lease active","code":"busy"}` {
		t.Errorf("failed lease response: %s", b)
	}
	b, _ = json.Marshal(DaemonStatusResponse{Result: OKResult(), GuardStatus: contracts.GuardStatus{Backstop: contracts.BackstopIdle}})
	if string(b) != `{"ok":true,"sleep_disabled":false,"lease":null,"backstop":"idle"}` {
		t.Errorf("status response: %s", b)
	}
	st := DaemonStatusResponse{Result: OKResult(), GuardStatus: contracts.GuardStatus{
		SleepDisabled: true, Backstop: contracts.BackstopArmed,
		Lease: &contracts.LeaseInfo{ID: "id1", Reason: "backpack 90m", UID: 501, TTLS: 90, AcquiredAt: exp, Expires: exp.Add(90 * time.Second)},
	}}
	b, _ = json.Marshal(st)
	var back DaemonStatusResponse
	if err := Unmarshal(b, &back, true); err != nil {
		t.Fatal(err)
	}
	if back.Lease == nil || back.Lease.ID != "id1" || !back.SleepDisabled || back.Backstop != contracts.BackstopArmed {
		t.Errorf("status round trip: %s -> %+v", b, back)
	}
}

func TestRemoteErrorMapping(t *testing.T) {
	cases := map[ErrorCode]error{
		CodeBusy:           contracts.ErrLeaseBusy,
		CodeNoLease:        contracts.ErrLeaseLost,
		CodeLeaseMismatch:  contracts.ErrLeaseLost,
		CodeNotAllowed:     contracts.ErrNotAllowed,
		CodeNotAbortable:   contracts.ErrNotAbortable,
		CodeNoSleepPending: contracts.ErrNoSleepPending,
	}
	for code, sentinel := range cases {
		err := Fail(code, "x").Err()
		if !errors.Is(err, sentinel) {
			t.Errorf("%s does not map to %v", code, sentinel)
		}
		if CodeOf(err) != code {
			t.Errorf("CodeOf = %q", CodeOf(err))
		}
	}
	if errors.Is(Fail(CodeInternal, "x").Err(), contracts.ErrLeaseLost) {
		t.Error("internal must not map to a sentinel")
	}
	if OKResult().Err() != nil {
		t.Error("ok result must have nil Err")
	}
}

func TestEngineRequestValidate(t *testing.T) {
	yes := true
	cases := []struct {
		req     EngineRequest
		wantErr bool
	}{
		{EngineRequest{Op: OpEngineStatus}, false},
		{EngineRequest{Op: OpEngineSubscribe}, false},
		{EngineRequest{Op: OpEngineStop}, false},
		{EngineRequest{Op: OpEngineStop, Sleep: &yes}, false},
		{EngineRequest{Op: OpEngineStop, Minutes: 5}, true},
		{EngineRequest{Op: OpEngineExtend, Minutes: 30}, false},
		{EngineRequest{Op: OpEngineExtend, Forever: true}, false},
		{EngineRequest{Op: OpEngineExtend}, true},
		{EngineRequest{Op: OpEngineExtend, Minutes: 30, Forever: true}, true},
		{EngineRequest{Op: OpEngineExtend, Minutes: -5}, true},
		{EngineRequest{Op: OpEngineExtend, Minutes: MaxExtendMinutes + 1}, true},
		{EngineRequest{Op: OpEngineStatus, Sleep: &yes}, true},
		{EngineRequest{Op: OpEngineAbortSleep}, false},
		{EngineRequest{Op: OpEngineAbortSleep, Minutes: 1}, true},
		{EngineRequest{Op: OpEngineAbortSleep, Sleep: &yes}, true},
		{EngineRequest{Op: "reboot"}, true},
	}
	for _, c := range cases {
		if err := c.req.Validate(); (err != nil) != c.wantErr {
			t.Errorf("%+v: err = %v, wantErr %v", c.req, err, c.wantErr)
		}
	}
}

func TestEngineWireShape(t *testing.T) {
	no := false
	var buf bytes.Buffer
	_ = WriteLine(&buf, EngineRequest{Op: OpEngineStop, Sleep: &no})
	_ = WriteLine(&buf, EngineRequest{Op: OpEngineExtend, Forever: true})
	want := `{"op":"stop","sleep":false}` + "\n" + `{"op":"extend","forever":true}` + "\n"
	if buf.String() != want {
		t.Errorf("got %q", buf.String())
	}

	snap := &contracts.Snapshot{PID: 7, Session: contracts.SessionStatus{Ended: true, EndReason: contracts.EndStopped}}
	buf.Reset()
	_ = WriteLine(&buf, EngineResponse{Result: OKResult(), Snapshot: snap, Done: true})
	var back EngineResponse
	if err := NewLineReader(&buf, MaxEngineResponseBytes).Decode(&back, false); err != nil {
		t.Fatal(err)
	}
	if !back.OK || !back.Done || back.Snapshot == nil || back.Snapshot.Session.EndReason != contracts.EndStopped {
		t.Errorf("round trip: %+v", back)
	}
}
