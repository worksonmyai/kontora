import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';
import { kontoraArchive, archiveDerive } from '../ui/archive.js';
import { kontoraAutomations } from '../ui/automations.js';
import { cardMenuPlacement, kontoraBoard } from '../ui/board.js';
import { kontoraDetail } from '../ui/detail.js';
import { kontoraDetailView } from '../ui/detail_view.js';
import { kontoraDialogs } from '../ui/dialogs.js';
import { kontoraMobile } from '../ui/mobile.js';
import { kontoraPalette } from '../ui/palette.js';
import { kontoraTickets } from '../ui/tickets.js';

const bundle = process.env.KONTORA_BUNDLE && readFileSync(process.env.KONTORA_BUNDLE, 'utf8');

function target(properties = {}) {
  const listeners = new Map();
  return Object.assign({
    listeners,
    addEventListener(type, fn) { if (!listeners.has(type)) listeners.set(type, new Set()); listeners.get(type).add(fn); },
    removeEventListener(type, fn) { listeners.get(type)?.delete(fn); },
    emit(type, event = {}) {
      for (const fn of listeners.get(type) || []) {
        fn(event);
        if (event.stopped) break;
      }
    },
  }, properties);
}

function fixture() {
  const bySelector = new Map(), byId = new Map(), frames = new Map(), watches = new Map(), observers = new Set(), ticks = [];
  const doc = target({
    querySelector: selector => bySelector.get(selector)?.[0] || null,
    querySelectorAll: selector => bySelector.get(selector) || [],
    getElementById: id => byId.get(id) || null,
    documentElement: { style: { setProperty() {}, removeProperty() {}, getPropertyValue() { return ''; }, getPropertyPriority() { return ''; } } },
  });
  const node = (parentElement = null, attrs = {}) => {
    const children = [];
    const classes = new Set();
    const el = target({
      parentElement, children, classes, style: {}, attrs: new Map(Object.entries(attrs)),
      isConnected: true, tabIndex: 0, shown: true, focused: 0, scrollTop: 0,
      clientLeft: 0, clientTop: 0, clientWidth: 0, clientHeight: 0, scrollHeight: 0, offsetHeight: 0,
      classList: { add: cls => classes.add(cls), remove: cls => classes.delete(cls) },
      getClientRects() { return this.isConnected && this.shown && (!this.parentElement || this.parentElement.getClientRects().length) ? [1] : []; },
      getBoundingClientRect() { return this.rect; },
      querySelector: selector => bySelector.get(selector)?.[0] || null,
      querySelectorAll: () => children,
      contains(other) { return !!other && (other === this || this.contains(other.parentElement)); },
      getAttribute(name) { return this.attrs.get(name) ?? null; },
      hasAttribute(name) { return this.attrs.has(name); },
      setAttribute(name, value) { this.attrs.set(name, value); },
      removeAttribute(name) { this.attrs.delete(name); },
      matches(selector) { return selector === ':disabled' && !!this.disabled; },
      closest(selector) {
        const candidates = selector.split(',').map(s => s.trim());
        if (candidates.some(candidate => candidate === '[aria-hidden="true"]' ? this.getAttribute('aria-hidden') === 'true'
          : candidate.startsWith('[') ? this.hasAttribute(candidate.slice(1, -1)) : this.classes.has(candidate.slice(1)))) return this;
        return this.parentElement?.closest(selector) || null;
      },
      focus() { this.focused++; doc.activeElement = this; doc.emit('focusin', { target: this }); },
      remove() { this.isConnected = false; },
      appendChild(child) { children.push(child); child.parentElement = this; },
    });
    parentElement?.appendChild(el);
    return el;
  };
  doc.body = node();
  doc.activeElement = doc.body;
  const win = target({ innerWidth: 1200, innerHeight: 800, visualViewport: target({ width: 1200, height: 800 }) });
  const location = win.location = { hash: '#/s/project%2Fx/t/a?case=untouched', assign(url) { this.assigned = url; } };
  let nextFrame = 0;
  const globals = { document: doc, window: win, location, URL, URLSearchParams,
    requestAnimationFrame(fn) { const id = ++nextFrame; frames.set(id, fn); return id; },
    cancelAnimationFrame: id => frames.delete(id), getComputedStyle: el => ({ visibility: 'visible', ...el.style }),
    ResizeObserver: class {
      constructor(callback) { this.callback = callback; observers.add(this); }
      observe() {}
      disconnect() { observers.delete(this); }
    },
    navigator: { platform: 'Win32' }, localStorage: { getItem() { return null; } }, getStoredTheme: () => 'dark',
  };
  const { navigator: _, ...nodeGlobals } = globals;
  Object.assign(globalThis, nodeGlobals);
  const state = bundle ? vm.runInNewContext(bundle + '\nkontora();', globals)
    : [kontoraDialogs(), kontoraMobile(), kontoraDetailView(), kontoraBoard(), kontoraTickets(), kontoraPalette(), kontoraAutomations(), kontoraDetail(), kontoraArchive()]
      .reduce((app, mixin) => Object.defineProperties(app, Object.getOwnPropertyDescriptors(mixin)), {});
  const set = (key, value) => { state[key] = value; watches.get(key)?.(); };
  const flush = () => { while (ticks.length) ticks.shift()(); };
  Object.assign(state, { outputSubject: () => null, flushEditSave() {}, mobileResetTerminalInput() {},
    $watch(key, fn) { watches.set(key, fn); return () => watches.delete(key); },
    $nextTick(fn) { if (fn) ticks.push(fn); return Promise.resolve(); },
    currentView: 'board', isMobile: false, closeAgentPicker() {},
    $refs: {},
  });
  const chrome = node(doc.body), main = node(doc.body), opener = node(main);
  bySelector.set('[data-mobile-sheet-background]', [chrome, main]);
  bySelector.set('[data-mobile-board-background]', [chrome]);
  const layers = [];
  bySelector.set('[data-dialog-layer]', layers);
  const root = name => {
    const layer = node(doc.body, { 'data-dialog-layer': name });
    layers.push(layer);
    const panel = node(layer);
    panel.tabIndex = -1;
    const a = node(panel, { 'data-dialog-initial': '' }), b = node(panel);
    panel.querySelector = selector => selector === '[data-dialog-initial]' ? a : byId.get(selector.slice(1));
    bySelector.set(`[data-dialog-layer="${name}"] [data-dialog-panel]`, [panel]);
    return { layer, panel, a, b };
  };
  const key = (key, options = {}) => {
    const event = { key, target: doc.activeElement, ...options,
      preventDefault() { this.defaultPrevented = true; }, stopImmediatePropagation() { this.stopped = true; },
      stopPropagation() { this.stopped = true; },
    };
    doc.emit('keydown', event);
    return event;
  };
  return { state, doc, win, location, node, root, bySelector, byId, frames, watches, observers, flush, key, set, chrome, main, opener };
}

for (const [name, property] of [
  ['init', 'initModal'], ['delete', 'deleteModal'], ['note-delete', 'noteDeleteTarget'], ['settings', 'settingsGuard'],
  ['archive', 'archivePromptId'], ['automation-delete', 'automationsDeleteOpen'], ['auth', 'needsAuth'], ['palette', 'paletteOpen'],
]) {
  test(`${name} owns focus, traps both Tab directions and guards focus return`, () => {
    const f = fixture();
    const { state, opener, main, doc, key } = f;
    const { panel, a, b, layer } = f.root(name);
    state.initDialogs(); f.flush();
    opener.focus();
    f.set(property, true); f.flush();
    assert.equal(doc.activeElement, a);
    assert.equal(main.inert, true);
    assert.equal(layer.inert, false);
    assert.equal(key('Tab', { shiftKey: true }).defaultPrevented, true);
    assert.equal(doc.activeElement, b);
    key('Tab');
    assert.equal(doc.activeElement, a);
    opener.focus();
    assert.equal(doc.activeElement, a, 'escaped focus is redirected');
    a.disabled = b.disabled = true;
    assert.equal(key('Tab').defaultPrevented, true);
    assert.equal(doc.activeElement, panel);
    f.set(property, false); f.flush();
    assert.equal(layer.inert, true, 'a layer in exit transition remains inert');
    assert.equal(main.inert, false);
    assert.equal(doc.activeElement, opener);
    state.destroyDialogs();
    assert.equal(layer.inert, false);
    assert.equal(f.watches.size, 0);
    assert.equal(doc.listeners.get('keydown').size, 0);
    assert.equal(doc.listeners.get('focusin').size, 0);
  });
}

test('authentication preempts pending confirmation and shortcuts; Escape respects the write owner', () => {
  const f = fixture();
  const { state, win, doc, opener } = f;
  const archive = f.root('archive'), auth = f.root('auth');
  let shortcut = 0;
  state.togglePalette = state.toggleAssistant = () => ++shortcut;
  state.initDialogs(); state._bindGlobalEvents(); f.flush(); opener.focus();
  f.set('archivePromptId', 'test'); f.flush();
  state.archiveSubmitting = true;
  assert.equal(f.key('Escape').stopped, true);
  assert.equal(state.archivePromptId, 'test');
  f.set('needsAuth', true); f.flush();
  assert.equal(doc.activeElement, auth.a);
  assert.equal(archive.layer.inert, true);
  f.key('Escape');
  assert.equal(state.needsAuth, true);
  assert.equal(state.archivePromptId, 'test');
  for (const key of ['k', 'j']) win.emit('keydown', { key, ctrlKey: true, preventDefault() {}, stopPropagation() {} });
  assert.equal(shortcut, 0);
  f.set('needsAuth', false); f.flush();
  assert.equal(doc.activeElement, archive.a);
  state.archiveSubmitting = false;
  f.key('Escape');
  assert.equal(state.archivePromptId, null);
  state.destroyDialogs(); state._disposeBoardEvents();
});

test('sheet and detail coverage compose with dialogs and pending detail focus cannot steal the dialog', () => {
  const f = fixture();
  const { state, doc, win, chrome, main, opener, node } = f;
  const dialog = f.root('settings');
  const detail = node(main); detail.classes.add('mobile-detail');
  const detailButton = node(detail);
  detail.querySelector = () => detailButton;
  const sheetRoot = node(doc.body), sheetPanel = node(sheetRoot), sheetButton = node(sheetPanel, { 'data-mobile-sheet-initial-focus': '' });
  f.bySelector.set('.mobile-detail', [detail]);
  f.bySelector.set('.mobile-sheet-viewport', [sheetRoot]);
  f.bySelector.set('[data-mobile-sheet-dialog]', [sheetPanel]);
  win.innerWidth = 440;
  state.ticketKey = t => t?.id;
  state.initDialogs(); state.initMobile(); f.flush();
  opener.focus();
  state.selectedTicket = { id: 'one' };
  state.syncDetailCoverage();
  assert.equal(chrome.inert, true);
  assert.notEqual(main.inert, true, 'main contains the detail');
  state.openActionsSheet(state.selectedTicket); f.flush();
  assert.equal(doc.activeElement, sheetButton);
  assert.equal(main.inert, true);
  f.set('settingsGuard', true); f.flush();
  assert.equal(sheetRoot.inert, true);
  assert.equal(doc.activeElement, dialog.a);
  state.closeSheet();
  assert.equal(main.inert, true, 'the blocking dialog still owns coverage');
  assert.equal(doc.activeElement, dialog.a);
  sheetRoot.isConnected = sheetPanel.isConnected = sheetButton.isConnected = false;
  f.set('settingsGuard', false); f.flush();
  assert.equal(main.inert, false);
  assert.equal(chrome.inert, true, 'the detail still owns board coverage');
  assert.equal(doc.activeElement, detailButton);
  state.selectedTicket = null; state.syncDetailCoverage(); f.flush();
  assert.equal(chrome.inert, false);
  assert.equal(doc.activeElement, opener);
  state.destroyMobile(); state.destroyDialogs();
});

test('init picker Escape returns to its trigger and Tab advances within dialog ancestry', () => {
  const f = fixture();
  const { state, doc, node, key } = f;
  const { panel, a, b } = f.root('init');
  const picker = node(panel, { 'data-agent-picker-panel': '' });
  const pickerInput = node(picker);
  picker.tabIndex = -1;
  f.byId.set('init-pipeline', a);
  state.agentPickerOwnsTarget = el => picker.contains(el) || el === a;
  state.agentPickerTriggerID = () => 'init-pipeline';
  state.closeAgentPicker = restore => { state._agentPickerOpenHost = null; if (restore) a.focus(); };
  state.initDialogs(); f.set('initModal', true); f.flush();
  state._agentPickerOpenHost = 'init'; pickerInput.focus();
  assert.equal(doc.activeElement, pickerInput);
  key('Escape');
  assert.equal(doc.activeElement, a);
  assert.equal(state.initModal, true);
  state._agentPickerOpenHost = 'init'; pickerInput.focus();
  assert.equal(key('Tab').defaultPrevented, true);
  assert.equal(doc.activeElement, b);
  state.destroyDialogs();
});

test('closing does not refocus a retired origin or override a destination that already took focus', () => {
  const f = fixture();
  const { state, doc, node, main, opener } = f;
  f.root('delete');
  const destination = node(main);
  state.initDialogs(); f.flush(); opener.focus();
  f.set('deleteModal', true); f.flush();
  opener.isConnected = false;
  f.set('deleteModal', false);
  destination.focus(); f.flush();
  assert.equal(doc.activeElement, destination);
  assert.equal(opener.focused, 1);
  state.destroyDialogs();
});

test('destroy cancels deferred dialog focus and preserves preexisting coverage', () => {
  const f = fixture();
  const { state, frames, main, chrome } = f;
  const root = f.root('delete'); root.layer.shown = false;
  chrome.setAttribute('inert', ''); chrome.setAttribute('aria-hidden', 'false');
  state.initDialogs(); f.set('deleteModal', true); f.flush();
  assert.equal(frames.size, 1);
  const pending = [...frames.values()][0];
  state.destroyDialogs();
  assert.equal(frames.size, 0);
  pending(); f.flush();
  assert.equal(root.a.focused, 0);
  assert.equal(main.inert, false);
  assert.equal(chrome.hasAttribute('inert'), true);
  assert.equal(chrome.getAttribute('aria-hidden'), 'false');
});

test('palette composition confirmation is not cancelled or dispatched; ordinary and modified Enter work', () => {
  const { state } = fixture();
  const selected = { kind: 'ticket' }, calls = [];
  state._paletteRows = [selected]; state.paletteSel = 0;
  state.paletteRun = (...args) => calls.push(args);
  for (const event of [{ isComposing: true }, { keyCode: 229 }]) {
    state.paletteEnter({ ...event, preventDefault() { assert.fail('composition was cancelled'); } });
  }
  let prevented = 0;
  for (const [metaKey, ctrlKey] of [[false, false], [true, false], [false, true]]) {
    state.paletteEnter({ metaKey, ctrlKey, preventDefault() { prevented++; } });
  }
  assert.deepEqual(calls, [[selected, false], [selected, true], [selected, true]]);
  assert.equal(prevented, 3);
});

test('palette Tab uses the dialog trap when a composition key cannot drill', () => {
  const f = fixture();
  const { state, doc } = f;
  const { a: input, b: hidden } = f.root('palette');
  hidden.shown = false;
  state.$refs.paletteInput = input;
  state._paletteRows = [{ kind: 'ticket', ticketId: 'one' }];
  state.paletteSel = 0;
  const drills = [];
  state.palettePush = id => drills.push(id);
  state.initDialogs(); f.set('paletteOpen', true); f.flush();
  for (const flags of [{ isComposing: true }, { keyCode: 229 }, { shiftKey: true }]) {
    assert.equal(f.key('Tab', flags).defaultPrevented, true);
    assert.equal(doc.activeElement, input);
  }
  assert.deepEqual(drills, []);
  assert.equal(f.key('Tab').defaultPrevented, true);
  assert.deepEqual(drills, ['one']);
  state.destroyDialogs();
});

test('token submission uses the actual deep-link hash without applying the route', () => {
  const { state, location } = fixture();
  state.tokenInput = '  token&#+  ';
  state.currentView = 'board';
  state.submitToken();
  assert.equal(location.assigned, '/?token=token%26%23%2B' + location.hash);
});

test('identical raw Archive chips deduplicate, while distinct same-key terms retain OR', () => {
  const rows = ['pi', 'codex', 'other'].map(agent => ({ id: agent, agent }));
  const state = { query: 'agent:=pi agent:=pi Agent:=pi agent:code', range: 'all', status: 'all', project: 'all', pipeline: 'all', agent: 'all', sortKey: 'id', sortDir: 'asc' };
  const result = archiveDerive(rows, state);
  assert.deepEqual(result.tokens.map(t => t.term), ['agent:=pi', 'Agent:=pi', 'agent:code']);
  assert.deepEqual(result.rows.map(r => r.id), ['codex', 'pi']);
});

test('board menu flips, clamps horizontal position, and caps oversized menus to the larger side', () => {
  const bounds = { left: 40, top: 100, right: 170, bottom: 300 };
  const low = { left: 45, right: 70, top: 250, bottom: 275 };
  assert.deepEqual(cardMenuPlacement(low, { width: 180, height: 180 }, bounds),
    { left: 40, top: 100, flip: true, maxWidth: 130, maxHeight: 146 });
  const high = { ...low, top: 105, bottom: 130 };
  assert.deepEqual(cardMenuPlacement(high, { width: 60, height: 45 }, bounds),
    { left: 40, top: 134, flip: false, maxWidth: 130, maxHeight: 166 });
});

test('delegated board action captures its detached menu origin and retires placement on action/remount', async () => {
  const f = fixture();
  const { state, doc, win, node, frames, observers } = f;
  const archive = f.root('archive');
  const root = node(doc.body), scroll = node(root), card = node(scroll), wrapper = node(card), kebab = node(wrapper);
  root.classes.add('board-root'); card.classes.add('kt-card'); kebab.classes.add('card-menu-btn');
  card.dataset = { ticketId: 'one' };
  card.querySelector = () => kebab;
  scroll.style.overflowY = 'auto'; scroll.clientWidth = scroll.clientHeight = 300;
  scroll.rect = { top: 10, left: 10 };
  wrapper.rect = kebab.rect = { top: 200, bottom: 224, left: 220, right: 244 };
  let menu;
  const ticket = { id: 'one' }, command = { kind: 'archive' };
  Object.assign(state, {
    lifecycleResolve: () => ticket, lifecycleCommand: () => command, ticketIdentity: id => id,
    ticketMoves: () => [], isSecondary: () => false, renderBoard() {}, _cardMenuHTML() {},
    _nodeFromHTML() { menu = node(); menu.classes.add('card-menu'); menu.rect = { width: 160, height: 110 }; return menu; },
  });
  f.byId.set('board-cols', root);
  let finish;
  state.dispatchLifecycle = async id => {
    await new Promise(resolve => { finish = resolve; });
    state.openArchivePrompt(id);
  };
  state.initDialogs(); f.flush(); state._mountBoard();
  const click = target => root.emit('click', { target, stopPropagation() {} });
  click(kebab);
  assert.equal(card.classes.has('menu-open'), true);
  assert.equal(menu.parentElement, wrapper);
  assert.equal(menu.style.top, '-114px');
  assert.equal(scroll.listeners.get('scroll').size, 1);
  const [observer] = observers;
  assert.ok(observer);
  scroll.emit('scroll'); win.emit('resize');
  assert.equal(frames.size, 1);
  const action = node(menu); action.classes.add('card-menu-item'); action.dataset = { act: 'archive' };
  action.focus(); click(action);
  assert.equal(menu.isConnected, false);
  assert.equal(scroll.listeners.get('scroll').size, 0);
  assert.equal(win.visualViewport.listeners.get('resize').size, 0);
  assert.equal(observers.size, 0);
  observer.callback(); // An already-delivered callback cannot re-arm placement.
  assert.equal(frames.size, 0);
  f.opener.focus();
  finish(); await Promise.resolve();
  f.watches.get('archivePromptId')(); f.flush();
  assert.equal(doc.activeElement, archive.a);
  state.closeArchivePrompt(); f.watches.get('archivePromptId')(); f.flush();
  assert.equal(doc.activeElement, kebab, 'returns to actual kebab rather than transient focus');
  click(kebab);
  assert.equal(scroll.listeners.get('scroll').size, 1);
  menu.isConnected = false;
  state._mountBoard();
  assert.equal(scroll.listeners.get('scroll').size, 0);
  assert.equal(root.listeners.get('click').size, 1);
  state._disposeBoardEvents(); state.destroyDialogs();
  assert.equal(root.listeners.get('click').size, 0);
});
