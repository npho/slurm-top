package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	"golang.org/x/term"
)

func runTop(out io.Writer, format, by string, interactive bool, watch, timeout time.Duration, promCfg ...PrometheusConfig) error {
	terminal := false
	if f, ok := out.(*os.File); ok {
		terminal = term.IsTerminal(int(f.Fd())) && os.Getenv("TERM") != "dumb"
	}
	if format == "json" && interactive {
		return fmt.Errorf("--interactive is unavailable with --format json")
	}
	if format == "top" && (interactive || terminal) {
		interval := watch
		if interval == 0 {
			interval = 5 * time.Second
		}
		return runTopUI(os.Stdin, os.Stdout, interval, timeout, by, promCfg...)
	}
	root, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	first := true
	for {
		ctx, cancel := context.WithTimeout(root, timeout)
		snap, err := collectTop(ctx)
		cancel()
		if root.Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}
		if terminal && format == "top" && watch > 0 && !first {
			if _, err := fmt.Fprint(out, "\x1b[H\x1b[2J"); err != nil {
				return err
			}
		}
		width := 0
		if terminal {
			if f, ok := out.(*os.File); ok {
				if w, _, e := term.GetSize(int(f.Fd())); e == nil {
					width = w
				}
			}
		}
		if format == "json" {
			sortUsers(snap.Users, by)
			if err := renderTop(out, snap, "json", width); err != nil {
				return err
			}
		} else if err := renderTop(out, snap, by, width); err != nil {
			return err
		}
		if watch == 0 {
			return nil
		}
		first = false
		select {
		case <-root.Done():
			return nil
		case <-time.After(watch):
		}
	}
}
