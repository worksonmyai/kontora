const CHIP = `<button type="button" class="detail-chip" :disabled="!relationKnown(ref) || !!ref.missing"
  :data-tip-t="relationKnown(ref) && !ref.missing ? (ref.title || ref.id) : 'Couldn’t resolve this ticket. Check the ID and configured ticket folders.'" @click="openTicketRef(ref)">
  <span x-show="relationKnown(ref) && !ref.missing" class="w-1.5 h-1.5 rounded-full shrink-0" :class="relationDotClass(ref)" aria-hidden="true"></span>
  <span x-text="relationKnown(ref) && !ref.missing ? ref.id : ref.id + ' · unresolved'"></span>
</button>`;

// Shared by the desktop and phone ticket views.
export const DETAIL_RELATIONSHIPS = `
<section class="detail-relationships detail-shared flex flex-col gap-2.5" data-detail-relationships aria-label="Depends on">
  <template x-if="isMobile && selectedTicket?.parent">
    <div class="detail-chip-row" aria-label="Parent">
      <span class="side-label mr-1">Parent</span>
      <div x-data="{ get ref() { return selectedTicket.parent } }">${CHIP}</div>
    </div>
  </template>
  <p x-show="chainError" role="status" class="text-xs text-warn" x-text="chainError"></p>
  <template x-if="chainVerdict() === 'cycle'"><div role="alert" class="detail-blocker text-err"><strong>Dependency cycle</strong><span class="break-all" x-text="(chain?.cycle || []).join(' → ')"></span></div></template>
  <template x-if="dependsOn().total || dependsOn().holder">
    <div class="flex items-baseline gap-2.5">
      <h2 class="side-label">Depends on</h2>
      <span x-show="dependsOn().total" class="font-mono text-[10.5px] text-tx-log" x-text="dependsOn().done + ' of ' + dependsOn().total + ' done'"></span>
    </div>
  </template>
  <template x-if="dependsOn().holder">
    <div class="depends-blocker" :class="dependsOn().holder.missing ? 'is-missing' : ''">
      <span class="depends-blocker-label">Waits for</span>
      <button type="button" class="depends-blocker-ref" :disabled="!!dependsOn().holder.missing || !relationKnown(dependsOn().holder)" @click="openTicketRef(dependsOn().holder)">
        <span class="depends-blocker-title" x-text="dependsOn().holder.title || dependsOn().holder.id"></span>
        <span class="depends-blocker-meta">
          <span class="text-tx-3" x-text="dependsOn().holder.id"></span>
          <span :class="relationHueClass(dependsOn().holder)" x-text="dependsOn().holder.missing ? 'unresolved' : statusLabel(dependsOn().holder.status)"></span>
        </span>
      </button>
      <button type="button" x-show="ticketCanControl(selectedTicket) && chainResumeMove()" :disabled="lifecycleBusy(dependsOn().holder)" @click="resumeChainHolder()" class="btn-outline depends-blocker-run">
        <svg width="9" height="9" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><polygon points="6 4 20 12 6 20"/></svg>
        <span x-text="chainResumeMove()?.label"></span>
      </button>
    </div>
  </template>
  <div x-show="dependsOn().others.length" class="detail-chip-row">
    <template x-for="ref in dependsOn().others" :key="ticketKey(ref)">${CHIP}</template>
  </div>
  <div x-show="relatedRefs().length" class="detail-chip-row">
    <span class="side-label mr-1">Related</span>
    <template x-for="ref in relatedRefs()" :key="ticketKey(ref)">${CHIP}</template>
  </div>
  <div x-show="(selectedTicket?.blocks || []).length" class="detail-chip-row">
    <span class="side-label mr-1">Unblocks</span>
    <template x-for="ref in selectedTicket?.blocks || []" :key="ticketKey(ref)">${CHIP}</template>
  </div>
  <template x-if="(selectedTicket?.children || []).length"><section class="detail-relation-group">
    <div class="flex gap-3 items-center"><h3 class="side-label">Children</h3><span class="text-xs text-tx-3" x-text="childRollup().done + ' of ' + childRollup().total + ' done'"></span></div>
    <div class="flex flex-col gap-1"><template x-for="c in childRows()" :key="c.id"><button type="button" class="detail-reference" :disabled="!relationKnown(c) || !!c.missing" @click="openTicketRef(c)">
      <span x-text="c.title || c.id"></span><span class="detail-reference-meta"><span class="font-mono" x-text="c.id"></span><span :class="relationHueClass(c)" x-text="c.missing || !relationKnown(c) ? 'Missing reference' : paletteStatusLabel(c.status)"></span><span x-text="childStageLine(c)"></span><span x-text="childElapsed(c)"></span></span>
    </button></template><button type="button" x-show="childrenHidden() > 0" @click="childrenExpanded = true" class="text-xs text-accent text-left" x-text="'Show ' + childrenHidden() + ' more children'"></button></div>
  </section></template>
</section>`;
