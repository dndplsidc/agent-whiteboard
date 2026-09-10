# Agent Whiteboard

Publish agent work as pages you can share, explore, and discuss with an agent.

Turn reports, diagrams, and prototypes into browser pages. **Page Agent** lets you continue the conversation right beside the content using your local Pi, Codex, or Cursor.

- **Publish rich content:** Markdown, Mermaid diagrams, interactive HTML, and images.
- **Point to what matters:** select text, sections, diagrams, or supported HTML components and add them directly to your message.
- **Keep working on the same page:** update content in place, retrieve its source, and find pages in the CLI's local catalog.
- **Host it yourself:** one Go binary, filesystem storage, and bundled browser assets.

## Quick start

**Let your coding agent set it up.** Copy this prompt:

```text
Set up Agent Whiteboard from https://github.com/dndplsidc/agent-whiteboard.
Install its agent-whiteboard and agent-whiteboard-setup skills for this project,
then follow them to install the CLI and run a local server with Page Agent.
Use an available local Pi, Codex, or Cursor provider and preserve existing setup.
Tell me if I need to reload my agent or complete provider login myself.
Publish a short example with a Mermaid diagram, expiring in one hour, and
verify the page and Page Agent connection without sending a model message.
Give me the link and any remaining manual steps.
```

Already have a server? Add its HTTPS URL to the prompt and ask your agent to use it instead of starting a local server.

The **CLI** publishes pages and runs the local **broker**, the process connecting your browser to your agent. The `agent-whiteboard-setup` skill handles installation and connections; `agent-whiteboard` handles publishing and updates. Skills alone do not install the CLI.

Requirements: macOS or Linux, Go 1.25 or 1.26, and a coding agent with shell access. Page Agent also needs a locally installed, authenticated Pi, Codex, or Cursor provider.

### Install the skills yourself

With Node.js and `npx` available, run in your project:

```sh
npx skills add dndplsidc/agent-whiteboard --skill agent-whiteboard
npx skills add dndplsidc/agent-whiteboard --skill agent-whiteboard-setup
```

Reload your agent, then paste the setup prompt above. Add `--global` to both commands to make the skills available across projects.

Prefer the terminal? Follow the [manual setup and publishing guide](docs/publishing.md).

## Try Page Agent

Ask your agent: **“Use agent-whiteboard to turn this explanation into a page with a diagram.”**

1. Open the returned link and the **Page Agent** panel.
2. Choose your provider, review the page context, and connect.
3. Select some text, choose **Add to message**, and ask “Can you explain this?”

You can also add entire sections, Mermaid diagrams, images, and supported HTML components. References make it easy to point at exactly what you mean without copying between windows.

Page Agent receives the full page source and **creator context**—the relevant goals, decisions, and assumptions recorded by the publishing agent. Selected references focus the question; they do not restrict the shared context to that excerpt. Your provider's normal tools and approval settings still apply.

See [Page Agent setup and troubleshooting](docs/page-agent.md) for provider configuration, origin trust, and broker management.

## Hosting and access

Agent Whiteboard is self-hosted. Local setup gives you a page on your own machine; sharing with others requires a server they can reach. This repository does not provide a hosted service. Each Page Agent reader runs their own local broker.

Links grant access: anyone holding a whiteboard's capability URL can read, update, or delete it, including its source and creator context. Keep sensitive content out of published pages. HTML is trusted active content. Read the [security model](docs/security.md) before deployment.

## CLI commands

Run commands as `agent-whiteboard <command>`. Use `--help` on any command for its flags.

| Commands | Use case |
| --- | --- |
| `create`, `update`, `get`, `delete` | Publish and manage Markdown or HTML pages. |
| `image upload`, `image update`, `image delete` | Publish and manage raster images. |
| `catalog list` | Find pages recorded by the CLI on this machine. |
| `serve` | Run a publishing server. |
| `agent serve`, `agent trust`, `agent daemon` | Connect Page Agent, manage trusted origins, and control the macOS broker service. |

See the [complete command reference](docs/publishing.md#cli-command-reference) for every subcommand, required arguments, and use cases.

## Documentation

| Goal | Guide |
| --- | --- |
| Install, publish, and manage pages from the CLI | [Publishing guide](docs/publishing.md) |
| Connect and troubleshoot Page Agent | [Page Agent guide](docs/page-agent.md) |
| Give your agent the instructions | [Setup skill](skills/agent-whiteboard-setup/SKILL.md) · [Publishing skill](skills/agent-whiteboard/SKILL.md) |
| Configure and deploy a server | [Configuration](docs/configuration.md) · [Security](docs/security.md) · [Storage](docs/storage.md) |
| Build an integration | [HTTP API](docs/http-api.md) · [Go API](docs/go-api.md) · [CLI JSON](docs/cli-json.md) |

Examples: [Markdown and Mermaid](docs/examples/diagram.md) · [Standalone HTML](docs/examples/standalone.html).

## Development

Build and test the Go application:

```sh
go build -trimpath -o ./bin/agent-whiteboard ./cmd/agent-whiteboard
go test ./...
go test -race ./...
go vet ./...
```

Browser asset development uses Node 24 and pnpm 11.4:

```sh
pnpm install --frozen-lockfile
pnpm test
pnpm run check:assets
pnpm run test:browser
```

See [Releasing Agent Whiteboard](docs/releasing.md) for the verified release checklist and annotated-tag helper.

Optional checks: [hosted-provider smoke test](docs/hosted-provider-smoke.md).

## License

Agent Whiteboard is available under the terms in [LICENSE](LICENSE).
