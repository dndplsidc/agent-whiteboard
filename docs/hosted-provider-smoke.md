# Optional hosted-provider smoke test

This procedure is manual and optional. It is excluded from CI because CI must not use public networks, hosted services, or credentials.

1. Use a disposable, non-sensitive Markdown fixture and creator context. Do not use production capability URLs, private source, personal data, or secrets.
2. Authenticate with Pi itself using its provider-native login flow. Do not pass API keys or tokens through agent-whiteboard flags, environment variables, prompts, or logs.
3. Enable `viewer.local_agent.enabled`, add the exact HTTPS publishing origin with `agent-whiteboard agent trust add`, and start the publishing server and `agent-whiteboard agent serve`.
4. In current Chrome, open the Markdown capability, grant Local Network Access if prompted, inspect the Page context disclosure, and select **Connect to Pi**. Confirm that connecting alone produces no hosted model request.
5. Send one distinctive, non-sensitive reader message. Confirm that the response streams in the drawer, Pi reports the intended model, and the provider receives one turn containing the complete Markdown/context envelope and reader message.
6. Reload and reconnect. Send a continuation and confirm that the page context is not resent when the revision is unchanged. Interrupt a separate test turn and confirm it is not automatically replayed.
7. Exercise New, archive restore, and archive deletion only with disposable sessions. Confirm deletion before removing any local test state.
8. Stop the broker, delete the disposable whiteboard and local test sessions, and remove the test origin from the trust list if it is no longer needed.

Do not capture provider request bodies, auth files, transcripts, capability IDs, or model output in shared logs or bug reports. Record only sanitized pass/fail observations and version information.
