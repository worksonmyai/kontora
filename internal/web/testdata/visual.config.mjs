import { fileURLToPath } from 'node:url';
import { defineConfig } from 'playwright/test';
import { candidateDir } from './visual_setup.mjs';

const fromHere = path => fileURLToPath(new URL(path, import.meta.url));
const update = process.env.KONTORA_VISUAL_MODE === 'update';

export default defineConfig({
  globalSetup: fromHere('./visual_setup.mjs'),
  testDir: fromHere('.'),
  testMatch: 'visual.spec.mjs',
  workers: 1,
  retries: 0,
  maxFailures: 1,
  timeout: 45_000,
  forbidOnly: true,
  updateSnapshots: 'none',
  outputDir: fromHere('../../../test-results/visual/'),
  reporter: [['list'], ['html', { outputFolder: fromHere('../../../playwright-report/'), open: 'never' }]],
  snapshotPathTemplate: `${update ? candidateDir : fromHere('./visual-baselines/')}{projectName}/{arg}{ext}`,
  expect: {
    toHaveScreenshot: {
      timeout: 5000,
      animations: 'disabled',
      caret: 'hide',
      scale: 'css',
      maxDiffPixels: 0,
      stylePath: fromHere('./visual.css'),
    },
  },
  use: {
    locale: 'en-US',
    timezoneId: 'UTC',
    reducedMotion: 'reduce',
    deviceScaleFactor: 1,
    serviceWorkers: 'block',
    trace: 'retain-on-failure',
  },
  projects: [{ name: 'linux-x64-chromium', use: { browserName: 'chromium' } }],
});
