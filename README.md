# ConcurTest

ConcurTest finds correctness failures that appear when state-changing operations
run concurrently, are retried, duplicated, reordered, or interrupted. It tests
an application through its external interfaces, regardless of the language or
framework used to build that application.

The current v0 supports sequential reproducibility trials. A trial can reset
the target, repeat one HTTP operation with bounded concurrency, and evaluate
one invariant. An invariant can check a top-level JSON integer observed after
the operations or limit how many operation responses have successful HTTP
statuses. An opt-in reduction pass can test smaller concurrent execution
settings after a failure reproduces across a clean majority of trials.

> [!WARNING]
> ConcurTest intentionally sends concurrent requests that may change or damage
> target data. Use the included example only on your local machine, and do not
> point a scenario at live or valuable data.

## Requirements

- Go 1.27 or newer

## Run the vulnerable inventory demonstration

The included inventory service starts with one item and deliberately handles
two simultaneous purchases incorrectly.

Start it in one terminal:

```bash
go run ./examples/vulnerable-inventory
```

It listens only on `127.0.0.1:8080`.

In a second terminal, run the checked-in scenario:

```bash
go run ./cmd/concurtest run examples/vulnerable-inventory/scenario.yaml
```

The checked-in scenario starts with four attempts at concurrency four and runs
10 independent trials. Each trial resets the inventory. After the failure
reproduces, ConcurTest tests smaller settings and selects two attempts at
concurrency two:

```text
Result: VIOLATED
Trials: 10
Completed: 10
Violations: 10 of 10 completed trials
First violation: trial 1
Reduction: REDUCED
Candidates evaluated: 1
Smallest observed failure:
  Attempts: 2
  Concurrency: 2
  Violations: 10 of 10 trials
```

The command exits with code `1`. That non-zero result is expected in this
demonstration: ConcurTest found the intended correctness failure.

The report describes the smallest failing configuration ConcurTest observed.
It does not claim that the result is mathematically minimal. Its reproduction
command uses execution overrides and disables another reduction pass:

```text
concurtest run --attempts 2 --concurrency 2 --no-reduce "examples/vulnerable-inventory/scenario.yaml"
```

## Detect a failure hidden by final state

The service also exposes an availability view that reports negative stock as
zero. That view looks valid after the oversell, but it cannot erase the two
purchase responses that were already accepted.

Run the history-based scenario against the same service:

```bash
go run ./cmd/concurtest run examples/vulnerable-inventory/history-scenario.yaml
```

It limits successful purchase attempts to one and explicitly treats HTTP `201`
as success:

```yaml
invariant:
  name: accepted purchases must not exceed stock
  maximum_successful_attempts: 1
  successful_status_codes: [201]
```

If `successful_status_codes` is omitted, every HTTP status from `200` through
`299` counts as success. The report identifies every successful attempt and
which stable attempt IDs are beyond the configured maximum. This scenario also
runs 10 trials and reduces the observed failure to two attempts at concurrency
two.

## Why the example fails

Each purchase checks that stock is available while holding a mutex. It then
releases the mutex before decrementing the stock. The two requests can
therefore both observe stock `1`, both decide that a purchase is valid, and
then decrement separately until stock becomes `-1`.

The mutex prevents a Go data race, but it does not make the full business
operation atomic. This distinction is central to ConcurTest: code can be
race-detector clean and still be incorrect under concurrency.

The example uses a two-request rendezvous to make this broken ordering
repeatable within each trial. ConcurTest has no knowledge of that coordination;
it interacts only through the HTTP requests declared in the
[state scenario](examples/vulnerable-inventory/scenario.yaml) or
[history scenario](examples/vulnerable-inventory/history-scenario.yaml).

## Development checks

```bash
go test ./...
go test -race ./...
go vet ./...
```

See the [architecture](docs/architecture.md) for the project boundaries and
design direction. ConcurTest is available under the
[Apache License 2.0](LICENSE).
