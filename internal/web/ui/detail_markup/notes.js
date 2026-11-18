// Shared conversation presentation. Request ownership and drafts live in notes.js.
export const DETAIL_NOTES = `
<div class="detail-notes detail-shared h-full min-h-0 flex flex-col bg-surface-frame" data-detail-notes>
  <div class="shrink-0 px-6 pt-[14px]"><div class="max-w-[820px] mx-auto flex items-center gap-[10px]"><h2 class="side-label !text-tx-3">Notes</h2><span class="font-mono text-[11px] text-tx-3" x-text="noteCount() || ''"></span><div class="flex-1 h-px bg-surface-700/80"></div><span class="font-mono text-[10.5px] text-tx-3" x-text="noteLastAgo()"></span></div></div>
  <div class="detail-notes-scroll flex-1 min-h-0 overflow-y-auto px-6 pt-2 pb-[10px]" x-effect="if (detailNotesShouldFollow($el)) $nextTick(() => { if (detailMode() === 'notes') $el.scrollTop = $el.scrollHeight; })">
    <div class="max-w-[820px] mx-auto flex flex-col">
      <template x-for="draft in noteRecoveryItems()" :key="draft.id"><section class="my-2 rounded-[10px] border border-edge-card bg-surface-900 p-3" aria-label="Recoverable note draft">
        <p class="text-xs text-tx-2" x-text="draft.kind + (draft.unresolved ? ' · unresolved target' : ' · recoverable draft')"></p>
        <p class="mt-1 text-xs text-tx-3" role="status" x-text="draft.reason"></p>
        <pre x-show="draft.text" x-text="draft.text" aria-label="Retained note text" class="mt-2 whitespace-pre-wrap break-words select-text text-sm text-tx"></pre>
        <div class="detail-note-controls justify-end"><button type="button" @click="noteDismissRecovery(draft.id)">Discard</button><button type="button" x-show="['Add', 'Reply', 'Edit'].includes(draft.kind)" :disabled="draft.unresolved || !ticketCanEdit(selectedTicket) || (draft.kind === 'Add' ? !!noteDraft : !!(noteReplyDraft || noteEditDraft))" @click="noteRestoreRecovery(draft.id)">Restore to editor (not sent)</button></div>
      </section></template>
      <template x-if="!(selectedTicket?.notes || []).length"><div class="pt-8 pb-6 max-w-[560px] flex flex-col gap-3"><span class="side-label !text-tx-3">No notes yet</span><p class="text-xs leading-relaxed text-tx-3">Reply to an agent between stages. Notes stay in the ticket body. Stage prompts that include the body also include its notes. You can add a note before the first run.</p></div></template>
      <template x-for="item in notesFeed()" :key="item.key"><div>
        <template x-if="item.kind === 'event'"><div class="flex flex-wrap items-center gap-[10px] py-[6px] pl-[5px]" :data-pipe-color="item.color"><span class="note-dot"></span><span class="font-mono text-[10.5px] text-tx-3" x-text="item.label"></span><span x-show="item.meta" class="font-mono text-[10.5px] text-tx-3" x-text="item.meta"></span><span class="ml-auto font-mono text-[10.5px] text-tx-3" :title="formatAbsDate(item.at)" x-text="noteAgo(item.at)"></span></div></template>
        <template x-if="item.kind === 'note'"><article class="py-[7px]">
          <div class="flex gap-3 items-start bg-surface-900 border border-edge-card rounded-[10px] px-[14px] py-3">
            <span class="note-avatar" :data-pipe-color="noteAuthorHue(item.note)" x-text="noteInitial(item.note)"></span>
            <div class="flex-1 min-w-0">
              <div class="detail-note-byline flex flex-wrap items-baseline gap-2 min-h-[22px]"><span x-show="item.note.author" class="text-[13px] font-semibold text-tx-hi" x-text="item.note.author"></span><span x-show="noteRole(item.note)" class="note-role !text-tx-3" x-text="noteRole(item.note)"></span><span class="font-mono text-[10.5px] text-tx-3" :title="item.note.at" x-text="noteAgo(item.note.at)"></span><span x-show="item.note.edited" class="font-mono text-[10.5px] text-tx-3">(edited)</span></div>
              <div class="detail-note-controls" x-show="ticketCanEdit(selectedTicket)">
                <button type="button" @click="noteOpenReply(item.note)">Reply</button>
                <button type="button" x-show="noteIsMine(item.note)" @click="noteOpenEdit(item.note)">Edit</button>
                <button type="button" x-show="noteIsMine(item.note)" @click="noteAskDelete(item.note)" :disabled="noteDeleteSubmitting">Delete</button>
                <button type="button" @click="toggleReaction(item.note, noteNextEmoji(item.note))" aria-label="React to this note">+ React</button>
              </div>
              <template x-if="noteEditOn !== item.note.id"><div class="ticket-prose ticket-read mt-[3px] text-[13.5px]" x-effect="setProse($el, item.note.text)"></div></template>
              <template x-if="noteEditOn === item.note.id"><fieldset :disabled="!ticketCanEdit(selectedTicket) || noteEditSubmitting" class="detail-note-editor mt-2">
                <textarea aria-label="Edit note" x-model="noteEditDraft" x-init="$nextTick(() => $el.focus())" @keydown.enter="if (!$event.isComposing && ($event.metaKey || $event.ctrlKey)) { $event.preventDefault(); saveNoteEdit(); }" @keydown.escape.stop="noteCancelEdit()" class="w-full min-h-[64px] resize-y bg-transparent text-sm focus:outline-none"></textarea>
                <div class="detail-note-controls justify-end"><button type="button" @click="noteCancelEdit()">Cancel</button><button type="button" @click="saveNoteEdit()" :disabled="!noteEditDraft.trim()" x-text="noteEditSubmitting ? 'Saving…' : 'Save'"></button></div>
              </fieldset></template>
              <div x-show="(item.note.reactions || []).length" class="flex items-center gap-[6px] flex-wrap mt-[9px]"><template x-for="chip in (item.note.reactions || [])" :key="chip.emoji"><button type="button" class="note-chip" :data-pipe-color="noteReactionHue(chip)" :disabled="!ticketCanEdit(selectedTicket)" @click="toggleReaction(item.note, chip.emoji)" :aria-label="chip.emoji + ': ' + (chip.actors || []).join(', ')" :title="(chip.actors || []).join(', ')"><span class="note-chip-emoji" x-text="chip.emoji"></span><span x-text="(chip.actors || []).length"></span></button></template></div>
              <div x-show="item.replies.length || noteReplyOn === item.note.id" class="mt-3 pt-3 border-t border-surface-700/60 flex flex-col gap-3">
                <template x-for="r in item.replies" :key="r.id"><div class="flex gap-[10px] items-start">
                  <span class="note-avatar note-avatar-sm" :data-pipe-color="noteAuthorHue(r)" x-text="noteInitial(r)"></span>
                  <div class="flex-1 min-w-0">
                    <div class="flex flex-wrap items-baseline gap-2"><span x-show="r.author" class="text-[12.5px] font-semibold text-tx-2" x-text="r.author"></span><span x-show="noteRole(r)" class="note-role !text-tx-3" x-text="noteRole(r)"></span><span class="font-mono text-[10.5px] text-tx-3" :title="r.at" x-text="noteAgo(r.at)"></span><span x-show="r.edited" class="font-mono text-[10.5px] text-tx-3">(edited)</span></div>
                    <div class="detail-note-controls" x-show="ticketCanEdit(selectedTicket) && noteIsMine(r)"><button type="button" @click="noteOpenEdit(r)">Edit</button><button type="button" @click="noteAskDelete(r)" :disabled="noteDeleteSubmitting">Delete</button></div>
                    <template x-if="noteEditOn !== r.id"><div class="ticket-prose ticket-read mt-[2px] text-[13px]" x-effect="setProse($el, r.text)"></div></template>
                    <template x-if="noteEditOn === r.id"><fieldset :disabled="!ticketCanEdit(selectedTicket) || noteEditSubmitting" class="detail-note-editor mt-2"><textarea aria-label="Edit reply" x-model="noteEditDraft" x-init="$nextTick(() => $el.focus())" @keydown.enter="if (!$event.isComposing && ($event.metaKey || $event.ctrlKey)) { $event.preventDefault(); saveNoteEdit(); }" @keydown.escape.stop="noteCancelEdit()" class="w-full min-h-[64px] resize-y bg-transparent text-sm focus:outline-none"></textarea><div class="detail-note-controls justify-end"><button type="button" @click="noteCancelEdit()">Cancel</button><button type="button" @click="saveNoteEdit()" :disabled="!noteEditDraft.trim()" x-text="noteEditSubmitting ? 'Saving…' : 'Save'"></button></div></fieldset></template>
                  </div>
                </div></template>
                <template x-if="noteReplyOn === item.note.id"><fieldset :disabled="!ticketCanEdit(selectedTicket) || noteReplySubmitting" class="detail-note-editor">
                  <textarea x-model="noteReplyDraft" x-init="$nextTick(() => $el.focus())" placeholder="Reply…" aria-label="Reply" @keydown.enter="if (!$event.isComposing && ($event.metaKey || $event.ctrlKey)) { $event.preventDefault(); submitReply(); }" @keydown.escape.stop="noteCancelReply()" class="w-full min-h-[52px] resize-y bg-transparent text-sm focus:outline-none"></textarea>
                  <div class="detail-note-controls justify-end"><button type="button" @click="noteCancelReply()">Cancel</button><button type="button" @click="submitReply()" :disabled="!noteReplyDraft.trim()" x-text="noteReplySubmitting ? 'Replying…' : 'Reply'"></button></div>
                </fieldset></template>
              </div>
            </div>
          </div>
        </article></template>
      </div></template>
    </div>
  </div>
  <div class="detail-notes-composer shrink-0 border-t border-surface-700/70 px-6 pt-[14px] pb-4">
    <fieldset :disabled="!ticketCanEdit(selectedTicket) || noteSubmitting" class="max-w-[820px] mx-auto flex gap-3 items-start">
      <span class="note-avatar" data-pipe-color="mauve" x-text="(noteMe() || '·').charAt(0)"></span>
      <div class="flex-1 min-w-0 border border-edge-input bg-surface-900 rounded-[10px] px-3 py-[10px]">
        <textarea x-model="noteDraft" placeholder="Add a note…" aria-label="Add a note" @keydown.enter="if (!$event.isComposing && ($event.metaKey || $event.ctrlKey)) { $event.preventDefault(); submitNote(); }" @keydown.escape.stop class="w-full min-h-[52px] resize-y bg-transparent border-0 p-0 text-[13.5px] leading-[1.6] text-tx focus:outline-none"></textarea>
        <div class="flex flex-wrap items-center gap-3 mt-2"><span class="text-xs text-tx-3" x-text="ticketCanEdit(selectedTicket) ? 'Markdown supported' : 'Notes are read only'"></span><button type="button" class="note-send ml-auto disabled:opacity-50 disabled:cursor-not-allowed" :data-pipe-color="noteDraft.trim() ? 'mauve' : 'none'" @click="submitNote()" :disabled="!noteDraft.trim() || noteSubmitting" x-text="noteSubmitting ? 'Adding…' : 'Add note'"></button></div>
      </div>
    </fieldset>
  </div>
</div>`;
