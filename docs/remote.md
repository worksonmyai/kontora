# Remote access

The CLI and web board can control a Kontora daemon on another machine. Use a trusted private network, such as a Tailscale tailnet. Keep the default loopback listener if you only need local access.

Anyone who can access an unprotected API can start agents with the daemon's permissions. Worktrees do not limit those permissions. Use a shared token for remote access, and TLS for any connection outside an encrypted private network.

## Configure the daemon host

Set `web.host` to the host's private-network address. Replace the example address below with yours:

```yaml
web:
  enabled: true
  host: 100.64.0.10
  port: 8080
```

Provide a long random secret through `KONTORA_WEB_TOKEN` when starting the daemon. Supply it through your shell or service manager without storing it in a shared config or shell history. This variable overrides `web.token`.

```sh
kontora start
```

Changing the listener or token requires a restart. The daemon must have Git, tmux, the agent binaries, agent credentials, and the repositories it will work on.

If clients use a DNS name that is not the machine's hostname, add it to `web.allowed_hosts`. See [Host and origin checks](configuration.md#host-and-origin-checks), especially when using a reverse proxy. A Host allowlist does not replace authentication.

For a container, publish the port on loopback unless you have configured remote protection, for example `-p 127.0.0.1:8080:8080`. The image listens on all container interfaces. Repository paths must exist inside the container, and configuration, credentials, tickets, logs, and worktrees need appropriate persistent mounts.

## Connect from the CLI

Set `KONTORA_URL` to the daemon URL and `KONTORA_TOKEN` to the same secret. The client token variable is different from the daemon's `KONTORA_WEB_TOKEN`.

With those variables set:

```sh
kontora ls --static
kontora view TICKET_ID
kontora logs TICKET_ID
kontora attach TICKET_ID
```

Replace `TICKET_ID` with an ID from the list. Remote mode needs no local config file or local copy of the repository.

Paths refer to the daemon host, not your client machine. For example, after replacing this repository path:

```sh
kontora new --status open --path /srv/projects/myrepo "Add a health check endpoint"
```

See the [CLI reference](cli.md#remote-mode) for supported commands. Setup, daemon startup, and operations on local ticket files must run on the daemon host.

## Connect from a browser

Open `http://HOST:PORT/?token=SECRET` once, replacing all three placeholders. Kontora stores a `kontora_token` cookie for later API and terminal requests. Treat this URL as a credential; do not share it or put it in screenshots.

When a token is configured, `/api/*` and `/ws/*` require authentication. The static page and `GET /health` stay public. Without a token, the API is open to clients that can reach it and pass the Host/origin checks.

## Troubleshooting

| Symptom | Check |
|---------|-------|
| Connection refused | Confirm the daemon is running and its address, port, and firewall permit the connection. |
| `401 Unauthorized` | Check the client token against the daemon's token. Restart after changing the daemon token. |
| `403 Forbidden` | Check `web.allowed_hosts` and any proxy's Origin headers. |
| Ticket path not found | Use a repository path on the daemon host or inside its container. |

The daemon serves plain HTTP. A bearer token sent over unencrypted HTTP can be read by anyone observing that traffic. Use TLS unless your private network already encrypts the connection.
