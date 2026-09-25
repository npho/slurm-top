package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	"golang.org/x/term"
	"syscall"
)

func readKeys(in *os.File, events chan<- byte) {
	var b [1]byte
	escape := 0
	for {
		if _, err := in.Read(b[:]); err != nil {
			return
		}
		c := b[0]
		if escape == 1 {
			if c == '[' {
				escape = 2
			} else {
				escape = 0
			}
			continue
		}
		if escape == 2 {
			escape = 0
			if c == 'C' {
				c = 'n'
			} else if c == 'D' {
				c = 'p'
			} else {
				continue
			}
		} else if c == 27 {
			escape = 1
			continue
		}
		select {
		case events <- c:
		default:
		}
	}
}

// An alternate-screen session is opt-in and never writes control codes to a pipe.
func runInteractive(in, out *os.File, all bool, view string, interval, timeout time.Duration) error {
	if !term.IsTerminal(int(in.Fd())) || !term.IsTerminal(int(out.Fd())) || os.Getenv("TERM") == "dumb" {
		return fmt.Errorf("--interactive requires terminal stdin and stdout")
	}
	root, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	load := func() ([]Node, error) {
		ctx, cancel := context.WithTimeout(root, timeout)
		defer cancel()
		data, err := collect(ctx)
		if err != nil {
			return nil, err
		}
		nodes := parseNodes(string(data), all)
		if len(nodes) == 0 {
			return nil, fmt.Errorf("Slurm returned no matching nodes (try --all)")
		}
		return nodes, nil
	}
	nodes, err := load()
	if err != nil {
		return err
	}
	saved, err := term.MakeRaw(int(in.Fd()))
	if err != nil {
		return err
	}
	defer term.Restore(int(in.Fd()), saved)
	if _, err := fmt.Fprint(out, "\x1b[?1049h\x1b[?25l"); err != nil {
		return err
	}
	defer fmt.Fprint(out, "\x1b[?25h\x1b[?1049l")
	color := os.Getenv("NO_COLOR") == "" && os.Getenv("CLICOLOR") != "0"
	keys := make(chan byte, 16)
	go readKeys(in, keys)
	resize := make(chan os.Signal, 1)
	signal.Notify(resize, syscall.SIGWINCH)
	defer signal.Stop(resize)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	page := 0
	message := ""
	draw := func() error {
		width, height, e := term.GetSize(int(out.Fd()))
		if e != nil || width < 1 || height < 1 {
			width, height = 80, 24
		}
		if width < 30 {
			width = 30
		}
		if height < 6 {
			height = 6
		}
		size := gridPageSize(width, height)
		if view == "detail" {
			columns := width / 45
			if columns < 1 {
				columns = 1
			}
			if columns > 4 {
				columns = 4
			}
			rows := (height - 5) / 7
			if rows < 1 {
				rows = 1
			}
			size = rows * columns
		}
		pages := gridPages(len(nodes), size)
		if page >= pages {
			page = pages - 1
		}
		var b bytes.Buffer
		if view == "detail" {
			// Detail renderer produces the same cluster summary on every page.
			start := page * size
			end := start + size
			if end > len(nodes) {
				end = len(nodes)
			}
			if err := renderDashboardFor(&b, nodes, nodes[start:end], time.Now(), width, color); err != nil {
				return err
			}
		} else if err := renderGrid(&b, nodes, time.Now(), width, page, size, color); err != nil {
			return err
		}
		footer := fmt.Sprintf("Page %d/%d  •  n/→ next  p/← previous  d toggle detail  r refresh  q quit", page+1, pages)
		if message != "" {
			footer += "  •  " + message
		}
		// Bound the frame to the viewport; leave the final column unused to avoid
		// terminals automatically wrapping at the right edge.
		lines := bytes.Split(bytes.TrimSuffix(b.Bytes(), []byte("\n")), []byte("\n"))
		var frame bytes.Buffer
		frame.WriteString("\x1b[H\x1b[2J")
		for i, line := range lines {
			if i >= height-1 {
				break
			}
			if i > 0 {
				frame.WriteString("\r\n")
			}
			frame.Write(line)
		}
		frame.WriteString("\x1b[")
		fmt.Fprintf(&frame, "%d;1H", height)
		frame.WriteString(fit(footer, width-1))
		_, e = io.Copy(out, &frame)
		return e
	}
	if err := draw(); err != nil {
		return err
	}
	for {
		select {
		case <-root.Done():
			return nil
		case <-resize:
			if err := draw(); err != nil {
				return err
			}
		case key := <-keys:
			switch key {
			case 'q', 'Q', 3:
				return nil
			case 'n', ' ':
				page++
			case 'p':
				if page > 0 {
					page--
				}
			case 'd':
				if view == "detail" {
					view = "grid"
				} else {
					view = "detail"
				}
				page = 0
			case 'r':
				updated, e := load()
				if root.Err() != nil {
					return nil
				}
				if e != nil {
					message = e.Error()
				} else {
					nodes = updated
					message = ""
				}
			default:
				continue
			}
			if err := draw(); err != nil {
				return err
			}
		case <-ticker.C:
			updated, e := load()
			if root.Err() != nil {
				return nil
			}
			if e != nil {
				message = e.Error()
			} else {
				nodes = updated
				message = ""
			}
			if err := draw(); err != nil {
				return err
			}
		}
	}
}
