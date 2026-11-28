import assert from 'node:assert/strict';
import test from 'node:test';
import '../ui/index.js';

globalThis.window = { innerWidth: 850, matchMedia: () => ({ matches: false }) };
globalThis.localStorage = { getItem: () => null, setItem() {} };
globalThis.document = { documentElement: { dataset: {} }, getElementById: () => null };

function setup(t, key, { hidden = false, reduceMotion = false } = {}) {
  const app = globalThis.kontora();
  const actions = [];
  const heading = { focus: options => actions.push(['focus', options.preventScroll]) };
  const column = {
    getClientRects: () => hidden ? [] : [{}],
    scrollIntoView: options => actions.push(['scroll', options.behavior]),
    querySelector: selector => selector === '.col-head' ? heading : null,
  };
  t.mock.method(window, 'matchMedia', () => ({ matches: reduceMotion }));
  t.mock.method(document, 'getElementById', id => id === 'col-' + key && !app.isCollapsed(key)
    ? { closest: selector => selector === '.kanban-col' ? column : null } : null);
  const frames = [];
  t.mock.method(globalThis, 'requestAnimationFrame', callback => frames.push(callback));
  Object.assign(app, {
    $nextTick: async callback => callback?.(),
    writeHash() {}, mobileBeforeNavigate() {},
    renderColumn() {},
  });
  app.toggleColumnCollapsed(key);
  return { app, actions, frames, show: () => { hidden = false; } };
}

globalThis.requestAnimationFrame = () => { throw new Error('unexpected animation frame'); };

for (const resume of ['save', 'discard', 'keep editing', 'failed save']) {
  test(`running pill retains its destination across dirty Settings / ${resume}`, async t => {
    const { app, actions, frames, show } = setup(t, 'in_progress', { hidden: true });
    let dirty = true;
    Object.assign(app, {
      currentView: 'settings',
      settingsDirty: () => dirty,
      discardSettings() { dirty = false; },
      async saveSettings() {
        if (resume === 'failed save') return false;
        dirty = false;
        return true;
      },
    });
    assert.equal(await app.showInProgress(), false);
    assert.equal(app.settingsGuard, true);
    assert.equal(app.isCollapsed('in_progress'), true);
    assert.deepEqual(actions, []);

    const continuation = resume === 'keep editing' ? app.settingsGuardKeepEditing()
      : resume === 'discard' ? app.settingsGuardDiscard() : app.settingsGuardSave();
    if (resume === 'save' || resume === 'discard') {
      await new Promise(resolve => setImmediate(resolve));
      assert.equal(app.currentView, 'board');
      assert.equal(app.isCollapsed('in_progress'), false);
      assert.deepEqual(actions, [], 'wait for the x-show to become visible');
      show();
      frames.shift()();
      frames.shift()();
      await continuation;
      assert.deepEqual(actions, [['scroll', 'smooth']]);
    } else {
      await continuation;
      assert.equal(app.currentView, 'settings');
      assert.equal(app.isCollapsed('in_progress'), true);
      assert.deepEqual(actions, []);
    }
  });
}

test('rail expansion scrolls before focusing the expanded header', async t => {
  const { app, actions } = setup(t, 'open');
  await app.expandBoardColumn('open', { focus: true });
  assert.equal(app.isCollapsed('open'), false);
  assert.deepEqual(actions, [['scroll', 'auto'], ['focus', true]]);
});

test('column reveal respects reduced motion and abandons a stale navigation', async t => {
  const { app, actions, frames, show } = setup(t, 'in_progress', { hidden: true, reduceMotion: true });
  const pending = app.showInProgress();
  await new Promise(resolve => setImmediate(resolve));
  ++app._navigationSeq;
  show();
  frames.shift()();
  frames.shift()();
  await pending;
  assert.deepEqual(actions, []);
  await app.showInProgress();
  assert.deepEqual(actions, [['scroll', 'auto']]);
});

test('secondary cards render the escaped folder as text and describe only control limits', () => {
  const app = globalThis.kontora();
  app.ticketSources = [{ id: 'team', path: '/mnt/teams/<infra>' }];
  const primary = { id: 'same-id', status: 'open', kontora: true, title: 'Same title' };
  const col = { key: 'open' };
  const html = app._cardHTML({ ...primary, source: 'team', can_edit: true }, col);
  assert.match(html, /class="card-source [^"]+"[^>]*>.*<span class="break-all">\/mnt\/teams\/&lt;infra&gt;<\/span>/);
  assert.match(html, /aria-description="From \/mnt\/teams\/&lt;infra&gt;\. This daemon can’t run it or change its status\."/);
  assert.doesNotMatch(html, /Read-only/i);
  assert.doesNotMatch(app._cardHTML(primary, col), /card-source|aria-description/);
});

test('the review-age tooltip and render signature distinguish equal timestamps from different fields', () => {
  const app = globalThis.kontora();
  const date = '2026-05-19T09:00:00Z';
  const col = { key: 'human_review' };
  const tickets = ['finished_at', 'updated_at', 'created_at'].map(field => ({ id: 'same-id', status: col.key, [field]: date }));
  for (const [i, label] of ['Finished', 'Updated', 'Created'].entries()) {
    assert.ok(app._cardHTML(tickets[i], col).includes(`data-finished="${date}" data-tip-t="${label}: `));
  }
  assert.equal(new Set(tickets.map(ticket => app._cardSig(ticket, col))).size, tickets.length);
});
