// Package contracts holds the interfaces and data types shared by the
// daemon, engine, providers, CLI and TUI.
//
// Rules:
//   - This package has no behavior beyond trivial helpers (String, Parse,
//     JSON marshaling). It imports only the standard library.
//   - Types that travel over a socket carry JSON tags. Durations on the wire
//     are integer seconds in fields suffixed _s; timestamps are RFC 3339.
package contracts
