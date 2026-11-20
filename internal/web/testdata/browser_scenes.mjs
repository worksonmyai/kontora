// Scene data and real-control helpers shared by the behavioral and visual suites.
import assert from 'node:assert/strict';
import { liveAgent, longID, ready, source, stamp, ticket } from './browser_fixture.mjs';

export async function visible(locator) { await locator.waitFor({ state: 'visible' }); }
export async function inViewport(locator) {
  await visible(locator);
  const box = await locator.boundingBox();
  const size = locator.page().viewportSize();
  assert.ok(box && box.width > 0 && box.height > 0 && box.x >= -1 && box.y >= -1 &&
    box.x + box.width <= size.width + 1 && box.y + box.height <= size.height + 1,
  `Control outside viewport: ${JSON.stringify(box)} in ${JSON.stringify(size)}`);
}

export const polishRecords = () => [
  ticket('kon-queued', 'Queued ordinary ticket', { status: 'todo', agent: liveAgent }),
  ticket('kon-paused', 'Paused ordinary ticket', { status: 'paused', agent: liveAgent }),
  ticket('kon-attention', 'Agent needs an answer before continuing', { status: 'in_progress',
    waiting_for_input: true, waiting_since: stamp, started_at: stamp, agent: liveAgent }),
  ticket('kon-scheduled', 'Scheduled pickup with long pipeline metadata', { scheduled_at: '2099-09-01T09:00:00Z' }),
  ticket(longID, 'Long identifiers must stay inside their card alongside source and agent metadata',
    { source, status: 'in_progress', started_at: stamp, agent: liveAgent,
      pipeline: 'pipeline-' + 'long-metadata-'.repeat(8) }),
];

export async function detailPanel(page, name) {
  const phone = page.viewportSize().width < 768;
  const label = name === 'Changes' ? /^Committed changes(?:\s+\d+)?$/ : phone ? name : ({ Ticket: 'ticket', Notes: 'notes', Output: 'activity' })[name];
  await page.getByRole('button', { name: label, exact: true }).filter({ visible: true }).first().click();
}
export async function phoneOutputView(page, view) {
  const output = page.locator('.mobile-detail-tabs').getByRole('button', { name: 'Output', exact: true });
  if (await output.getAttribute('aria-pressed') !== 'true') await output.click();
  const select = page.locator('.mobile-detail .mobile-output-row').getByRole('combobox', { name: 'Output view', exact: true });
  await inViewport(select);
  await select.selectOption({ label: view });
}

export function parityEpicRecords() {
  return [
    ticket('kon-order', 'Release epic with retained order', { kind: 'epic', epic_mode: 'passive',
      child_order: ['kon-outside', 'kon-open-b', 'kon-review', 'kon-open-a'] }),
    ticket('kon-other', 'Other epic owns its own order', { kind: 'epic', epic_mode: 'passive', child_order: ['kon-other-child'] }),
    ticket('kon-outside', 'Stored nonmember stays outside'),
    ticket('kon-open-a', 'Alpha child', { parent: { id: 'kon-order' }, stage: 'code', agent: 'draft-only',
      history: [{ stage: 'plan', started_at: stamp, completed_at: '2026-10-01T10:01:02Z', exit_code: 0 }] }),
    ticket('kon-open-b', 'Beta child', { parent: { id: 'kon-order' }, created_at: '2026-10-01T09:00:00Z' }),
    ticket('kon-unordered', 'Unordered child', { parent: { id: 'kon-order' }, created_at: '2026-10-02T10:00:00Z' }),
    ticket('kon-review', 'Approval holder', { parent: { id: 'kon-order' }, status: 'human_review', stage: 'code' }),
    ticket('kon-other-child', 'Other epic child', { parent: { id: 'kon-other' } }),
    ticket('kon-candidate', 'Searchable existing ticket'),
  ];
}
export const parityChild = (page, id) => page.locator('.epic-mobile-child:visible, .epic-managed-row:visible').filter({ hasText: id });

export const ordinaryThreadID = 'a0123456789abcdef';
export function parityAssistantOptions() {
  return { savedThread: ordinaryThreadID,
    assistantConfig: { enabled: true, agent: 'fixture-agent', autonomy: 'ask' },
    assistantThreads: [{ id: ordinaryThreadID, title: 'Recorded parity chat', turns: 2, writes: 1, running: false, autonomy: 'ask',
      messages: [{ n: 1, text: 'Inspect the release safely.', at: stamp, error: 'Recorded turn failed: fixture quota.' }],
      events: [
        { kind: 'model', model: 'fixture-reasoning-model', time: stamp },
        { kind: 'tool', id: 'inspect-result', tool: 'read_ticket', arg: 'kon-draft', time: stamp, result: 'Recorded tool result: primary draft remains open.' },
        { kind: 'tool', id: 'inspect-error', tool: 'inspect_missing', arg: 'kon-missing', time: stamp,
          is_error: true, result: 'Recorded tool error: ticket does not exist.' },
        { kind: 'text', text: 'Recorded explanation, not a live daemon response.', time: stamp },
      ], gate: { id: 'fixture-gate', kind: 'write', tool: 'move_ticket', arg: 'kon-draft → open' } },
    { id: 'b0123456789abcdef', title: 'Disposable history chat', turns: 3, writes: 2, running: false, autonomy: 'read', messages: [] }],
  };
}
export const assistantSurface = page => page.locator('#assistant-pane:visible, #assistant-sheet:visible');
export async function openAssistantControl(page) {
  if (!await assistantSurface(page).isVisible()) {
    await page.getByRole('button', { name: 'Assistant', exact: true }).filter({ visible: true }).and(page.locator(':not([inert], [inert] *)')).first().click();
  }
  await visible(assistantSurface(page).getByRole('navigation', { name: 'Assistant views', exact: true }));
}

export async function automationDetailReady(page, id) {
  await page.waitForFunction(id => { const app = Alpine.$data(document.body); return app.automationsSelected === id && app.automationsDetail && !app.automationsEditing && !app.automationsHistoryLoading; }, id);
  await ready(page);
}

export function terminalGrid({ marker, minRows = 1, diagnostic = false } = {}) {
  const term = window.termState.term;
  const host = document.getElementById(Alpine.$data(document.body).outputContainerID('terminal'));
  const screen = host?.querySelector('.xterm-screen');
  if (!host || !term?.element || !screen) return false;
  const box = host.getBoundingClientRect(), style = getComputedStyle(host);
  const content = {
    left: box.left + parseFloat(style.borderLeftWidth) + parseFloat(style.paddingLeft),
    right: box.right - parseFloat(style.borderRightWidth) - parseFloat(style.paddingRight),
    top: box.top + parseFloat(style.borderTopWidth) + parseFloat(style.paddingTop),
    bottom: box.bottom - parseFloat(style.borderBottomWidth) - parseFloat(style.paddingBottom),
  };
  const rect = screen.getBoundingClientRect();
  const cell = term._core._renderService.dimensions.css.cell;
  const grid = { left: rect.left, top: rect.top,
    right: rect.left + term.cols * cell.width, bottom: rect.top + term.rows * cell.height };
  const contains = (outer, inner, x = true, y = true) =>
    (!x || (inner.left >= outer.left - 1 && inner.right <= outer.right + 1)) &&
    (!y || (inner.top >= outer.top - 1 && inner.bottom <= outer.bottom + 1));
  const clips = [];
  for (let el = host; el; el = el.parentElement) {
    const css = getComputedStyle(el), bounds = el.getBoundingClientRect();
    const x = /^(hidden|clip|auto|scroll)$/.test(css.overflowX);
    const y = /^(hidden|clip|auto|scroll)$/.test(css.overflowY);
    if (x || y) clips.push({ id: el.id, x, y,
      left: bounds.left + parseFloat(css.borderLeftWidth), right: bounds.right - parseFloat(css.borderRightWidth),
      top: bounds.top + parseFloat(css.borderTopWidth), bottom: bounds.bottom - parseFloat(css.borderBottomWidth) });
    // The mobile overlay is fixed to the viewport, outside its ancestors' overflow clips.
    if (css.position === 'fixed') break;
  }
  const proposed = window.termState.fit.proposeDimensions();
  const lastRow = term.buffer.active.getLine(term.buffer.active.baseY + term.rows - 1)?.translateToString(true);
  const fits = cell.width > 0 && cell.height > 0 && term.rows >= minRows && term.cols > 0 &&
    rect.width > 0 && rect.height > 0 && content.right > content.left && content.bottom > content.top &&
    term.element.parentElement === host && proposed?.rows === term.rows && proposed?.cols === term.cols &&
    contains(rect, grid) && contains(content, grid) && contains(content, rect) &&
    clips.every(clip => contains(clip, grid, clip.x, clip.y) && contains(clip, rect, clip.x, clip.y)) &&
    contains({ left: 0, top: 0, right: innerWidth, bottom: innerHeight }, rect) &&
    (!marker || (lastRow === marker && term.buffer.active.viewportY === term.buffer.active.baseY));
  return diagnostic ? { fits, host: host.id, content, screen: rect.toJSON(), grid,
    cell, rows: term.rows, cols: term.cols, proposed, clips, minRows, marker, lastRow,
    baseY: term.buffer.active.baseY, viewportY: term.buffer.active.viewportY } : fits;
}
