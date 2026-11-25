import { DETAIL_METADATA } from './detail_markup/metadata.js';
import { DETAIL_RELATIONSHIPS } from './detail_markup/relationships.js';
import { DETAIL_NOTES } from './detail_markup/notes.js';
import { DETAIL_RUN_EVIDENCE, DETAIL_CHANGES, DETAIL_HISTORY } from './detail_markup/results.js';
import { DETAIL_WORKFLOW } from './detail_markup/workflow.js';

// Constant x-html companions, like the Agent picker. No editor, request or
// terminal ownership here; responsive shells mount only one set at a time.
export function kontoraDetailMarkup() {
  return {
    detailMetadataMarkup: DETAIL_METADATA,
    detailRelationshipsMarkup: DETAIL_RELATIONSHIPS,
    detailNotesMarkup: DETAIL_NOTES,
    detailRunEvidenceMarkup: DETAIL_RUN_EVIDENCE,
    detailChangesMarkup: DETAIL_CHANGES,
    detailHistoryMarkup: DETAIL_HISTORY,
    detailWorkflowMarkup: DETAIL_WORKFLOW,

    detailSelectedRun() {
      if (this.outputTab() === 'session') return null;
      return this.activityHistoryRecord();
    },

    detailSelectedRunCost() {
      const entry = this.detailSelectedRun();
      if (!entry || entry.imported || this.isSecondary(this.selectedTicket)) return null;
      const index = (this.outputSubject()?.history || []).indexOf(entry);
      return index < 0 ? null : this.ticketRunCost(index);
    },
  };
}
