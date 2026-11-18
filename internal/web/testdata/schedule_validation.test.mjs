import assert from 'node:assert/strict';
import test from 'node:test';
import { parseScheduleInput, storedISO } from '../ui/format.js';

// Explicit zones keep DST coverage independent of the machine running Node.
function inZone(zone, run) {
  const previous = process.env.TZ;
  process.env.TZ = zone;
  try { return run(); }
  finally {
    if (previous === undefined) delete process.env.TZ;
    else process.env.TZ = previous;
  }
}

const invalidLocal = [
  '2030-00-10 09:00', '2030-13-10 09:00', '2030-01-00 09:00', '2030-01-32 09:00',
  '2030-04-31 09:00', '2030-02-29 09:00', '2100-02-29 09:00', '2030-02-30 09:00',
  '2030-01-10 24:00', '2030-01-10 25:00', '2030-01-10 09:60',
  '2030-01-10 09:00:60', '2030-01-10 09:00:99', '2030-01-10 09:00:-1',
  '2030-01-10 09:00:1', '2030-01-10 09:00:01.1', '2030-01-10 09:00:01Z',
];
for (const text of invalidLocal) {
  test(`reject invalid local components: ${text}`, () => inZone('UTC', () => {
    const result = parseScheduleInput(text, new Date('2029-01-01T00:00:00Z'));
    assert.ok(result.error, 'must not silently normalize calendar input');
    assert.equal(result.iso, '');
    assert.equal(result.at, null);
  }));
}

for (const [zone, text] of [
  ['America/New_York', '2030-03-10 02:30'],
  ['America/New_York', '2030-03-10 02:59:59'],
  ['Europe/Berlin', '2030-03-31 02:30'],
  ['Australia/Lord_Howe', '2030-10-06 02:15'],
]) {
  test(`reject nonexistent DST wall time: ${zone} ${text}`, () => inZone(zone, () => {
    const result = parseScheduleInput(text, new Date('2030-01-01T00:00:00Z'));
    assert.ok(result.error);
    assert.equal(result.iso, '');
    assert.equal(result.at, null);
  }));
}

for (const [zone, text, iso] of [
  ['UTC', '2032-02-29 09:05', '2032-02-29T09:05:00Z'],
  ['UTC', '2000-02-29T09:05:59', '2000-02-29T09:05:00Z'],
  ['UTC', '2030-12-31 23:59:59', '2030-12-31T23:59:00Z'],
  ['America/New_York', '2030-03-10 03:00', '2030-03-10T07:00:00Z'],
  ['America/New_York', '2030-11-03 01:30:59', '2030-11-03T05:30:00Z'],
  ['Europe/Berlin', '2030-10-27 02:30', '2030-10-27T00:30:00Z'],
  ['Australia/Lord_Howe', '2030-10-06 02:30', '2030-10-05T15:30:00Z'],
]) {
  test(`valid local minute precision and UTC serialization: ${zone} ${text}`, () => inZone(zone, () => {
    const result = parseScheduleInput(text, new Date('1999-01-01T00:00:00Z'));
    assert.equal(result.error, '');
    assert.equal(result.iso, iso);
    assert.equal(result.at.getSeconds(), 0);
    assert.equal(storedISO(result.at), iso);
  }));
}

for (const [zone, now, text, iso] of [
  ['America/New_York', '2030-03-09T17:12:34Z', '1d', '2030-03-10T17:12:34Z'],
  ['America/New_York', '2030-11-02T16:12:34Z', '24h', '2030-11-03T16:12:34Z'],
  ['America/New_York', '2030-11-03T05:30:00Z', '1h', '2030-11-03T06:30:00Z'],
  ['UTC', '2030-01-01T00:00:00Z', '1w2d3h', '2030-01-10T03:00:00Z'],
  ['UTC', '2030-01-01T00:00:00Z', '1.5h', '2030-01-01T01:30:00Z'],
  ['UTC', '2030-01-01T00:00:00Z', '1ms', '2030-01-01T00:00:00Z'],
]) {
  test(`duration stays elapsed across DST: ${zone} ${now} + ${text}`, () => inZone(zone, () => {
    const result = parseScheduleInput(text, new Date(now));
    assert.equal(result.error, '');
    assert.equal(result.iso, iso);
    if (text === '1ms') assert.equal(result.at.getMilliseconds(), 1);
  }));
}

for (const [text, now] of [
  ['9999999999999w', new Date('2030-01-01T00:00:00Z')],
  ['9'.repeat(400) + 'h', new Date('2030-01-01T00:00:00Z')],
  ['1d', new Date(8640000000000000)],
  ['1h', new Date(NaN)],
]) {
  test(`invalid duration result is an error, not an empty successful instant: ${text.slice(0, 24)} ${now}`, () => {
    const result = parseScheduleInput(text, now);
    assert.ok(result.error);
    assert.equal(result.iso, '');
    assert.equal(result.at, null);
  });
}

for (const [now, valid] of [
  ['2030-01-01T09:00:00Z', true],
  ['2030-01-01T09:01:00Z', true],
  ['2030-01-01T09:01:00.001Z', false],
]) {
  test(`past grace remains one minute: ${now}`, () => inZone('UTC', () => {
    const result = parseScheduleInput('2030-01-01 09:00:59', new Date(now));
    assert.equal(!result.error, valid);
    assert.equal(result.iso, valid ? '2030-01-01T09:00:00Z' : '');
  }));
}

test('empty schedule retains the optional-input contract', () => {
  assert.deepEqual(parseScheduleInput('  '), { iso: '', at: null, error: '' });
});
