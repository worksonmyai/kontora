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

export const iosPwa = {
  running: 'ios-104',
  progress: ['ios-104', 'ios-105', 'ios-103', 'ios-102'],
  queue: ['ios-111', 'ios-112', 'ios-113'],
  firstReview: 'ios-121',
};

// The redesign scenes never inherit the desktop seed (not even its namesakes).
export function iosPwaOptions(now) {
  const since = minutes => new Date(+now - minutes * 60_000).toISOString().replace(/\.\d{3}Z$/, 'Z');
  const web = { name: 'Storefront', path: '/fixtures/storefront', pipeline: 'standard', agent: 'pi' };
  const api = { name: 'Catalog API', path: '/fixtures/catalog-api', pipeline: 'fix', agent: 'claude' };
  const docs = { name: 'Docs', path: '/fixtures/docs', pipeline: 'docs', agent: 'pi' };
  const pipelines = [
    { name: 'standard', stages: ['plan', 'implement', 'verify', 'review'] },
    { name: 'fix', stages: ['plan', 'implement', 'verify'] },
    { name: 'docs', stages: ['draft', 'review'] },
  ];
  const row = (id, title, project, extra = {}) => {
    const { path, agent, pipeline } = project;
    const { stages } = pipelines.find(p => p.name === pipeline);
    return ticket(id, title, { path, agent, pipeline, stages, stage: stages[0],
      created_at: since(60), updated_at: since(60), ...extra });
  };
  const review = (id, title, project, minutes) => row(id, title, project, {
    status: 'human_review', finished_at: since(minutes), updated_at: since(minutes), created_at: since(minutes + 120),
  });
  return { onlyRecords: true, config: {
    projects: [web, api, docs], pipelines: pipelines.map(p => p.name), pipeline_infos: pipelines,
    agents: ['pi', 'claude'], agent_infos: [{ name: 'pi', binary: 'pi' }, { name: 'claude', binary: 'claude' }],
    stages: [...new Set(pipelines.flatMap(p => p.stages))], custom_statuses: ['blocked'],
  }, records: [
    row(iosPwa.running, 'Add cursor pagination to catalog search', api, {
      status: 'in_progress', stage: 'implement', branch: 'fixture/catalog-cursor', started_at: since(11),
      invocation_id: 'ios-fixture-invocation', attempt: 1, can_edit: false, waiting_for_input: true,
      waiting_since: since(3), waiting_tool: 'question',
      waiting_question: 'Should the API keep offset pagination for older clients, or switch every client to cursors now?',
      history: [{ stage: 'plan', run: 0, agent: api.agent, started_at: since(16), completed_at: since(11), exit_code: 0 }],
    }),
    row('ios-105', 'Improve checkout validation', web, { status: 'in_progress', stage: 'verify', started_at: since(6), can_edit: false }),
    row('ios-103', 'Update the migration guide', docs, { status: 'paused' }),
    row('ios-102', 'Record request latency metrics', api, { status: 'todo' }),
    row('ios-111', 'Cache product thumbnails', web, { updated_at: since(28) }),
    row('ios-112', 'Add test data for returns', api, { updated_at: since(32), pipeline: '' }),
    row('ios-113', 'Document the release checklist', docs, { updated_at: since(38) }),
    row('ios-114', 'Verify scheduled index rollout', api, { updated_at: since(43), scheduled_at: since(-660) }),
    row('ios-115', 'Triage partner feedback', web, { updated_at: since(50), kontora: false }),
    review(iosPwa.firstReview, 'Check the accessible checkout flow', web, 18),
    review('ios-122', 'Review error handling changes', api, 53),
    review('ios-123', 'Check the onboarding guide', docs, 95),
    review('ios-124', 'Verify currency formatting', web, 1_440),
    review('ios-125', 'Review audit events', api, 1_650),
    review('ios-126', 'Verify the changelog', docs, 3 * 1_440),
    row('ios-130', 'Remove the legacy coupon field', web, { status: 'done' }),
    row('ios-131', 'Prototype an alternate webhook', api, { status: 'cancelled' }),
    row('ios-132', 'Confirm regional settings', docs, { status: 'blocked' }),
  ] };
}

export async function detailPanel(page, name) {
  const phone = page.viewportSize().width < 768;
  const label = name === 'Changes' ? /^Changes(?:\s+\d+)?$/ : !phone && name === 'Output' ? 'Activity' : name;
  await page.getByRole('button', { name: label, exact: true }).filter({ visible: true }).first().click();
}
export async function phoneOutputView(page, view) {
  const output = page.locator('.mobile-detail-tabs').getByRole('button', { name: 'Output', exact: true });
  if (await output.getAttribute('aria-pressed') !== 'true') await output.click();
  const label = { Terminal: 'Live', 'Stage logs': 'Logs' }[view] || view;
  const segment = page.locator('.mobile-detail .mobile-output-row').getByRole('button', { name: label, exact: true });
  await inViewport(segment);
  if (await segment.getAttribute('aria-pressed') !== 'true') await segment.click();
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
export const assistantSurface = page => page.locator('#assistant-pane:visible, #assistant-page:visible');
export async function openAssistantControl(page) {
  if (!await assistantSurface(page).isVisible()) {
    const launch = page.getByRole('button', { name: 'Assistant', exact: true }).filter({ visible: true }).and(page.locator(':not([inert], [inert] *)')).first();
    if (!await launch.isVisible()) {
      await page.locator('[data-mobile-ticket-menu]:visible').click();
    }
    await launch.click();
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
