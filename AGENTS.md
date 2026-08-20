# ConcurTest Agent Instructions

ConcurTest is an open-source adversarial correctness testing tool for stateful backend systems.

Before making architectural or substantial implementation changes, read:

- `docs/architecture.md`
- relevant existing code and tests

Do not invent new product directions or expand scope without explicit instruction.

## Core engineering principles

- The core implementation language is Go.
- Correctness is more important than cleverness.
- Prefer simple, explicit, idiomatic Go over unnecessary abstractions.
- Keep packages cohesive and APIs small.
- Avoid premature generalisation.
- Avoid hidden global state.
- Make concurrency ownership explicit.
- Propagate `context.Context` through cancellable operations.
- Return and handle errors deliberately.
- Do not silently ignore errors.
- Prefer the Go standard library unless an external dependency provides substantial value.
- Keep external dependencies minimal.
- Do not add frameworks merely for convenience.

## Concurrency

Concurrency is central to this project.

Any concurrency-sensitive implementation must be reviewed for:

- goroutine lifecycle
- cancellation
- channel ownership
- data races
- deadlocks
- leaked goroutines
- deterministic cleanup
- bounded resource usage

Do not introduce concurrency simply because Go makes it easy.

## Testing requirements

For meaningful code changes, run:

```bash
go test ./...
go test -race ./...
go vet ./...
```

Do not claim work is complete if relevant tests fail.

New behaviour must include tests where practical.

Concurrency bugs should preferably have deterministic or repeatable regression tests.

## Architecture rules

Keep the core engine independent from:

- specific web frameworks
- specific application languages
- SaaS/cloud infrastructure
- AI providers
- user-interface frameworks

ConcurTest should primarily observe systems through external interfaces.

HTTP/JSON is the initial transport.

The architecture should allow other transports, state observers, fault strategies, and reporters to be added later without forcing the initial implementation to support them now.

## Scope discipline

ConcurTest is not intended to become:

- a generic load-testing platform
- an API client like Postman
- an uptime monitor
- an AI code reviewer
- a general-purpose fuzzing framework
- a cloud dashboard at this stage

Its central concern is correctness under concurrency and failure.

Before adding a feature, ask:

> Does this materially improve ConcurTest's ability to discover, reproduce, or explain correctness failures caused by concurrency, retries, duplicate execution, ordering, or failures?

If not, do not add it without explicit approval.

## Working with the maintainer

Substantial implementation changes should remain idiomatic, reviewable, and understandable to contributors.

For substantial Go implementation:

- document non-obvious Go patterns when they materially affect architecture, correctness, or maintainability;
- prefer idiomatic, production-quality Go;
- do not hide complexity behind generated code;
- explain important concurrency decisions;
- point out trade-offs and risks;
- do not replace understandable engineering with unnecessary sophistication.

Generated code must remain understandable and maintainable by a human.

## Change discipline

Work on one clearly bounded objective at a time.

Do not implement future objectives merely because they appear logically related.

Do not perform unrelated refactors while completing a focused task.

Before making a large architectural change, explain why the current architecture is insufficient.
