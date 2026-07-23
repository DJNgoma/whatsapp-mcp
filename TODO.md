# Roadmap

## Recovery and synchronization

- [ ] Add a durable sync-job record with requested anchor, response count, and completion/error state.
- [ ] Add integration fixtures for LID/phone union queries, history anchors, call-log records, and duplicate media filenames.
- [ ] Improve diagnostics when an oldest-anchor request returns no earlier source records.

## API and media

- [ ] Continue documenting and testing the paginated API contract (`limit`, `offset`, stable ordering, and `phone_number`).
- [ ] Add a bridge endpoint for explicit media metadata status when a download is unavailable.

## Verification

- `go test ./...`
- Build and restart the bridge before live checks.
- Verify `/api/health` and confirm the store directory before any recovery request.
