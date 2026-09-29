package log

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"testing"

	"github.com/ilias-t/bckp/internal/contracts"
)

func TestSend(t *testing.T) {
	var buf bytes.Buffer
	n := New(slog.New(slog.NewJSONHandler(&buf, nil)))
	err := n.Send(context.Background(), contracts.Notification{Priority: 4, Title: "Hot", Body: "CPU 92°C", Tags: []string{"fire"}})
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("%q: %v", buf.String(), err)
	}
	delete(rec, "time")
	want := map[string]any{
		"level": "INFO", "msg": "notification", "priority": float64(4),
		"title": "Hot", "body": "CPU 92°C", "tags": []any{"fire"},
	}
	if !reflect.DeepEqual(rec, want) {
		t.Errorf("got  %v\nwant %v", rec, want)
	}
	if err := n.Send(context.Background(), contracts.Notification{}); err == nil {
		t.Error("empty notification accepted")
	}
	if New(nil) == nil {
		t.Error("New(nil) returned nil")
	}
}
