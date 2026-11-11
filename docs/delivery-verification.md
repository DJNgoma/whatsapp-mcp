# Delivery verification

## Failure cases to cover before implementation

- Server acceptance is not recipient delivery; old `messages.status` values cannot prove delivery.
- A receipt can arrive before the outbound message is saved, or before a status query.
- History sync or a later sender/retry receipt must not overwrite confirmed delivery/read evidence.
- Own-device, read-self, inactive, history-sync and unknown receipts must not count as recipient delivery.
- A group member receipt cannot establish delivery to the entire group.
- Phone-number and LID aliases must stay bound to the same recipient and exact message ID.
- Missing messages, wrong chats and inbound-only messages must not acquire someone else's receipt.
- SQLite failures, bridge disconnects, HTTP timeouts and missing message IDs must stay unverified; no automatic resend.
- A read-only wait must have a deadline, respect cancellation and perform no sends.
- Text, documents, audio and quoted replies must all return delivery evidence after confirmed sending.
- Preview and confirmation behavior must stay intact; checking delivery must not require send approval.

Verification uses real temporary SQLite stores and HTTP requests with injected WhatsApp receipt events. No customer message is resent for tests. Local/live validation evidence is kept outside Git; no account state or message bodies are committed.

## Implemented contract

Only WhatsApp recipient `delivered`, `read` or `played` receipts confirm direct
chat delivery. Retry/decryption failures remain `retrying`; server errors are
reported as `failed`, without automatic resend. Unknown/own-device receipt types
are ignored. Recipient aliases come from provider-bound receipt metadata or the
existing WhatsApp LID mapping, never a message-ID-only lookup. Read evidence
wins over delivery, retry and failure events. Group receipts remain partial.

A durable acceptance/receipt ledger survives receipt-before-message ordering and
history replacement. Both REST message history and receipt SSE events use its
folded status. Old optimistic `messages.status` labels cannot prove delivery.
The MCP send tools preserve confirmation and perform one send, then a read-only
15-second wait. They return `success` for server acceptance separately from
`delivery_confirmed`. A missing receipt never triggers a duplicate send.
`get_message_delivery_status` supports immediate checks or waits up to 30 seconds.

## Repeatable verification

From `whatsapp-bridge`, run `go test -json ./...` and `go vet ./...`.
`TestDeliveryHTTPReceiptLifecycle`, `TestDeliveryOwnGroupAndAliasReceipts`,
`TestDeliveryWaitBoundaries` and `TestDeliveryHistoryAndEventsUseVerifiedFold`
exercise temporary SQLite stores, real HTTP handlers and receipt events. The
wait test emits the resulting JSON status as a verifiable artifact. No real
WhatsApp account is required and the fixtures never send customer messages.
From `whatsapp-mcp-server`, run `uv run python -m unittest discover -s tests -v`.
Transport and receipt checks are mocked in Python contract tests.
