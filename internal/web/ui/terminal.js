// xterm handles live here, not on the Alpine component. Alpine wraps the whole
// x-data object in a deep reactive Proxy, so a Terminal stored there reads back
// proxied, and every internal property hop in xterm's parser, buffer, and
// renderer pays a trap; loadAddon hands the same proxy to the addons. Reach
// these through termState only: one hop through the component brings the proxy
// back. Module scope is safe because only one kontora() component exists.
export const termState = {
  ws: null,
  term: null,
  fit: null,
  webgl: null,
  // Resolves once xterm's stylesheet is in the document.
  cssLoad: null,
  // Set once the vendored xterm modules have been imported.
  Terminal: null,
  FitAddon: null,
  Unicode11Addon: null,
  WebglAddon: null,
  inputDisposable: null,
  resizeObserver: null,
  resizeTimer: null,
  webglRetried: false,
  paneReset: false,
  streamSeq: 0,
  // The breakpoint watcher updates isMobile before closing the old terminal.
  mobile: false,
};

// A terminal can outlive its socket. Keep new resets behind any write already
// inside xterm, and discard queued frames from streams that no longer own it.
const terminalWrites = new WeakMap();
function queueTerminalFrame(term, current, frame) {
  var queue = terminalWrites.get(term);
  if (!queue) {
    queue = { frames: [], pending: 0, scheduled: false };
    terminalWrites.set(term, queue);
  }
  queue.frames.push({ current: current, frame: frame });
  scheduleTerminalFrames(term, queue);
}

function scheduleTerminalFrames(term, queue) {
  if (queue.scheduled) return;
  queue.scheduled = true;
  queueMicrotask(function() { drainTerminalFrames(term, queue); });
}

function drainTerminalFrames(term, queue) {
  queue.scheduled = false;
  while (queue.frames.length) {
    var next = queue.frames[0];
    if (!next.current()) {
      queue.frames.shift();
      continue;
    }
    if (next.frame.type === 'reset') {
      if (queue.pending) return;
      queue.frames.shift();
      term.reset();
      if (next.frame.cols !== undefined) term.resize(next.frame.cols, next.frame.rows);
    } else {
      queue.frames.shift();
      if (!next.frame.length) continue;
      // xterm batches consecutive writes. Only resets need their callbacks to finish.
      queue.pending++;
      term.write(next.frame, function() {
        queue.pending--;
        if (!queue.pending && queue.frames.length) scheduleTerminalFrames(term, queue);
      });
    }
  }
}

// The xterm session: its lifecycle, the websocket, and the terminal theme.
export function kontoraTerminal() {
  return {
    // xterm's stylesheet used to sit in <head>, where it blocked the first
    // paint of every board for a terminal most visits never open. It loads
    // here instead, alongside the xterm modules.
    _loadTerminalCSS() {
      if (termState.cssLoad) return termState.cssLoad;
      termState.cssLoad = new Promise(function (resolve, reject) {
        var link = document.createElement('link');
        link.rel = 'stylesheet';
        link.href = '/vendor/xterm@5.5.0/xterm.css';
        link.onload = resolve;
        link.onerror = reject;
        document.head.appendChild(link);
      });
      return termState.cssLoad;
    },

    terminalConnected: false,
    terminalError: null,
    terminalSubjectKey: null,

    outputSubjectKey() {
      const subject = this.outputSubject();
      return subject ? (subject.kind === 'automation' ? 'automation:' + subject.id + ':' + subject.n : (subject.source || 'primary') + ':' + subject.id) : null;
    },

    terminalKeys: [
      { label: 'Esc', name: 'Escape', bytes: '\x1b' },
      { label: 'Tab', name: 'Tab', bytes: '\t' },
      { label: '⇤', name: 'Shift+Tab', bytes: '\x1b[Z' },
      { label: '↑', name: 'Up', bytes: '\x1b[A' },
      { label: '↓', name: 'Down', bytes: '\x1b[B' },
      { label: '→', name: 'Right', bytes: '\x1b[C' },
      { label: '←', name: 'Left', bytes: '\x1b[D' },
      { label: '^C', name: 'Ctrl+C', bytes: '\x03' },
      { label: '↵', name: 'Enter', bytes: '\r' },
    ],

    sendTerminalKey(bytes) {
      if (!this.terminalRW || !this.terminalConnected) return;
      termState.term?.input(bytes);
    },

    sendTerminalCompose(text) {
      if (!this.terminalRW || !this.terminalConnected || !termState.term || !termState.ws) return false;
      var clean = text.replace(/[\x00-\x08\x0b-\x1f\x7f-\x9f]/g, '');
      if (!clean.trim()) return false;
      var term = termState.term;
      var ws = termState.ws;
      term.paste(clean);
      setTimeout(() => {
        // A mode toggle keeps the Terminal but replaces its socket.
        if (termState.term === term && termState.ws === ws) this.sendTerminalKey('\r');
      }, 150);
      return true;
    },

    outputSubject() {
      if (this.currentView === 'automations') return this.automationsOutputSubject();
      var subject = this.selectedTicket || this.selectedEpic;
      return subject && !this.isSecondary(subject) && (subject.kind !== 'epic' || this.epicIsAgent(subject)) ? subject : null;
    },

    outputTab() {
      if (this.currentView === 'automations') return this.automationsTab;
      if (this.selectedEpic) return this.epicTab;
      return this.isMobile ? (this.detailTab === 'terminal' ? 'session' : this.detailTab) : this.activeTab;
    },

    outputContainerID(kind) {
      if (this.currentView === 'automations') return kind === 'terminal' ? 'automation-terminal-session' : 'automation-log-scroll';
      if (this.selectedEpic) return kind === 'terminal'
        ? (this.isMobile ? 'epic-terminal-mobile' : 'epic-terminal-session')
        : (this.isMobile ? 'epic-activity-scroll-mobile' : 'epic-activity-scroll');
      return kind === 'terminal' ? (this.isMobile ? 'terminal-container-mobile' : 'terminal-session') : 'activity-scroll';
    },

    outputNoRunText() {
      var t = this.outputSubject();
      if (!t) return 'This epic has no supervisor.';
      if (this.executionActive(t)) return 'Waiting for the supervisor output...';
      if (t.status === 'todo') return 'Supervisor queued. Output will appear when it starts.';
      if (t.last_error && !(t.history || []).length) return 'Supervisor failed before recording a run.';
      if ((t.history || []).length) return 'No live session. Choose a recorded run above.';
      return 'Supervisor has not recorded a run yet.';
    },

    async openTerminal() {
      if (!this.outputSubject() || !this.terminalWanted() || this.terminalOpen) return;
      var seq = ++this._terminalSeq;
      var ticketId = this.outputSubjectKey();
      this.terminalSubjectKey = ticketId;
      this.terminalError = null;
      this.terminalOpen = true;
      this._terminalOpening = true;
      try {
        if (!termState.Terminal || !termState.FitAddon) {
          var [termMod, fitMod, unicodeMod, webglMod] = await Promise.all([
            import('/vendor/xterm@5.5.0/xterm.mjs'),
            import('/vendor/addon-fit@0.10.0/addon-fit.mjs'),
            import('/vendor/addon-unicode11@0.8.0/addon-unicode11.mjs'),
            // Optional: a failed load means the terminal falls back to the DOM renderer.
            import('/vendor/addon-webgl@0.18.0/addon-webgl.mjs').catch(function(e) {
              console.warn('webgl addon failed to load, using DOM renderer:', e);
              return null;
            }),
            // Last on purpose, after the four entries destructured above: it
            // resolves to nothing and only has to finish before the first paint.
            this._loadTerminalCSS(),
          ]);
          termState.Terminal = termMod.Terminal;
          termState.FitAddon = fitMod.FitAddon;
          termState.Unicode11Addon = unicodeMod.Unicode11Addon;
          termState.WebglAddon = webglMod ? webglMod.WebglAddon : null;
        }
        await this.$nextTick();
        if (!this.terminalOpen || this._terminalSeq !== seq) return;
        if (!this.terminalWanted() || this.outputSubjectKey() !== ticketId) {
          this.closeTerminal();
          return;
        }
        this._connectTerminal(seq);
      } catch (e) {
        console.error('terminal load error:', e);
        if (this._terminalSeq !== seq) return;
        this.terminalError = 'Failed to load terminal';
        // _connectTerminal may already have attached the resize observer.
        this.closeTerminal();
      } finally {
        if (this._terminalSeq === seq) this._terminalOpening = false;
      }
    },

    reconnectTerminal() {
      if (!this.outputSubject() || !this.terminalWanted() || this._terminalOpening) return;
      if (this.terminalOpen) this.closeTerminal();
      this.openTerminal();
    },

    _connectTerminal(seq) {
      if (this._terminalSeq !== seq || !this.terminalOpen) return;
      // On phone width the live terminal attaches into the mobile detail's own
      // container; on desktop into the panel container (fullscreen keeps the
      // same container — the panel just grows to fill the viewport).
      var container = document.getElementById(this.outputContainerID('terminal'));
      // Returning with terminalOpen still set would strand the tab: switchTab
      // only refits a terminal it believes is open, so nothing would retry.
      if (!container) {
        this.closeTerminal();
        return;
      }

      // A terminal whose element sits somewhere else lost its layout layer when
      // the breakpoint changed, and cannot be reattached.
      if (termState.term && termState.term.element && termState.term.element.parentNode !== container) {
        this._destroyTerminal({ preserveInput: true });
      }
      var self = this;
      termState.paneReset = false;
      if (termState.term) {
        // The buffer still holds whatever the last stream wrote into it.
        var term = termState.term;
        queueTerminalFrame(term, function() {
          return self._terminalSeq === seq && self.terminalOpen && termState.term === term;
        }, { type: 'reset' });
      } else {
        this._createTerminal(container);
      }

      termState.resizeObserver = new ResizeObserver(function() {
        clearTimeout(termState.resizeTimer);
        termState.resizeTimer = setTimeout(function() { self.refitTerminal(); }, 100);
      });
      termState.resizeObserver.observe(container);

      requestAnimationFrame(function() {
        if (!termState.term || !self.terminalOpen || self._terminalSeq !== seq) return;
        if (!termState.paneReset) termState.fit.fit();
        self._connectWs(seq);
      });
    },

    _createTerminal(container) {
      container.textContent = '';
      var fit = new termState.FitAddon();
      var term = new termState.Terminal({
        theme: this._getTerminalTheme(),
        fontSize: 13,
        fontFamily: "'JetBrains Mono', monospace",
        cursorBlink: this.terminalRW,
        disableStdin: this.isMobile,
        // tmux uses the alternate screen; zero also removes FitAddon's scrollbar reserve.
        scrollback: this.isMobile ? 0 : 5000,
        allowProposedApi: true,
      });
      term.loadAddon(fit);
      term.loadAddon(new termState.Unicode11Addon());
      term.unicode.activeVersion = '11';
      term.open(container);
      // tmux answers supported queries; xterm replies would become agent input.
      // Unknown OSC 4 palette queries remain unanswered in control mode.
      var paneStream = () => this.outputSubject()?.kind !== 'automation';
      var swallow = function() { return paneStream(); };
      [
        { final: 'c' }, { prefix: '>', final: 'c' },
        { final: 'n' }, { prefix: '?', final: 'n' },
        { intermediates: '$', final: 'p' }, { prefix: '?', intermediates: '$', final: 'p' },
      ].forEach(function(id) { term.parser.registerCsiHandler(id, swallow); });
      term.parser.registerDcsHandler({ intermediates: '$', final: 'q' }, swallow);
      [4, 10, 11, 12].forEach(function(code) {
        term.parser.registerOscHandler(code, function(data) {
          if (!paneStream()) return false;
          var specs = data.split(';');
          if (code === 4) {
            if (!specs.length || specs.length % 2 !== 0) return false;
            specs = specs.filter(function(_, i) { return i % 2 === 1; });
          }
          return specs.every(function(spec) { return spec === '?'; });
        });
      });
      // Escape drops read-write mode instead of reaching the agent, and stops
      // there: the body Escape chain would otherwise see a read-only terminal
      // on the same keypress and close the ticket.
      var self = this;
      term.attachCustomKeyEventHandler(function(e) {
        if (e.type !== 'keydown' || e.key !== 'Escape' || !self.terminalRW) return true;
        e.preventDefault();
        e.stopPropagation();
        self.toggleTerminalRW();
        return false;
      });
      termState.term = term;
      termState.mobile = this.isMobile;
      this._setTerminalInput(false);
      termState.fit = fit;
      termState.webglRetried = false;
      this._loadWebgl();
    },

    // Must run after open(). Returns false when the DOM renderer stays active.
    _loadWebgl() {
      if (!termState.WebglAddon || !termState.term) return false;
      var self = this;
      try {
        var addon = new termState.WebglAddon();
        addon.onContextLoss(function() { self._onWebglContextLoss(addon); });
        termState.term.loadAddon(addon);
        termState.webgl = addon;
        return true;
      } catch (e) {
        console.warn('webgl renderer unavailable, using DOM renderer:', e);
        return false;
      }
    },

    // A lost context is recoverable, and the terminal outlives navigation, so
    // giving up on the first loss would pin the page to the DOM renderer for the
    // rest of its life. One retry only: retrying every loss would spin.
    _onWebglContextLoss(addon) {
      addon.dispose();
      if (termState.webgl !== addon) return;
      termState.webgl = null;
      if (termState.webglRetried || !this._loadWebgl()) {
        console.warn('webgl context lost, using DOM renderer');
        return;
      }
      termState.webglRetried = true;
    },

    _setTerminalInput(ready) {
      var term = termState.term;
      if (!term) return;
      var mobile = this.isMobile || termState.mobile;
      term.options.disableStdin = mobile && !ready;
      // A reused terminal keeps the cursor of whatever mode built it, and
      // terminalRW resets to false on ticket switch and detail close.
      term.options.cursorBlink = this.terminalRW && (!mobile || ready);
      if (term.textarea) {
        var readinessBlur = mobile && ready && term.textarea.readOnly && document.activeElement === term.textarea;
        if (mobile && (!ready || term.textarea.readOnly)) term.blur();
        term.textarea.readOnly = mobile && !ready;
        if (readinessBlur) this.mobileTerminalInputReady();
      }
    },

    _connectWs(seq) {
      if (this._terminalSeq !== seq || !this.terminalOpen || !termState.term) return;
      var streamSeq = ++termState.streamSeq;
      this.terminalConnected = false;
      this._setTerminalInput(false);
      if (termState.inputDisposable) {
        termState.inputDisposable.dispose();
        termState.inputDisposable = null;
      }
      if (!this.executionActive(this.outputSubject())) return;
      // Whoever asked for a stream gets exactly one. Without this, a read-write
      // toggle landing before _connectTerminal's requestAnimationFrame orphans
      // the socket it opened, and its kontora-view session outlives the page.
      if (termState.ws) {
        var previous = termState.ws;
        termState.ws = null;
        previous.close();
      }
      var self = this;
      var term = termState.term;
      var rw = this.terminalRW;
      // Received frames survive socket closure, but not a new stream or navigation.
      var ownsOutput = function() {
        return self._terminalSeq === seq && self.terminalOpen && termState.streamSeq === streamSeq && termState.term === term;
      };
      var current = function() {
        return ownsOutput() && termState.ws === ws;
      };
      var cols = term.cols;
      var rows = term.rows;
      var proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
      var subject = self.outputSubject();
      var subjectKey = self.outputSubjectKey();
      var paneStream = subject.kind !== 'automation';
      var path = paneStream ? '/ws/terminal/' + encodeURIComponent(subject.id) : '/ws/automations/' + encodeURIComponent(subject.id) + '/' + subject.n;
      var url = proto + '//' + location.host + path
        + '?cols=' + cols + '&rows=' + rows + (paneStream ? '&stream=pane' : '') + (rw ? '&rw=1' : '');
      var ws = new WebSocket(url);
      ws.binaryType = 'arraybuffer';
      termState.ws = ws;
      ws.onmessage = function(e) {
        if (!current() || self.outputSubjectKey() !== subjectKey) {
          ws.close();
          return;
        }
        if (typeof e.data === 'string') {
          var frame;
          try { frame = JSON.parse(e.data); } catch (err) { return; }
          if (!frame || frame.type !== 'reset' || !Number.isInteger(frame.cols) || frame.cols < 1
            || !Number.isInteger(frame.rows) || frame.rows < 1) return;
          termState.paneReset = true;
          queueTerminalFrame(term, ownsOutput, frame);
        } else {
          queueTerminalFrame(term, ownsOutput, new Uint8Array(e.data));
        }
      };
      ws.onopen = function() {
        if (!current() || ws.readyState !== WebSocket.OPEN) return;
        self.terminalConnected = true;
        self.terminalError = null;
        if (!paneStream) termState.fit.fit();
        // The pane server owns the grid; attach streams use the local terminal size.
        var dimensions = termState.fit.proposeDimensions();
        if (dimensions && (dimensions.cols !== cols || dimensions.rows !== rows)) {
          ws.send(JSON.stringify({ type: 'resize', cols: dimensions.cols, rows: dimensions.rows }));
        }
        // A socket callback cannot open the keyboard. Input starts with a later tap.
        self._setTerminalInput(rw && self.terminalRW);
      };
      var disconnected = function() {
        if (!current()) return;
        termState.ws = null;
        self.terminalConnected = false;
        self.terminalError = 'Terminal disconnected. Retry to reconnect.';
        if (self.isMobile || termState.mobile) {
          self.mobileResetTerminalInput();
          self.terminalRW = false;
        }
        self._setTerminalInput(false);
        if (termState.inputDisposable) {
          termState.inputDisposable.dispose();
          termState.inputDisposable = null;
        }
        ws.close();
      };
      ws.onclose = disconnected;
      ws.onerror = disconnected;
      if (rw) {
        termState.inputDisposable = term.onData(function(data) {
          if (current() && self.terminalRW && self.terminalConnected && ws.readyState === WebSocket.OPEN) {
            ws.send(JSON.stringify({ type: 'input', data: data }));
          }
        });
      }
    },

    // Ends the stream and its listeners but keeps the terminal, so returning to
    // it costs no Terminal construction and no new WebGL context. _terminalSeq is
    // left alone: cancelling an in-flight openTerminal is the caller's call.
    _disconnectStream({ preserveInput = false } = {}) {
      termState.streamSeq++;
      if (!preserveInput && (this.isMobile || termState.mobile)) {
        this.mobileResetTerminalInput();
        this.terminalRW = false;
      }
      this._setTerminalInput(false);
      this.terminalConnected = false;
      clearTimeout(termState.resizeTimer);
      termState.resizeTimer = null;
      if (termState.resizeObserver) {
        termState.resizeObserver.disconnect();
        termState.resizeObserver = null;
      }
      if (termState.inputDisposable) {
        termState.inputDisposable.dispose();
        termState.inputDisposable = null;
      }
      if (termState.ws) {
        var ws = termState.ws;
        termState.ws = null;
        ws.close();
      }
    },

    // Browsers cap live WebGL contexts, so this runs only when the container
    // itself is going away, including across the 768px breakpoint.
    _destroyTerminal(options = {}) {
      this._disconnectStream(options);
      if (termState.term) {
        var term = termState.term;
        // xterm 5.5 leaves viewport callbacks pending after open() and reset().
        requestAnimationFrame(function() {
          setTimeout(function() { try { term.dispose(); } catch (e) {} }, 0);
        });
      }
      termState.term = null;
      termState.fit = null;
      termState.webgl = null;
      termState.mobile = false;
    },

    closeTerminal() {
      this._terminalSeq++;
      this._terminalOpening = false;
      this._disconnectStream();
      this.terminalOpen = false;
      this.terminalConnected = false;
      this.terminalSubjectKey = null;
    },

    setTerminalRW(enabled) {
      if (!enabled && (this.isMobile || termState.mobile)) this.mobileResetTerminalInput();
      if (this.terminalRW === enabled) return;
      this.terminalRW = enabled;
      if (!this.terminalOpen || !termState.term) return;
      // Read-only is a flag on the tmux attach, so the mode change needs a fresh
      // stream. The terminal and its WebGL context stay.
      this._connectWs(this._terminalSeq);
    },

    toggleTerminalRW() {
      this.setTerminalRW(!this.terminalRW);
    },

    refitTerminal() {
      if (!termState.term || !termState.fit || !this.terminalOpen) return;
      var el = termState.term.element;
      if (!el || !el.clientWidth || !el.clientHeight) return;
      var dimensions = termState.fit.proposeDimensions();
      if (!dimensions || (dimensions.cols === termState.term.cols && dimensions.rows === termState.term.rows)) return;
      if (this.outputSubject()?.kind === 'automation') termState.fit.fit();
      if (termState.ws && termState.ws.readyState === WebSocket.OPEN) {
        termState.ws.send(JSON.stringify({ type: 'resize', cols: dimensions.cols, rows: dimensions.rows }));
      }
    },

    _getTerminalTheme() {
      var s = getComputedStyle(document.documentElement);
      return { background: this._cssVar('--surface-deep', s), foreground: this._cssVar('--tx', s), cursor: this._cssVar('--accent', s), selectionBackground: this._cssVar('--surface-700', s) };
    },

    _applyTerminalTheme() {
      if (!termState.term) return;
      termState.term.options.theme = this._getTerminalTheme();
    },
  };
}
