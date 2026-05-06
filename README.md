# Wallet Transfer Service

Small Go/PostgreSQL service for wallet-to-wallet transfers with idempotent request handling, stored wallet balances, and double-entry ledger records.

## API

### `POST /api/transfers`

`POST /api/transfers` is the primary transfer endpoint. `POST /transfers` is also registered as a compatibility alias.

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

The unique constraint on `transfers.idempotency_key` is the durable guard for concurrent duplicate requests. If two requests race past the pre-check, one insert wins and the other receives a unique-constraint error, reloads the existing transfer, verifies the payload matches, and returns that result.

## Transaction and Concurrency Handling

Transfer processing happens inside a database transaction after the transfer row exists. The service locks the transfer row with `FOR UPDATE`, then locks both wallet rows with `FOR UPDATE`.

Wallet locks are acquired in deterministic UUID order. This protects concurrent transfers touching the same pair of wallets from double spending while reducing deadlock risk. The debit, credit, ledger insert, and `PENDING -> PROCESSED` transition are committed together.

For expected business failures such as insufficient funds or missing wallets, the transfer is marked `FAILED` with a reason while the locked transfer is still `PENDING`. Unexpected processing failures roll back the transaction and leave the transfer `PENDING` so the same idempotency key can safely retry instead of clobbering a concurrently processed transfer.

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

## Docker

Create your local environment file from the example and set your own password:

```sh
cp .env.example .env
```

Optional local seed wallets are disabled by default. Set `SEED_DATA=true` only for local/dev databases when you want the sample wallets inserted at startup.

Run the full service stack with PostgreSQL:

```sh
docker compose up -d --build
```

The API is available at `http://localhost:8080`, and PostgreSQL is exposed on `localhost:5432`.

Stop the stack:

```sh
docker compose down
```

Remove the database volume too:

```sh
docker compose down -v
```

The tests cover:

- request validation
- numeric and string transfer amounts
- `/api/transfers` routing and the `/transfers` compatibility alias
- idempotent replay behavior
- idempotency payload conflicts
- pending transfer retry/resume behavior
- ledger debit/credit correctness
- insufficient-funds and missing-wallet failures
- concurrent duplicate requests with the same idempotency key
- rollback/retry behavior when ledger insert fails

## AI Usage Note

AI assistance used: ChatGPT,Claude chatbot and OpenAI codex in IDE.

I used the tool to inspect the assignment, identify gaps, and make focused code/test/documentation changes. I reviewed the generated changes and verified them with `go test ./...`.

Prompts used in this session:

- `In production ready system were does idempotency key is stored? does it have separate table or it can be stored in transfer table only`
- `Give proper PR description`
- `List any missing edge case`
- `Write test case based on current transfer_service`

## Tradeoffs / Assumptions

### 1. Stored Balance vs Ledger-Derived Balance

#### Decision

Used stored balances in the `wallets` table while also maintaining immutable ledger entries.

#### Why?

- Balance checks become fast and efficient
- Avoids recalculating balances from entire ledger history
- Simplifies validation during concurrent transfers
- Better operational performance under load

#### Tradeoff

Maintaining stored balances introduces synchronization complexity because both:

- wallet balances
- ledger entries

must remain consistent.

This was addressed by executing balance updates and ledger writes inside the same database transaction.

#### Alternative Considered

Deriving balances directly from the ledger.

Pros:

- simpler accounting source of truth
- reduced risk of balance mismatch

Cons:

- expensive aggregation queries
- slower reads under high transaction volume
- more difficult concurrency validation

---

### 2. Pessimistic Locking vs Optimistic Locking

#### Decision

Used row-level pessimistic locking with:

```sql
SELECT ... FOR UPDATE
```

#### Why?

- Guarantees serialized balance updates
- Prevents overspending during concurrent debits
- Easier reasoning about transactional correctness
- Better fit for financial consistency requirements

#### Tradeoff

Pessimistic locking reduces parallelism because competing transactions must wait for locks.

However, correctness and consistency were prioritized over maximum throughput.

#### Alternative Considered

Optimistic locking using version fields.

Pros:

- higher concurrency
- reduced lock contention

Cons:

- retry complexity
- more difficult conflict handling
- greater risk of implementation bugs
- less deterministic under heavy contention

---

### 3. Synchronous Workflow vs Async/Event-Driven Processing

#### Decision

Used synchronous transfer execution within a single database transaction.

#### Why?

- Strong transactional guarantees
- Easier debugging and reasoning
- Simpler implementation for exactly-once semantics
- Reduced operational complexity

#### Tradeoff

This approach is less scalable than asynchronous processing because request latency depends on transaction completion.

#### Alternative Considered

Event-driven architecture using queues/outbox pattern.

Pros:

- improved scalability
- better throughput
- easier integration with distributed systems

Cons:

- eventual consistency complexity
- harder idempotency coordination
- increased operational overhead
- more complex failure recovery

---

### 4. Database-Enforced Idempotency

#### Decision

Used database unique constraints on `transfers` table.

#### Why?

- Prevents duplicate transfer creation at persistence layer
- Provides strong retry safety guarantees
- Makes duplicate detection deterministic
- Protects against race conditions during retries

#### Tradeoff

This approach introduces additional persistence and lookup overhead for each request.

The additional complexity was considered acceptable because idempotency correctness is critical in payment-style systems.

#### Alternative Considered

In-memory caching or distributed cache-based idempotency.

Pros:

- lower latency
- faster duplicate lookups

Cons:

- cache inconsistency risk
- weaker durability guarantees
- distributed invalidation complexity

---

### 5. Transaction Boundary Design

#### Decision

Wrapped the entire transfer workflow inside a single database transaction.

#### Why?

Ensures atomicity across:

- balance updates
- transfer creation
- ledger writes
- state transitions
- idempotency handling

#### Tradeoff

Larger transactions hold locks longer and may increase contention under high throughput.

This tradeoff was accepted to guarantee strong consistency.

#### Alternative Considered

Splitting operations into multiple smaller transactions.

Pros:

- shorter lock duration
- potentially higher throughput

Cons:

- risk of partial failures
- inconsistent ledger state
- difficult rollback coordination

---

### 6. Ordered Wallet Locking

#### Decision

Wallet rows are always locked in deterministic order.

#### Why?

Prevents cyclic lock acquisition patterns and reduces deadlock probability during concurrent transfers.

#### Tradeoff

Adds small implementation complexity but significantly improves concurrency stability.

---
