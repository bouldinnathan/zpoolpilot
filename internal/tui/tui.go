// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package tui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
	"github.com/bouldinnathan/zpoolpilot/internal/monitor"
)

// Options controls the interactive dashboard. Run automatically chooses raw
// single-key input for a terminal and a line-oriented fallback for pipes and
// tests. The fallback accepts the same keys followed by Enter.
type Options struct {
	In             io.Reader
	Out            io.Writer
	Color          bool
	ReadOnly       bool
	HardwareMaxAge time.Duration
	Refresh        func(context.Context) (domain.Snapshot, error)
}

var pages = []string{
	"Overview",
	"Pools / VDevs",
	"Drives / Bays",
	"Datasets / Settings",
	"ARC / Performance",
	"Tasks",
	"Actions",
	"Command Trace",
}

type inputEvent struct {
	key string
	err error
}

type refreshResult struct {
	snapshot domain.Snapshot
	err      error
}

// Run starts the dashboard. It never executes a storage mutation; Actions is
// an explanatory page and Refresh calls only the collector supplied by the
// caller.
func Run(ctx context.Context, initial domain.Snapshot, opts Options) (runErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.In == nil {
		opts.In = os.Stdin
	}
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	// Subscribe before changing terminal attributes. This closes the small but
	// important window in which an interrupt could otherwise leave echo or
	// canonical input disabled.
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, terminationSignals()...)
	defer signal.Stop(interrupts)
	resizes := make(chan os.Signal, 1)
	if sigs := resizeSignals(); len(sigs) > 0 {
		signal.Notify(resizes, sigs...)
		defer signal.Stop(resizes)
	}

	session, err := openTerminal(opts.In, opts.Out)
	if err != nil {
		return fmt.Errorf("configure terminal: %w", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	var inputDone <-chan struct{}
	defer func() {
		cancel()
		// A raw reader wakes at least every 100 ms. Let it leave the read loop
		// before canonical mode is restored so no goroutine remains stuck on
		// this terminal after Run returns.
		if inputDone != nil {
			select {
			case <-inputDone:
			case <-time.After(250 * time.Millisecond):
			}
		}
		if closeErr := session.close(opts.Out); runErr == nil && closeErr != nil {
			runErr = fmt.Errorf("restore terminal: %w", closeErr)
		}
	}()
	if err := session.enter(opts.Out); err != nil {
		return fmt.Errorf("enter terminal screen: %w", err)
	}

	events := make(chan inputEvent, 32)
	if session.raw {
		done := make(chan struct{})
		inputDone = done
		go func() {
			defer close(done)
			readRawEvents(runCtx, session.input, events)
		}()
	} else {
		go readLineEvents(runCtx, opts.In, events)
	}

	var ticker *time.Ticker
	var tick <-chan time.Time
	if session.fullscreen {
		ticker = time.NewTicker(time.Second)
		tick = ticker.C
		defer ticker.Stop()
	}

	page := 0
	scroll := 0
	snapshot := initial
	help := false
	refreshing := false
	refreshDone := make(chan refreshResult, 1)
	status := navigationHint(session.raw)

	for {
		width, height := session.size()
		maxScroll := maxPageScroll(snapshot, page, help, opts, height)
		if scroll > maxScroll {
			scroll = maxScroll
		}
		if err := render(opts.Out, snapshot, page, scroll, status, help, opts, width, height, session.fullscreen); err != nil {
			return fmt.Errorf("render dashboard: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-interrupts:
			// The CLI maps context cancellation to the conventional signal
			// exit code (130). Return the same sentinel whether this handler or
			// the caller's signal-aware context observes the signal first.
			return context.Canceled
		case <-resizes:
			// Recalculate dimensions and redraw.
		case <-tick:
			// Redraw snapshot age and any task progress already in the cache.
		case result := <-refreshDone:
			refreshing = false
			if result.err != nil {
				status = "Refresh failed: " + oneLine(result.err.Error())
			} else {
				snapshot = result.snapshot
				scroll = 0
				status = "Snapshot refreshed at " + displayTime(result.snapshot.Generated)
			}
		case event, ok := <-events:
			if !ok {
				return nil
			}
			if event.err != nil {
				return fmt.Errorf("read terminal input: %w", event.err)
			}
			key := strings.ToLower(strings.TrimSpace(event.key))
			switch key {
			case "q", "quit", "exit", "ctrl-d":
				return nil
			case "n", "right", "tab", "", "next":
				page = (page + 1) % len(pages)
				scroll = 0
				help = false
				status = "Page " + fmt.Sprintf("%d/%d", page+1, len(pages)) + ": " + pages[page]
			case "p", "left", "shift-tab", "previous", "prev":
				page = (page + len(pages) - 1) % len(pages)
				scroll = 0
				help = false
				status = "Page " + fmt.Sprintf("%d/%d", page+1, len(pages)) + ": " + pages[page]
			case "h", "?", "help":
				help = !help
				scroll = 0
				if help {
					status = "Help open; press h or ? to return."
				} else {
					status = navigationHint(session.raw)
				}
			case "r", "refresh":
				if refreshing {
					status = "A refresh is already in progress."
					continue
				}
				if opts.Refresh == nil {
					status = "Refresh is unavailable; run `zpoolpilot status --refresh`."
					continue
				}
				refreshing = true
				status = "Refreshing read-only data…"
				go func() {
					fresh, refreshErr := opts.Refresh(runCtx)
					select {
					case refreshDone <- refreshResult{snapshot: fresh, err: refreshErr}:
					case <-runCtx.Done():
					}
				}()
			case "j", "down":
				if scroll < maxScroll {
					scroll++
					status = fmt.Sprintf("Scrolled to content line %d/%d.", scroll+1, maxScroll+1)
				} else {
					status = "Already at the end of this page."
				}
			case "k", "up":
				if scroll > 0 {
					scroll--
					status = fmt.Sprintf("Scrolled to content line %d/%d.", scroll+1, maxScroll+1)
				} else {
					status = "Already at the top of this page."
				}
			case "page-down", "pgdn":
				step := height - 7
				if step < 1 {
					step = 1
				}
				scroll += step
				if scroll > maxScroll {
					scroll = maxScroll
				}
				status = fmt.Sprintf("Scrolled to content line %d/%d.", scroll+1, maxScroll+1)
			case "page-up", "pgup":
				step := height - 7
				if step < 1 {
					step = 1
				}
				scroll -= step
				if scroll < 0 {
					scroll = 0
				}
				status = fmt.Sprintf("Scrolled to content line %d/%d.", scroll+1, maxScroll+1)
			default:
				if target, ok := pageForKey(key); ok {
					page = target
					scroll = 0
					help = false
					status = "Page " + fmt.Sprintf("%d/%d", page+1, len(pages)) + ": " + pages[page]
				} else {
					status = "Unknown key " + fmt.Sprintf("%q", key) + "; press ? for help."
				}
			}
		}
	}
}

func pageForKey(key string) (int, bool) {
	switch key {
	case "1", "overview", "home":
		return 0, true
	case "2", "pools", "pool":
		return 1, true
	case "3", "drives", "drive", "bays", "bay":
		return 2, true
	case "4", "datasets", "dataset", "settings":
		return 3, true
	case "5", "arc", "performance", "cache":
		return 4, true
	case "6", "tasks", "task":
		return 5, true
	case "7", "actions", "action":
		return 6, true
	case "8", "commands", "command", "trace", "end":
		return 7, true
	default:
		return 0, false
	}
}

func navigationHint(raw bool) string {
	hint := "←/p →/n page  ↑/k ↓/j scroll  1–8 jump  r refresh  ? help  q quit"
	if !raw {
		hint += "  (press Enter after a key)"
	}
	return hint
}

func readLineEvents(ctx context.Context, r io.Reader, out chan<- inputEvent) {
	defer close(out)
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		event := inputEvent{key: scanner.Text()}
		select {
		case out <- event:
		case <-ctx.Done():
			return
		}
	}
	if err := scanner.Err(); err != nil {
		select {
		case out <- inputEvent{err: err}:
		case <-ctx.Done():
		}
	}
}

// readRawEvents parses ordinary keys and the terminal sequences used for
// arrows, Home/End, and back-tab. Raw mode retains ISIG, so Ctrl-C is handled
// through the signal path and terminal state is restored before Run returns.
func readRawEvents(ctx context.Context, f *os.File, out chan<- inputEvent) {
	defer close(out)
	buf := make([]byte, 32)
	pending := make([]byte, 0, 32)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		n, err := f.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
			var keys []string
			pending, keys = parseRawBytes(pending)
			for _, key := range keys {
				select {
				case out <- inputEvent{key: key}:
				case <-ctx.Done():
					return
				}
			}
		}
		if err != nil {
			if errors.Is(err, os.ErrClosed) {
				return
			}
			select {
			case out <- inputEvent{err: err}:
			case <-ctx.Done():
			}
			return
		}
	}
}

func parseRawBytes(in []byte) ([]byte, []string) {
	var keys []string
	for len(in) > 0 {
		if in[0] == 0x1b {
			if len(in) < 2 {
				break
			}
			if len(in) < 3 && (in[1] == '[' || in[1] == 'O') {
				break
			}
			if len(in) >= 3 && (in[1] == '[' || in[1] == 'O') {
				switch in[2] {
				case 'A':
					keys = append(keys, "up")
					in = in[3:]
					continue
				case 'B':
					keys = append(keys, "down")
					in = in[3:]
					continue
				case 'C':
					keys = append(keys, "right")
					in = in[3:]
					continue
				case 'D':
					keys = append(keys, "left")
					in = in[3:]
					continue
				case 'H':
					keys = append(keys, "home")
					in = in[3:]
					continue
				case 'F':
					keys = append(keys, "end")
					in = in[3:]
					continue
				case 'Z':
					keys = append(keys, "shift-tab")
					in = in[3:]
					continue
				}
				// Wait for a possibly split CSI sequence. Recognize the common
				// numeric Home/End/PageUp/PageDown forms, then discard others.
				end := -1
				for i := 2; i < len(in); i++ {
					if in[i] >= 0x40 && in[i] <= 0x7e {
						end = i + 1
						break
					}
				}
				if end < 0 {
					break
				}
				sequence := string(in[:end])
				switch sequence {
				case "\x1b[1~", "\x1b[7~":
					keys = append(keys, "home")
				case "\x1b[4~", "\x1b[8~":
					keys = append(keys, "end")
				case "\x1b[5~":
					keys = append(keys, "page-up")
				case "\x1b[6~":
					keys = append(keys, "page-down")
				}
				in = in[end:]
				continue
			}
			// Escape alone is not an action; discard it without turning a
			// following printable key into an unknown escape sequence.
			in = in[1:]
			continue
		}
		if in[0] == '\t' {
			keys = append(keys, "tab")
			in = in[1:]
			continue
		}
		if in[0] == '\r' || in[0] == '\n' {
			keys = append(keys, "next")
			in = in[1:]
			continue
		}
		if in[0] == 0x04 {
			keys = append(keys, "ctrl-d")
			in = in[1:]
			continue
		}
		if !utf8.FullRune(in) {
			break
		}
		r, size := utf8.DecodeRune(in)
		if r == utf8.RuneError && size == 1 {
			in = in[1:]
			continue
		}
		keys = append(keys, string(r))
		in = in[size:]
	}
	return in, keys
}

// Summary is a stable, line-oriented snapshot summary for non-interactive
// output. Severity is always represented in text, even when color is enabled.
func Summary(w io.Writer, s domain.Snapshot, color bool) {
	s = sanitizeSnapshot(s)
	fmt.Fprintf(w, "%s  generated %s", paint(color, "ZpoolPilot ALPHA", "1;36"), age(s.Generated))
	if s.Partial {
		fmt.Fprintf(w, "  %s", severity(color, domain.SeverityUnknown))
	}
	fmt.Fprintln(w)
	if len(s.Pools) == 0 {
		fmt.Fprintln(w, emptyPoolsMessage(s))
	}
	for _, p := range s.Pools {
		sev := poolSeverity(p)
		if sourceFailed(s, "zpool.status") {
			sev, p.State = domain.SeverityUnknown, "UNKNOWN"
		}
		boot := ""
		if p.BootCritical {
			boot = "  BOOT-CRITICAL"
		}
		trim := ""
		if !sourceFailed(s, "zpool.status") && p.Trim.State != "" {
			trim = fmt.Sprintf("  trim=%s %.1f%% errors=%d support=%s", p.Trim.State, p.Trim.Percent, p.Trim.Errors, trimSupportedText(p.Trim.Supported))
		}
		fmt.Fprintf(w, "%s %-18s %-10s %s%s%s\n", severity(color, sev), valueOr(p.Name, "unknown"), valueOr(p.State, "UNKNOWN"), poolCapacityText(s, p), boot, trim)
	}
	for _, f := range sortedFindings(s.Findings) {
		fmt.Fprintf(w, "%s %s: %s\n", severity(color, f.Severity), valueOr(f.Scope, "host"), valueOr(f.Title, "untitled finding"))
	}
}

func render(w io.Writer, s domain.Snapshot, page, scroll int, status string, help bool, opts Options, width, height int, fullscreen bool) error {
	frame := renderFrameAt(s, page, scroll, status, help, opts, width, height)
	if fullscreen {
		_, err := io.WriteString(w, "\x1b[H\x1b[J"+frame)
		return err
	}
	_, err := io.WriteString(w, frame)
	return err
}

func renderFrame(s domain.Snapshot, page int, status string, help bool, opts Options, width, height int) string {
	return renderFrameAt(s, page, 0, status, help, opts, width, height)
}

func renderFrameAt(s domain.Snapshot, page, scroll int, status string, help bool, opts Options, width, height int) string {
	if width <= 0 {
		width = 100
	}
	if width > 1000 {
		width = 1000
	}
	if height <= 0 {
		height = 30
	}
	if height > 500 {
		height = 500
	}
	if page < 0 || page >= len(pages) {
		page = 0
	}

	partial := ""
	if s.Partial {
		partial = " · " + severity(opts.Color, domain.SeverityUnknown) + " PARTIAL DATA"
	}
	header := fmt.Sprintf("%s · %s · snapshot %s%s", paint(opts.Color, "ZpoolPilot ALPHA", "1;36"), pages[page], age(s.Generated), partial)
	nav := pageTabs(page, opts.Color, width)
	separator := strings.Repeat("─", width)
	body := pageContent(s, page, help, opts)

	// Two header rows, two separators, status, and shortcut footer are kept
	// visible. Content is clipped to the remaining terminal height.
	const fixedRows = 6
	bodyRows := height - fixedRows
	if bodyRows < 0 {
		bodyRows = 0
	}
	maxScroll := len(body) - bodyRows
	if maxScroll < 0 {
		maxScroll = 0
	}
	if scroll < 0 {
		scroll = 0
	}
	if scroll > maxScroll {
		scroll = maxScroll
	}
	if maxScroll > 0 {
		header += fmt.Sprintf(" · view %d/%d", scroll+1, maxScroll+1)
	}
	if bodyRows == 0 {
		body = nil
	} else {
		end := scroll + bodyRows
		if end > len(body) {
			end = len(body)
		}
		view := append([]string(nil), body[scroll:end]...)
		if end < len(body) {
			// Reserve the final row for a clear scrolling affordance.
			hidden := len(body) - end + 1
			if len(view) > 0 {
				view = view[:len(view)-1]
			}
			view = append(view, severity(opts.Color, domain.SeverityInfo)+fmt.Sprintf(" ↓ %d more line(s); j/↓ or PageDown", hidden))
		}
		body = view
	}

	lines := []string{header, nav, separator}
	lines = append(lines, body...)
	raw := readerIsTerminal(opts.In)
	lines = append(lines, separator, oneLine(status), "Keys: "+navigationHint(raw))
	if len(lines) > height {
		lines = lines[:height]
	}
	for i := range lines {
		lines[i] = truncateANSI(lines[i], width)
	}
	return strings.Join(lines, "\n") + "\n"
}

func sessionMode(in io.Reader) string {
	if readerIsTerminal(in) {
		return "Raw terminal: keys take effect immediately."
	}
	return "Line mode: press Enter after each key."
}

func readerIsTerminal(in io.Reader) bool {
	f, ok := in.(*os.File)
	return ok && isTerminalFile(f)
}

func pageContent(s domain.Snapshot, page int, help bool, opts Options) []string {
	var content strings.Builder
	if help {
		helpPage(&content, sessionMode(opts.In), opts.ReadOnly, opts.Color)
		return splitLines(content.String())
	}
	s = sanitizeSnapshot(s)
	if opts.HardwareMaxAge > 0 {
		// A dashboard may stay open past the freshness threshold without a
		// collector refresh. Reevaluate age during each redraw, on the copy.
		monitor.RefreshTelemetryFindings(&s, time.Now().UTC(), opts.HardwareMaxAge)
	}
	switch page {
	case 0:
		overview(&content, s, opts.Color)
	case 1:
		pools(&content, s, opts.Color)
	case 2:
		drives(&content, s, opts.Color)
	case 3:
		datasets(&content, s, opts.Color)
	case 4:
		arc(&content, s, opts.Color)
	case 5:
		tasks(&content, s, opts.Color)
	case 6:
		actions(&content, opts.ReadOnly, opts.Color)
	case 7:
		commands(&content, s, opts.Color)
	}
	return splitLines(content.String())
}

func maxPageScroll(s domain.Snapshot, page int, help bool, opts Options, height int) int {
	if height > 500 {
		height = 500
	}
	bodyRows := height - 6
	if bodyRows <= 0 {
		return 0
	}
	maximum := len(pageContent(s, page, help, opts)) - bodyRows
	if maximum < 0 {
		return 0
	}
	return maximum
}

func helpPage(w io.Writer, mode string, readOnly, color bool) {
	fmt.Fprintln(w, "KEYBOARD")
	fmt.Fprintln(w, "  ← or p      previous page       → or n or Tab   next page")
	fmt.Fprintln(w, "  ↑ or k      scroll up           ↓ or j          scroll down")
	fmt.Fprintln(w, "  PageUp/Dn   scroll one screen")
	fmt.Fprintln(w, "  1…8         jump directly        Home / End      first / last page")
	fmt.Fprintln(w, "  r           refresh snapshot     h or ?          toggle this help")
	fmt.Fprintln(w, "  q / Ctrl-D  quit safely")
	fmt.Fprintln(w)
	fmt.Fprintln(w, mode)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "SAFETY LABELS")
	fmt.Fprintf(w, "  %s observation only; %s changes state; %s destructive or identity-sensitive.\n", severity(color, domain.SeverityOK), severity(color, domain.SeverityWarning), severity(color, domain.SeverityCritical))
	if readOnly {
		fmt.Fprintln(w, "  READ-ONLY mode is enabled; this dashboard exposes no mutation path.")
	} else {
		fmt.Fprintln(w, "  This dashboard still never mutates storage. Plans are applied separately.")
	}
}

func overview(w io.Writer, s domain.Snapshot, color bool) {
	fmt.Fprintf(w, "HOST  %s", valueOr(s.Host.Hostname, "unknown"))
	if s.Host.OS != "" {
		fmt.Fprintf(w, " · %s", s.Host.OS)
	}
	if s.Host.Kernel != "" {
		fmt.Fprintf(w, " · kernel %s", s.Host.Kernel)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "ZFS   %s", valueOr(s.Host.OpenZFSVersion, "version not collected"))
	if s.Host.ProxmoxVersion != "" || s.Host.ProxmoxNode != "" {
		fmt.Fprintf(w, " · Proxmox %s · node %s · boot %s", valueOr(s.Host.ProxmoxVersion, "unknown"), valueOr(s.Host.ProxmoxNode, "unknown"), valueOr(s.Host.ProxmoxBootMode, "unknown"))
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "DATA  %d pool(s) · %d drive(s) · %d dataset(s) · %d finding(s) · %d collection gap(s)\n", len(s.Pools), len(s.Drives), len(s.Datasets), len(s.Findings), len(s.Errors))

	if len(s.Pools) == 0 {
		fmt.Fprintln(w, "\n"+emptyPoolsMessage(s))
	} else {
		fmt.Fprintln(w, "\nPOOLS")
		for _, p := range s.Pools {
			boot := ""
			if p.BootCritical {
				boot = " · BOOT-CRITICAL"
			}
			sev := poolSeverity(p)
			if sourceFailed(s, "zpool.status") {
				sev, p.State = domain.SeverityUnknown, "UNKNOWN"
			}
			fmt.Fprintf(w, "  %s %-18s %-10s %s%s\n", severity(color, sev), valueOr(p.Name, "unknown"), valueOr(p.State, "UNKNOWN"), poolCapacityText(s, p), boot)
		}
	}
	if len(s.Findings) > 0 {
		fmt.Fprintln(w, "\nFINDINGS")
		for _, f := range sortedFindings(s.Findings) {
			fmt.Fprintf(w, "  %s %s · %s\n", severity(color, f.Severity), valueOr(f.Scope, "host"), valueOr(f.Title, "untitled finding"))
			if f.Evidence != "" {
				fmt.Fprintf(w, "      Evidence: %s\n", oneLine(f.Evidence))
			}
			if f.Remediation != "" {
				fmt.Fprintf(w, "      Suggested: %s\n", oneLine(f.Remediation))
			}
		}
	}
	if len(s.Errors) > 0 {
		fmt.Fprintln(w, "\nCOLLECTION GAPS")
		for _, e := range s.Errors {
			partial := ""
			if e.Partial {
				partial = " · partial"
			}
			fmt.Fprintf(w, "  %s %-12s %s (%s%s)\n", severity(color, domain.SeverityUnknown), valueOr(e.Source, "unknown"), oneLine(valueOr(e.Message, "no detail")), valueOr(e.Kind, "unknown"), partial)
		}
	}
}

func pools(w io.Writer, s domain.Snapshot, color bool) {
	if len(s.Pools) == 0 {
		fmt.Fprintln(w, emptyPoolsMessage(s))
		return
	}
	for _, p := range s.Pools {
		sev := poolSeverity(p)
		if sourceFailed(s, "zpool.status") {
			sev, p.State = domain.SeverityUnknown, "UNKNOWN"
		}
		fmt.Fprintf(w, "%s POOL %s · state %s", severity(color, sev), valueOr(p.Name, "unknown"), valueOr(p.State, "UNKNOWN"))
		if sourceFailed(s, "zpool.list") {
			fmt.Fprintln(w, " · capacity and fragmentation UNKNOWN (collection failed)")
		} else {
			fmt.Fprintf(w, " · raw %s / %s allocated (%.1f%%) · frag %.1f%%\n", formatBytes(p.AllocatedBytes), formatBytes(p.SizeBytes), p.CapacityPercent, p.FragmentationPct)
		}
		if p.BootCritical {
			fmt.Fprintln(w, paint(color, "    [RED] BOOT-CRITICAL: storage changes can make this host unbootable.", "1;31"))
		}
		if sourceFailed(s, "zpool.status") {
			fmt.Fprintln(w, "    Pool errors, tasks, and vdev topology UNKNOWN (status collection failed).")
			continue
		}
		fmt.Fprintf(w, "    errors read=%d write=%d checksum=%d · GUID %s\n", p.ReadErrors, p.WriteErrors, p.ChecksumErrors, valueOr(p.GUID, "not collected"))
		if p.Trim.State != "" {
			fmt.Fprintf(w, "    trim: %s · %.2f%% · %s / %s · errors=%d · supported=%s", p.Trim.State, p.Trim.Percent, formatBytes(p.Trim.BytesDone), formatBytes(p.Trim.BytesTotal), p.Trim.Errors, trimSupportedText(p.Trim.Supported))
			if !p.Trim.ActionTime.IsZero() {
				fmt.Fprintf(w, " · action time %s", displayTime(p.Trim.ActionTime))
			}
			fmt.Fprintln(w)
		}
		if p.Status != "" {
			fmt.Fprintf(w, "    status: %s\n", oneLine(p.Status))
		}
		if p.Action != "" {
			fmt.Fprintf(w, "    action: %s\n", oneLine(p.Action))
		}
		if len(p.VDevs) == 0 {
			fmt.Fprintln(w, "    No vdev topology was collected.")
		} else {
			fmt.Fprintln(w, "    VDEVS (class / type / state / errors / path)")
			vdevRows(w, p.VDevs, "      ", color)
		}
		fmt.Fprintln(w)
	}
}

func vdevRows(w io.Writer, vdevs []domain.VDev, indent string, color bool) {
	for _, v := range vdevs {
		sev := vdevSeverity(v)
		class := valueOr(v.Class, "data")
		typeName := valueOr(v.Type, "disk")
		where := v.Path
		if where == "" {
			where = v.StableDriveID
		}
		fmt.Fprintf(w, "%s%s %s · %s/%s · %s · R%d/W%d/C%d", indent, severity(color, sev), valueOr(v.Name, "unnamed"), class, typeName, valueOr(v.State, "UNKNOWN"), v.ReadErrors, v.WriteErrors, v.ChecksumErrors)
		if where != "" {
			fmt.Fprintf(w, " · %s", where)
		}
		if v.SizeBytes > 0 {
			fmt.Fprintf(w, " · %s", formatBytes(v.SizeBytes))
		}
		if v.Trim.State != "" {
			fmt.Fprintf(w, " · trim=%s %.2f%% supported=%s", v.Trim.State, v.Trim.Percent, trimSupportedText(v.Trim.Supported))
		}
		fmt.Fprintln(w)
		vdevRows(w, v.Children, indent+"  ", color)
	}
}

func drives(w io.Writer, s domain.Snapshot, color bool) {
	if len(s.Drives) == 0 {
		fmt.Fprintln(w, "No block drives were discovered in this snapshot.")
		return
	}
	for _, d := range s.Drives {
		bay := valueOr(d.Bay.Label, "UNKNOWN BAY")
		if d.Bay.Enclosure != "" {
			bay = d.Bay.Enclosure + "/" + bay
		}
		identity := firstValue(d.Serial, d.WWN, d.ByID, d.ID, "unknown identity")
		use := "available"
		if d.InUse {
			use = valueOr(d.Use, "in use")
		}
		fmt.Fprintf(w, "%s %s · %s · %s · %s · %s · %s\n", severity(color, monitor.DriveSeverity(s, d)), bay, valueOr(d.DevNode, "no devnode"), identity, valueOr(strings.TrimSpace(d.Vendor+" "+d.Model), "unknown model"), formatBytes(d.CapacityBytes), valueOr(d.MediaType, "unknown media"))
		fmt.Fprintf(w, "    bay source=%s confidence=%s · transport=%s · firmware=%s · logical/physical sector=%s/%s · %s\n", valueOr(d.Bay.Source, "unknown"), valueOr(d.Bay.Confidence, "unknown"), valueOr(d.Transport, "unknown"), valueOr(d.Firmware, "unknown"), formatSector(d.LogicalSectorBytes), formatSector(d.PhysicalSectorBytes), use)
		fmt.Fprint(w, "    SMART: ")
		if d.Health.Unsupported {
			fmt.Fprintf(w, "unsupported%s", messageSuffix(d.Health.Message))
		} else if d.Health.Sleeping {
			fmt.Fprint(w, "standby; drive was not woken")
		} else {
			fmt.Fprintf(w, "overall=%s", boolState(d.Health.SMARTPassed))
			writeHealthValue(w, "temperature", d.Health.TemperatureC, "°C")
			writeHealthValue(w, "life used", d.Health.PercentageUsed, "%")
			writeUintHealth(w, "power-on", d.Health.PowerOnHours, "h")
			writeUintHealth(w, "media errors", d.Health.MediaErrors, "")
			writeUintHealth(w, "reallocated", d.Health.Reallocated, "")
			writeUintHealth(w, "pending", d.Health.Pending, "")
			writeUintHealth(w, "uncorrectable", d.Health.OfflineUncorrect, "")
			writeUintHealth(w, "CRC", d.Health.CRCErrorCount, "")
			if d.Health.Message != "" {
				fmt.Fprintf(w, " · %s", oneLine(d.Health.Message))
			}
		}
		if !d.Health.FreshAt.IsZero() {
			fmt.Fprintf(w, " · sampled %s", age(d.Health.FreshAt))
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w, "\nUNKNOWN BAY is allowed for observation, but destructive plans require exact drive identity confirmation.")
}

func datasets(w io.Writer, s domain.Snapshot, color bool) {
	if len(s.Datasets) == 0 {
		fmt.Fprintln(w, "No ZFS datasets were collected.")
		return
	}
	for _, d := range s.Datasets {
		fmt.Fprintf(w, "%s %s · type=%s · used=%s · available=%s · referenced=%s", severity(color, domain.SeverityInfo), valueOr(d.Name, "unknown"), valueOr(d.Type, "unknown"), formatBytes(d.UsedBytes), formatBytes(d.AvailableBytes), formatBytes(d.ReferencedBytes))
		if d.CompressionRatio > 0 {
			fmt.Fprintf(w, " · compressratio=%.2fx", d.CompressionRatio)
		}
		fmt.Fprintln(w)
		fmt.Fprintf(w, "    compression=%s · dedup=%s · encryption=%s · keylocation=%s\n", property(d, "compression"), property(d, "dedup"), property(d, "encryption"), property(d, "keylocation"))
		fmt.Fprintf(w, "    recordsize=%s · mountpoint=%s · sync=%s · atime=%s · xattr=%s\n", property(d, "recordsize"), property(d, "mountpoint"), property(d, "sync"), property(d, "atime"), property(d, "xattr"))
	}
	fmt.Fprintln(w, "\nValues show effective setting (source) when source data exists. Dataset available space is not raw pool free space.")
}

func arc(w io.Writer, s domain.Snapshot, color bool) {
	fmt.Fprintln(w, "ARC and L2ARC values are host-wide; this snapshot has no per-pool cache attribution.")
	if sourceFailed(s, "arcstats") || (s.ARC.SizeBytes == 0 && s.ARC.TargetBytes == 0 && s.ARC.MaxBytes == 0 && s.ARC.Hits == 0 && s.ARC.Misses == 0) {
		fmt.Fprintf(w, "%s ARC counters were unavailable or not collected. Zero is not presented as measured usage.\n", severity(color, domain.SeverityUnknown))
		return
	}
	fmt.Fprintf(w, "%s ARC size %s · target %s · maximum %s\n", severity(color, domain.SeverityInfo), formatBytes(s.ARC.SizeBytes), formatBytes(s.ARC.TargetBytes), formatBytes(s.ARC.MaxBytes))
	ratio, known := hitRatio(s.ARC.Hits, s.ARC.Misses, s.ARC.HitRatio)
	if known {
		fmt.Fprintf(w, "    hits %d · misses %d · hit ratio %.2f%%\n", s.ARC.Hits, s.ARC.Misses, ratio*100)
	} else {
		fmt.Fprintln(w, "    ARC hit ratio unavailable: no hit/miss observations were collected.")
	}
	if s.ARC.L2SizeBytes == 0 && s.ARC.L2Hits == 0 && s.ARC.L2Misses == 0 {
		fmt.Fprintln(w, "    L2ARC absent or counters unavailable; the snapshot cannot distinguish those cases.")
	} else {
		l2Ratio, l2Known := hitRatio(s.ARC.L2Hits, s.ARC.L2Misses, 0)
		fmt.Fprintf(w, "    L2ARC size %s · hits %d · misses %d", formatBytes(s.ARC.L2SizeBytes), s.ARC.L2Hits, s.ARC.L2Misses)
		if l2Known {
			fmt.Fprintf(w, " · hit ratio %.2f%%", l2Ratio*100)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w, "\nInstantaneous per-vdev I/O rates and latency are not represented by this snapshot schema.")
}

func tasks(w io.Writer, s domain.Snapshot, color bool) {
	if sourceFailed(s, "zpool.status") {
		fmt.Fprintf(w, "%s Task activity is unknown because pool status collection failed.\n", severity(color, domain.SeverityUnknown))
		return
	}
	reported := 0
	for _, p := range s.Pools {
		if p.Scan.Function != "" {
			reported++
			state := strings.ToUpper(p.Scan.State)
			sev := domain.SeverityInfo
			if p.Scan.Errors > 0 {
				sev = domain.SeverityCritical
			}
			fmt.Fprintf(w, "%s %s · %s · %s", severity(color, sev), valueOr(p.Name, "unknown pool"), valueOr(p.Scan.Function, "unknown task"), valueOr(p.Scan.State, "state unknown"))
			if p.Scan.BytesTotal > 0 || p.Scan.Percent > 0 {
				fmt.Fprintf(w, " · %.2f%% · %s / %s", p.Scan.Percent, formatBytes(p.Scan.BytesExamined), formatBytes(p.Scan.BytesTotal))
			}
			fmt.Fprintf(w, " · errors=%d", p.Scan.Errors)
			if !p.Scan.EndedAt.IsZero() {
				fmt.Fprintf(w, " · ended %s", displayTime(p.Scan.EndedAt))
			} else if state == "FINISHED" || state == "CANCELED" {
				fmt.Fprint(w, " · end time not collected")
			}
			fmt.Fprintln(w)
		}
		if p.Trim.State != "" {
			reported++
			sev := domain.SeverityInfo
			if p.Trim.Errors > 0 {
				sev = domain.SeverityCritical
			} else if p.Trim.State == "unknown" || p.Trim.State == "mixed" {
				sev = domain.SeverityUnknown
			} else if p.Trim.State == "suspended" {
				sev = domain.SeverityWarning
			}
			fmt.Fprintf(w, "%s %s · manual trim · %s · %.2f%% · %s / %s · errors=%d · supported=%s", severity(color, sev), valueOr(p.Name, "unknown pool"), p.Trim.State, p.Trim.Percent, formatBytes(p.Trim.BytesDone), formatBytes(p.Trim.BytesTotal), p.Trim.Errors, trimSupportedText(p.Trim.Supported))
			if !p.Trim.ActionTime.IsZero() {
				fmt.Fprintf(w, " · action time %s", displayTime(p.Trim.ActionTime))
			}
			fmt.Fprintln(w)
		}
	}
	if reported == 0 {
		fmt.Fprintln(w, "No current or recent scrub, resilver, RAIDZ expansion, or trim task was reported.")
	}
	fmt.Fprintln(w, "\nTask data is a point-in-time observation; press r for a new collector snapshot.")
}

// Exact source failures suppress their measurements. A failed JSON probe
// followed by successful text fallback has a separate *.json source error.
func sourceFailed(s domain.Snapshot, source string) bool {
	for _, sourceError := range s.Errors {
		if sourceError.Source == source {
			return true
		}
	}
	return false
}

func emptyPoolsMessage(s domain.Snapshot) string {
	if sourceFailed(s, "zpool.status") || sourceFailed(s, "zpool.list") {
		return "Imported pools are UNKNOWN because pool discovery failed."
	}
	return "No imported ZFS pools were discovered."
}

func poolCapacityText(s domain.Snapshot, p domain.Pool) string {
	if sourceFailed(s, "zpool.list") {
		return "capacity UNKNOWN (collection failed)"
	}
	return fmt.Sprintf("%5.1f%% used · %s raw free", p.CapacityPercent, formatBytes(p.FreeBytes))
}

func trimSupportedText(supported *bool) string {
	if supported == nil {
		return "unknown/mixed"
	}
	if *supported {
		return "yes"
	}
	return "no"
}

func actions(w io.Writer, readOnly, color bool) {
	if readOnly {
		fmt.Fprintln(w, paint(color, "[GREEN] READ-ONLY MODE: no mutation workflow is exposed by this process.", "1;32"))
	} else {
		fmt.Fprintln(w, "This dashboard itself is observation-only. Storage changes use typed, expiring plans and a separate apply process.")
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "COMMON WORKFLOWS")
	fmt.Fprintln(w, "  zpoolpilot plan create-pool …       create mirror or RAIDZ topology")
	fmt.Fprintln(w, "  zpoolpilot plan replace-disk …      replace one identity-checked member")
	fmt.Fprintln(w, "  zpoolpilot plan replace-boot-disk … dedicated Proxmox boot-disk replacement")
	fmt.Fprintln(w, "  zpoolpilot plan attach-mirror …     attach a second mirror member")
	fmt.Fprintln(w, "  zpoolpilot plan expand-raidz …      expand a supported RAIDZ vdev")
	fmt.Fprintln(w, "  zpoolpilot plan add-vdev …          add a matching top-level data vdev")
	fmt.Fprintln(w, "  zpoolpilot plan add-spare …         add a hot spare")
	fmt.Fprintln(w, "  zpoolpilot plan prepare-disk …      explicitly remove stale signatures")
	fmt.Fprintln(w, "  sudo zpoolpilot apply PLAN.json     revalidate and apply a saved plan")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "%s reads run automatically. %s changes require confirmation. %s destructive steps require typed yes plus the displayed identity challenge.\n", severity(color, domain.SeverityOK), severity(color, domain.SeverityWarning), severity(color, domain.SeverityCritical))
	fmt.Fprintln(w, "Commands remain visible before confirmation; generic storage plans block boot-critical pools.")
}

func commands(w io.Writer, s domain.Snapshot, color bool) {
	if len(s.Commands) == 0 {
		fmt.Fprintln(w, "No command trace is present. Enable monitor.include_command_trace to cache read-command metadata.")
		fmt.Fprintln(w, "The trace records argv and execution status; command output is not displayed here.")
		return
	}
	for i, c := range s.Commands {
		flags := []string{string(c.Risk)}
		if c.TimedOut {
			flags = append(flags, "TIMED OUT")
		}
		if c.OutputHidden {
			flags = append(flags, "output hidden")
		}
		if c.Capability != "" {
			flags = append(flags, "capability="+c.Capability)
		}
		fmt.Fprintf(w, "%s %02d · %s · %s %s\n", severity(color, riskSeverity(c.Risk)), i+1, strings.Join(flags, " · "), valueOr(c.Program, "unknown"), quoteArgs(c.Args))
		fmt.Fprintf(w, "    started=%s · exit=%d · duration=%dms", displayTime(c.StartedAt), c.ExitCode, c.DurationMS)
		if c.Error != "" {
			fmt.Fprintf(w, " · error=%s", oneLine(c.Error))
		}
		fmt.Fprintln(w)
	}
}

func riskSeverity(r domain.Risk) domain.Severity {
	switch r {
	case domain.RiskDestructive:
		return domain.SeverityCritical
	case domain.RiskChange:
		return domain.SeverityWarning
	case domain.RiskRead:
		return domain.SeverityOK
	default:
		return domain.SeverityUnknown
	}
}

func poolSeverity(p domain.Pool) domain.Severity {
	state := strings.ToUpper(p.State)
	if state == "ONLINE" {
		if p.ReadErrors+p.WriteErrors+p.ChecksumErrors+p.Trim.Errors > 0 {
			return domain.SeverityWarning
		}
		return domain.SeverityOK
	}
	if state == "" || state == "UNKNOWN" {
		return domain.SeverityUnknown
	}
	return domain.SeverityCritical
}

func vdevSeverity(v domain.VDev) domain.Severity {
	state := strings.ToUpper(v.State)
	if state == "ONLINE" || state == "HEALTHY" {
		if v.ReadErrors+v.WriteErrors+v.ChecksumErrors+v.Trim.Errors > 0 {
			return domain.SeverityWarning
		}
		return domain.SeverityOK
	}
	if state == "" || state == "UNKNOWN" {
		return domain.SeverityUnknown
	}
	return domain.SeverityCritical
}

func sortedFindings(in []domain.Finding) []domain.Finding {
	out := append([]domain.Finding(nil), in...)
	sort.SliceStable(out, func(i, j int) bool { return severityOrder(out[i].Severity) > severityOrder(out[j].Severity) })
	return out
}

func severityOrder(s domain.Severity) int {
	switch s {
	case domain.SeverityCritical:
		return 4
	case domain.SeverityUnknown:
		return 3
	case domain.SeverityWarning:
		return 2
	case domain.SeverityInfo:
		return 1
	default:
		return 0
	}
}

func property(d domain.Dataset, name string) string {
	v := valueOr(d.Properties[name], "unknown")
	if src := d.PropertySources[name]; src != "" {
		return v + " (" + src + ")"
	}
	return v + " (source unknown)"
}

func quoteArgs(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		a = sanitizeTerminalText(a)
		if strings.ContainsAny(a, " \t\n'\"") || a == "" {
			out[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		} else {
			out[i] = a
		}
	}
	return strings.Join(out, " ")
}

func age(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	return d.Round(time.Second).String() + " ago"
}

func displayTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.Local().Format(time.RFC3339)
}

func formatBytes(v uint64) string {
	const unit = 1024
	if v < unit {
		return fmt.Sprintf("%d B", v)
	}
	div, exp := uint64(unit), 0
	for n := v / unit; n >= unit && exp < 5; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(v)/float64(div), "KMGTPE"[exp])
}

func formatSector(v uint64) string {
	if v == 0 {
		return "unknown"
	}
	return fmt.Sprintf("%d", v)
}

func severity(color bool, s domain.Severity) string {
	switch s {
	case domain.SeverityCritical:
		return paint(color, "[RED]", "1;31")
	case domain.SeverityWarning:
		return paint(color, "[YELLOW]", "1;33")
	case domain.SeverityUnknown:
		return paint(color, "[UNKNOWN]", "1;35")
	case domain.SeverityInfo:
		return paint(color, "[INFO]", "1;36")
	case domain.SeverityOK:
		return paint(color, "[GREEN]", "1;32")
	default:
		return paint(color, "[UNKNOWN]", "1;35")
	}
}

func paint(enabled bool, text, code string) string {
	if !enabled {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func pageTabs(active int, color bool, width int) string {
	styles := [][]string{
		{"Overview", "Pools/VDevs", "Drives/Bays", "Settings", "ARC", "Tasks", "Actions", "Trace"},
		{"Overview", "Pools", "Drives", "Data", "ARC", "Tasks", "Act", "Trace"},
		{"O", "P", "D", "S", "C", "T", "A", "X"},
	}
	for styleIndex, names := range styles {
		parts := make([]string, len(names))
		for i, name := range names {
			plain := fmt.Sprintf("%d:%s", i+1, name)
			parts[i] = plain
			if i == active {
				parts[i] = paint(color, "["+plain+"]", "1;37;44")
			}
		}
		joined := strings.Join(parts, "  ")
		if visibleWidth(joined) <= width || styleIndex == len(styles)-1 {
			return joined
		}
	}
	return ""
}

func splitLines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func truncateANSI(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if visibleWidth(s) <= width {
		return s
	}
	limit := width
	if width > 1 {
		limit = width - 1
	}
	var b strings.Builder
	visible := 0
	hadANSI := false
	for len(s) > 0 && visible < limit {
		if s[0] == 0x1b {
			end := ansiSequenceEnd(s)
			if end > 0 {
				b.WriteString(s[:end])
				s = s[end:]
				hadANSI = true
				continue
			}
		}
		r, size := utf8.DecodeRuneInString(s)
		if r == utf8.RuneError && size == 1 {
			s = s[1:]
			continue
		}
		b.WriteRune(r)
		s = s[size:]
		visible++
	}
	if width > 1 {
		b.WriteRune('…')
	}
	if hadANSI {
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

func visibleWidth(s string) int {
	width := 0
	for len(s) > 0 {
		if s[0] == 0x1b {
			if end := ansiSequenceEnd(s); end > 0 {
				s = s[end:]
				continue
			}
		}
		_, size := utf8.DecodeRuneInString(s)
		if size == 0 {
			break
		}
		s = s[size:]
		width++
	}
	return width
}

func ansiSequenceEnd(s string) int {
	if len(s) < 2 || s[0] != 0x1b || s[1] != '[' {
		return 0
	}
	for i := 2; i < len(s); i++ {
		if s[i] >= 0x40 && s[i] <= 0x7e {
			return i + 1
		}
	}
	return 0
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(sanitizeTerminalText(s)), " ")
}

func valueOr(value, fallback string) string {
	value = sanitizeTerminalText(value)
	if strings.TrimSpace(value) == "" {
		return sanitizeTerminalText(fallback)
	}
	return strings.TrimSpace(value)
}

func firstValue(values ...string) string {
	for _, value := range values {
		value = sanitizeTerminalText(value)
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func boolState(v *bool) string {
	if v == nil {
		return "unknown"
	}
	if *v {
		return "passed"
	}
	return "FAILED"
}

func writeHealthValue(w io.Writer, label string, value *float64, suffix string) {
	if value != nil {
		fmt.Fprintf(w, " · %s %.1f%s", label, *value, suffix)
	}
}

func writeUintHealth(w io.Writer, label string, value *uint64, suffix string) {
	if value != nil {
		fmt.Fprintf(w, " · %s %d%s", label, *value, suffix)
	}
}

func messageSuffix(message string) string {
	if message == "" {
		return ""
	}
	return " · " + oneLine(message)
}

func hitRatio(hits, misses uint64, supplied float64) (float64, bool) {
	if hits+misses > 0 {
		return float64(hits) / float64(hits+misses), true
	}
	if supplied > 0 {
		return supplied, true
	}
	return 0, false
}
