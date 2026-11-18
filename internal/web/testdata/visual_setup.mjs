import { readdir } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';

export const candidateDir = fileURLToPath(new URL('../../../test-results/visual-candidates/', import.meta.url));

export default async function verifySnapshotMode(config) {
  const update = process.env.KONTORA_VISUAL_MODE === 'update';
  if (update && (process.env.CI || process.env.GITHUB_ACTIONS || process.env.KONTORA_VISUAL_RENDERER !== '1' ||
      process.platform !== 'linux' || process.arch !== 'x64')) {
    throw new Error('Visual baseline updates require make test-visual-update outside CI');
  }
  if (config.updateSnapshots !== (update ? 'all' : 'none')) {
    throw new Error(update
      ? 'Candidate capture requires snapshot mode `all`. Run `make test-visual-update` outside CI.'
      : 'Screenshot checks require snapshot mode `none`. Run `make test-visual`.');
  }
  if (update) {
    // With an existing PNG, Playwright can accept an unstable frame in `all` mode.
    const entries = await readdir(candidateDir).catch(error => {
      if (error.code !== 'ENOENT') throw error;
      return [];
    });
    if (entries.length) {
      throw new Error('Candidate capture requires an empty candidate directory. Run `make test-visual-update` outside CI.');
    }
  }
}
