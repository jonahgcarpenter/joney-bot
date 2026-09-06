# Oswald AI - Uncensored Digital Servant

> Fully local, fully uncensored, with no paid API required.

## Overview

Oswald AI is a local-first, self-hosted assistant that brings your chosen language model to iMessage, Discord, and Home Assistant.
It combines tools, private long-term memory, conversation continuity, image understanding, and connected services into one assistant that follows you across linked accounts while keeping you in control of your data.

## Features

- Chat through iMessage, Discord, or the [Home Assistant integration](https://github.com/jonahgcarpenter/has-oswald-conversation)
- Send text, images, animated GIFs, and replies with quoted context
- Search the web, check the current time, generate images with ComfyUI, and use connected MCP tools
- Remember your preferences, projects, and other useful details across conversations
- Keep continuity in long conversations and search earlier conversation details
- Link your accounts so your personal memory follows you across gateways
- List or forget your stored personal memories

## Memory

Oswald uses four memory layers:

- **Soul:** Operator-managed personality and policy loaded from `data/memory/soul/soul.md`.
- **Global memory:** Administrator-curated facts about Oswald that the model searches when relevant.
- **Personal memory:** Private preferences, projects, relationships, and other durable facts shared across your linked accounts.
- **Conversation memory:** Recent exchanges and summaries that preserve continuity within the current conversation.

Personal memories use two write paths after a response is delivered. The primary agent can stage a small number of model-assessed observations from the current user message, while a private background extractor looks for repeated implicit patterns across delivered turns. Structurally grounded observations are retained, confidence controls activation and later reinforcement, and all recalled or profile memory remains lower-authority user context.

Use `/reset` to start a new conversation without deleting personal memory. Use `/memories` to list or forget personal memories. Administrators manage shared facts with the `/global-memory` commands.

## Usage

### Discord/iMessage Bot

In DMs or direct chats, send any message:

```text
What is the current weather?
```

In server channels or group chats, mention Oswald:

```text
@Oswald What is the capital of France?
```

You can also reply to a message and mention Oswald to include that message as context:

```text
[Replying to Jonah: "The capital of the US is New York"]
@Oswald Is this true?
```

Replies to Oswald do not need another mention:

```text
[Reply to Oswald's message]
Can you elaborate on that?
```

## Startup Output

Oswald prints a large `OSWALD-AI` block-letter banner and the project's GitHub URL in bright magenta once per startup when stdout is a terminal. Normal Docker deployments, pipes, and redirected output omit the banner. An explicitly allocated TTY (such as `docker run -it`) enables it. The banner always uses ANSI color and requires a UTF-8-capable terminal.

Production logs remain uncolored, single-line JSON on stderr. First-administrator bootstrap instructions still use stdout, so JSON-only log collectors should ingest the container's stderr stream without TTY stream merging. The terminal banner announces startup, not readiness; it can also appear before a startup failure.

## Bootstrap

When no administrator exists, Oswald prints a process-local, single-use bootstrap code to the terminal. From an authenticated Discord, iMessage, or Home Assistant conversation, run:

```text
/bootstrap <code>
```

The account that submits the code becomes the first administrator. The code is consumed only after the account update succeeds. If the code is lost, restart Oswald to generate a replacement while no administrator exists. Once any administrator exists, startup no longer generates bootstrap codes.

## Commands

Commands are gateway-level slash commands. They are handled before requests reach the model.

In Discord servers and iMessage groups, slash commands must mention Oswald

### User Commands

| Command        | Usage                                                                                     | Description                                                                                                |
| -------------- | ----------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------- |
| `/help`        | `/help [command]`                                                                         | List available commands or show usage for one command.                                                     |
| `/bootstrap`   | `/bootstrap <code>`                                                                       | Claim initial administrator access.                                                                        |
| `/connect`     | `/connect [code\|cancel]`                                                                 | Create, confirm, or cancel a 10-minute account-link code.                                                  |
| `/disconnect`  | `/disconnect [account_number]`                                                            | List or disconnect linked accounts. The final account cannot be removed.                                   |
| `/reset`       | `/reset`                                                                                  | Clear the current conversation history and load the latest user profile.                                   |
| `/stop`        | `/stop`                                                                                   | Stop the currently running response in this conversation without removing queued prompts.                  |
| `/memories`    | `/memories list`, `/memories forget <id\|all>`                                            | List or forget your personal memories.                                                                     |
| `/mcp servers` | `/mcp servers`                                                                            | List your user-scoped MCP servers and their model-visible descriptions.                                    |
| `/mcp add`     | `/mcp add <name> <https-url> [auth-bearer=<token>] [header:<name>=<value>] <description>` | Add or update a server with a required description. URLs and headers, but not descriptions, are encrypted. |
| `/mcp remove`  | `/mcp remove <name>`                                                                      | Remove one of your MCP servers.                                                                            |
| `/mcp enable`  | `/mcp enable <name>`                                                                      | Enable one of your MCP servers.                                                                            |
| `/mcp disable` | `/mcp disable <name>`                                                                     | Disable one of your MCP servers.                                                                           |
| `/mcp test`    | `/mcp test <name>`                                                                        | Connect to one of your MCP servers and report its tool count.                                              |

### Admin Commands

| Command                 | Usage                                                                                            | Description                                                                                                  |
| ----------------------- | ------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------ |
| `/users`                | `/users`                                                                                         | List canonical users.                                                                                        |
| `/stop all`             | `/stop all`                                                                                      | Stop all active and queued foreground requests across every user and conversation.                           |
| `/user`                 | `/user <canonical_id>`                                                                           | Show one user's account, admin, and ban details.                                                             |
| `/admin`                | `/admin <canonical_id>`                                                                          | Grant administrator access to a user.                                                                        |
| `/unadmin`              | `/unadmin <canonical_id>`                                                                        | Remove administrator access from a user.                                                                     |
| `/ban`                  | `/ban <canonical_id> [reason]`                                                                   | Ban a user from using Oswald.                                                                                |
| `/unban`                | `/unban <canonical_id>`                                                                          | Unban a user.                                                                                                |
| `/deleteuser`           | `/deleteuser <canonical_id>`                                                                     | Immediately delete another user and their retained Oswald data.                                              |
| `/global-memory add`    | `/global-memory add <memory text>`                                                               | Add an administrator-curated fact shared with authenticated users; exact normalized duplicates are rejected. |
| `/global-memory list`   | `/global-memory list [page]`                                                                     | List global memories and their stable IDs.                                                                   |
| `/global-memory forget` | `/global-memory forget <id>`                                                                     | Permanently delete one global memory.                                                                        |
| `/mcp global servers`   | `/mcp global servers`                                                                            | List MCP servers visible to all users.                                                                       |
| `/mcp global add`       | `/mcp global add <name> <https-url> [auth-bearer=<token>] [header:<name>=<value>] <description>` | Add or update a global MCP server with a required model-visible description.                                 |
| `/mcp global remove`    | `/mcp global remove <name>`                                                                      | Remove a global MCP server.                                                                                  |
| `/mcp global enable`    | `/mcp global enable <name>`                                                                      | Enable a global MCP server.                                                                                  |
| `/mcp global disable`   | `/mcp global disable <name>`                                                                     | Disable a global MCP server.                                                                                 |
| `/mcp global test`      | `/mcp global test <name>`                                                                        | Connect to a global MCP server and report its tool count.                                                    |

## Roadmap

- [x] Uncensored tool calling model
- [x] Multi-gateway response routing and queuing
- [x] Persistent conversation history (multi-user context)
- [ ] Support for images, gifs, and files
  - [x] Images
  - [x] GIFs
  - [ ] Files
- [x] Global vs User defined MCP servers
- [ ] Scheduled task (cron tool)
- [ ] STT & TTS support

## License

MIT. See [`LICENSE`](LICENSE).
