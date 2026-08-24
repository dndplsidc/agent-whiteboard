# Releasing Agent Whiteboard

Agent Whiteboard releases use annotated semantic-version Git tags such as `v0.1.0`.

## Prepare the release commit

Use the commit intended for the release and verify that its worktree is clean. Run the checks applicable to the release:

```sh
go vet ./...
go test ./...
go test -race ./...
pnpm test
pnpm run check:assets
pnpm run test:browser
```

## Create the tag

```sh
scripts/release.sh v0.1.0
```

The script:

- accepts a `vMAJOR.MINOR.PATCH` semantic version, with an optional prerelease suffix;
- requires a clean Git worktree;
- refuses to replace an existing local tag;
- creates an annotated tag at the current commit;
- does not push the commit or tag.

Inspect the tag before publishing:

```sh
git show v0.1.0
```

Publish explicitly when the release commit is available from the remote:

```sh
git push origin HEAD
git push origin refs/tags/v0.1.0
```
