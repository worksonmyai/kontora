package daemon

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/worksonmyai/kontora/internal/ticket"
	"github.com/worksonmyai/kontora/internal/web"
)

func TestDaemonDescriptionUpdate(t *testing.T) {
	const prefix = "# Original\n\nDetails  \r\n\n"
	const tail = " \t## Notes \r\n\n**legacy**\n\nold note\n## Later\nkeep\n## Notes\nsecond section"
	for _, tc := range []struct {
		name    string
		req     web.UpdateTicketRequest
		want    string
		invalid bool
	}{
		{name: "omitted", want: prefix + tail},
		{name: "round trip", req: web.UpdateTicketRequest{Description: new(prefix)}, want: prefix + tail},
		{name: "replace", req: web.UpdateTicketRequest{Description: new("# Changed")}, want: "# Changed\n" + tail},
		{name: "clear", req: web.UpdateTicketRequest{Description: new("")}, want: tail},
		{name: "full body replaces notes", req: web.UpdateTicketRequest{Body: new("# Full\n## Notes\nreplacement")}, want: "# Full\n## Notes\nreplacement"},
		{name: "full body clears", req: web.UpdateTicketRequest{Body: new("")}},
		{name: "mixed fields", req: web.UpdateTicketRequest{Body: new(""), Description: new(""), Pipeline: new("two-stage")}, invalid: true},
		{name: "delimiter", req: web.UpdateTicketRequest{Description: new("new\n \t## Notes \r\n"), Pipeline: new("two-stage")}, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			d := h.newDaemon(h.cfg)
			path := h.writeTicket("tst-desc.md", "---\nid: tst-desc\nstatus: open\npipeline: one-stage\ncustom: keep\n---\n"+prefix+tail)
			d.tickets["tst-desc"] = newTicketState(h.readTask("tst-desc.md"), path)
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			events, unsubscribe := d.Subscribe()
			defer unsubscribe()
			err = d.UpdateTicket("tst-desc", tc.req)
			if tc.invalid {
				require.Error(t, err)
				after, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, before, after)
				assert.Equal(t, prefix+tail, d.tickets["tst-desc"].ticket.Body)
				select {
				case <-events:
					t.Fatal("rejected update published an event")
				default:
				}
				return
			}
			require.NoError(t, err)
			stored := h.readTask("tst-desc.md")
			assert.Equal(t, tc.want, stored.Body)
			assert.Equal(t, tc.want, d.tickets["tst-desc"].ticket.Body)
			assert.Equal(t, "one-stage", stored.Pipeline)
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Contains(t, string(after), "custom: keep\n")
			if tc.want == prefix+tail {
				assert.Equal(t, before, after)
			}

			detail, err := d.GetTicket("tst-desc")
			require.NoError(t, err)
			description, descriptionTail := ticket.SplitDescription(tc.want)
			require.NotNil(t, detail.Description)
			require.NotNil(t, detail.DescriptionTail)
			assert.Equal(t, description, *detail.Description)
			assert.Equal(t, descriptionTail, *detail.DescriptionTail)
			assert.Equal(t, tc.want, detail.Body)
			rendered, err := d.renderTicketPrompt(h.cfg, "{{.Ticket.Description}}", stored, path, h.repoDir)
			require.NoError(t, err)
			if tc.want == "" {
				assert.Empty(t, rendered)
			} else {
				assert.Contains(t, rendered, tc.want+"\n\n---\nTask ID: tst-desc")
			}
			board := d.ListTickets(web.ListTicketsOptions{})
			require.Len(t, board, 1)
			assertDescriptionOmitted(t, board[0])
			select {
			case event := <-events:
				assertDescriptionOmitted(t, event.Ticket)
				assert.Empty(t, event.Ticket.Body)
				assert.Equal(t, detail.Notes, event.Ticket.Notes)
			default:
				t.Fatal("update did not publish an event")
			}
		})
	}
}

func assertDescriptionOmitted(t *testing.T, info web.TicketInfo) {
	t.Helper()
	assert.Nil(t, info.Description)
	assert.Nil(t, info.DescriptionTail)
	encoded, err := json.Marshal(info)
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(encoded, &fields))
	assert.NotContains(t, fields, "description")
	assert.NotContains(t, fields, "description_tail")
}

func TestDaemonDescriptionNoteOrder(t *testing.T) {
	for _, noteFirst := range []bool{false, true} {
		name := "description_then_note"
		if noteFirst {
			name = "note_then_description"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			d := h.newDaemon(h.cfg)
			const tail = "## Notes\n\n**legacy**\n\nold note\n## Later\nkeep\n"
			path := h.writeTicket("tst-order.md", "---\nid: tst-order\nstatus: open\n---\n# Old\n\n"+tail)
			d.tickets["tst-order"] = newTicketState(h.readTask("tst-order.md"), path)
			addNote := func() { require.NoError(t, d.AddNote("tst-order", web.AddNoteRequest{Text: "new note"})) }
			if noteFirst {
				addNote()
			}
			_, beforeTail := ticket.SplitDescription(h.readTask("tst-order.md").Body)
			require.NoError(t, d.UpdateTicket("tst-order", web.UpdateTicketRequest{Description: new("# New")}))
			_, afterTail := ticket.SplitDescription(h.readTask("tst-order.md").Body)
			assert.Equal(t, beforeTail, afterTail)
			if !noteFirst {
				addNote()
			}
			stored := h.readTask("tst-order.md")
			description, _ := ticket.SplitDescription(stored.Body)
			assert.Equal(t, "# New\n", description)
			notes := ticket.ParseNotes(stored.Body)
			require.Len(t, notes, 2)
			assert.Equal(t, "old note", notes[0].Text)
			assert.Equal(t, "new note", notes[1].Text)
			assert.Contains(t, stored.Body, "## Later\nkeep\n")
			assert.Equal(t, stored.Body, d.tickets["tst-order"].ticket.Body)
		})
	}
}
