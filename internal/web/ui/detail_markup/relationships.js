// Relationships stay readable without entity tooltips. Navigation remains in relations.js.
export const DETAIL_RELATIONSHIPS = `
<section class="detail-relationships detail-shared" data-detail-relationships aria-label="Relationships">
  <p x-show="chainError" role="status" class="text-xs text-warn mb-2" x-text="chainError"></p>
  <template x-if="chainVerdict() === 'cycle'"><div role="alert" class="detail-blocker text-err"><strong>Dependency cycle</strong><span class="break-all" x-text="(chain?.cycle || []).join(' → ')"></span></div></template>
  <template x-if="chainHolder() && chainHolder().direction === 'upstream'"><div class="detail-blocker">
    <strong>Blocked by</strong>
    <button type="button" class="detail-reference" :disabled="!relationKnown(chainHolder()) || !!chainHolder().missing" @click="openTicketRef(chainHolder())">
      <span x-text="chainHolder().title || chainHolder().id"></span><span class="detail-reference-meta"><span x-text="chainHolder().id"></span><span x-text="chainHolder().missing ? 'Missing reference' : paletteStatusLabel(chainHolder().status)"></span></span>
    </button>
    <button type="button" x-show="ticketCanControl(selectedTicket) && chainResumeMove()" :disabled="lifecycleBusy(chainHolder())" @click="resumeChainHolder()" class="text-xs text-accent" x-text="chainResumeMove()?.label"></button>
  </div></template>
  <template x-for="row in relationRows()" :key="row.key"><section class="detail-relation-group">
    <h3 class="side-label" x-text="({ parent: 'Parent', deps: 'Dependencies', blocks: 'Dependents', links: 'Related tickets' })[row.key] || row.label"></h3>
    <div class="flex flex-col gap-1"><template x-for="ref in row.refs" :key="ref.id"><button type="button" class="detail-reference" :disabled="!relationKnown(ref) || !!ref.missing" @click="openTicketRef(ref)">
      <span x-text="ref.title || ref.id"></span><span class="detail-reference-meta"><span class="font-mono" x-text="ref.id"></span><span :class="relationHueClass(ref)" x-text="ref.missing || !relationKnown(ref) ? 'Missing reference' : paletteStatusLabel(ref.status)"></span></span>
    </button></template></div>
  </section></template>
  <template x-if="(selectedTicket?.children || []).length"><section class="detail-relation-group">
    <div class="flex gap-3 items-center"><h3 class="side-label">Children</h3><span class="text-xs text-tx-3" x-text="childRollup().done + ' of ' + childRollup().total + ' done'"></span></div>
    <div class="flex flex-col gap-1"><template x-for="c in childRows()" :key="c.id"><button type="button" class="detail-reference" :disabled="!relationKnown(c) || !!c.missing" @click="openTicketRef(c)">
      <span x-text="c.title || c.id"></span><span class="detail-reference-meta"><span class="font-mono" x-text="c.id"></span><span :class="relationHueClass(c)" x-text="c.missing || !relationKnown(c) ? 'Missing reference' : paletteStatusLabel(c.status)"></span><span x-text="childStageLine(c)"></span><span x-text="childElapsed(c)"></span></span>
    </button></template><button type="button" x-show="childrenHidden() > 0" @click="childrenExpanded = true" class="text-xs text-accent text-left" x-text="'Show ' + childrenHidden() + ' more children'"></button></div>
  </section></template>
  <template x-if="chain && chainVerdict() !== 'cycle' && chainSummary().total > 1"><details class="detail-disclosure mt-3">
    <summary><span>Dependency chain</span> <span class="text-tx-3" x-text="chainSummary().text"></span></summary>
    <p class="text-xs text-tx-3 my-2" x-text="[chainSummary().place, chainSummary().state, chainSummary().releases].filter(Boolean).join(' · ')"></p>
    <div class="flex flex-col gap-1"><template x-for="row in chainRows()" :key="row.id"><button type="button" class="detail-reference" :class="row.is_self ? 'bg-accent/[.09]' : ''" :disabled="row.is_self || row.missing || !relationKnown(row)" @click="openTicketRef(row)">
      <span x-text="row.title || row.id"></span><span class="detail-reference-meta"><span class="font-mono" x-text="row.id"></span><span :class="relationHueClass(row)" x-text="row.missing ? 'Missing reference' : paletteStatusLabel(row.status)"></span><span x-text="row.holds_chain ? 'Holds the chain' : row.meta"></span></span>
    </button></template></div>
    <p x-show="chainSummary().total > (chain.nodes || []).length" class="text-xs text-tx-3 mt-2" x-text="(chainSummary().total - (chain.nodes || []).length) + ' more not shown'"></p>
  </details></template>
</section>`;
