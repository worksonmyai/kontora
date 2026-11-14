import assert from 'node:assert/strict';
import test from 'node:test';
import { kontoraTickets } from '../ui/tickets.js';

test('deployment instructions use the captured checkout rather than Git common directory', () => {
  const s = kontoraTickets();
  const wait = {
    kind: 'deployment', id: 'wait', phase: 'pending',
    repository: '/repos/project/.git', worktree: '/checkouts/release candidate',
    environment: 'production', required_commit: '0123456789012345678901234567890123456789',
  };
  const ticket = { id: 'ticket', status: 'paused', graph_execution: { wait } };
  const instructions = s.workflowReleaseInstructions(ticket);
  assert.ok(instructions.includes('--repo ' + JSON.stringify(wait.worktree)));
  assert.ok(instructions.includes('--environment "production"'));
  assert.ok(instructions.includes('--rev <full-deployed-commit> --event-id <unique-id>'));
  assert.ok(instructions.includes('required: ' + wait.required_commit));
  assert.ok(!instructions.includes(wait.repository));
});
