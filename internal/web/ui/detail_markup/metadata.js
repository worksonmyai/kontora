// Constant presentation only. The existing editor/notification queues own saves.
export const DETAIL_METADATA = `
<section class="detail-metadata detail-shared" data-detail-metadata aria-label="Ticket details">
  <div class="flex items-center gap-3 mb-3">
    <h2 class="side-label">Ticket details</h2>
    <button type="button" x-show="!detailLoading && !editing && editorCanEdit(selectedTicket)" :disabled="lifecycleBusy(selectedTicket) || editorClaimLocked()" @click="startEditing()" class="text-xs text-accent">Edit details</button>
    <span role="status" class="text-xs text-tx-3" x-text="!editing || !editorCanEdit(selectedTicket) ? '' : editSubmitting ? 'Saving…' : editSaved ? 'Saved' : ''"></span>
  </div>
  <fieldset :disabled="detailLoading || lifecycleBusy(selectedTicket) || editorClaimLocked()" class="min-w-0">
    <div class="detail-metadata-primary">
      <label class="detail-field"><span>Pipeline</span>
        <template x-if="editing && editorCanEdit(selectedTicket) && selectedTicket?.kind !== 'epic'">
          <select aria-label="Pipeline" x-model="editForm.pipeline" @change="saveEdit()" x-effect="editForm.pipeline; configCache?.pipelines; $nextTick(() => { $el.value = editForm.pipeline || ''; })">
            <option value="" :selected="!editForm.pipeline">—</option>
            <template x-if="editForm.pipeline && !(configCache?.pipelines || []).includes(editForm.pipeline)"><option :value="editForm.pipeline" selected x-text="editForm.pipeline + ' (stored)'"></option></template>
            <template x-for="p in (configCache?.pipelines || [])" :key="p"><option :value="p" :selected="p === editForm.pipeline" x-text="pipelineLabel(p)"></option></template>
          </select>
        </template>
        <span x-show="!editing || !editorCanEdit(selectedTicket) || selectedTicket?.kind === 'epic'" class="detail-value" x-text="selectedTicket?.pipeline || '—'"></span>
      </label>
      <div class="detail-field min-w-0">
        <label id="edit-agent-label" :for="_agentPickerPopoverSupported ? agentPickerTriggerID('edit') : agentPickerFallbackID('edit')">Agent</label>
        <template x-if="editing && editorCanEdit(selectedTicket)"><div x-data="{ pickerHost: 'edit' }" x-html="agentPickerControl"></div></template>
        <span x-show="!editing || !editorCanEdit(selectedTicket)" class="detail-value" x-text="selectedTicket?.agent || 'Default (inherited)'"></span>
      </div>
      <label class="detail-field"><span>Stage</span>
        <template x-if="ticketCanControl(selectedTicket) && canSetStage()">
          <select aria-label="Stage" :value="selectedTicket?.stage || ''" @change="setStage($event.target.value)">
            <option value="" :selected="!selectedTicket?.stage">Set stage…</option>
            <template x-for="s in selectedTicket?.stages || []" :key="s"><option :value="s" :selected="s === selectedTicket?.stage" x-text="s"></option></template>
          </select>
        </template>
        <span x-show="!(ticketCanControl(selectedTicket) && canSetStage())" class="detail-value" x-text="displayStage() || '—'"></span>
        <span x-show="executionActive(selectedTicket)" class="text-xs text-tx-3">Pause and wait for the agent to stop before changing stages.</span>
      </label>
    </div>
    <details class="detail-disclosure mt-3">
      <summary>Details</summary>
      <div class="detail-metadata-long mt-3">
        <label class="detail-field"><span>Path</span>
          <template x-if="editing && editorCanEdit(selectedTicket)"><input type="text" aria-label="Path" x-model="editForm.path" @change="onEditPathChange()"></template>
          <span x-show="!editing || !editorCanEdit(selectedTicket)" class="detail-value" x-text="selectedTicket?.path || '—'"></span>
        </label>
        <label class="detail-field"><span>Branch</span>
          <template x-if="editing && editorCanEdit(selectedTicket)"><input type="text" aria-label="Branch" x-model="editForm.branch" @change="saveEdit()" :placeholder="branchPlaceholder(selectedTicket?.auto_branch)"></template>
          <button x-show="!editing || !editorCanEdit(selectedTicket)" type="button" class="detail-value text-left" @click="copyBranch(selectedTicket?.branch)" x-text="selectedTicket?.branch || '—'"></button>
        </label>
        <label class="detail-field"><span>Base branch</span>
          <template x-if="editing && editorCanEdit(selectedTicket)"><input type="text" aria-label="Base branch" x-model="editForm.base_branch" @change="saveEdit()" placeholder="repo default"></template>
          <span x-show="!editing || !editorCanEdit(selectedTicket)" class="detail-value" x-text="selectedTicket?.base_branch || 'repo default'"></span>
        </label>
        <div class="detail-field"><span>Notify</span>
          <button type="button" x-show="!notifyEditing" :disabled="!notifyCanEdit()" @click="openNotifyEditor()" class="detail-value text-left" x-text="notifyRailLabel()"></button>
          <template x-if="notifyCanEdit() && notifyEditing">
            <div class="flex flex-col gap-2">
              <div class="flex flex-wrap gap-3 items-center"><span role="status" class="text-xs text-tx-3" x-text="notifySaving ? 'Saving…' : notifySaved ? 'Saved' : ''"></span><button type="button" class="text-xs text-accent" :aria-expanded="notifyChips" @click="notifyChips = !notifyChips">Custom</button><button type="button" class="text-xs text-accent" @click="closeNotifyEditor()">Done</button></div>
              <div class="notify-seg notify-seg-sm notify-seg-stack" role="radiogroup" aria-label="notify me"><template x-for="m in notifyModes()" :key="m.key"><button type="button" role="radio" :aria-checked="notifyModeOf(notifyDraft) === m.key" @click="notifyPickMode(notifyDraft, m.key); notifyChips = false" x-text="m.label"></button></template></div>
              <div x-show="notifyChips" class="flex flex-wrap gap-2"><template x-for="s in notifyStatuses()" :key="s"><button type="button" class="notify-chip" :data-status="s" :data-on="notifyStatusOn(notifyDraft, s) ? '1' : null" :aria-pressed="notifyStatusOn(notifyDraft, s)" @click="notifyToggleStatus(notifyDraft, s)" x-text="s"></button></template></div>
              <template x-if="notifyPicksChannel(notifyDraft)"><div class="flex flex-col gap-2"><div class="flex flex-wrap gap-2"><template x-for="c in notifyChannelChips(notifyDraft)" :key="c.key"><button type="button" class="notify-chip" :data-kind="c.kind" :data-on="c.on ? '1' : null" :aria-pressed="c.on" @click="notifyToggleChannel(notifyDraft, c)" x-text="c.label"></button></template></div><p class="text-xs text-tx-3" x-text="notifyChannelHint(notifyDraft)"></p></div></template>
              <span class="text-xs text-tx-3 break-all" x-text="notifyEcho(notifyDraft)"></span><span x-show="notifyError" role="alert" class="text-xs text-err" x-text="notifyError"></span>
            </div>
          </template>
        </div>
        <div class="detail-field"><span>ID</span><button type="button" class="detail-value text-left" @click="copyTicketId(selectedTicket?.id)" x-text="copiedId ? '✓ copied' : selectedTicket?.id"></button></div>
        <div class="detail-field"><span>Created</span><span class="detail-value" x-text="formatAbsDate(selectedTicket?.created_at)"></span></div>
        <div class="detail-field" x-show="selectedTicket?.scheduled_at || canSchedule(selectedTicket)"><span>Starts</span>
          <button type="button" class="detail-value text-left" :disabled="!canSchedule(selectedTicket)" @click="isMobile ? openScheduleSheet(selectedTicket) : openScheduleEditor()" x-text="scheduleLabel(selectedTicket) || 'Not scheduled'"></button>
          <template x-if="!isMobile && canSchedule(selectedTicket) && scheduleEditing"><div class="flex flex-wrap gap-2"><input type="datetime-local" aria-label="Scheduled start" x-model="scheduleDraft" :min="scheduleMin()"><button type="button" :disabled="scheduleSubmitting" @click="submitSchedule()">Save</button><button type="button" @click="closeScheduleEditor()">Cancel</button></div></template>
          <button type="button" x-show="selectedTicket?.scheduled_at && canSchedule(selectedTicket)" :disabled="scheduleSubmitting" @click="clearTicketSchedule()" class="text-xs text-accent">Clear schedule</button>
          <span x-show="scheduleError" role="alert" class="text-xs text-err" x-text="scheduleError"></span>
        </div>
        <div class="detail-field" x-show="!isSecondary(selectedTicket) && selectedTicket?.last_log"><span>Log</span><button type="button" class="detail-value text-left" @click="copyCmd(selectedTicket?.last_log)" x-text="selectedTicket?.last_log"></button></div>
      </div>
    </details>
  </fieldset>
</section>`;
