// x-html mounts constants only. Token paints and recorded rows update their own
// Alpine bindings, without replacing the transcript host or composer.
const ASSISTANT_CONTEXT = `
<div x-show="assistantView === 'thread' && !companionThread() && assistantContext.length" x-cloak class="assistant-context flex items-center gap-1.5 px-3 py-1.5 shrink-0 border-b border-surface-700 overflow-x-auto">
  <span class="font-mono text-[10px] text-surface-600 shrink-0">Context</span>
  <template x-for="chip in assistantContext" :key="chip.key">
    <span class="font-mono text-[10.5px] px-1.5 py-0.5 rounded bg-surface-850 border border-edge-input text-tx-3 whitespace-nowrap" x-text="chip.label"></span>
  </template>
</div>`;

const ASSISTANT_HISTORY = `
<template x-if="!assistantVisibleThreads().length">
  <p class="font-mono text-[11.5px] text-surface-600 p-3 m-0">No chats yet.</p>
</template>
<template x-for="t in assistantVisibleThreads()" :key="t.id">
  <div class="flex items-center gap-2 px-2.5 py-2 rounded hover:bg-surface-900 transition-colors">
    <button type="button" @click="assistantSelectThread(t.id)" class="flex-1 min-w-0 text-left">
      <span class="block text-[12.5px] text-tx-2 truncate" x-text="t.title"></span>
      <span class="block font-mono text-[10.5px] text-surface-600 truncate" x-text="t.turns + (t.turns === 1 ? ' turn' : ' turns') + (t.writes ? ', ' + t.writes + ' changes' : '')"></span>
    </button>
    <button type="button" @click="assistantDeleteThread(t.id)" class="assistant-history-delete shrink-0 rounded text-surface-600 hover:text-err flex items-center justify-center" :aria-label="'Delete ' + t.title">Delete</button>
  </div>
</template>`;

const ASSISTANT_APPROVAL_ACTIONS = `
<div class="flex items-center gap-2">
  <button type="button" id="assistant-approve" @click="resolveAssistantGate(true)" class="font-mono text-[11.5px] h-[26px] px-3 rounded-[6px] bg-accent text-accent-fg hover:bg-accent-bright transition-colors">Approve</button>
  <button type="button" id="assistant-skip" @click="resolveAssistantGate(false)" class="font-mono text-[11.5px] h-[26px] px-3 rounded-[6px] bg-surface-850 border border-edge-input text-tx-3 hover:text-tx-2 transition-colors">Skip</button>
</div>`;

const assistantTranscript = (approvalActions = '') => `
<div x-show="assistantReadError" x-cloak role="status" class="font-mono text-[11px] text-warn">
  <span x-text="'Transcript updates unavailable. ' + assistantReadError"></span>
  <button type="button" x-show="assistantReadRetryable && assistantReadRetries > 4" @click="assistantRetryActivity()" class="underline">Retry</button>
</div>
<div x-show="companionThread()" x-html="companionEvidenceMarkup"></div>
<template x-if="!assistantMessages.length && !assistantEvents.length">
  <div class="m-auto max-w-[300px] text-center flex flex-col gap-2">
    <p class="text-[13px] text-tx-2 m-0" x-text="companionThread() ? 'Ask about the captured records.' : 'Ask about the board.'"></p>
    <p x-show="companionThread()" class="companion-note">Explanations use recorded evidence. The companion cannot run commands or change tickets.</p>
    <p x-show="!companionThread()" class="text-[11.5px] leading-[1.6] text-surface-600 m-0">Ask what is running, why a ticket is stuck, or what the last run changed. Writes use the same API as the CLI.</p>
  </div>
</template>
<template x-for="row in assistantThreadRows()" :key="row.key">
  <div :class="row.msg ? 'self-end max-w-[85%] rounded-[10px] px-2.5 py-1.5 bg-surface-850 border border-edge-input' : 'min-w-0'">
    <template x-if="row.msg">
      <div>
        <span class="block text-[12.5px] leading-[1.5] text-tx whitespace-pre-wrap" x-text="row.msg.text"></span>
        <span x-show="row.msg.error" class="block font-mono text-[10.5px] text-err mt-1" x-text="row.msg.error"></span>
      </div>
    </template>
    <template x-if="row.event">
      <div class="min-w-0">
        <template x-if="row.event.kind === 'text'">
          <div class="ticket-prose prose-assistant min-w-0 text-[12.5px]" x-effect="setProse($el, row.event.text)"></div>
        </template>
        <template x-if="row.event.kind === 'model'">
          <div class="font-mono text-[10.5px] text-surface-600" x-text="row.event.model"></div>
        </template>
        <template x-if="row.event.kind === 'tool'">
          <div class="assistant-tool-row rounded-[7px] border border-edge-input bg-surface-900">
            <button type="button" @click="toggleAssistantTool(row.event, row.index)" class="w-full flex items-center gap-2 px-2 py-1 text-left min-w-0" :aria-expanded="assistantToolExpanded(row.event, row.index) ? 'true' : 'false'">
              <span class="font-mono text-[10.5px] shrink-0" :class="assistantToolFailed(row.event) ? 'text-err' : 'text-ok'" x-text="assistantToolFailed(row.event) ? '✗' : '▸'"></span>
              <span class="font-mono text-[11px] text-tx-3 shrink-0" x-text="row.event.tool"></span>
              <span class="font-mono text-[11px] text-surface-600 truncate min-w-0" x-text="row.event.arg"></span>
              <span class="font-mono text-[10px] text-tx-faint shrink-0 ml-auto" x-text="assistantEventTime(row.event)"></span>
            </button>
            <template x-if="assistantToolExpanded(row.event, row.index)">
              <div class="px-2 pb-1.5">
                <pre class="font-mono text-[10.5px] leading-[1.5] text-tx-3 whitespace-pre-wrap m-0 max-h-[240px] overflow-y-auto" x-text="row.event.result || row.event.summary"></pre>
              </div>
            </template>
          </div>
        </template>
      </div>
    </template>
  </div>
</template>
<!-- Streaming text stays outside the recorded-row loop. -->
<template x-if="assistantPartial">
  <div id="assistant-partial" class="min-w-0 text-[12.5px] leading-[1.6] text-tx-2 whitespace-pre-wrap"><span x-text="assistantPartial"></span><span x-show="assistantPartialTyping" class="kblink" aria-hidden="true">▋</span></div>
</template>
<template x-if="assistantPartialTool">
  <div id="assistant-partial-tool" class="flex items-center gap-2 font-mono text-[11px] text-surface-600"><span class="shrink-0">▸</span><span x-text="assistantPartialTool"></span></div>
</template>
<template x-if="assistantStreaming && !assistantGate">
  <div class="flex items-center gap-2 font-mono text-[11px] text-surface-600" aria-live="polite"><span x-text="assistantWorkingLabel()"></span></div>
</template>
<template x-if="assistantGate">
  <div id="assistant-proposal" tabindex="-1" class="rounded-[9px] border border-warn/60 bg-surface-900 p-2.5 flex flex-col gap-2">
    <div class="flex items-baseline gap-2 min-w-0">
      <span class="font-mono text-[10px] text-warn shrink-0" x-text="assistantApprovalLabel()"></span>
      <span class="font-mono text-[11px] text-tx-3 break-words min-w-0" x-text="assistantGate.tool"></span>
    </div>
    <code class="font-mono text-[11px] leading-[1.5] text-tx-2 bg-surface-850 border border-edge-input rounded px-2 py-1.5 break-words" x-text="assistantGate.arg"></code>
    ${approvalActions}
  </div>
</template>`;

const ASSISTANT_SHEET_APPROVAL = `
<template x-if="assistantGate && assistantView === 'thread' && assistantThreadVisible()">
  <div class="assistant-approval-dock" role="group" aria-label="Pending approval">
    <button type="button" class="assistant-approval-details" @click="assistantRevealApproval()" aria-controls="assistant-sheet-proposal">Request details</button>
    ${ASSISTANT_APPROVAL_ACTIONS}
  </div>
</template>`;

const ASSISTANT_CONFIGURE = `
<template x-if="!assistantEnabled() && !companionThread() && assistantView === 'thread'">
  <div id="assistant-configure" class="shrink-0 border-t border-surface-700 p-3 flex flex-col gap-2">
    <p class="text-[12px] leading-[1.55] text-tx-3 m-0" x-text="assistantAgent ? assistantAgent.hint : 'Loading…'"></p>
    <p class="text-[11.5px] leading-[1.55] text-surface-600 m-0">Choose a configured Claude or Pi agent. Each chat uses one agent session.</p>
    <code class="font-mono text-[11px] leading-[1.55] text-tx-2 bg-surface-850 border border-edge-input rounded-[7px] px-2.5 py-2 whitespace-pre">assistant:
  agent: claude
  autonomy: ask</code>
    <button type="button" @click="gotoView('settings')" class="self-start font-mono text-[11.5px] h-[26px] px-3 rounded-[6px] bg-surface-850 border border-edge-input text-tx-3 hover:text-tx-2">Open settings</button>
  </div>
</template>`;

const ASSISTANT_COMPOSER = `
<!-- Composer -->
<template x-if="(assistantEnabled() || companionThread()) && assistantView === 'thread' && assistantThreadVisible()">
  <div class="assistant-composer shrink-0 border-t border-surface-700 p-2 relative" @keydown.escape="if (assistantMenu || assistantAutonomyOpen) { $event.preventDefault(); $event.stopPropagation(); assistantMenu = null; assistantAutonomyOpen = false; }">
    <div x-show="assistantMenu" x-cloak id="assistant-menu" class="assistant-suggestions absolute left-2 right-2 bottom-full mb-1 max-h-[220px] overflow-y-auto rounded-[9px] border border-surface-700 bg-surface-850 py-1" role="listbox" aria-label="Composer suggestions">
      <template x-for="(row, ri) in (assistantMenu ? assistantMenu.rows : [])" :key="row.id">
        <button type="button" @click="acceptAssistantMenu(row)" role="option" :id="assistantComposerID('suggestion-' + ri)" :aria-selected="assistantMenu && ri === assistantMenu.index ? 'true' : 'false'" class="w-full flex items-baseline gap-2 px-2.5 py-1 text-left min-w-0" :class="assistantMenu && ri === assistantMenu.index ? 'bg-surface-800' : ''">
          <span class="font-mono text-[11.5px] text-tx-2 shrink-0" x-text="row.label"></span>
          <span class="text-[11px] text-surface-600 truncate min-w-0" x-text="row.sub"></span>
        </button>
      </template>
    </div>
    <textarea id="assistant-input" x-model="assistantDraft" @input="assistantDraftChanged()" @keydown="assistantComposerKey($event)" @compositionstart="assistantCompositionStart()" @compositionend="assistantCompositionEnd()" @blur="assistantCompositionReset()" rows="2" placeholder="Ask about the board. / for commands, @ for a ticket" aria-label="Message the assistant" role="combobox" aria-multiline="true" :aria-controls="assistantComposerID('menu')" :aria-expanded="!!assistantMenu" :aria-activedescendant="assistantMenu ? assistantComposerID('suggestion-' + assistantMenu.index) : null" aria-autocomplete="list" class="w-full resize-none rounded-[8px] bg-surface-900 border border-edge-input px-2.5 py-2 text-[12.5px] leading-[1.5] text-tx placeholder:text-surface-600 outline-none focus:border-edge-hover transition-colors"></textarea>
    <div class="assistant-composer-actions flex items-center gap-2 mt-1.5">
      <div class="relative">
        <span x-show="companionThread()" class="companion-note">Observation only</span>
        <button type="button" id="assistant-autonomy" x-show="!companionThread()" @click="assistantAutonomyOpen = !assistantAutonomyOpen" class="font-mono text-[11px] h-[24px] px-2 rounded-[6px] bg-surface-900 border border-edge-input text-tx-3 hover:text-tx-2 flex items-center gap-1.5" :aria-expanded="assistantAutonomyOpen ? 'true' : 'false'" x-text="assistantAutonomyLabel()"></button>
        <div x-show="assistantAutonomyOpen" x-cloak @click.outside="assistantAutonomyOpen = false" class="assistant-autonomy-menu absolute left-0 bottom-full mb-1 w-[220px] rounded-[9px] border border-surface-700 bg-surface-850 py-1 z-10">
          <template x-for="mode in assistantAutonomies()" :key="mode.key">
            <button type="button" @click="setAssistantAutonomy(mode.key)" class="w-full text-left px-2.5 py-1.5 hover:bg-surface-800 transition-colors" :class="assistantEffectiveAutonomy() === mode.key ? 'bg-surface-800' : ''">
              <span class="block font-mono text-[11.5px] text-tx-2" x-text="mode.label"></span>
              <span class="block text-[10.5px] leading-[1.4] text-surface-600" x-text="mode.hint"></span>
            </button>
          </template>
        </div>
      </div>
      <span class="assistant-agent font-mono text-[10px] text-tx-faint truncate min-w-0" x-text="assistantAgent ? assistantAgent.agent : ''"></span>
      <button type="button" x-show="assistantStreaming" @click="stopAssistantTurn()" class="ml-auto font-mono text-[11px] h-[24px] px-2.5 rounded-[6px] bg-surface-900 border border-edge-input text-tx-3 hover:text-err">Stop</button>
      <button type="button" id="assistant-send" x-show="!assistantStreaming" @click="sendAssistantMessage()" :disabled="assistantSending || !assistantDraft.trim() || (companionThread() && !companionCanAsk())" class="ml-auto font-mono text-[11.5px] h-[24px] px-3 rounded-[6px] bg-accent text-accent-fg hover:bg-accent-bright disabled:opacity-40">Send</button>
    </div>
  </div>
</template>`;

// Both shells are exclusive. Keep the desktop IDs used by focus and tests;
// phone IDs are computed once here, never from reactive thread state.
const sheetIDs = markup => markup.replaceAll('id="assistant-', 'id="assistant-sheet-');

export function kontoraAssistantMarkup() {
  return {
    assistantContextMarkup: ASSISTANT_CONTEXT,
    assistantHistoryMarkup: ASSISTANT_HISTORY,
    assistantTranscriptMarkup: assistantTranscript(ASSISTANT_APPROVAL_ACTIONS),
    assistantConfigureMarkup: ASSISTANT_CONFIGURE,
    assistantComposerMarkup: ASSISTANT_COMPOSER,
    assistantSheetTranscriptMarkup: sheetIDs(assistantTranscript()),
    assistantSheetApprovalMarkup: sheetIDs(ASSISTANT_SHEET_APPROVAL),
    assistantSheetConfigureMarkup: sheetIDs(ASSISTANT_CONFIGURE),
    assistantSheetComposerMarkup: sheetIDs(ASSISTANT_COMPOSER),
  };
}
