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

const EXECUTION_HISTORY = `
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
    </template>`;

export const DETAIL_WORKFLOW_TRACE = `
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
    ${EXECUTION_HISTORY}
    <details>
      <summary>Current configuration</summary>
      <p>These nodes and edges describe the configured graph. They do not prove which path a run took.</p>
      <pre class="${TRACE_TEXT}" x-text="workflowConfigText(workflowCurrentDefinition(selectedTicket))"></pre>
    </details>
    <button type="button" x-show="canRestartGraph()" @click="restartGraphWorkflow()" class="text-warn">Restart at configured entry</button>
  </section>`;

export const DETAIL_WORKFLOW_RELEASE = `
  <template x-for="band in [workflowReleaseBand()].filter(Boolean)" :key="ticketKey(selectedTicket)">
    <section class="workflow-release" aria-label="Workflow release" :data-state="band.state">
      <div class="workflow-release-status" role="status">
        <span class="waiting-chip"><span class="waiting-dot" aria-hidden="true"></span><span x-text="band.label"></span></span>
        <span class="workflow-release-node" x-text="band.node"></span>
        <p x-text="band.message"></p>
        <span class="workflow-release-meta" x-show="band.meta" x-text="band.meta"></span>
      </div>
      <template x-for="inspected in (band.form ? [band.form] : [])" :key="inspected">
        <form x-data="{ reason: '' }" @submit.prevent="releaseWorkflowWait(inspected, reason, $el)" class="workflow-release-actions">
          <input type="text" required aria-label="Release reason" placeholder="Reason, e.g. checked the diff locally"
                 x-model="reason" :disabled="lifecycleBusy(selectedTicket)">
          <button type="submit" class="workflow-release-submit" :disabled="!reason.trim() || lifecycleBusy(selectedTicket)"
                  x-text="band.state === 'pending' ? 'Releasing…' : 'Release wait'"></button>
          <span class="workflow-release-help"><template x-if="band.target"><span>Continues to <code x-text="band.target"></code>. </span></template>Doesn't approve the diff.</span>
          <button type="button" class="workflow-release-copy" :disabled="lifecycleBusy(selectedTicket)" @click="workflowCopyReleaseCommand(band.command)"
                  x-text="workflowCopiedCommand === band.command ? '✓ copied' : '⧉ copy CLI command'"></button>
        </form>
      </template>
      <div class="workflow-release-actions" x-show="workflowCanRefreshRelease() || band.state === 'disarmed'">
        <button class="workflow-release-action" type="button" x-show="workflowCanRefreshRelease()" :disabled="lifecycleBusy(selectedTicket)"
                @click="workflowRefreshRelease()">↻ refresh wait</button>
        <span class="workflow-release-meta" x-show="workflowCanRefreshRelease()"
              x-text="band.state === 'conflict' ? 'Reloads the execution' : 'Looks for a receipt matching the request'"></span>
        <button class="workflow-release-action" type="button" x-show="band.state === 'disarmed'" :disabled="!!selectedTicket?._graphDetailPending"
                :aria-expanded="workflowWaitEvidenceOpen()" @click="workflowToggleWaitEvidence()"
                x-text="workflowWaitEvidenceOpen() ? 'hide evidence' : 'show evidence'"></button>
      </div>
      <p x-show="band.error" class="workflow-release-meta" role="status" x-text="band.error"></p>
      <template x-for="warning in band.warnings" :key="warning"><p class="workflow-release-meta" role="status" x-text="warning"></p></template>
    </section>
  </template>`;

const FACTS = entries => `
  <dl class="workflow-facts">
    <template x-for="([label, value]) in ${entries}" :key="label">
      <div class="contents"><dt x-text="label"></dt><dd x-text="value"></dd></div>
    </template>
  </dl>`;

export const DETAIL_WORKFLOW = `
  <section class="workflow-pane" aria-label="Workflow graph" data-workflow-graph
           x-data="{ get graph() { return workflowGraph(selectedTicket) }, get inspectedNode() { return workflowInspectedNode(this.graph) } }">
    <header class="workflow-graph-header">
      <h2 class="side-label">graph</h2>
      <span x-show="!selectedTicket?._graphDetailPending && selectedTicket?.graph_execution"
            x-text="selectedTicket?.graph_execution?.execution_id + ' · revision ' + selectedTicket?.graph_execution?.revision"></span>
      <div class="workflow-legend" aria-label="Graph legend">
        <span><i data-key="ran" aria-hidden="true"></i>ran</span>
        <span><i data-key="failed" aria-hidden="true"></i>failed</span>
        <span><i data-key="current" aria-hidden="true"></i>current</span>
        <span><i data-key="idle" aria-hidden="true"></i>not taken</span>
        <span><i data-key="next" aria-hidden="true"></i>possible next</span>
        <span x-show="graph.inspection" x-text="'⊘ ends at ' + graph.inspection"></span>
      </div>
      <button class="workflow-restart" type="button" x-show="currentView !== 'archive' && canRestartGraph()"
              :disabled="lifecycleBusy(selectedTicket) || !!selectedTicket?._graphDetailPending" @click="restartGraphWorkflow()"
              :data-tip-t="'Starts a new execution at ' + (graph.entry || 'the configured entry') + ' with fresh loop counters. History and worktree are kept.'">↻ restart at entry</button>
    </header>
    <p x-show="graph.unmatched" class="workflow-notice">Some recorded transitions are missing from the current configuration. See the full trace below.</p>
    <p x-show="selectedTicket?.last_error" class="workflow-notice" x-text="selectedTicket?.last_error"></p>
    <div class="workflow-graph-body" :data-empty="String(!graph.nodes.length)">
      <div class="workflow-graph-viewport" x-show="graph.nodes.length" role="region" aria-label="Workflow nodes" tabindex="0">
        <div class="workflow-canvas" :style="{width: graph.width + 'px', height: graph.height + 'px'}">
          <template x-for="edge in graph.edges" :key="edge.key">
            <div class="workflow-edge" :data-state="edge.state">
              <svg :width="graph.width" :height="graph.height" aria-hidden="true" focusable="false">
                <path :d="edge.d"></path><polygon :points="edge.arrow"></polygon>
              </svg>
              <span class="workflow-edge-label" :data-side="edge.labelSide" :style="{left: edge.labelX + 'px', top: edge.labelY + 'px'}" x-text="edge.label"></span>
            </div>
          </template>
          <template x-for="node in graph.nodes" :key="node.id">
            <button type="button" class="workflow-node" :data-node="node.id" :data-type="node.type" :data-state="node.state"
                    :aria-pressed="inspectedNode?.id === node.id" :aria-current="node.current ? 'step' : null"
                    :aria-label="node.id + ', ' + node.typeLabel + ', ' + node.statusLabel" :title="node.id + (node.sub ? ' · ' + node.sub : '')"
                    :style="{left: node.x + 'px', top: node.y + 'px', width: node.w + 'px', height: node.h + 'px'}"
                    @click="workflowSelectNode(node.id)">
              <template x-if="!['condition', 'loop', 'end'].includes(node.type)">
                <span class="workflow-node-shape workflow-node-box">
                  <span class="workflow-node-row"><strong x-text="node.id"></strong><span class="workflow-type" x-text="node.typeLabel"></span></span>
                  <span class="workflow-node-row"><span class="workflow-node-sub" x-text="node.sub"></span>
                    <span class="workflow-pips" aria-hidden="true">
                      <template x-for="visit in node.visits" :key="visit.key">
                        <span class="workflow-pip-group" :class="{ 'workflow-pip-retry': visit.attempts.length > 1 }">
                          <template x-for="a in visit.attempts" :key="a.key"><i :data-state="a.state" :title="a.outcome"></i></template>
                        </span>
                      </template>
                    </span>
                  </span>
                </span>
              </template>
              <template x-if="node.type === 'condition' || node.type === 'loop'">
                <span class="workflow-node-control">
                  <span class="workflow-node-shape"><span x-show="node.type === 'loop'" x-text="node.counter"></span></span>
                  <span class="workflow-node-label" :data-side="node.labelSide" aria-hidden="true">
                    <strong x-text="node.id"></strong><span x-text="node.sub"></span>
                    <template x-for="exit in node.exits" :key="exit.port"><span class="workflow-exit" :data-state="exit.state" :title="'Ends at ' + exit.to" x-text="exit.port + ' ⊘'"></span></template>
                  </span>
                </span>
              </template>
              <template x-if="node.type === 'end'">
                <span class="workflow-node-shape workflow-node-end"><span class="workflow-end-icon" aria-hidden="true" x-text="node.sink ? '⊘' : '■'"></span><strong x-text="node.id"></strong><span class="workflow-node-sub" x-text="node.sub"></span></span>
              </template>
              <span class="workflow-exits" x-show="!['condition', 'loop'].includes(node.type) && node.exits.length" aria-hidden="true">
                <template x-for="exit in node.exits" :key="exit.port"><span class="workflow-exit" :data-state="exit.state" :title="'Ends at ' + exit.to" x-text="'⊘ ' + exit.port"></span></template>
              </span>
            </button>
          </template>
        </div>
      </div>
      <aside class="rail-card workflow-inspector" aria-label="Node inspector">
        <p x-show="selectedTicket?._graphDetailPending" x-text="ticketDetailRefreshError ? 'Execution detail unavailable.' : 'Recorded execution details have not loaded yet'"></p>
        <p x-show="!graph.nodes.length">No graph is configured for this pipeline.</p>
        <template x-for="node in [inspectedNode].filter(Boolean)" :key="node.id">
          <div class="workflow-inspector-content" :data-state="node.state">
            <header class="workflow-inspector-header">
              <h3 x-text="node.id"></h3><span class="workflow-type" x-text="node.typeLabel"></span>
              <span class="waiting-chip"><span class="waiting-dot" aria-hidden="true"></span><span x-text="node.statusLabel"></span></span>
            </header>
            <p x-text="node.description"></p>
            ${FACTS('node.config')}
            <div class="workflow-inspector-records" x-show="!selectedTicket?._graphDetailPending">
              <p x-show="node.current && currentView !== 'archive' && !isSecondary(selectedTicket) && selectedTicket?.graph_execution?.wait?.kind !== 'human' && selectedTicket?.graph_execution?.wait"
                 x-text="workflowReleaseInstructions(selectedTicket)"></p>
              <h4 class="side-label" x-show="node.visits.length" x-text="node.visits.length + (node.visits.length === 1 ? ' visit' : ' visits')"></h4>
              <template x-for="visit in node.visits" :key="visit.key">
                <article class="workflow-visit">
                  <div class="workflow-visit-header"><strong x-text="visit.id"></strong><span x-text="visit.meta"></span></div>
                  <template x-for="a in visit.attempts" :key="a.key">
                    <div class="workflow-attempt" :data-state="a.state">
                      <div class="workflow-attempt-header">
                        <span x-show="visit.attempts.length > 1" x-text="'attempt ' + a.number + (a.continuation ? ' (continuation)' : '')"></span>
                        <span class="workflow-attempt-outcome" x-text="a.outcome.replaceAll('_', ' ')"></span>
                        <span class="workflow-result" x-show="a.result" x-text="a.result"></span>
                        <button type="button" x-show="a.row && workflowCanOpenRun(selectedTicket, a.row)" @click="openWorkflowRun(a.row)"
                                :aria-label="a.row ? 'Open Activity: ' + a.row.label : null">activity ↗</button>
                      </div>
                      <p x-show="a.reason" x-text="a.reason"></p>
                      <template x-if="a.evidence.length">
                        <details class="workflow-wait-evidence" :open="!!(a.wait.disarmed && node.current && workflowWaitEvidenceOpen())">
                          <summary>Wait evidence</summary>
                          ${FACTS('a.evidence')}
                        </details>
                      </template>
                    </div>
                  </template>
                </article>
              </template>
              <h4 class="side-label" x-show="node.decisions.length" x-text="node.decisions.length + (node.decisions.length === 1 ? ' decision' : ' decisions')"></h4>
              <template x-for="decision in node.decisions" :key="decision.key">
                <div class="workflow-visit workflow-attempt" :data-state="decision.tone">
                  <div class="workflow-attempt-header"><span x-text="'r' + decision.revision"></span><span class="workflow-result" x-text="decision.port"></span><span x-text="'→ ' + decision.to"></span></div>
                  <span x-show="decision.result" x-text="decision.result"></span>
                </div>
              </template>
              <p x-show="!node.visits.length && !node.decisions.length" x-text="!selectedTicket?.graph_execution ? 'No recorded execution.' : node.state === 'idle' ? 'Not reached in this execution.' : node.current ? 'Current execution node.' : 'No visit details recorded.'"></p>
            </div>
          </div>
        </template>
      </aside>
    </div>
    <div class="workflow-recorded text-xs text-tx-3" x-show="!selectedTicket?._graphDetailPending">
      <p x-show="selectedTicket?.graph_execution" class="${TRACE_TEXT}" x-text="workflowCursorText(selectedTicket)"></p>
      <details x-show="selectedTicket?.graph_execution?.wait && !graph.nodes.some(n => n.id === graph.current)">
        <summary>Current wait evidence</summary>
        <pre class="${TRACE_TEXT}" x-text="workflowWaitText(selectedTicket?.graph_execution?.wait)"></pre>
      </details>
      ${EXECUTION_HISTORY}
      <details>
        <summary>Current configuration</summary>
        <pre class="${TRACE_TEXT}" x-text="workflowConfigText(workflowCurrentDefinition(selectedTicket))"></pre>
      </details>
    </div>
  </section>`;
