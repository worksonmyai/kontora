// Recorded evidence and committed-file statistics, independent of top-level summary.
export const DETAIL_RUN_EVIDENCE = `
<section class="detail-run-evidence detail-shared" aria-label="Selected run evidence">
  <template x-if="detailSelectedRun()"><div class="flex flex-col gap-2">
    <div class="flex flex-wrap items-baseline gap-2 text-xs"><strong class="text-tx-2" x-text="historyLabel(detailSelectedRun())"></strong><span :class="runOutcomeClass(detailSelectedRun())" x-text="runOutcomeLabel(detailSelectedRun())"></span><span class="text-tx-3" x-text="formatElapsed(detailSelectedRun().started_at, detailSelectedRun().completed_at)"></span><span class="font-mono text-tx-3" :title="costTooltip(detailSelectedRunCost())" x-text="costLabel(detailSelectedRunCost())"></span></div>
    <p x-show="detailSelectedRun().outcome_reason" class="text-xs text-tx-3 whitespace-pre-wrap" x-text="detailSelectedRun().outcome_reason"></p>
    <div x-show="detailSelectedRun().summary" class="ticket-prose ticket-read text-sm" x-effect="setProse($el, detailSelectedRun().summary || '')"></div>
    <p x-show="historyProvenance(detailSelectedRun())" class="text-xs text-tx-3 whitespace-pre-wrap" x-text="historyProvenance(detailSelectedRun())"></p>
    <p x-show="detailSelectedRun().imported || isSecondary(selectedTicket)" class="text-xs text-tx-3">Imported history is readable here. Local terminal, transcript and stage logs are not available for this record.</p>
  </div></template>
  <p x-show="!detailSelectedRun() && executionActive(outputSubject())" class="text-xs text-tx-3">Run in progress. Recorded outcome and summary appear when it completes.</p>
  <p x-show="!detailSelectedRun() && !executionActive(outputSubject()) && !outputRuns().length" class="text-xs text-tx-3">No recorded runs yet.</p>
</section>`;

export const DETAIL_CHANGES = `
<section class="detail-changes detail-shared flex flex-col gap-4" aria-label="Committed changes">
  <div class="flex flex-wrap items-center gap-3"><h2 class="side-label">Committed changes</h2><span x-show="canCompareChanges() && ticketChanges?.base" class="text-xs text-tx-3" x-text="'vs ' + ticketChanges?.base"></span><button type="button" x-show="canCompareChanges()" :disabled="ticketChangesLoading" @click="fetchChanges(selectedTicket.id)" class="ml-auto text-xs text-accent">Refresh changes</button></div>
  <p x-show="isSecondary(selectedTicket)" class="text-xs text-tx-3">Changes are available for primary tickets only.</p>
  <p x-show="selectedTicket && !isSecondary(selectedTicket) && !selectedTicket.branch" class="text-sm text-tx-3">This ticket has no branch yet.</p>
  <p x-show="!isSecondary(selectedTicket) && selectedTicket?.branch && !selectedTicket.path" class="text-sm text-tx-3">Cannot compare commits: no repository path is stored for this ticket.</p>
  <p x-show="canCompareChanges() && ticketChangesLoading" role="status" class="text-sm text-tx-3">Loading changes…</p>
  <div x-show="canCompareChanges() && ticketChangesError" role="alert" class="text-sm text-err"><p class="whitespace-pre-wrap" x-text="ticketChangesError"></p><button type="button" :disabled="ticketChangesLoading" @click="fetchChanges(selectedTicket.id)" class="underline">Retry changes</button></div>
  <template x-if="canCompareChanges() && !ticketChangesLoading && !ticketChangesError"><div class="flex flex-col gap-4">
    <p x-show="!ticketChanges?.commits?.length && !ticketChanges?.files?.length" class="text-sm text-tx-3" x-text="diffEmptyText()"></p>
    <template x-if="ticketChanges?.commits?.length"><div class="flex flex-col gap-2"><h3 class="side-label">Commits</h3><template x-for="c in ticketChanges.commits" :key="c.sha"><div class="detail-commit"><span class="font-mono text-xs text-tx-3" x-text="c.sha"></span><span class="text-sm text-tx-2" x-text="c.subject"></span></div></template></div></template>
    <template x-if="ticketChanges?.files?.length"><div class="flex flex-col gap-2"><div class="flex flex-wrap gap-3 text-xs"><h3 class="side-label" x-text="churn().count + ' changed files'"></h3><span class="font-mono text-ok" x-text="'+' + churn().added"></span><span class="font-mono text-err" x-text="'−' + churn().deleted"></span></div><template x-for="f in ticketChanges.files" :key="f.path"><div class="detail-file font-mono text-xs"><span class="text-tx-2" x-text="f.path"></span><span class="text-ok" x-text="'+' + f.added"></span><span class="text-err" x-text="'−' + f.deleted"></span></div></template></div></template>
  </div></template>
  <template x-if="showReviewWorktree()"><section class="flex flex-col gap-2 text-sm text-tx-3">
    <h3 class="side-label">Review the worktree</h3>
    <dl class="flex flex-col gap-2">
      <div><dt>Repository</dt><dd class="font-mono select-text break-all" x-text="selectedTicket.path || 'No repository path stored for this ticket.'"></dd></div>
      <div><dt>Branch</dt><dd class="font-mono select-text break-all" x-text="selectedTicket.branch || 'No branch stored for this ticket.'"></dd></div>
    </dl>
    <template x-if="selectedTicket.path && selectedTicket.branch"><div class="flex flex-col gap-2">
      <p>On the daemon host, run <code>git worktree list</code> from the repository above. Find the worktree for the stored branch.</p>
      <p>In that worktree, run <code>git status --short</code> and <code>git diff HEAD</code>. Inspect untracked files separately.</p>
      <p>An empty committed comparison does not mean the worktree is clean. If Git lists no matching worktree, the browser cannot verify uncommitted changes.</p>
    </div></template>
  </section></template>
</section>`;

export const DETAIL_HISTORY = `
<section class="detail-history detail-shared flex flex-col gap-2" aria-label="Run history">
  <h2 class="side-label">History</h2>
  <template x-for="(h, hi) in selectedTicket?.history || []" :key="hi"><button type="button" @click="if (!isSecondary(selectedTicket)) openActivity(h.stage, historyArtifactRun(hi))" :disabled="isSecondary(selectedTicket)" class="detail-reference text-left">
    <span class="flex flex-wrap items-baseline gap-2 text-xs"><span :class="runOutcomeClass(h)" x-text="runOutcomeLabel(h)"></span><span class="text-tx-2" x-text="historyLabel(h)"></span><span x-show="h.imported" class="text-tx-3">Imported</span><span class="font-mono text-tx-3" :title="costTooltip(ticketRunCost(hi))" x-text="!isSecondary(selectedTicket) && !h.imported ? costLabel(ticketRunCost(hi)) : ''"></span></span>
    <span class="detail-reference-meta"><span x-text="'runner exit ' + h.exit_code"></span><span x-text="formatElapsed(h.started_at, h.completed_at)"></span><span x-text="historySettings(h)"></span></span>
    <span x-show="h.outcome_reason" class="text-xs text-tx-3" x-text="h.outcome_reason"></span><span x-show="h.invocation_id" class="text-xs text-tx-3 break-all" x-text="'Invocation ' + h.invocation_id"></span>
    <span x-show="isSecondary(selectedTicket) || h.imported" class="text-xs text-tx-3" x-text="[h.started_at, h.completed_at].filter(Boolean).map(at => formatAbsDate(at)).join(' / ')"></span>
    <span x-show="(isSecondary(selectedTicket) || h.imported) && h.summary" class="text-sm text-tx-3 whitespace-pre-wrap" x-text="h.summary"></span><span x-show="historyProvenance(h)" class="text-xs text-tx-3 whitespace-pre-wrap" x-text="historyProvenance(h)"></span>
  </button></template>
</section>`;
