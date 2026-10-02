// Package celenv builds the one CEL environment a garm guard is compiled in.
//
// Three callers need it and must agree: `garm lint` rule A4, which decides
// whether a manifest may be published; internal/catalogue, which decides
// whether an agent may be served; and internal/guard, which decides whether a
// call may be sent. An option that only one of them enables is a manifest
// that lints and will not load, or one that loads and cannot be linted, and
// the operator finds out at the worst of the three moments.
//
// `garm lint` is in another repository and cannot import this one, so its
// copy stays a copy — pinned by the test in this package, which names each
// option it is pinning so a diff against garm's guardEnv is a reading
// exercise rather than an archaeology one. The two callers HERE share this
// code rather than duplicating it, which is what the duplication was worth:
// one copy of the agreement per repository, not per package.
//
// The environment is deliberately small: the catalogue's descriptors, `args`
// declared at the tool's request message, the standard macros and the
// standard library. No optional types, no cross-type numeric comparisons, no
// extension libraries, no custom functions. Every one of those would be a
// dialect of CEL a manifest author could write and a reviewer reading the
// spec could not predict.
package celenv

import (
	"fmt"
	"sort"
	"time"

	"cel.dev/cel-go/cel"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// Var is the name a guard reads its arguments through.
const Var = "args"

// CostLimit is the ceiling on ONE guard evaluation, in cel-go's cost units
// (roughly: one unit per interpreter step, more for string and list work).
//
// A guard reads a message the MODEL filled in, and CEL's comprehensions are
// nestable: `args.tags.all(x, args.tags.all(y, …))` costs the square of a
// repeated field whose length nothing in the manifest bounds. Without a
// limit the ceiling on a guard's runtime is whatever the model chose to send,
// in the one process that is supposed to be narrowing what the model does.
//
// A hundred thousand units is orders of magnitude above any predicate a
// manifest plausibly carries — a field comparison is single digits, a
// comprehension over a hundred-element list is low thousands. It is a bound
// on interpreter steps, not on time: see EvalTimeout below for why a runaway
// can still take over a second before it trips. A guard that needs more than
// this is not a guard; it is a program, and it belongs in the tool rather
// than in the manifest.
const CostLimit uint64 = 100_000

// interruptCheckFrequency is how many comprehension iterations run between
// two checks of the caller's context. The cost limit bounds counted work;
// this bounds how long an interrupted evaluation keeps doing it.
const interruptCheckFrequency uint = 100

// EvalTimeout is the wall-clock ceiling on ONE guard evaluation, applied on
// top of whatever deadline the caller brought.
//
// The cost limit is not a wall-clock bound, and measurement is why this
// constant exists. cel-go charges a comprehension roughly four units per
// element, but iterating a repeated field of a dynamicpb message is
// quadratic in practice: `args.tags.all(x, x != "")` over twenty thousand
// entries costs 80,003 units — inside CostLimit, at eighty percent of it — and burns 1.2
// seconds of CPU. The cost model is a bound on interpreter STEPS, and the
// model chooses how much work a step is.
//
// So there are three bounds and each does one job: CostLimit stops an
// expensive expression, EvalTimeout stops a slow one, and the caller's own
// context stops one the run no longer needs. A quarter of a second is two to
// three orders of magnitude above any predicate a manifest plausibly
// carries, and it is paid once per tool call on the runner's hot path.
const EvalTimeout = 250 * time.Millisecond

// Env declares `args` at the tool's request message, against the catalogue's
// own descriptors.
//
// The type is the point. `args` at the exact message type rather than as a
// map or a dyn is what makes `args.ammount_minor_units` a COMPILE error
// naming the field, instead of a runtime error on every call that a loop can
// only report as "the guard refused".
func Env(files *protoregistry.Files, md protoreflect.MessageDescriptor) (*cel.Env, error) {
	env, err := EnvVars(files, map[string]protoreflect.MessageDescriptor{Var: md})
	if err != nil {
		return nil, fmt.Errorf("for %s: %w", md.FullName(), err)
	}
	return env, nil
}

// Check type-checks expr as a boolean predicate AND plans it.
//
// The two are one call on purpose. A caller that type-checked without
// planning would accept a guard that cannot be run, and would find out at the
// first call rather than at load — which is the exact failure the compile
// check exists to prevent, moved four hours later.
func Check(env *cel.Env, expr string) (*cel.Ast, cel.Program, error) {
	if expr == "" {
		return nil, nil, fmt.Errorf("an empty expression is not a predicate")
	}
	ast, iss := env.Compile(expr)
	if iss.Err() != nil {
		return nil, nil, fmt.Errorf("%q does not compile: %w", expr, iss.Err())
	}
	// A4: a guard is a boolean predicate. EXACTLY a bool, not something
	// assignable to one: a dyn-typed expression is "maybe a bool", and a
	// guard that is maybe a bool has an answer nobody reviewed. An expression
	// of any other type would be truthy nowhere and would refuse every call,
	// silently.
	if out := ast.OutputType(); !out.IsExactType(cel.BoolType) {
		return nil, nil, fmt.Errorf("%q evaluates to %s; a guard must be a bool", expr, out)
	}
	prg, err := env.Program(ast,
		cel.CostLimit(CostLimit),
		cel.InterruptCheckFrequency(interruptCheckFrequency),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("%q could not be planned: %w", expr, err)
	}
	return ast, prg, nil
}

// EnvVars is Env for more than one subject.
//
// A guard reads one message and declares one variable; a workflow graph's four
// expression sites read `input`, `state` and `response` (design §3, the table
// of four sites). The OPTION LIST is the same one — that is the whole point of
// this package, and Env now delegates here so the test that pins the options
// pins them for both callers rather than for one.
//
// The names are the caller's and the types are the catalogue's: every
// variable is declared at an exact message type, so `state.ammount` is a
// COMPILE error naming the field rather than a zero value at run time, which
// is the same property Env buys for `args`.
func EnvVars(files *protoregistry.Files, vars map[string]protoreflect.MessageDescriptor) (*cel.Env, error) {
	opts := make([]cel.EnvOption, 0, len(vars)+1)
	// The whole generation's descriptors, so an expression referring to
	// another message resolves without this package walking the import graph.
	opts = append(opts, cel.TypeDescs(files))
	// Sorted, so two environments built from the same map are built from the
	// same option list — a cel-go error naming "the second variable" must not
	// depend on map iteration order.
	names := make([]string, 0, len(vars))
	for name := range vars {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		md := vars[name]
		if md == nil {
			return nil, fmt.Errorf("building a CEL environment: %q has no message type", name)
		}
		opts = append(opts, cel.Variable(name, cel.ObjectType(string(md.FullName()))))
	}
	env, err := cel.NewEnv(opts...)
	if err != nil {
		return nil, fmt.Errorf("building a CEL environment for %v: %w", names, err)
	}
	return env, nil
}

// Plan type-checks and plans a VALUE expression — one whose result is written
// into a field rather than read as a predicate.
//
// Separate from Check because Check's bool requirement is a guard's, and a
// workflow's `initial`, `with` and `set` expressions produce values of the
// target field's type. `when` is a predicate and goes through Check.
//
// The cost limit and the interrupt frequency are Check's, unchanged: an
// expression over a tool's reply is over a message this process did not
// author, so the ceiling on evaluating it must not be whatever the reply
// happened to contain.
func Plan(env *cel.Env, expr string) (cel.Program, error) {
	if expr == "" {
		return nil, fmt.Errorf("an empty expression produces no value")
	}
	ast, iss := env.Compile(expr)
	if iss.Err() != nil {
		return nil, fmt.Errorf("%q does not compile: %w", expr, iss.Err())
	}
	prg, err := env.Program(ast,
		cel.CostLimit(CostLimit),
		cel.InterruptCheckFrequency(interruptCheckFrequency),
	)
	if err != nil {
		return nil, fmt.Errorf("%q could not be planned: %w", expr, err)
	}
	return prg, nil
}
