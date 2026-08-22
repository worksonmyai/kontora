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
};

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

    async openTerminal() {
      if (!this.selectedTicket || this.terminalOpen) return;
      var seq = ++this._terminalSeq;
      var ticketId = this.selectedTicket.id;
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
        if (!this.terminalWanted() || this.selectedTicket?.id !== ticketId) {
          this.closeTerminal();
          return;
        }
        this._connectTerminal(seq);
      } catch (e) {
        console.error('terminal load error:', e);
        this.error = 'Failed to load terminal';
        // _connectTerminal may already have attached the resize observer.
        this.closeTerminal();
      } finally {
        if (this._terminalSeq === seq) this._terminalOpening = false;
      }
    },

    reconnectTerminal() {
      if (!this.selectedTicket || !this.terminalWanted() || this._terminalOpening) return;
      if (this.terminalOpen) this.closeTerminal();
      this.openTerminal();
    },

    _connectTerminal(seq) {
      if (this._terminalSeq !== seq || !this.terminalOpen) return;
      // On phone width the live terminal attaches into the mobile detail's own
      // container; on desktop into the panel container (fullscreen keeps the
      // same container — the panel just grows to fill the viewport).
      var container = document.getElementById(this.isMobile ? 'terminal-container-mobile' : 'terminal-session');
      // Returning with terminalOpen still set would strand the tab: switchTab
      // only refits a terminal it believes is open, so nothing would retry.
      if (!container) {
        this.closeTerminal();
        return;
      }

      // A terminal whose element sits somewhere else lost its layout layer when
      // the breakpoint changed, and cannot be reattached.
      if (termState.term && termState.term.element && termState.term.element.parentNode !== container) {
        this._destroyTerminal();
      }
      if (termState.term) {
        // The buffer still holds whatever the last stream wrote into it.
        termState.term.reset();
      } else {
        this._createTerminal(container);
      }

      var self = this;
      termState.resizeObserver = new ResizeObserver(function() {
        clearTimeout(termState.resizeTimer);
        termState.resizeTimer = setTimeout(function() { self.refitTerminal(); }, 100);
      });
      termState.resizeObserver.observe(container);

      requestAnimationFrame(function() {
        if (!termState.term || !self.terminalOpen || self._terminalSeq !== seq) return;
        termState.fit.fit();
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
        disableStdin: false,
        scrollback: 5000,
        allowProposedApi: true,
      });
      term.loadAddon(fit);
      term.loadAddon(new termState.Unicode11Addon());
      term.unicode.activeVersion = '11';
      term.open(container);
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

    _connectWs(seq) {
      if (termState.inputDisposable) {
        termState.inputDisposable.dispose();
        termState.inputDisposable = null;
      }
      if (this._terminalSeq !== seq || !termState.term) return;
      // Whoever asked for a stream gets exactly one. Without this, a read-write
      // toggle landing before _connectTerminal's requestAnimationFrame orphans
      // the socket it opened, and its kontora-view session outlives the page.
      if (termState.ws) {
        termState.ws.close();
        termState.ws = null;
      }
      var self = this;
      var term = termState.term;
      // A reused terminal keeps the cursor of whatever mode built it, and
      // terminalRW resets to false on ticket switch and detail close.
      term.options.cursorBlink = this.terminalRW;
      var proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
      var url = proto + '//' + location.host + '/ws/terminal/' + self.selectedTicket.id
        + '?cols=' + term.cols + '&rows=' + term.rows + (self.terminalRW ? '&rw=1' : '');
      var ws = new WebSocket(url);
      ws.binaryType = 'arraybuffer';
      termState.ws = ws;
      ws.onmessage = function(e) {
        if (self._terminalSeq !== seq) {
          ws.close();
          return;
        }
        term.write(new Uint8Array(e.data));
      };
      ws.onclose = function() { if (termState.ws === ws) termState.ws = null; };
      ws.onerror = function() { if (termState.ws === ws) termState.ws = null; };
      if (self.terminalRW) {
        termState.inputDisposable = term.onData(function(data) {
          if (self._terminalSeq === seq && ws.readyState === WebSocket.OPEN) {
            ws.send(JSON.stringify({ type: 'input', data: data }));
          }
        });
      }
    },

    // Ends the stream and its listeners but keeps the terminal, so returning to
    // it costs no Terminal construction and no new WebGL context. _terminalSeq is
    // left alone: cancelling an in-flight openTerminal is the caller's call.
    _disconnectStream() {
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
        termState.ws.close();
        termState.ws = null;
      }
    },

    // Browsers cap live WebGL contexts, so this runs only when the container
    // itself is going away: crossing the 768px breakpoint.
    _destroyTerminal() {
      this._disconnectStream();
      if (termState.term) {
        try { termState.term.dispose(); } catch (e) {}
      }
      termState.term = null;
      termState.fit = null;
      termState.webgl = null;
    },

    closeTerminal() {
      this._terminalSeq++;
      this._terminalOpening = false;
      this._disconnectStream();
      this.terminalOpen = false;
    },

    toggleTerminalRW() {
      this.terminalRW = !this.terminalRW;
      if (!this.terminalOpen || !termState.term) return;
      // Read-only is a flag on the tmux attach, so the mode change needs a fresh
      // stream. The terminal and its WebGL context stay.
      this._connectWs(this._terminalSeq);
    },

    refitTerminal() {
      if (!termState.term || !termState.fit || !this.terminalOpen) return;
      // A container hidden by x-show measures zero, and fitting to that would
      // reflow the whole scrollback twice: once down to nothing, once on return.
      var el = termState.term.element;
      if (!el || !el.clientWidth || !el.clientHeight) return;
      var oldCols = termState.term.cols;
      var oldRows = termState.term.rows;
      termState.fit.fit();
      if (termState.term.cols === oldCols && termState.term.rows === oldRows) return;
      if (termState.ws && termState.ws.readyState === WebSocket.OPEN) {
        termState.ws.send(JSON.stringify({ type: 'resize', cols: termState.term.cols, rows: termState.term.rows }));
      }
      // Clear viewport to remove reflow artifacts from cursor-positioned content.
      // tmux will redraw the screen after receiving the resize via SIGWINCH.
      termState.term.write('\x1b[2J\x1b[H');
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
