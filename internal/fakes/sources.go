package fakes

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/ilias-t/bckp/internal/contracts"
)

// errNothingScripted is returned by a scripted source that never had a
// step.
var errNothingScripted = errors.New("fakes: nothing scripted")

type step[T any] struct {
	v   T
	err error
}

// script is a FIFO of readings/errors shared by the scripted sources.
// Once the queue is empty, the last step repeats.
type script[T any] struct {
	mu      sync.Mutex
	queue   []step[T]
	last    *step[T]
	reads   int
	stampAt func(*T) // fills a zero At from the clock, if configured
}

func (s *script[T]) push(v T, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queue = append(s.queue, step[T]{v: v, err: err})
}

func (s *script[T]) next(ctx context.Context) (T, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	var st step[T]
	switch {
	case len(s.queue) > 0:
		st = s.queue[0]
		s.queue = s.queue[1:]
		last := st // unstamped copy, so repeats get a fresh At
		s.last = &last
	case s.last != nil:
		st = *s.last
	default:
		return zero, errNothingScripted
	}
	if st.err == nil && s.stampAt != nil {
		s.stampAt(&st.v)
	}
	return st.v, st.err
}

func (s *script[T]) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

// Thermal is a scripted contracts.ThermalSource.
type Thermal struct {
	s script[contracts.ThermalReading]
}

var _ contracts.ThermalSource = (*Thermal)(nil)

// NewThermal returns a source that yields readings in order. If clock is
// non-nil, readings with a zero At are stamped with clock.Now().
func NewThermal(clock contracts.Clock, readings ...contracts.ThermalReading) *Thermal {
	t := &Thermal{}
	if clock != nil {
		t.s.stampAt = func(r *contracts.ThermalReading) {
			if r.At.IsZero() {
				r.At = clock.Now()
			}
		}
	}
	for _, r := range readings {
		t.Push(r)
	}
	return t
}

func (t *Thermal) Read(ctx context.Context) (contracts.ThermalReading, error) { return t.s.next(ctx) }

// Push appends a reading.
func (t *Thermal) Push(r contracts.ThermalReading) { t.s.push(r, nil) }

// PushErr appends a failed read.
func (t *Thermal) PushErr(err error) { t.s.push(contracts.ThermalReading{}, err) }

// Reads returns how many times Read was called.
func (t *Thermal) Reads() int { return t.s.count() }

// Power is a scripted contracts.PowerSource.
type Power struct {
	s script[contracts.PowerReading]
}

var _ contracts.PowerSource = (*Power)(nil)

// NewPower returns a source that yields readings in order. If clock is
// non-nil, readings with a zero At are stamped with clock.Now().
func NewPower(clock contracts.Clock, readings ...contracts.PowerReading) *Power {
	p := &Power{}
	if clock != nil {
		p.s.stampAt = func(r *contracts.PowerReading) {
			if r.At.IsZero() {
				r.At = clock.Now()
			}
		}
	}
	for _, r := range readings {
		p.Push(r)
	}
	return p
}

func (p *Power) Read(ctx context.Context) (contracts.PowerReading, error) { return p.s.next(ctx) }

// Push appends a reading.
func (p *Power) Push(r contracts.PowerReading) { p.s.push(r, nil) }

// PushErr appends a failed read.
func (p *Power) PushErr(err error) { p.s.push(contracts.PowerReading{}, err) }

// Reads returns how many times Read was called.
func (p *Power) Reads() int { return p.s.count() }

// Network is a fake contracts.Network: a scripted Snapshot queue plus an
// in-memory model of service order, service enablement and Wi-Fi power.
// Setters mutate the model and are recorded as calls such as
// "SetServiceEnabled iPhone USB false".
type Network struct {
	snaps script[contracts.NetSnapshot]

	mu       sync.Mutex
	order    []string
	disabled map[string]bool
	wifiOn   bool
	calls    []string
	failNext map[string]error
}

var _ contracts.Network = (*Network)(nil)

// Network method names, for FailNext.
const (
	methodSnapshot          = "Snapshot"
	MethodServiceOrder      = "ServiceOrder"
	MethodSetServiceOrder   = "SetServiceOrder"
	MethodSetServiceEnabled = "SetServiceEnabled"
	MethodWiFiPower         = "WiFiPower"
	MethodSetWiFiPower      = "SetWiFiPower"
)

// NewNetwork returns a fake with the given service order, all services
// enabled, and Wi-Fi on. If clock is non-nil, snapshots with a zero At are
// stamped with clock.Now().
func NewNetwork(clock contracts.Clock, order ...string) *Network {
	n := &Network{order: slices.Clone(order), disabled: map[string]bool{}, wifiOn: true, failNext: map[string]error{}}
	if clock != nil {
		n.snaps.stampAt = func(s *contracts.NetSnapshot) {
			if s.At.IsZero() {
				s.At = clock.Now()
			}
		}
	}
	return n
}

// PushSnapshot appends a snapshot.
func (n *Network) PushSnapshot(s contracts.NetSnapshot) { n.snaps.push(s, nil) }

// Snapshots returns how many times Snapshot was called.
func (n *Network) Snapshots() int { return n.snaps.count() }

// FailNext makes the next call of method return err (once).
func (n *Network) FailNext(method string, err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.failNext[method] = err
}

func (n *Network) takeFail(method string) error {
	err := n.failNext[method]
	delete(n.failNext, method)
	return err
}

func (n *Network) Snapshot(ctx context.Context) (contracts.NetSnapshot, error) {
	n.mu.Lock()
	n.calls = append(n.calls, methodSnapshot)
	err := n.takeFail(methodSnapshot)
	n.mu.Unlock()
	if err != nil {
		return contracts.NetSnapshot{}, err
	}
	return n.snaps.next(ctx)
}

func (n *Network) ServiceOrder(ctx context.Context) ([]string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, MethodServiceOrder)
	if err := n.takeFail(MethodServiceOrder); err != nil {
		return nil, err
	}
	return slices.Clone(n.order), ctx.Err()
}

// SetServiceOrder requires order to be a permutation of the current order,
// mirroring networksetup -ordernetworkservices.
func (n *Network) SetServiceOrder(ctx context.Context, order []string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, fmt.Sprintf("%s %v", MethodSetServiceOrder, order))
	if err := n.takeFail(MethodSetServiceOrder); err != nil {
		return err
	}
	a, b := slices.Clone(order), slices.Clone(n.order)
	slices.Sort(a)
	slices.Sort(b)
	if !slices.Equal(a, b) {
		return fmt.Errorf("fakes: service order %v is not a permutation of %v", order, n.order)
	}
	n.order = slices.Clone(order)
	return ctx.Err()
}

func (n *Network) SetServiceEnabled(ctx context.Context, service string, on bool) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, fmt.Sprintf("%s %s %t", MethodSetServiceEnabled, service, on))
	if err := n.takeFail(MethodSetServiceEnabled); err != nil {
		return err
	}
	if !slices.Contains(n.order, service) {
		return fmt.Errorf("fakes: unknown network service %q", service)
	}
	n.disabled[service] = !on
	return ctx.Err()
}

func (n *Network) WiFiPower(ctx context.Context) (bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, MethodWiFiPower)
	if err := n.takeFail(MethodWiFiPower); err != nil {
		return false, err
	}
	return n.wifiOn, ctx.Err()
}

func (n *Network) SetWiFiPower(ctx context.Context, on bool) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, fmt.Sprintf("%s %t", MethodSetWiFiPower, on))
	if err := n.takeFail(MethodSetWiFiPower); err != nil {
		return err
	}
	n.wifiOn = on
	return ctx.Err()
}

// Calls returns recorded calls in order.
func (n *Network) Calls() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.calls)
}

// Order returns the current modeled service order.
func (n *Network) Order() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.order)
}

// Enabled reports whether a service is enabled in the model.
func (n *Network) Enabled(service string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return !n.disabled[service]
}

// WiFiOn reports modeled Wi-Fi power.
func (n *Network) WiFiOn() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.wifiOn
}
