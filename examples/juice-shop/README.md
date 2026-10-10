# Juice Shop checkout case study

These scenarios exercise duplicate checkout in a local OWASP Juice Shop instance.
The source inspected for this example identifies itself as Juice Shop `20.2.0`.
Other versions or custom seed data may use different accounts, IDs, or access
rules. The [inventory regression demo](../vulnerable-inventory/README.md) requires
no credentials or application changes and is the quickest starting point.

| Scenario | Check | Evidence |
| --- | --- | --- |
| [history-scenario.yaml](history-scenario.yaml) | At most one checkout attempt returns HTTP 200 | Recorded checkout responses |
| [stock-scenario.yaml](stock-scenario.yaml) | Checkout decreases product stock by exactly one | Inventory before and after the operations |

Both scenarios use four attempts at concurrency four, ten trials, and reduction.
They are templates: replace the credential placeholders in local copies before
running. ConcurTest treats header values literally; it does not expand environment
variables in YAML.

## Prepare the local instance

1. Start a disposable Juice Shop instance on `http://127.0.0.1:3000` using the
   [upstream setup instructions](https://github.com/juice-shop/juice-shop#setup).
   Keep other clients idle during a run. This test creates orders, changes stock,
   and empties the selected basket.
2. Obtain a customer token and a separate accounting token through
   `POST /rest/user/login`. Its JSON request contains `email` and `password`;
   the response contains `authentication.token` and `authentication.bid`.
   The inspected seed data uses `jim@juice-sh.op` as the customer for basket 2
   and `accountant@juice-sh.op` for accounting. Credentials are defined in Juice
   Shop's `data/static/users.yml` and may differ in a customized instance.
   An administrator token does not satisfy the accounting role check.
3. Confirm that the customer's returned basket ID is `2`. If it differs, update
   every `/rest/basket/2/checkout` path and the setup body's `BasketId` in both
   local scenarios. Inventory paths assume record 6 belongs to product 6; the
   path selects an inventory record ID, not a product ID. Verify this after
   enabling inventory access in the following steps, and update all inventory
   paths if another record owns product 6.
4. Permit loopback access to the inventory record in the local test instance.
   The inspected Juice Shop source protects `/api/Quantitys/:id` with accounting
   authorization and an IP allowlist. In that checkout's `server.ts`, the
   `IpFilter` on this route can include `127.0.0.1`, `::ffff:127.0.0.1`, and `::1`
   alongside its existing entries. Rebuild the server and restart after changing
   it. This adjustment is part of preparing the local Juice Shop instance.
5. Obtain fresh tokens after the final restart and check inventory access with
   the accounting token. `GET /api/Quantitys/6` must return HTTP 200 with an integer
   `data.quantity` and the expected `data.ProductId`. A local `PUT` with
   `{"quantity":1000}` must also succeed. Stock changes must happen after startup:
   the inspected version recreates its database during startup.

A login request has this shape; replace both values with local credentials:

```json
{"email":"<EMAIL>","password":"<PASSWORD>"}
```

A 403 with `IpDeniedError` means the client address is outside the inventory
allowlist. A 403 from the role check means accounting authorization did not
succeed. An invalid-token error can indicate that a placeholder was left in a
header. Failed setup or observation is an execution error, not a demonstrated
invariant violation.

## Run the scenarios

From the ConcurTest repository root, create ignored local copies:

```bash
mkdir -p .local/juice-shop
cp examples/juice-shop/history-scenario.yaml .local/juice-shop/history-scenario.yaml
cp examples/juice-shop/stock-scenario.yaml .local/juice-shop/stock-scenario.yaml
chmod 600 .local/juice-shop/*.yaml
```

Replace `<CUSTOMER_TOKEN>` and `<ACCOUNTING_TOKEN>` in both local files with the
corresponding tokens, preserving the `Bearer ` prefix. Keep credential-bearing
files and raw login responses under `.local/`, which is ignored by Git.

```bash
go run ./cmd/concurtest run .local/juice-shop/history-scenario.yaml
go run ./cmd/concurtest run .local/juice-shop/stock-scenario.yaml
```

Each trial, including each reduction trial, runs these setup requests in order:

1. Perform one sequential checkout to drain previous basket contents.
2. Restore the selected product's stock to 1,000 with accounting authorization.
3. Add exactly one unit of product 6 to the customer's empty basket.

Draining the basket is a real checkout and creates an order. It happens before
stock restoration, so its decrement does not affect the trial's prepared stock.
The first drain can also consume stock for other products already in the basket.
Setup requests are excluded from the history invariant's attempt count. Any
failed setup step stops that trial before the concurrent checkout requests run.

This sequence controls the basket and selected inventory for the declared checks.
It does not reset previous orders, invoices, wallet changes, or all other Juice
Shop state. Use a fresh disposable instance between sessions; a scenario involving
those side effects needs additional preparation. A per-trial stock baseline
measures net change and does not provide full state isolation.

## Interpret the results

The history scenario counts checkout responses with HTTP 200. More than one
qualifying response violates its declared at-most-once checkout contract. This
establishes multiple successful HTTP attempts; it does not by itself establish
multiple paid orders containing the item. Juice Shop can also accept checkout of
an empty basket. Order contents require additional evidence. Empty-basket checkouts can also
make this history check fail sequentially. A sequential control helps determine
whether concurrency is necessary for a particular failure:

```bash
go run ./cmd/concurtest run --concurrency 1 --no-reduce .local/juice-shop/history-scenario.yaml
```

The stock scenario reads inventory after setup and again after all operations.
With one item in the prepared basket, `change: -1` requires exactly one unit to
be deducted. It observes `[data, quantity]`, not a basket item's quantity.
A zero or repeated decrement violates this check. A passing net change cannot
rule out lost updates or duplicate orders: several checkouts may read the same
stock and overwrite each other's decrements.

For example, two checkout requests could both return HTTP 200 after this sequence:

```text
Checkout A reads stock: 100
Checkout B reads stock: 100
Checkout A writes stock: 99
Checkout B writes stock: 99
```

The stock invariant (`change: -1`) passes because stock decreased by one. The
history invariant (`maximum_successful_attempts: 1`) fails because two checkout
requests succeeded. A passing invariant means the specific property held in the
tested executions; other business rules may still have been violated.

A previously captured local history run produced the following excerpt. It
shows the baseline invariant evidence and reduced settings from that run;
omitted sections and formatting reflect the earlier capture. It is not a
fresh verification of these credential-free templates, and the stock scenario
has no guaranteed outcome.

```text
ConcurTest · juice shop duplicate checkout

VIOLATED
10 of 10 completed trials demonstrated the violation.

Invariant
  basket must only be checked out once
  Expected        At most 1 successful attempt
  Observed        4 successful attempts

Reduction
  Status          REDUCED
  Smallest observed failure
    Attempts      2
    Concurrency   2
    Violations    10 of 10 trials
```

Exit code `1` indicates a demonstrated violation, `0` means the declared check
passed in every trial, and `2` indicates an errored or inconclusive run without
a demonstrated violation. HTTP authorization or setup errors should be resolved
before interpreting the checkout behavior.
