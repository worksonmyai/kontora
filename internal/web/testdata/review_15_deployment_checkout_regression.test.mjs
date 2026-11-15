import assert from 'node:assert/strict';
import test from 'node:test';
import { execFileSync } from 'node:child_process';
import { kontoraTickets } from '../ui/tickets.js';

for (const value of ['release candidate', "release'$HOME`exit 91`$(exit 92)\\雪\nnext"]) {
  test('deployment instructions preserve the checkout and environment: ' + value, () => {
    const s = kontoraTickets();
    const wait = {
      kind: 'deployment', id: 'wait', phase: 'pending',
      repository: '/repos/project/.git', worktree: '/checkouts/' + value,
      environment: value, required_commit: '0123456789012345678901234567890123456789',
    };
    const ticket = { id: 'ticket', status: 'paused', graph_execution: { wait } };
    const instructions = s.workflowReleaseInstructions(ticket);
    const args = instructions.split('kontora release ')[1].split(' --rev ')[0];
    const actual = execFileSync('sh', ['-c', "printf '%s\\0' " + args], { encoding: 'utf8' }).split('\0');
    assert.deepEqual(actual, ['--repo', wait.worktree, '--environment', wait.environment, '']);
    assert.ok(instructions.includes('--rev <full-deployed-commit> --event-id <unique-id>'));
    assert.ok(instructions.includes('required: ' + wait.required_commit));
    assert.ok(!instructions.includes(wait.repository));
  });
}
