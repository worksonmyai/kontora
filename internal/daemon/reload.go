package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sync"

	"github.com/worksonmyai/kontora/internal/config"
	"github.com/worksonmyai/kontora/internal/watcher"
)

// errNoConfigPath is returned when a reload is requested but the daemon was
// started without WithConfigPath, so there is no file to re-read.
var errNoConfigPath = errors.New("no config path set")

// reloadConfig re-reads the config file, validates it in full, pins the
// restart-only settings to the values the daemon started with, and publishes
// the result. A parse or validation failure leaves the running config in place
// and returns the error; nothing is applied piecemeal.
//
// Agents already running keep the prompt, arguments, timeout, and binary they
// started with. A reload affects the next stage that starts.
func (d *Daemon) reloadConfig() error {
	// Serializes reloads from the different triggers: SIGHUP, the config file
	// watcher, and the raw-config endpoint.
	d.reloadMu.Lock()
	defer d.reloadMu.Unlock()

	if d.configPath == "" {
		return errNoConfigPath
	}

	next, err := config.Load(d.configPath)
	if err != nil {
		return err
	}
	// Same order as startup: the file, then the environment, then the
	// command-line overrides.
	next.ApplyServerEnvOverrides()
	if d.configOverride != nil {
		d.configOverride(next)
	}

	cur := d.config()
	pinRestartOnly(cur, next, d.log)
	d.cfg.Store(next)

	d.log.Info("config reloaded",
		"agents", len(next.Agents),
		"stages", len(next.Stages),
		"pipelines", len(next.Pipelines))
	return nil
}

// pinRestartOnly copies the settings a live reload cannot apply from the
// running config into next, and warns once per field whose on-disk value
// differs. These are the values the daemon freezes at construction or in Run:
// the worktree manager root, the instance name used for ticket claims, the
// semaphore capacity, the web listener, and the directories the watcher and
// the initial scan were pointed at. The log directory is pinned too, for a
// different reason: nothing freezes it, but `kontora logs` reads it live, so a
// change would strand every existing log file. Applying half of one of these
// settings, writing new tickets to a directory nothing watches for example, is
// worse than ignoring it.
func pinRestartOnly(cur, next *config.Config, log *slog.Logger) {
	warn := func(field string, running, onDisk any) {
		log.Warn("config field needs a daemon restart, keeping the running value",
			"field", field, "running", running, "on_disk", onDisk)
	}
	warnRedacted := func(field string) {
		log.Warn("config field needs a daemon restart, keeping the running value", "field", field)
	}

	if next.TicketsDir != cur.TicketsDir {
		warn("tickets_dir", cur.TicketsDir, next.TicketsDir)
		next.TicketsDir = cur.TicketsDir
	}
	if next.WorktreesDir != cur.WorktreesDir {
		warn("worktrees_dir", cur.WorktreesDir, next.WorktreesDir)
		next.WorktreesDir = cur.WorktreesDir
	}
	if next.LogsDir != cur.LogsDir {
		warn("logs_dir", cur.LogsDir, next.LogsDir)
		next.LogsDir = cur.LogsDir
	}
	if next.InstanceName != cur.InstanceName {
		warn("instance_name", cur.InstanceName, next.InstanceName)
		next.InstanceName = cur.InstanceName
	}
	if next.MaxConcurrentAgents != cur.MaxConcurrentAgents {
		warn("max_concurrent_agents", cur.MaxConcurrentAgents, next.MaxConcurrentAgents)
		next.MaxConcurrentAgents = cur.MaxConcurrentAgents
	}

	// One warning per differing web child, so "port changed" doesn't read as
	// "the whole web block changed". The token value is never logged.
	if !reflect.DeepEqual(next.Web.Enabled, cur.Web.Enabled) {
		warn("web.enabled", derefBool(cur.Web.Enabled), derefBool(next.Web.Enabled))
	}
	if next.Web.Host != cur.Web.Host {
		warn("web.host", cur.Web.Host, next.Web.Host)
	}
	if next.Web.Port != cur.Web.Port {
		warn("web.port", cur.Web.Port, next.Web.Port)
	}
	if next.Web.Token != cur.Web.Token {
		warnRedacted("web.token")
	}
	next.Web = cur.Web
}

func derefBool(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}

// configWatchDirs returns the directories to watch for changes to configPath.
// That is the config file's own directory, plus the directory holding the
// symlink target when configPath is a symlink pointing elsewhere. The second
// entry is what makes an edit through a dotfiles symlink reload on Linux,
// where inotify watches the directory itself and never sees a write to a
// target in another directory.
func configWatchDirs(configPath string) (dirs []string, paths []string) {
	if configPath == "" {
		return nil, nil
	}
	abs, err := filepath.Abs(configPath)
	if err != nil {
		abs = configPath
	}
	dirs = make([]string, 0, 2)
	dirs = append(dirs, filepath.Dir(abs))
	paths = make([]string, 0, 2)
	paths = append(paths, abs)

	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil || resolved == abs {
		return dirs, paths
	}
	paths = append(paths, resolved)

	// Only add the target's directory when it is a genuinely different
	// directory. Comparing resolved forms keeps a symlinked *parent* (a temp
	// dir on macOS, say) from producing two watchers on the same directory.
	targetDir := filepath.Dir(resolved)
	configDir, err := filepath.EvalSymlinks(dirs[0])
	if err != nil {
		configDir = dirs[0]
	}
	if targetDir != configDir {
		dirs = append(dirs, targetDir)
	}
	return dirs, paths
}

// reloadAndLog reloads the config and reports a failure. Used by the triggers
// that have no caller to return the error to.
func (d *Daemon) reloadAndLog() {
	if err := d.reloadConfig(); err != nil {
		d.log.Error("config reload failed, keeping the running config", "err", err)
	}
}

// startReloadTriggers starts the two reload triggers the daemon owns: the
// SIGHUP consumer, and one watcher per directory holding the config file. The
// third trigger is the raw-config endpoint, which calls reloadConfig directly.
// Run registers hup before anything that can fail, and passes it in here. The
// returned function closes the watchers.
func (d *Daemon) startReloadTriggers(ctx context.Context, wg *sync.WaitGroup, hup <-chan os.Signal) func() {
	// SIGHUP reloads the config. SIGINT/SIGTERM stay with the caller's context.
	wg.Go(func() {
		for {
			select {
			case <-hup:
				d.reloadAndLog()
			case <-ctx.Done():
				return
			}
		}
	})
	return d.startConfigWatchers(ctx, wg)
}

// startConfigWatchers watches the config file (and its symlink target) for
// changes and reloads on every matching event, whatever the op: an editor
// writing in place and an atomic rename both mean "the file changed". It
// returns a function that closes the watchers.
func (d *Daemon) startConfigWatchers(ctx context.Context, wg *sync.WaitGroup) func() {
	dirs, paths := configWatchDirs(d.configPath)
	if len(dirs) == 0 {
		return func() {}
	}

	filter := watcher.PathSet(paths...)
	var watchers []*watcher.Watcher
	for _, dir := range dirs {
		w, err := watcher.New(dir, d.debounce, filter)
		if err != nil {
			d.log.Warn("config watcher failed to start, continuing without it", "dir", dir, "err", err)
			continue
		}
		watchers = append(watchers, w)
		wg.Go(func() {
			for {
				select {
				case _, ok := <-w.Events():
					if !ok {
						return
					}
					d.reloadAndLog()
				case err, ok := <-w.Errors():
					if !ok {
						return
					}
					d.log.Error("config watcher error", "err", err)
				case <-ctx.Done():
					return
				}
			}
		})
	}
	if len(watchers) > 0 {
		d.log.Info("watching config for changes", "paths", paths)
	}

	return func() {
		for _, w := range watchers {
			_ = w.Close()
		}
	}
}
