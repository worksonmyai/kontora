import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { after, before, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { chromium, webkit } from 'playwright';
import { expect } from 'playwright/test';

const engine = process.env.KONTORA_BROWSER_ENGINE || 'chromium';
assert.ok(['chromium', 'webkit'].includes(engine));
const tipsPath = fileURLToPath(new URL('../static/tips.js', import.meta.url));
const modules = ['board', 'relations'].map(name =>
  readFileSync(new URL(`../ui/${name}.js`, import.meta.url), 'utf8').replaceAll('export function ', 'function '));
let browser;
before(async () => { browser = await ({ chromium, webkit })[engine].launch({ headless: true }); });
after(async () => { await browser?.close(); });

async function fixture(t, markup, setup = () => {}) {
  const page = await browser.newPage({ hasTouch: true });
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  t.after(async () => { await page.close(); assert.deepEqual(errors, []); });
  await page.setContent(`
    <style>[role="tooltip"] { position: fixed; pointer-events: none; }</style>
    <div id="global-tip"></div><div id="global-tip-t"></div>
    <div id="global-tip-e"><div><span></span><span></span></div><div></div><div></div></div>
    <main id="mount">${markup}</main>
  `);
  for (const content of modules) await page.addScriptTag({ content });
  await page.evaluate(setup);
  await page.addScriptTag({ path: tipsPath });
  return page;
}

async function ticketFixture(t, guarded) {
  const page = await fixture(t, '<section id="references"></section>', () => {
    window.opened = 0;
    const relations = Object.assign(kontoraRelations(), {
      ticketTip: () => ({ title: 'A ticket', tag: '', tagColor: 'none', body: 'Recorded ticket', hint: 'Open ticket' }),
      relationChipClass: () => 'ticket-reference',
      relationKnown: () => true,
      openTicketRef() {
        if (window.guarded && !confirm('Discard the unsaved automation?')) return;
        window.opened++;
        document.getElementById('references').remove();
      },
    });
    window.chip = relations._ticketChip(document.createElement('span'), { id: 'kon-reference' });
    chip.id = 'ticket-chip';
    chip.textContent = 'kon-reference';
    document.getElementById('references').append(chip);
  });
  await page.evaluate(value => { window.guarded = value; }, guarded);
  return page;
}

for (const key of ['Enter', 'Space']) {
  test(`ticket chip ${key} asks to discard an unsaved automation once`, async t => {
    const page = await ticketFixture(t, true);
    const dialogs = [];
    page.on('dialog', async dialog => { dialogs.push(dialog.message()); await dialog.dismiss(); });
    await page.locator('#ticket-chip').focus();
    await page.keyboard.press(key);
    assert.deepEqual(dialogs, ['Discard the unsaved automation?']);
    assert.equal(await page.evaluate(() => window.opened), 0);
    await expect(page.locator('#ticket-chip')).toHaveAttribute('role', 'link');
    await expect(page.locator('#ticket-chip')).not.toHaveAttribute('data-tip-info');
  });

  test(`timestamp ${key} opens information without navigating its board card`, async t => {
    const page = await fixture(t, `
      <div id="board-cols"><article class="kt-card" data-ticket-id="kon-card" tabindex="0">
        Ticket <span id="timestamp" data-tip-t="Started: November 16, 2026">2m</span>
      </article></div>`, () => {
      window.selected = 0;
      const board = Object.assign(kontoraBoard(), {
        allBoardTickets: () => [{ id: 'kon-card' }],
        ticketKey: ticket => ticket.id,
        selectTicket: () => { window.selected++; },
        _closeCardMenu() {},
      });
      board._bindBoardEvents(document.getElementById('board-cols'));
    });
    const timestamp = page.locator('#timestamp'), tip = page.locator('#global-tip-t');
    await expect(timestamp).toHaveAttribute('role', 'button');
    await expect(timestamp).toHaveAttribute('tabindex', '0');
    await timestamp.focus();
    await page.keyboard.press('Escape');
    await expect(tip).toHaveAttribute('aria-hidden', 'true');
    await page.keyboard.press(key);
    assert.equal(await page.evaluate(() => window.selected), 0);
    await expect(tip).toHaveAttribute('aria-hidden', 'false');
    await page.keyboard.press('Escape');
    await expect(timestamp).toBeFocused();
    await expect(tip).toHaveAttribute('aria-hidden', 'true');
    await timestamp.tap();
    assert.equal(await page.evaluate(() => window.selected), 0);
    await expect(tip).toHaveAttribute('aria-hidden', 'false');
    await page.locator('.kt-card').focus();
    await page.keyboard.press(key);
    assert.equal(await page.evaluate(() => window.selected), 1, 'The card itself still opens the ticket');
  });
}

test('clicking a ticket reference dismisses its tooltip when navigation removes the trigger', async t => {
  const page = await ticketFixture(t, false);
  await page.locator('#ticket-chip').hover();
  await expect(page.locator('#global-tip-e')).toHaveAttribute('aria-hidden', 'false');
  await page.locator('#ticket-chip').click();
  assert.equal(await page.evaluate(() => window.opened), 1);
  await expect(page.locator('#ticket-chip')).toHaveCount(0);
  await expect(page.locator('#global-tip-e')).toHaveAttribute('aria-hidden', 'true');
  assert.equal(await page.evaluate(() => chip.hasAttribute('aria-describedby')), false);
});

for (const [attr, id] of [['data-tip', 'global-tip'], ['data-tip-t', 'global-tip-t'], ['data-tip-e', 'global-tip-e']]) {
  test(`pinned ${attr} closes when navigation removes its containing view`, async t => {
    const page = await fixture(t, `<section id="view"><span id="info" ${attr}="Recorded explanation" aria-describedby="existing-help">Info</span></section>`);
    const info = page.locator('#info'), tip = page.locator(`#${id}`);
    await info.tap();
    await expect(tip).toHaveAttribute('aria-hidden', 'false');
    await expect(info).toHaveAttribute('aria-describedby', `existing-help ${id}`);
    await page.evaluate(() => {
      window.removedInfo = document.getElementById('info');
      document.getElementById('view').remove();
    });
    await expect(tip).toHaveAttribute('aria-hidden', 'true');
    assert.equal(await page.evaluate(() => removedInfo.getAttribute('aria-describedby')), 'existing-help');
  });

  test(`generated ${attr} label follows updates to a retained statistics card`, async t => {
    const page = await fixture(t, `<div id="stats" ${attr}="0 tokens">0</div>`);
    const stats = page.locator('#stats');
    await expect(stats).toHaveAccessibleName('0 tokens');
    for (const text of ['2.2k tokens', '4.4k tokens']) {
      await stats.evaluate((node, { attr, text }) => { node.setAttribute(attr, text); node.textContent = text; }, { attr, text });
      await expect(stats).toHaveAccessibleName(text);
    }
    await stats.evaluate((node, attr) => { node.setAttribute('aria-label', 'Author label'); node.setAttribute(attr, '8.8k tokens'); }, attr);
    await expect(stats).toHaveAccessibleName('Author label');
  });
}

test('information activation preserves the trigger copy handler without activating its containing card', async t => {
  const page = await fixture(t, `
    <article id="card"><span id="copy" data-tip-e="branch-name" data-tip-e-hint="click to copy">branch-name</span></article>`, () => {
    window.copies = 0;
    window.navigations = 0;
    document.getElementById('copy').addEventListener('click', () => { window.copies++; });
    document.getElementById('card').addEventListener('click', () => { window.navigations++; });
  });
  await page.locator('#copy').tap();
  await page.locator('#copy').focus();
  await page.keyboard.press('Enter');
  await page.keyboard.press('Space');
  assert.equal(await page.evaluate(() => window.copies), 3);
  assert.equal(await page.evaluate(() => window.navigations), 0);
});

test('preparation handles a newly mounted information control and keeps its generated label current', async t => {
  const page = await fixture(t, '');
  await page.evaluate(() => {
    const span = document.createElement('span');
    span.id = 'new-info';
    span.setAttribute('data-tip-t', '0 tokens');
    document.getElementById('mount').append(span);
  });
  const info = page.locator('#new-info');
  await expect(info).toHaveAttribute('role', 'button');
  await info.evaluate(node => node.setAttribute('data-tip-t', '2.2k tokens'));
  await expect(info).toHaveAccessibleName('2.2k tokens');
  await info.focus();
  await page.keyboard.press('Enter');
  await expect(page.locator('#global-tip-t')).toHaveAttribute('aria-hidden', 'false');
});

test('preparation preserves authored names and interactive control semantics', async t => {
  const page = await fixture(t, `
    <span id="authored" aria-label="Statistics explanation" data-tip-t="0 tokens">Stats</span>
    <button id="action" aria-label="Run" data-tip-t="Run automation">Run</button>
    <span id="custom-action" role="button" tabindex="0" aria-label="Open ticket" data-tip-e="A ticket">Ticket</span>
    <button id="info-button" class="tip" data-tip="Information" aria-label="About statistics">i</button>`);
  await page.locator('#authored').evaluate(node => node.setAttribute('data-tip-t', '2.2k tokens'));
  await expect(page.locator('#authored')).toHaveAccessibleName('Statistics explanation');
  for (const selector of ['#action', '#custom-action', '#info-button']) {
    await expect(page.locator(selector)).not.toHaveAttribute('data-tip-info');
  }
  await page.locator('#info-button').tap();
  await expect(page.locator('#global-tip')).toHaveAttribute('aria-hidden', 'false');
  await page.locator('#info-button').tap();
  await expect(page.locator('#global-tip')).toHaveAttribute('aria-hidden', 'true');
});
