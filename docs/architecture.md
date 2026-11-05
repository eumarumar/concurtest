# ConcurTest Architecture

## Purpose

ConcurTest is an open-source adversarial correctness testing engine for stateful backend systems.

It helps developers discover bugs that may not appear during normal unit, integration, or API testing but emerge when operations happen concurrently, are retried, duplicated, reordered, interrupted, or partially fail.

The system being tested may be written in any programming language or framework.

ConcurTest should interact with applications primarily through observable external interfaces.

## Core idea

A developer defines:

1. how to prepare a test scenario;
2. which operations can be performed;
3. how those operations should be exercised adversarially;
4. which properties must remain true.

ConcurTest executes the operations, records what occurred, evaluates the
declared invariant from history or observed state, and reports violations.

The selected invariant determines which evaluation path is used:

```text
Scenario
    ↓
Operations
    ↓
Adversarial execution
    ↓
Execution history
    ├──→ History invariant evaluation ───┐
    │                                    ├──→ Failure reproduction/report
    └──→ State observation               │
              ↓                          │
         State invariant evaluation ─────┘
```

## Example

Suppose an inventory system contains one remaining product.

A normal test may perform purchases sequentially and pass.

ConcurTest may issue many purchase operations concurrently.

The developer can express properties such as:

```text
stock >= 0
successful purchases <= initial stock
```

If two purchases succeed against one available item, ConcurTest should identify and report the violation.

## Core concepts

### Scenario

A scenario describes the environment and behaviour to test.

It may define:

- target application
- setup operations
- test operations
- execution strategy
- observations
- invariants

### Operation

An operation is an action performed against the target system.

Initially operations are HTTP requests.

An operation should capture enough information for ConcurTest to record:

- start time
- completion time
- request
- response
- errors
- execution identity

### Execution strategy

The execution strategy determines how operations are exercised.

Initial strategies should focus on:

- concurrent execution
- repeated execution
- duplicate execution
- retries

Future strategies may include:

- delayed requests
- dropped responses
- reordered operations
- service/network faults

The architecture must not require those future capabilities to exist immediately.

### Reproducibility trials

A scenario may request a bounded number of repeated trials. Trials execute
sequentially so setup can establish controlled preconditions before every
concurrent operation group. The scenario author is responsible for resetting
the state relevant to the invariant; the engine cannot guarantee independence
from earlier trials merely by running setup or reading a fresh baseline.
Concurrency remains bounded within a trial; the trial orchestrator itself does
not add goroutines.

Setup accepts the legacy request mapping or a list of named request steps in
YAML. Both forms normalize to one ordered engine slice, bounded to 100 steps.
The run goroutine executes each step synchronously, including reading and closing
its response, before starting the next step. Setup does not use operation workers
or introduce goroutines. All steps must succeed with a 2xx response before a
baseline observation or concurrent operation can start. A failed step stops that
trial and retains the attempted prefix, including the failure. Cancellation
stops further steps and trials. Later ordinary trials restart the full setup
sequence; no rollback or cleanup is implied.

Named setup steps may declare one optional response assertion. A separate optional
precondition request checks initial state after all setup and before a change
baseline or operation workers. Both use a named Boolean equality or the existing
absolute integer bounds/equality semantics. Setup and precondition assertions
are validated before traffic. They are separate from the one post-operation
invariant and do not add a general expression language or multiple invariants.

All preparation work belongs to the run goroutine. It completes each exchange,
closes the body, and evaluates its check before starting another stage. Immediate
assertions add no polling workers or channels. Existing request contexts and
client timeouts apply to precondition requests. Exact JSON path
traversal is shared with integer invariants through RawMessage, avoiding float64
coercion. Assertion definitions are cloned into evaluation evidence; unknown
scalar values have nil pointers, distinct from false and zero.

A mismatched or unreadable declared assertion returns `precondition_failed` and
makes the trial errored before any operation can run. Request failures preserve
their causes and context identity. The attempted setup prefix and precondition
exchange remain available, with observed values or an unverified assertion.
Subsequent ordinary trials and reduction candidates run all checks again through
the existing scenario runner. A failed check cannot qualify as a demonstrated
post-operation violation or an eligible reduction candidate. Checks remain opt-in;
they do not establish complete isolation or application quiescence.

Optional readiness and completion conditions add bounded synchronous polling.
Readiness runs before setup, protecting resets from known pending work; completion
runs after operation workers join and before final observation. Change baselines
remain before operations. Conditions use the existing scalar assertions against
application-defined completion signals, never the business invariant as a retry
condition. Only a valid nonmatching scalar causes another poll.

Each wait has an explicit timeout and interval and at most 100 polls. A child
context covers both HTTP execution and intervals. The run goroutine owns and
stops interval timers; no polling goroutine or additional worker is introduced.
Every response closes before another request can begin. All polls retain ordered
execution and scalar evidence, with response bodies limited to 512 bytes after
evaluation against the full captured body. Headers and request bodies are omitted
from retained poll metadata, avoiding repeated large or private inputs.

An unverified readiness/completion boundary stops the trial sequence instead of
resetting potentially unfinished target work. The failed trial retains the error;
RunTrials returns the recorded result, as with ordinary non-cancellation trial
errors, but starts no later trial. Parent-context cancellation remains an
orchestration error. An already-demonstrated response-count violation retains
priority even when completion fails; no final state predicate is evaluated from
an unsettled observation. Reduction similarly stops with no selection, retaining
any active candidate and preserving the baseline finding. Its `stopped` status
distinguishes incomplete settlement from a completed unsuccessful search.

These are opt-in application-contract checks, not inferred quiescence. Their
predicates must include all relevant deferred work. Stable values or a resettable
pending counter cannot by themselves establish isolation or rule out later writes.

Each trial retains its complete structured run evidence and is classified as
passed, violated, inconclusive, or errored. Ordinary trial errors are recorded
and later trials still run, except that unverified waits prevent further resets.
Parent-context cancellation preserves the active
partial trial, stops new trials from starting, and makes the overall sequence
incomplete.

Aggregate classification gives demonstrated invariant violations precedence
over trial errors, then inconclusive results, then passes. This keeps a proven
correctness failure visible even when another trial could not be evaluated.

### Observed failure reduction

Reduction is an explicit scenario option because it can send substantially more
traffic. It first requires the configured baseline to demonstrate violations in
a strict majority of at least three clean trials. Errored or inconclusive
trials prevent reduction from starting.

Candidates vary only attempt count and concurrency. They run sequentially in a
stable attempts-first order, use the baseline trial count, and run all setup
steps before every trial through the existing complete scenario runner. Candidate
concurrency starts at two so reduction remains focused on concurrent failures.

The search stops at the first qualifying candidate or after 100 candidates.
Rejected candidates retain bounded status summaries; the baseline, selected
candidate, and any interrupted active candidate retain complete evidence.
Reports call the result the smallest observed failure and do not claim
mathematical minimality or statistical confidence.

### Execution history

ConcurTest must maintain an accurate record of what occurred during a run.

An execution history may contain:

- operation identifier
- start timestamp
- completion timestamp
- inputs
- outputs
- failures
- ordering information
- relevant metadata

Trial sequences additionally retain their requested count, stable one-based
trial ordering, aggregate timing and status, and every trial's complete run
history.

Execution history should be structured data, not only formatted terminal output.

History-aware invariants evaluate recorded operation outcomes directly. The
initial history invariant limits the number of attempts whose final HTTP status
matches an explicit list, or the default 2xx range. It retains every qualifying
attempt ID and the stable suffix beyond the configured maximum. Missing or
failed executions do not count as successful; they keep a non-violating trial
from being reported as a trustworthy pass.

Operation outcome policy is a separate check of whether the trial exercised its
declared HTTP response contract. The default accepts the history invariant's
explicit status filter, or 2xx otherwise; expects no rejection statuses; and
requires one accepted response per trial. Scenarios can declare acceptance and
expected rejection lists and a minimum acceptance count, including an explicit
zero for rejection-only tests. Status sets must be disjoint and the minimum must
fit the configured attempt count. Validation happens before setup can send traffic.

After operation workers join, a synchronous evaluator partitions stable attempt
IDs into accepted, rejected, unexpected, and unknown outcomes. Missing or failed
exchanges are unknown even if response headers were captured. HTTP statuses do
not change transport errors or imply whether effects committed. The policy and
evaluation are snapshotted into evidence without adding goroutines or shared
state. Non-violating trials with insufficient coverage become inconclusive;
demonstrated violations retain precedence. State observation still runs after
unexpected responses. Reduction additionally requires usable outcomes in every
baseline and candidate trial, and preserves that decision in candidate summaries.
Matching failure identity remains a separate concern.

### Observation

An observation reads externally visible state from the target system. It is
required when the configured invariant evaluates state and optional when the
invariant evaluates execution history.

Initially this may be another HTTP request. A JSON integer change invariant
uses the same observation request after setup and before operations, then again
after operations. Baseline capture is synchronous and must succeed, return a
complete body, and contain a valid integer before operations start. Its execution
is retained in partial run evidence even if a later stage fails. Every trial and
reduction candidate captures its own baseline; no baseline is shared across runs.
Evaluation checks the exact signed 64-bit difference between final and baseline,
returning an error on subtraction overflow. It measures net change, so unrelated
or offsetting writes can affect the result.

Future observers may include databases, queues, caches, or custom plugins.

### Invariant

An invariant describes something that must remain true.

Examples include:

```text
stock >= 0
wallet balances never become negative
one idempotency key produces at most one logical operation
one seat cannot have multiple confirmed owners
```

Invariant evaluation should be separate from transport execution.

A scenario continues to declare exactly one invariant. The current concrete
forms are a JSON integer constraint at a configured path and a maximum
number of successful HTTP attempts. JSON integer constraints support an inclusive
minimum, an inclusive maximum, both bounds, exact equality, or an exact change
from a baseline. Equality cannot be combined with bounds; change cannot be
combined with any other constraint. A minimum must not exceed its maximum. The
concrete forms are represented explicitly rather than through an expression
language or plugin system.

JSON integer paths traverse objects and arrays. YAML accepts string keys and
non-negative integer indexes, normalizing indexes to decimal strings in the
engine and JSON report. Each entry selects a literal key on an object or a
zero-based index on an array. Array indexes use canonical decimal strings;
numeric object keys remain valid. Traversal preserves exact JSON integer values
and reports missing keys, invalid indexes, and unexpected value types as
evaluation errors. Text reports retain the same quoted-segment path notation
for both keys and indexes.

### Failure

A failure represents a demonstrated violation, not merely an HTTP error.

ConcurTest should distinguish between:

- transport failures;
- operation failures expected by the target system;
- invariant violations;
- ConcurTest internal failures.

### Reproduction

When a violation is discovered, the long-term goal is to reduce the execution to the smallest useful reproduction.

Reports should answer:

- what invariant failed;
- which operations contributed;
- what order/timing occurred;
- what state was observed;
- how the developer can reproduce it.

## Architecture boundaries

The project should keep these concerns separate:

```text
configuration
execution
transport
history
observation
invariants
reduction
reporting
```

They do not necessarily need to become separate Go packages immediately.

Package boundaries should emerge when there is a real separation of responsibility.

Do not create interfaces or plugin systems before multiple implementations actually require them.

## Initial transport

HTTP/JSON is the first supported application interface.

The core model should not depend directly on a particular application framework such as:

- Laravel
- NestJS
- Django
- Rails
- Spring
- Gin

The target application is a black box from ConcurTest's perspective.

Every engine HTTP execution stops at the original response instead of following
redirects. ExecuteHTTP copies the caller's client to override redirect handling
without mutating the client shared by workers; it reuses the supplied transport,
timeout, and cookie jar.

HTTP tracing counts completed request-header writes in a per-execution atomic
counter. The counter is separate from the returned execution because transport
callbacks can run on other goroutines. More than one observed write becomes a
structured `request_retried` execution error, retaining the final response. The
existing outcome rules then prevent a clean pass, stop failed setup/observation
stages, and preserve demonstrated invariant violations. No retry scheduler or
additional goroutine is introduced. This observes implicit retries rather than
preventing them; buffering, incomplete writes, custom transports without tracing,
and upstream proxy retries limit what it can establish about actual delivery.

## Configuration

Human-readable configuration is expected to be YAML.

Configuration should be:

- explicit
- versionable
- suitable for CI
- easy to review
- deterministic where possible

Configuration parsing should be separate from execution logic.

## CLI

The primary interface is the `concurtest` command.

A typical invocation should eventually feel roughly like:

```bash
concurtest run scenario.yaml
```

The CLI should remain thin.

Business logic belongs in reusable internal packages rather than directly inside CLI commands.

## Reporting contract

The CLI defaults to a human-readable text report and provides one explicit JSON
format for automation. Both formats consume the same structured engine and
reduction results and preserve the same exit status meanings: pass, demonstrated
violation, or incomplete/untrustworthy execution.

JSON has two discriminated top-level forms: a run report and an early error
report. Run reports contain scenario metadata, aggregate counts, every trial in
stable one-based order, complete run and invariant evidence, reduction
summaries and retained candidate trials, errors, timing, and reproduction
arguments. Early error reports cover command and scenario-loading failures for
which no trustworthy trial result exists.

Errors cross package boundaries as a small typed tree with a stable code,
human-readable message, and ordered causes. Standard context cancellation and
deadline errors retain their Go identity while receiving distinct report codes.
This keeps text useful to people without requiring CI consumers to parse error
strings.

The checked-in JSON Schema uses semantic versions and closed objects. The
current pre-launch contract is `1.0.0`; the schema may evolve before launch.
After launch, changing emitted fields or their shapes requires a new major
version. Schema validation is a test concern, not a runtime dependency of the
reporter.

Report evidence stays bounded and safe. Response excerpts retain at most 512
bytes and identify UTF-8 or base64 encoding plus truncation. HTTP headers and
request bodies are never emitted. The top-level baseline is not duplicated
inside reduction output; rejected candidates retain summaries, while selected
or interrupted candidates may retain their complete ordered trials.

Setup definitions and execution evidence use ordered JSON arrays. Definitions
contain each step's name, safe request metadata, and nullable assertion definition;
trial evidence contains only attempted steps, each with its name, HTTP execution,
and nullable assertion evaluation. The optional precondition has separate definition
and execution fields, with its check result retained alongside its HTTP exchange.
Empty arrays represent
no configured or attempted steps. The legacy single-request YAML form normalizes
to a step with an empty name. Compact text identifies a failed step by position
and name; verbose text also shows successful steps and marks unattempted steps.

The default text presentation has a fixed evidence budget. It shows one
violating trial from the smallest selected reduction, or the first baseline
violation when no reduced result exists. It includes at most four relevant
attempts with 160-byte response excerpts and one example of each problem
status. Additional evidence is summarized. Verbose text expands every retained
trial and retained reduction candidate with 512-byte response excerpts.

Terminal color is a CLI presentation decision rather than engine state. Auto
mode requires a terminal, respects a non-empty `NO_COLOR`, and emits no ANSI
escapes when output is redirected. Explicit always and never modes override
automatic detection. JSON never receives terminal presentation options and its
versioned contract is unchanged.

Text and JSON reproduction commands always include effective attempts and
concurrency, plus `--no-reduce` when reduction is disabled. When reduction has
selected a result, commands use its selected values and disable further
reduction; otherwise they use the effective baseline settings. Other settings,
including enabled reduction, continue to come from the referenced scenario file.
Text commands use POSIX shell quoting, remain free of color escapes, and omit
presentation flags. JSON reproduction arguments retain `--format json`.

## Performance

ConcurTest should efficiently run many simultaneous network operations, but raw throughput is not the primary product goal.

Correct execution histories and trustworthy results are more important than artificially high request-per-second benchmarks.

Concurrency should be bounded and configurable.

## Safety

ConcurTest performs intentionally disruptive testing.

The tool should make it difficult for a developer to accidentally interpret it as safe production traffic.

Potentially destructive capabilities should be explicit.

Never assume a target is disposable.

## Engineering direction

Prefer:

- small composable components;
- deterministic behaviour where possible;
- structured results;
- explicit ownership;
- bounded concurrency;
- good error messages;
- reproducible tests.

Avoid:

- speculative abstraction;
- premature distributed execution;
- unnecessary dependencies;
- coupling to one framework;
- coupling to one cloud;
- adding AI where deterministic logic is sufficient.

## Success criterion

ConcurTest becomes useful when it can discover a correctness bug that ordinary application tests failed to expose, explain the violated invariant clearly, and give the developer enough information to reproduce and fix the bug.
