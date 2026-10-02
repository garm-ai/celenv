# celenv

The one CEL dialect a garm guard — or a workflow's `initial`, `with`, `set`
and `when` expressions — is compiled in.

Four callers need this and must agree on what it means, because a guard that
compiles for one of them and not another is a manifest that lints and will
not load, or loads and cannot be linted:

- **`garm lint`** (in `garm-ai/garm`), rule A4 — decides whether a manifest
  may be published.
- **`garmd`**, at step 5 of the call chain — decides whether a call may be
  sent.
- **The STS** (`garm-ai/sts`), at mint — decides what a delegation token may
  be exchanged for.
- **The runner** (`garm-ai/agentd`) — decides whether an agent may be served,
  and evaluates the guard on the runner's hot path.

A second copy of this dialect is the bug this module exists to prevent. Each
caller importing its own copy of "what CEL options are enabled" is an
agreement by inspection — correct until someone changes one copy and not the
others, and the only way to find out is a manifest that behaves differently
depending on which process read it. Importing this module instead of copying
it makes drift impossible rather than merely reviewed-against.

## What it is

The environment is deliberately small: the catalogue's descriptors, `args`
(or a caller's named variables) declared at an exact message type, the
standard macros, the standard library. No optional types, no cross-type
numeric comparisons, no extension libraries, no custom functions. Every one
of those would be a dialect of CEL a manifest author could write and a
reviewer reading the spec could not predict.

```go
func Env(files *protoregistry.Files, md protoreflect.MessageDescriptor) (*cel.Env, error)
func EnvVars(files *protoregistry.Files, vars map[string]protoreflect.MessageDescriptor) (*cel.Env, error)
func Check(env *cel.Env, expr string) (*cel.Ast, cel.Program, error)
func Plan(env *cel.Env, expr string) (cel.Program, error)
```

`Check` compiles expr as a boolean predicate — a guard — and refuses anything
that is not EXACTLY a bool, including a `dyn`-typed expression. `Plan`
compiles expr as a value expression — a workflow's `initial`, `with` and
`set` sites — and accepts any result type. Both plan as well as type-check,
so a caller cannot forget to, and both carry the same cost limit and
interrupt frequency: a guard or a value expression reads a message this
process did not author, so the ceiling on evaluating it must not be whatever
that message happened to contain.

## What it is not

`garm lint`'s copy is a separate implementation, not a user of this module —
`garm` cannot import `celenv` without creating the dependency cycle this
module exists to avoid (garmd and the STS importing the CLI). Its copy is
pinned against this package's own test suite instead, so a diff between the
two is a reading exercise rather than an archaeology one.

## No garm dependency

Enforced by a test, not a convention: `TestTheModuleDependsOnNothingFromGarm`
runs `go list -deps .` and fails if anything under `github.com/garm-ai/`
besides this module itself shows up. That property is what makes the module
importable by garmd, the CLI and the STS alike without a cycle.

## History

Extracted from `agentd/internal/celenv` on 2026-10-02, as part of the
standing-grants work. Signatures are unchanged; only the import path moved.
