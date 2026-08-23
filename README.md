# ConcurTest

ConcurTest finds correctness failures that appear when state-changing operations
run concurrently, are retried, duplicated, reordered, or interrupted. It tests
an application through its external interfaces, regardless of the language or
framework used to build that application.

The current v0 supports one HTTP operation repeated with bounded concurrency,
one final HTTP observation, and one top-level JSON integer minimum invariant.

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

The report includes both purchase attempts and the final observed state. Its
key result is:

```text
Result: VIOLATED
Expected: "stock" >= 0
Observed: "stock" = -1
```

The command exits with code `1`. That non-zero result is expected in this
demonstration: ConcurTest found the intended correctness failure.

## Why the example fails

Each purchase checks that stock is available while holding a mutex. It then
releases the mutex before decrementing the stock. The two requests can
therefore both observe stock `1`, both decide that a purchase is valid, and
then decrement separately until stock becomes `-1`.

The mutex prevents a Go data race, but it does not make the full business
operation atomic. This distinction is central to ConcurTest: code can be
race-detector clean and still be incorrect under concurrency.

The example uses a two-request rendezvous to make this broken ordering
repeatable. ConcurTest has no knowledge of that coordination; it interacts only
through the HTTP requests declared in the
[scenario](examples/vulnerable-inventory/scenario.yaml).

## Development checks

```bash
go test ./...
go test -race ./...
go vet ./...
```

See the [architecture](docs/architecture.md) for the project boundaries and
design direction. ConcurTest is available under the
[Apache License 2.0](LICENSE).
