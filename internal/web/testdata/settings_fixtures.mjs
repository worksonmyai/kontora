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

export const settingsNotificationMergeCases = [
  ['inline merge', 'notifications: { <<: { default_statuses: [done] } }\n', ['done']],
  ['nested merge', 'notifications: { <<: { <<: { default_statuses: [done] } } }\n', ['done']],
  ['merge sequence precedence', 'notifications: { <<: [{ default_statuses: [done] }, { default_statuses: [paused] }] }\n', ['done']],
  ['merge sequence skips missing key', 'notifications: { <<: [{ enabled: false }, { default_statuses: [done] }] }\n', ['done']],
  ['merge sequence with map alias', 'notifications: { <<: [&base { default_statuses: [done] }, *base] }\n', ['done']],
  ['merged status sequence alias', 'projects: { repo: { path: /repo, notify: &choice [done] } }\nnotifications: { <<: { default_statuses: *choice } }\n', ['done']],
  ['explicit status sequence alias', 'projects: { repo: { path: /repo, notify: &choice [] } }\nnotifications: { <<: { default_statuses: [done] }, default_statuses: *choice }\n', []],
  ['explicit statuses before merge', 'notifications: { default_statuses: [waiting], <<: { default_statuses: [done] } }\n', ['waiting']],
  ['explicit Off after merge', 'notifications: { <<: { default_statuses: [done] }, default_statuses: [] }\n', []],
  ['explicit null after merge', 'notifications: { <<: { default_statuses: [done] }, default_statuses: null }\n', null],
  ['explicit blank after merge', 'notifications:\n  <<: { default_statuses: [done] }\n  default_statuses:\n', null],
  ['merged Off', 'notifications: { <<: [{ default_statuses: [] }, { default_statuses: [done] }] }\n', []],
  ['merged null', 'notifications: { <<: [{ default_statuses: null }, { default_statuses: [done] }] }\n', null],
  ['commented merge with shared status alias', `# root comment
notifications:
  <<: &base # merge source
    default_statuses: &choice [done] # shared statuses
    all_changes: true # origin preference
  enabled: false # delivery preference
  default: [hook] # destinations
  channels:
    hook: { type: webhook, url: 'https://example.invalid' } # channel
projects:
  repo: { path: /repo, notify: *choice } # shared project preference
`, ['done']],
];

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
  const notifySource = fs.readFileSync(new URL('../ui/notify.js', import.meta.url), 'utf8')
    .replace(/^import .*;\n/gm, '').replace(/^export /gm, '');
  vm.runInContext(notifySource + '\n' + source + '\nthis.makeSettings = kontoraSettings;', context);
  const state = context.makeSettings();
  Object.assign(state, { _settingsYAML: yaml, _navigationSeq: 0, currentView: 'settings',
    $nextTick: fn => Promise.resolve().then(fn), closeAgentPicker() {},
    assistantAutonomies: () => [], ...globals.state });
  await state._settingsParse(text);
  return state;
}
