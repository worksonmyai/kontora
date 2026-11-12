import fs from 'node:fs';
import vm from 'node:vm';
import * as yaml from '../static/vendor/yaml@2.8.1/yaml.mjs';

export const settingsFixture = `# Preserve this comment
max_concurrent_agents: 3
agents:
  claude: { binary: claude }
  pi: { binary: pi }
stages:
  implement:
    prompt: Keep this prompt private
    timeout: 30m
environment:
  PRIVATE: never-search-this-value
notifications:
  channels:
    hook:
      type: webhook
      url: https://example.invalid
      headers:
        Authorization: never-search-this-header
`;

export function deferred() {
  let resolve;
  const promise = new Promise(r => { resolve = r; });
  return { promise, resolve };
}

export async function settingsState(text = settingsFixture, globals = {}) {
  const source = fs.readFileSync(new URL('../ui/settings.js', import.meta.url), 'utf8')
    .replace(/^import .*;\n/gm, '').replace('export function kontoraSettings', 'function kontoraSettings');
  const context = vm.createContext({ console, URLSearchParams, setTimeout, clearTimeout,
    requestAnimationFrame: fn => setTimeout(fn, 0), cancelAnimationFrame: clearTimeout,
    clockHM: () => '12:00', ...globals });
  vm.runInContext(source + '\nthis.makeSettings = kontoraSettings;', context);
  const state = context.makeSettings();
  Object.assign(state, { _settingsYAML: yaml, _navigationSeq: 0, currentView: 'settings',
    $nextTick: fn => Promise.resolve().then(fn), closeAgentPicker() {},
    assistantAutonomies: () => [], ...globals.state });
  await state._settingsParse(text);
  return state;
}
