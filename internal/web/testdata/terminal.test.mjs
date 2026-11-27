import assert from 'node:assert/strict';
import test from 'node:test';
import { kontoraTerminal, termState } from '../ui/terminal.js';

const input = data => ({ type: 'input', data });
const paste = text => input('\x1b[200~' + text + '\x1b[201~');
const enter = input('\r');

function terminal(t) {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  class Socket {
    static OPEN = 1;
    readyState = 0;
    sent = [];
    open() { this.readyState = Socket.OPEN; this.onopen(); }
    send(frame) {
      if (this.failWrite) throw new Error('socket write failed');
      this.sent.push(JSON.parse(frame));
    }
    close() { this.readyState = 3; }
  }
  const oldSocket = globalThis.WebSocket;
  const oldLocation = globalThis.location;
  globalThis.WebSocket = Socket;
  globalThis.location = { protocol: 'http:', host: '127.0.0.1' };
  t.after(() => {
    globalThis.WebSocket = oldSocket;
    globalThis.location = oldLocation;
    Object.assign(termState, { term: null, ws: null, fit: null, inputDisposable: null });
  });

  const listeners = new Set();
  const term = {
    cols: 80, rows: 24, options: {}, modes: { bracketedPasteMode: true },
    onData(cb) { listeners.add(cb); return { dispose: () => listeners.delete(cb) }; },
    input(data) { if (!this.options.disableStdin) listeners.forEach(cb => cb(data)); },
    paste(text) {
      const data = text.replace(/\r?\n/g, '\r');
      this.input(this.modes.bracketedPasteMode && !this.options.ignoreBracketedPasteMode ? paste(data).data : data);
    },
  };
  Object.assign(termState, { term, fit: { proposeDimensions: () => null } });
  const state = Object.assign(kontoraTerminal(), {
    isMobile: true, currentView: 'board', detailTab: 'terminal',
    selectedTicket: { id: 'live', status: 'in_progress', waiting_question: 'Which branch?' },
    _terminalSeq: 1, _ticketDetailSeq: 1, _navigationSeq: 1,
    terminalOpen: true, terminalRW: true, mobileTerminalInputTarget: 'composer',
    isSecondary: subject => subject?.source === 'remote',
    executionActive: subject => subject?.status === 'in_progress',
    terminalWanted() { return this.outputTab() === 'session' && this.executionActive(this.outputSubject()); },
    mobileResetTerminalInput() { this.mobileTerminalInputTarget = null; },
  });
  const subjectKey = state.terminalSubjectKey = state.outputSubjectKey();
  const question = state.selectedTicket.waiting_question;
  const guard = { subjectKey, isCurrent: () => state.selectedTicket?.waiting_question === question
    && state.mobileTerminalInputTarget === 'composer' };
  state._connectWs(state._terminalSeq);
  const ws = termState.ws;
  ws.open();
  return { state, ws, term, guard, send: (text = 'answer') => state.sendGuardedTerminalCompose(text, guard),
    finish: () => t.mock.timers.tick(150) };
}

test('guarded compose counts paste and deferred Enter, excluding duplicate submits', async t => {
  const h = terminal(t);
  const reply = h.send('hello\x1b\x00\r\nworld');
  assert.deepEqual(h.ws.sent, [paste('hello\rworld')]);
  assert.equal(h.state.terminalGuardedComposePending, true);
  assert.equal(await h.send(), 'busy');
  assert.equal(h.state.sendTerminalCompose('duplicate'), false);
  t.mock.timers.tick(149);
  assert.deepEqual(h.ws.sent, [paste('hello\rworld')]);
  t.mock.timers.tick(1);
  assert.equal(await reply, 'sent');
  assert.deepEqual(h.ws.sent, [paste('hello\rworld'), enter]);
  assert.equal(h.state.terminalGuardedComposePending, false);
});

for (const [cause, separator, bracketedPasteMode, ignoreBracketedPasteMode] of [
  ['mode off with LF', '\n', false, false],
  ['mode off with CR', '\r', false, false],
  ['mode overridden with CRLF', '\r\n', true, true],
  ['mode off with Unicode line separator', '\u2028', false, false],
]) {
  test(`guarded compose rejects unbracketed multiline drafts without sending input: ${cause}`, async t => {
    const h = terminal(t);
    h.term.modes.bracketedPasteMode = bracketedPasteMode;
    h.term.options.ignoreBracketedPasteMode = ignoreBracketedPasteMode;
    const reply = h.send(`hello${separator}world`);
    assert.deepEqual(h.ws.sent, []);
    h.finish();
    assert.equal(await reply, 'multiline-unavailable');
    assert.deepEqual(h.ws.sent, []);
    assert.equal(h.state.terminalGuardedComposePending, false);
  });
}

test('guarded compose can send a single line with bracketed paste off', async t => {
  const h = terminal(t);
  h.term.modes.bracketedPasteMode = false;
  const reply = h.send('an\x0b\x0c\x0f\x85swer');
  assert.deepEqual(h.ws.sent, [input('answer')]);
  h.finish();
  assert.equal(await reply, 'sent');
  assert.deepEqual(h.ws.sent, [input('answer'), enter]);
});

test('guarded compose returns busy while ordinary Enter is pending', async t => {
  const h = terminal(t);
  assert.equal(h.state.sendTerminalCompose('ordinary'), true);
  assert.equal(await h.send(), 'busy');
  h.finish();
  assert.deepEqual(h.ws.sent, [paste('ordinary'), enter]);
  const reply = h.send();
  h.finish();
  assert.equal(await reply, 'sent');
  assert.deepEqual(h.ws.sent, [paste('ordinary'), enter, paste('answer'), enter]);
});

for (const [cause, invalidate] of [
  ['stale question', h => { h.state.selectedTicket.waiting_question = 'Which project?'; }],
  ['stale subject', h => { h.state.selectedTicket = { ...h.state.selectedTicket, id: 'other' }; }],
  ['input owner changed', h => { h.state.mobileTerminalInputTarget = 'terminal'; }],
  ['socket not open', h => { h.ws.readyState = 0; }],
  ['read-only', h => { h.state.terminalRW = false; }],
  ['paste callback missing', h => { h.term.paste = () => {}; }],
  ['paste write failed', h => { h.ws.failWrite = true; }],
]) {
  test(`guarded compose never schedules Enter without a confirmed paste: ${cause}`, async t => {
    const h = terminal(t);
    invalidate(h);
    assert.equal(await h.send(), 'blocked');
    h.finish();
    assert.deepEqual(h.ws.sent, []);
    assert.equal(h.state.terminalGuardedComposePending, false);
  });
}

for (const [cause, invalidate] of [
  ['question changed', h => { h.state.selectedTicket.waiting_question = 'Which project?'; }],
  ['navigation superseded', h => { h.state._navigationSeq++; }],
  ['detail reopened', h => { h.state._ticketDetailSeq++; }],
  ['subject changed', h => { h.state.selectedTicket = { ...h.state.selectedTicket, id: 'other' }; }],
  ['input owner changed', h => { h.state.mobileTerminalInputTarget = 'terminal'; }],
  ['socket replaced', h => { h.state._connectWs(h.state._terminalSeq); termState.ws.open(); }],
  ['socket disconnected', h => h.ws.onclose()],
  ['Enter callback missing', h => { h.term.input = () => {}; }],
  ['Enter write failed', h => { h.ws.failWrite = true; }],
]) {
  test(`guarded compose reports a partial send without replay: ${cause}`, async t => {
    const h = terminal(t);
    const reply = h.send();
    assert.deepEqual(h.ws.sent, [paste('answer')]);
    invalidate(h);
    h.finish();
    assert.equal(await reply, 'partial');
    assert.deepEqual(h.ws.sent, [paste('answer')]);
    if (termState.ws && termState.ws !== h.ws) assert.deepEqual(termState.ws.sent, []);
    assert.equal(h.state.terminalGuardedComposePending, false);
  });
}

test('guarded compose checks the question again at the actual write', async t => {
  const h = terminal(t);
  const emitPaste = h.term.paste.bind(h.term);
  h.term.paste = text => {
    h.state.selectedTicket.waiting_question = 'Which project?';
    emitPaste(text);
  };
  assert.equal(await h.send(), 'blocked');
  assert.deepEqual(h.ws.sent, []);
});
