# Filesystem storage

The default store implements two domain-owned interfaces, `agentwb.WhiteboardStore` and `agentwb.ImageStore`. Business domains never reach into another domain's persistence layer; composition occurs through services.

The root contains `whiteboards/`, `images/`, and `.readiness/`. Each capability ID has a private `0700` directory. Managed files are `0600`. The configured root and managed category directories are also forced to `0700`.

## Metadata schemas and paired generations

Images retain metadata schema 1. Legacy Markdown also uses schema 1 and references one `source-<32 hex>.md` generation with no context file. Such a resource remains readable and produces an empty `Context` value.

New and updated Markdown and all HTML use paired metadata schema 2. A resource directory contains:

```text
metadata.json
source-<32 hex>.md|html
context-<same 32 hex>.md
```

Schema-2 metadata records kind, seconds/nanoseconds timestamps, nullable expiration, and both filenames. The shared random token is validated, so metadata cannot combine source and context from different generations. Markdown uses `.md`, HTML uses `source-<32 hex>.html`, and images use `content-<32 hex>` plus extension and detected media type in schema-1 metadata.

The first update of legacy Markdown must provide both current/new Markdown and a non-empty context. It publishes a schema-2 pair while preserving immutable identity and creation time. There is no in-place or context-only migration. Schema-1 HTML is rejected; the pre-production HTML feature has no legacy migration path.

## Atomicity and failure handling

A whiteboard create or replacement writes source and context to separate exclusive random temporary files, sets `0600`, writes and syncs both, and publishes two immutable generation names with one token. It syncs the resource directory before writing and syncing temporary metadata. Atomic metadata rename is the commit point.

Readers follow only the pair named by verified metadata. A concurrent read therefore observes the complete old pair or complete new pair, never mixed source/context generations. A failure before metadata publication keeps the old pair on replacement; a failed create attempts to remove the incomplete capability directory.

After metadata publication, the new pair may already be visible even if a later directory sync or old-generation cleanup returns an error. The store preserves artifacts when publication state cannot be determined, avoiding deletion of a possibly committed pair. Periodic cleanup resolves recognized unreferenced artifacts. Custom stores must define equivalent atomic replacement and failure semantics.

If create rollback cannot verify that the generated capability directory is absent, the filesystem error implements `agentwb.UncertainCreateError` with `ResourceMayExist() == true`. Public whiteboard create methods return the generated result alongside that error so callers retain the ID for a check or delete. Ordinary create errors promise absence.

## Cleanup, expiration, and deletion

Expiration is evaluated during reads and periodic cleanup. Expired resources behave as not found. Whiteboard source and context share one metadata record, expiration, replacement, cleanup, and deletion lifecycle; the pair is never expired or deleted independently.

Cleanup removes expired resources, recognized unreferenced source/context/image generations, and recognized `.content-temp-*`, `.context-temp-*`, and `.metadata-temp-*` artifacts. It preserves the live referenced pair and unknown files. A metadata-free incomplete resource is removed only when its entries are recognized managed artifacts; unknown operator files prevent that cleanup. Do not manually edit a live root.

## Concurrency and containment

Locks are process-local and granular by namespace plus capability ID. Unrelated whiteboards/images proceed concurrently; ordinary reads use the resource's shared read lock, while mutation and cleanup take its exclusive lock. The supported deployment assumption is exactly one server process per filesystem root. Do not point multiple processes at the same root; there is intentionally no inter-process file lock.

All traversal is rooted through verified `os.Root` handles. Capability IDs and internal filenames are validated; symlink roots/resources, non-directories, traversal, identity swaps, and name collisions are rejected. Metadata and source/context/content generations are opened and reverified as regular files.

Custom stores must honor contexts, support concurrent calls, preserve `Context` and immutable identity/creation time on replacement, treat expiration consistently, return stable error codes, implement readiness, and make close idempotent. They must require Markdown and HTML source/context pairs while permitting legacy Markdown empty-context records to be read and migrated on first update. Atomic replacement, crash-safe durability, cleanup, and `UncertainCreateError` behavior are the custom implementation's responsibility.

## Private Page Agent image workspaces

Page Agent attachments use the separate owner-only agent state tree, not the public `images/` capability store above. Each native conversation workspace may contain a guarded `.agent-images/` directory with a `0600` manifest and validated PNG, JPEG, GIF, or WebP originals. The directory and its parents are `0700`; browser display names are metadata only and never become filesystem names.

Unclaimed uploads expire after 15 minutes. Startup and bounded periodic sweeping remove expired staging and recognized unreferenced files without following links or crossing the conversation workspace. Claimed images remain for queued messages, browser history previews, and provider-native resume, up to 512 MiB per workspace. Queue removal and definitive pre-acceptance rejection release the relevant files; uncertain acceptance retains them. Starting a new conversation archives the prior workspace, and archive deletion removes the complete workspace through the guarded conversation cleanup path.
