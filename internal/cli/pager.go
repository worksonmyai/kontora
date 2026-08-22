package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
)

// PagerEnvVar is kontora's own pager setting.
const PagerEnvVar = "KONTORA_PAGER"

// TicketPagerEnvVar is the name the standalone `ticket` CLI used, honoured after
// KONTORA_PAGER so a shell that still exports it keeps working.
const TicketPagerEnvVar = "TICKET_PAGER"

// PagerCommand resolves the pager command line: KONTORA_PAGER, then
// TICKET_PAGER, then PAGER, then the config's pager. A variable that is set but
// blank means "no pager" and stops the search rather than falling through, which
// is the per-invocation way to turn paging off.
func PagerCommand(cfgPager string) string {
	for _, name := range []string{PagerEnvVar, TicketPagerEnvVar, "PAGER"} {
		v, ok := os.LookupEnv(name)
		if !ok {
			continue
		}
		return strings.TrimSpace(v)
	}
	return strings.TrimSpace(cfgPager)
}

// Page runs fn with the writer its output belongs on, spawning a pager in
// between when one is configured and stdout is a terminal. It is the only way a
// pager starts, so a command that does not call it is never paged. Page returns
// only after the pager has exited, which matters because every caller ends in
// log.Fatal on error: a deferred close would never run.
//
// os.Stdout is never reassigned. lipgloss binds its colour profile to the real
// os.Stdout at package init, so styling survives the pipe, and Go only turns a
// write into a fatal SIGPIPE on fd 1 and 2, so a pager the user quits early
// gives an ordinary EPIPE here instead of killing the process.
func Page(pager string, fn func(w io.Writer) error) error {
	return page(os.Stdout, os.Stderr, pager, fn)
}

// page decides whether a pager applies. Nothing is spawned when none is
// configured or when out is not a terminal, so a redirect or a pipe is never
// paged.
func page(out, warn io.Writer, pager string, fn func(w io.Writer) error) error {
	if strings.TrimSpace(pager) == "" || outputWidth(out) == 0 {
		return fn(out)
	}
	return pageSpawn(out, warn, pager, fn)
}

// pageSpawn runs fn with the pager's stdin, then waits for the pager to exit.
func pageSpawn(out, warn io.Writer, pager string, fn func(w io.Writer) error) error {
	parts := strings.Fields(pager)
	if len(parts) == 0 {
		return fn(out)
	}

	cmd := exec.Command(parts[0], parts[1:]...)
	cmd.Stdout = out
	cmd.Stderr = warn
	cmd.Env = pagerEnv()

	w, err := cmd.StdinPipe()
	if err != nil {
		return fn(out)
	}
	if err := cmd.Start(); err != nil {
		// Losing the output because the pager is missing would be worse than
		// not paging at all, which is what the shell script it replaces does.
		fmt.Fprintf(warn, "pager %q: %v\n", parts[0], err)
		return fn(out)
	}

	// Ctrl-C belongs to the pager while it runs, so it can restore the screen
	// instead of being orphaned by a dying kontora.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)

	runErr := fn(w)
	_ = w.Close()
	_ = cmd.Wait()

	signal.Stop(sig)
	signal.Reset(os.Interrupt)

	// Quitting the pager before the output ends is normal use, not a failure.
	if errors.Is(runErr, syscall.EPIPE) || errors.Is(runErr, os.ErrClosed) {
		return nil
	}
	return runErr
}

// pagerEnv tells less and lv to keep colour and to exit on a short page, unless
// the user already said otherwise. Styling reaches the pager because lipgloss
// reads the real os.Stdout, not the pipe.
func pagerEnv() []string {
	env := os.Environ()
	if _, ok := os.LookupEnv("LESS"); !ok {
		env = append(env, "LESS=FRX")
	}
	if _, ok := os.LookupEnv("LV"); !ok {
		env = append(env, "LV=-c")
	}
	return env
}
