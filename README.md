# Wallet Transfer Service

Small Go/PostgreSQL service for wallet-to-wallet transfers with idempotent request handling, stored wallet balances, and double-entry ledger records.

## API

### `POST /transfers`

`POST /transfers` is also kept as a compatibility alias.

```json
{
  "idempotencyKey": "abc123",
  "fromWalletId": "22222222-2222-2222-2222-222222222222",
  "toWalletId": "11111111-1111-1111-1111-111111111111",
  "amount": 100
}
```

`amount` may be sent as either a JSON number or a string. The `Idempotency-Key` header is also accepted and takes precedence over the body field.

## Database Design

The schema uses three main tables:

- `wallets`: stores the current balance as `NUMERIC(20,8)` with `CHECK(balance >= 0)`.
- `transfers`: stores transfer intent, idempotency key, source/destination wallets, amount, status, and failure reason.
- `ledger_entries`: stores immutable double-entry rows with one `DEBIT` and one `CREDIT` for each successful transfer.

Important constraints:

- `transfers.idempotency_key` is unique, so retries cannot create duplicate transfers.
- wallet and ledger rows use foreign keys back to the owning transfer/wallet.
- transfer status is constrained to `PENDING`, `PROCESSED`, or `FAILED`.
- ledger amount must be positive, and the repository only accepts bulk inserts with exactly two entries.

## Idempotency Strategy

Each request is keyed by `idempotencyKey`. The service first checks for an existing transfer:

- same key and same payload returns the original result.
- same key and different payload returns a conflict.
- same key with a `PENDING` transfer locks the transfer row and either observes the settled state or resumes processing.

The unique constraint on `transfers.idempotency_key` is the durable guard for concurrent duplicate requests. If two requests race past the pre-check, one insert wins and the other receives a unique-constraint error, reloads the existing transfer, and returns that result.

## Transaction and Concurrency Handling

Transfer processing happens inside a database transaction after the transfer row exists. The service locks the transfer row with `FOR UPDATE`, then locks both wallet rows with `FOR UPDATE`.

Wallet locks are acquired in deterministic UUID order. This protects concurrent transfers touching the same pair of wallets from double spending while reducing deadlock risk. The debit, credit, ledger insert, and `PENDING -> PROCESSED` transition are committed together.

For expected business failures such as insufficient funds or missing wallets, the transfer is marked `FAILED` with a reason. For unexpected failures during balance or ledger work, the transaction rolls back and the transfer is marked failed outside the rolled-back transaction.

## Failure and Retry Behavior

- Duplicate successful requests return the original processed transfer.
- Duplicate failed requests return the original failed transfer.
- Duplicate pending requests are retry-safe because processing is guarded by the locked transfer row.
- A crash after creating a `PENDING` transfer can be recovered by retrying the same idempotency key.
- Ledger entries are only written for successful transfers and must be exactly two rows.

## Testing

Run:

```sh
go test ./...
```

The tests cover:

- request validation
- numeric and string transfer amounts
- `/transfers` routing
- idempotent replay behavior
- idempotency payload conflicts
- pending transfer retry/resume behavior
- ledger debit/credit correctness
- insufficient-funds and missing-wallet failures
- concurrent duplicate requests with the same idempotency key
- rollback/failure behavior when ledger insert fails

## AI Usage Note

AI assistance used: ChatGPT,Claude chatbot.

I used the tool to inspect the assignment, identify gaps, and make focused code/test/documentation changes. I reviewed the generated changes and verified them with `go test ./...`.

Prompts used in this session:

- `In production ready system were does idempotency key is stored? does it have separate table or it can be stored in transfer table only`
- `give proper PR description`
