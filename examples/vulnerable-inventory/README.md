# Inventory regression demo

This local Go application makes an inventory correctness bug repeatable. It
requires no credentials or external services and listens on `127.0.0.1:8080`.

From the repository root, start the application:

```bash
go run ./examples/vulnerable-inventory
```

In another terminal, run either scenario against that same instance:

```bash
go run ./cmd/concurtest run examples/vulnerable-inventory/observation-scenario.yaml
go run ./cmd/concurtest run examples/vulnerable-inventory/history-scenario.yaml
```

Run the commands sequentially: both scenarios reset and modify the same stock.

| Scenario | Declared check | Expected evidence |
| --- | --- | --- |
| [observation-scenario.yaml](observation-scenario.yaml) | Observed stock must stay non-negative | Final stock is `-1` |
| [history-scenario.yaml](history-scenario.yaml) | At most one purchase returns HTTP 201 | Two purchases return HTTP 201, even though reported availability is `0` |

Each scenario starts with four attempts at concurrency four and runs ten trials.
`POST /reset` restores stock to one and resets the purchase rendezvous before
**every** trial, including reduction trials. Two requests pass the stock check
before either decrements it; both return HTTP 201. Other attempts return HTTP 409.
Both scenarios should report ten violating trials and reduce to two attempts at
concurrency two. Exit code `1` is the expected result of discovering the bug.

## Application routes

| Request | Behavior |
| --- | --- |
| `POST /reset` | Restore one unit and release requests waiting in the previous round; HTTP 204 |
| `POST /purchase` | Deliberately separate the stock check from the decrement; HTTP 201 or 409 |
| `GET /state` | Return actual stock as `{"stock":...}` |
| `GET /available-stock` | Return availability clamped to zero as `{"stock":...}` |

The observation scenario checks `/state`. The history scenario includes
`/available-stock` as optional context; its verdict comes from recorded purchase
responses. Clamping availability to zero hides negative stock but cannot erase
the two accepted purchases.

## Why the failure repeats

A mutex protects each stock access, but the availability check and decrement
occur in separate critical sections. A two-request rendezvous deliberately
makes both requests check the same remaining unit before either can decrement
it. The service is free of this Go data race while still violating the inventory
contract. The rendezvous belongs to the demonstration application; ConcurTest
only sends the requests declared in YAML.

At least two concurrent purchase requests are required to release the rendezvous.
A single purchase waits until reset, cancellation, or request timeout. The
published scenarios and reduction search use concurrency of at least two.

The CLI end-to-end tests load these checked-in scenarios, run them against an
isolated application instance, and check the violation and reduction evidence.
For an external application with additional setup requirements, see the
[Juice Shop case study](../juice-shop/README.md).
