const TRACE_TEXT = 'whitespace-pre-wrap break-words';

const RUN_ROWS = execution => `
  <ul>
    <template x-for="row in workflowRunRows(selectedTicket, ${execution})" :key="row.key">
      <li class="py-1">
        <span x-text="row.label"></span>
        <span x-show="row.history.run_outcome" x-text="' · ' + runOutcomeLabel(row.history)"></span>
        <span x-show="Number.isInteger(row.history.exit_code)" x-text="' · exit ' + row.history.exit_code"></span>
        <span x-show="row.history.verdict" x-text="' · verdict ' + row.history.verdict"></span>
        <button type="button" x-show="workflowCanOpenRun(selectedTicket, row)" @click="openWorkflowRun(row)"
                class="text-accent underline" :aria-label="'Open Activity: ' + row.label">Activity</button>
        <p x-show="row.history.outcome_reason" x-text="row.history.outcome_reason"></p>
      </li>
    </template>
  </ul>`;

export const DETAIL_WORKFLOW = `
  <section aria-label="Workflow graph" class="detail-shared text-xs text-tx-3" data-workflow-trace>
    <p class="font-bold text-tx-2">Recorded execution</p>
    <p x-show="selectedTicket?._graphDetailPending" x-text="ticketDetailRefreshError ? 'Execution detail unavailable.' : 'Recorded execution details have not loaded yet'"></p>
    <p x-show="!selectedTicket?.graph_execution && !selectedTicket?._graphDetailPending">No recorded execution.</p>
    <p x-show="selectedTicket?.last_error" class="${TRACE_TEXT} text-warn" x-text="selectedTicket?.last_error"></p>
    <pre x-show="selectedTicket?.graph_execution" class="${TRACE_TEXT}" x-text="workflowCursorText(selectedTicket)"></pre>
    <template x-if="selectedTicket?.graph_execution?.wait">
      <div class="py-1">
        <p class="font-bold" x-text="'Waiting: ' + selectedTicket?.graph_execution?.wait?.kind"></p>
        <p x-show="selectedTicket?.graph_execution?.wait?.deadline" x-text="'Deadline: ' + selectedTicket?.graph_execution?.wait?.deadline"></p>
        <p x-show="selectedTicket?.graph_execution?.wait?.next" x-text="selectedTicket?.graph_execution?.wait?.next"></p>
        <p x-show="selectedTicket && selectedTicket.status !== 'archived' && !isSecondary(selectedTicket) && !workflowInspectedWait()" class="${TRACE_TEXT}" x-text="workflowReleaseInstructions(selectedTicket)"></p>
        <details>
          <summary>Wait details and probe evidence</summary>
          <pre class="${TRACE_TEXT}" x-text="workflowWaitText(selectedTicket?.graph_execution?.wait)"></pre>
        </details>
      </div>
    </template>
    <template x-if="selectedTicket?.status !== 'archived'">
      <div>
        <template x-for="inspected in (workflowReleaseForm() ? [workflowReleaseForm()] : [])" :key="inspected">
          <form x-data="{ reason: '' }" @submit.prevent="releaseWorkflowWait(inspected, reason, $el)" class="flex flex-wrap items-end gap-2 py-1">
            <label class="flex flex-col gap-1">
              Release reason
              <input type="text" required x-model="reason" :disabled="lifecycleBusy(selectedTicket)" class="border border-edge-input bg-surface-900 rounded px-2 py-1 text-tx-2">
            </label>
            <button type="submit" :disabled="!reason.trim() || lifecycleBusy(selectedTicket)" class="border border-edge-input rounded px-2 py-1 text-tx-2">Release wait</button>
            <span>Allows the workflow to continue; does not approve a diff.</span>
          </form>
        </template>
        <template x-for="message in workflowReleaseMessages()" :key="message">
          <p role="status" x-text="message"></p>
        </template>
        <button type="button" x-show="workflowCanRefreshRelease()" :disabled="lifecycleBusy(selectedTicket)"
                @click="workflowRefreshRelease()" class="text-accent underline">Refresh wait details</button>
      </div>
    </template>
    <template x-if="selectedTicket?.graph_execution && !selectedTicket._graphDetailPending">
      <details>
        <summary>Full trace and runs</summary>
        <pre class="${TRACE_TEXT}" x-text="workflowTraceText(selectedTicket?.graph_execution) || 'No transitions recorded.'"></pre>
        ${RUN_ROWS('selectedTicket?.graph_execution')}
        <template x-for="previous in selectedTicket?.graph_execution?.previous_executions || []" :key="previous.execution_id">
          <details>
            <summary x-text="'Previous execution: ' + previous.execution_id"></summary>
            <pre class="${TRACE_TEXT}" x-text="workflowTraceText(previous)"></pre>
            <pre x-show="previous.wait" class="${TRACE_TEXT}" x-text="workflowWaitText(previous.wait, 'Retained wait')"></pre>
            ${RUN_ROWS('previous')}
          </details>
        </template>
      </details>
    </template>
    <details>
      <summary>Current configuration</summary>
      <p>These nodes and edges describe the configured graph. They do not prove which path a run took.</p>
      <pre class="${TRACE_TEXT}" x-text="workflowConfigText(workflowCurrentDefinition(selectedTicket))"></pre>
    </details>
    <button type="button" x-show="canRestartGraph()" @click="restartGraphWorkflow()" class="text-warn">Restart at configured entry</button>
  </section>`;
