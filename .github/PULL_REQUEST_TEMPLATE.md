## What changes

<!-- The behaviour that changes, in one or two sentences. -->

## Why

<!-- The creator or integrator problem. Link the issue if there is one. -->

## Verification

<!-- Real commands and their real results. Do not paste expectations. -->

- [ ] `go build ./...` and `go vet ./...`
- [ ] `node scripts/check-generated.mjs` reports zero diff
- [ ] Unit tests for changed logic
- [ ] Integration tests against a disposable database, if state is touched
- [ ] Full sharded gate before merge to `main`

## Contract impact

- [ ] No contract change
- [ ] Contract changed: source updated, artefacts regenerated, and operation
      counts, capability table, source hash, profiles and SDK types updated together

## Remaining limitations

<!-- What this change deliberately does not do, and why. -->

## Checklist

- [ ] No credentials, private conversations, database dumps or generated runtime data
- [ ] No assertion weakened to make a gate pass
- [ ] English is primary; the Chinese companion is updated if user-facing docs changed
