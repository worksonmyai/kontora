# API Reference

When the web server is enabled, the following endpoints are exposed:

| Endpoint | Description |
|----------|-------------|
| `GET /` | Static dashboard UI. |
| `GET /api/tickets` | List all tickets (JSON). |
| `POST /api/tickets` | Create a new ticket (JSON body: `title`, `path`, optional `pipeline`, `status`). |
| `GET /api/tickets/{id}` | Get ticket details (JSON). |
| `DELETE /api/tickets/{id}` | Delete the ticket markdown file without worktree cleanup. Requires `X-Kontora-Confirm: delete-ticket-file`. Only deletes files inside `tickets_dir`. |
| `POST /api/tickets/{id}/pause` | Pause a running ticket. |
| `POST /api/tickets/{id}/retry` | Retry a paused ticket. |
| `POST /api/tickets/{id}/skip` | Skip the current pipeline stage. |
| `POST /api/tickets/{id}/set-stage` | Move ticket to a specific pipeline stage (`{"stage": "..."}` body). |
| `POST /api/tickets/{id}/move` | Set ticket status (`{"status": "..."}` body). |
| `GET /api/config` | Available repos and pipelines (JSON). |
| `GET /api/tickets/{id}/logs` | Get agent logs for a ticket (optional `?stage=` query param). |
| `POST /api/tickets/{id}/init` | Initialize a non-kontora ticket (`pipeline`, `path`, optional `agent`). |
| `PUT /api/tickets/{id}` | Update an open ticket's body or frontmatter fields. |
| `POST /api/tickets/upload` | Import tickets from raw `.md` file content (multipart form). |
| `GET /api/events` | Server-Sent Events stream of ticket updates. |
| `GET /ws/terminal/{id}` | Read-only WebSocket relay of a running agent's tmux session. |
| `GET /health` | Health check (returns 200). |
