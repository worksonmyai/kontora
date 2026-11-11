# Getting started

Kontora runs coding agents on tickets. A ticket describes the work; a pipeline splits that work into steps such as implementation and review. The daemon is the service that starts agents and updates the board.

Start with one repository and one ticket. You do not need projects, hooks, epics, or recurring automations for this walkthrough.

## Install and check your tools

You need macOS or Linux, Git, tmux, and an installed coding agent. Sign in to the agent and check that it works before configuring Kontora. Agent usage uses your account and can incur charges.

```sh
git --version
tmux -V
```

Install with Homebrew:

```sh
brew tap worksonmyai/kontora https://github.com/worksonmyai/kontora
brew install kontora
kontora version
```

For a source build, see the [README](https://github.com/worksonmyai/kontora#install).

## Set up one agent

```sh
kontora setup
```

Choose your agent and the `default` pipeline. Leave the dashboard on `127.0.0.1` for local use. Consider setting concurrency to `1` while learning the workflow; the wizard starts at `3` workers.

The wizard writes two pipelines:

| Choice | What happens |
|--------|--------------|
| `default` | One implementation and test run, up to 30 minutes, then `human_review`. Failure pauses the ticket. |
| `implement-review-commit` | Implement, review, fix, and commit, then `human_review`. This uses more agent runs and commits before your review. |
| No default pipeline | Ordinary tickets without another pipeline run once and finish as `done` on success. There is no stage timeout. |

Both named pipelines remain available regardless of your selection. Neither asks the agent to push. A timeout limits duration, not spending.

The wizard's Claude arguments include `--dangerously-skip-permissions`. Kontora gives each ticket a separate checkout but does not restrict filesystem or network access. Configure agent permissions or an external sandbox before running on sensitive repositories. A `human_review` status does not prevent earlier steps from changing files or making network requests.

The usual config path is `~/.config/kontora/config.yaml`. Existing files are not overwritten. To have an agent help edit one, give it the output of `kontora setup --agent`. See [config selection](configuration.md) if you use a custom path.

Check the result:

```sh
kontora doctor
```

Resolve failed checks before starting. A busy web port can mean a daemon is already running. `doctor` does not check agent authentication or parse stage prompt templates.

## Start the daemon

```sh
kontora start
```

Keep this terminal open. The command runs until Ctrl-C. It can pick up existing `todo` tickets as soon as it starts.

Open http://127.0.0.1:8080 for the board. A different configured port changes that URL.

## Create a draft, then run it

In another terminal, enter your repository's main checkout:

```sh
cd ~/projects/myrepo
kontora new --status open "Add a health check endpoint"
```

The command prints an ID. Replace `TICKET_ID` in the remaining commands with that ID.

```sh
kontora edit TICKET_ID
```

Add enough detail to judge the result. For example:

> Add `GET /health` to the existing HTTP server. Return status 200 and the JSON body `{"status":"ok"}`. Add a test. Do not change authentication for other routes.

Check the ticket, then start it:

```sh
kontora view TICKET_ID
kontora run TICKET_ID
```

`open` is a draft. `todo` is eligible to run. Without `--status open`, `kontora new` creates a `todo` ticket, which the daemon normally picks up immediately.

Kontora creates the work branch from the repository's default branch, not necessarily your checked-out branch. Use `--base-branch BRANCH` at creation when the work must start elsewhere. Uncommitted changes in your checkout are not copied into the ticket's worktree.

## Follow the run

Use the web board or run `kontora` for the terminal board. These commands also help:

| Command | Use |
|---------|-----|
| `kontora view TICKET_ID` | Read status, notes, and the current stage. |
| `kontora logs TICKET_ID` | Read the latest stage output. |
| `kontora attach TICKET_ID` | Open the live terminal. |
| `kontora changes TICKET_ID` | List commits and committed file changes. |
| `kontora pause TICKET_ID` | Stop the ticket and keep its work for later. |

On failure, read the error and logs before retrying. With automatic pickup enabled, `kontora retry TICKET_ID` returns a paused ticket to the queue. See [retry](cli.md#kontora-retry-ticket_id) if automatic pickup is disabled.

## Review and finish

The starter pipelines stop in `human_review`. The `default` pipeline leaves changes uncommitted, so review its worktree directly.

Run `git worktree list` from the main repository. Find the branch shown by `kontora view TICKET_ID`, then enter that worktree's directory. Inspect the changes:

```sh
git status --short
git diff HEAD
```

Read any untracked files listed by `git status`; they are not included in `git diff`. Run any remaining checks in this worktree.

The Changes tab and `kontora changes` show committed branch changes only. They can be empty even when the agent changed files. They are useful after a commit, but do not replace review of uncommitted work.

Kontora does not automatically merge the branch. Commit and integrate the changes using your repository's normal process. Push or open a pull request only when you intend to publish.

After accepting and saving the work:

```sh
kontora done TICKET_ID
```

Completion removes a clean worktree but keeps the branch. Kontora preserves a worktree with uncommitted changes. `done` does not commit or merge for you.

For another edit pass, add requirements to the ticket's notes and retry the appropriate stage. If you use Plannotator, the [review integration](configuration.md#reviewing-the-code) can send feedback to a rework stage.

## Set defaults for your repositories

A project entry saves you from selecting a pipeline for every ticket. Add one under `projects:` in your existing config:

```yaml
projects:
  myrepo:
    path: ~/projects/myrepo
    pipeline: default
```

Use the main checkout's absolute path, or a path beginning with `~`. A linked worktree or subdirectory does not match that project entry. From elsewhere, create tickets with `--path ~/projects/myrepo`.

An optional project `agent` overrides the agent on every pipeline step. Leave it out when different steps should use different agents.

For repositories without a project entry, `default_pipeline` chooses the workflow. Without that setting, those tickets run standalone and can finish without human review. Config changes affect future tickets; they do not replace a pipeline already stored on a ticket.

See [Configuration](configuration.md) for complete examples, models, wrappers, and reload behavior. Read [Remote access](remote.md) before exposing the dashboard beyond your machine.
