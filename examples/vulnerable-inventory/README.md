# Inventory regression demo

This local application makes an inventory correctness bug repeatable. Choose
the Node.js or Go version; both use the same routes and scenarios. They require
no credentials or external services and listen on `127.0.0.1:8080`.

From the repository root, start either version in one terminal.

With Node.js (no npm packages or build step):

```bash
node examples/vulnerable-inventory/node/server.js
```

With Go 1.27 or newer:

```bash
go run ./examples/vulnerable-inventory
```

In another terminal, use the installed ConcurTest binary to run either scenario
against that same instance:

```bash
concurtest run examples/vulnerable-inventory/observation-scenario.yaml
concurtest run examples/vulnerable-inventory/history-scenario.yaml
```

With a downloaded ConcurTest binary and the Node.js demo, Go is not required.
Go contributors can use `go run ./cmd/concurtest` in place of `concurtest`.
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

Both versions use a two-request rendezvous that makes both requests check the
same remaining unit before either can decrement it. In Go, a mutex protects
each stock access, but the check and decrement occur in separate critical
sections. In Node.js, each purchase awaits a promise after the check; the
second purchase releases both requests. JavaScript runs on one event loop, but
the asynchronous gap still permits the oversell. The rendezvous belongs to the
demonstration application; ConcurTest only sends the requests declared in YAML.

At least two concurrent purchase requests are required to release the rendezvous.
A single purchase waits until reset, cancellation, or request timeout. The
published scenarios and reduction search use concurrency of at least two.

For an external application with additional setup requirements, see the
[Juice Shop case study](../juice-shop/README.md).
