package main

import (
	"log/slog"

	"github.com/ilias-t/bckp/internal/cli"
	"github.com/ilias-t/bckp/internal/config"
	"github.com/ilias-t/bckp/internal/contracts"
	lognotify "github.com/ilias-t/bckp/internal/notify/log"
	"github.com/ilias-t/bckp/internal/notify/macos"
	"github.com/ilias-t/bckp/internal/notify/ntfy"
	"github.com/ilias-t/bckp/internal/notify/outbox"
	"github.com/ilias-t/bckp/internal/system"
)

// notifierChain builds the session's notifiers: ntfy when a topic is set
// and macOS banners when notify.macos is on, sent to concurrently. The
// engine logs every notification itself, so there is no log notifier here.
//
// With outboxDir set, only ntfy goes through the disk-backed outbox, and
// the result is then a contracts.Outbox for it. Banners stay outside it:
// the outbox is for network outages, and re-sending a queued message
// through the whole chain would repeat banners that already went out, or
// queue local failures that a retry cannot fix.
func notifierChain(cfg config.Config, outboxDir string) contracts.Notifier {
	var remote contracts.Notifier
	if cfg.Notify.NtfyTopic != "" {
		remote = ntfy.New(cfg.Notify.NtfyServer, cfg.Notify.NtfyTopic, nil)
		if outboxDir != "" {
			remote = outbox.New(outboxDir, remote, system.Clock{})
		}
	}
	var banner contracts.Notifier
	if cfg.Notify.MacOS {
		banner = macos.New(system.Exec{})
	}
	return outbox.Multi(remote, banner)
}

// namedNotifiers is cli.Wiring.NewNotifiers: the backends of
// notifierChain, without the outbox, each on its own so `notify test` can
// report them one by one, plus a log backend. The log backend writes to log (the CLI's event
// log), never to the terminal.
func namedNotifiers(cfg config.Config, log *slog.Logger) []cli.NamedNotifier {
	var out []cli.NamedNotifier
	if cfg.Notify.NtfyTopic != "" {
		out = append(out, cli.NamedNotifier{Name: "ntfy", Notifier: ntfy.New(cfg.Notify.NtfyServer, cfg.Notify.NtfyTopic, nil)})
	}
	if cfg.Notify.MacOS {
		out = append(out, cli.NamedNotifier{Name: "macOS", Notifier: macos.New(system.Exec{})})
	}
	return append(out, cli.NamedNotifier{Name: "log", Notifier: lognotify.New(log)})
}
