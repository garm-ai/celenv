package celenv_test

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"cel.dev/cel-go/cel"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/garm-ai/celenv"
)

// request is a reduced payment request: one int64, one string, one repeated
// string — enough to type-check against and enough to write a runaway
// comprehension over.
func request(t *testing.T) (*protoregistry.Files, protoreflect.MessageDescriptor) {
	t.Helper()
	s := func(v string) *string { return &v }
	i32 := func(v int32) *int32 { return &v }
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	rep := descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	fdp := &descriptorpb.FileDescriptorProto{
		Name: s("p.proto"), Package: s("payments.v1"), Syntax: s("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: s("InitiatePaymentRequest"),
			Field: []*descriptorpb.FieldDescriptorProto{
				{Name: s("amount_minor_units"), Number: i32(3), Label: opt,
					Type:           descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
					Proto3Optional: proto.Bool(true), OneofIndex: i32(0)},
				{Name: s("currency_code"), Number: i32(4), Label: opt,
					Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
				{Name: s("tags"), Number: i32(8), Label: rep,
					Type: descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()},
			},
			OneofDecl: []*descriptorpb.OneofDescriptorProto{{Name: s("_amount_minor_units")}},
		}},
	}
	files, err := protodesc.NewFiles(&descriptorpb.FileDescriptorSet{
		File: []*descriptorpb.FileDescriptorProto{fdp},
	})
	if err != nil {
		t.Fatal(err)
	}
	d, err := files.FindDescriptorByName("payments.v1.InitiatePaymentRequest")
	if err != nil {
		t.Fatal(err)
	}
	return files, d.(protoreflect.MessageDescriptor)
}

func env(t *testing.T) (*cel.Env, protoreflect.MessageDescriptor) {
	t.Helper()
	files, md := request(t)
	e, err := celenv.Env(files, md)
	if err != nil {
		t.Fatal(err)
	}
	return e, md
}

// The option list, pinned. This is the whole reason the package exists: the
// same environment has to be built for `garm lint` rule A4, for the
// catalogue's load-time check and for the runner's evaluation, and a fourth
// caller adding an option would make a manifest that lints unloadable or a
// manifest that loads unlintable. Each case below names the option it pins.
func TestTheEnvironmentIsExactlyMacrosAndTheStandardLibrary(t *testing.T) {
	e, _ := env(t)
	cases := []struct {
		name, expr string
		compiles   bool
		option     string
	}{
		{"the request's own field", "args.amount_minor_units <= 500000", true,
			`cel.Variable("args", cel.ObjectType(<request>))`},
		{"a field the request does not have", "args.nope == 1", false,
			"args is the request type, not a map or a dyn"},
		{"the standard macros", `args.tags.exists(x, x == "a")`, true, "standard macros"},
		{"has()", "has(args.amount_minor_units)", true, "standard macros"},
		{"the standard library", `args.currency_code.startsWith("G")`, true, "standard library"},
		// Not enabled, and must not become enabled by accident.
		{"optional field selection", "args.?currency_code == optional.of(\"GBP\")", false,
			"cel.OptionalTypes is NOT enabled"},
		{"cross-type numeric comparison", "args.amount_minor_units < 1.5", false,
			"cel.CrossTypeNumericComparisons is NOT enabled"},
		{"a string extension function", `args.currency_code.lowerAscii() == "gbp"`, false,
			"no ext library and no custom function is registered"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := celenv.Check(e, c.expr)
			if c.compiles && err != nil {
				t.Errorf("%q did not compile: %v (pins: %s)", c.expr, err, c.option)
			}
			if !c.compiles && err == nil {
				t.Errorf("%q compiled (pins: %s)", c.expr, c.option)
			}
		})
	}
}

// Exact, not assignable: a dyn-typed expression is "maybe a bool", and a
// guard that is maybe a bool is a guard whose answer depends on the
// arguments in a way nobody reviewed.
func TestOnlyAnExactBoolIsAPredicate(t *testing.T) {
	e, _ := env(t)
	for _, expr := range []string{"args.amount_minor_units", "dyn(true)", `"yes"`, "1 == 1 ? dyn(true) : dyn(false)"} {
		if _, _, err := celenv.Check(e, expr); err == nil {
			t.Errorf("%q passed as a predicate", expr)
		} else if !strings.Contains(err.Error(), "bool") {
			t.Errorf("%q: err = %v, want it to say a guard must be a bool", expr, err)
		}
	}
	if _, _, err := celenv.Check(e, "args.amount_minor_units <= 500000"); err != nil {
		t.Errorf("a bool predicate was refused: %v", err)
	}
}

// Check plans as well as type-checks, so a caller cannot forget to and a
// guard that type-checks but cannot be planned is refused where the manifest
// is read rather than where the call is made.
func TestCheckReturnsARunnableProgram(t *testing.T) {
	e, md := env(t)
	_, prg, err := celenv.Check(e, "args.amount_minor_units <= 500000")
	if err != nil {
		t.Fatal(err)
	}
	if prg == nil {
		t.Fatal("Check returned no program")
	}
	msg := dynamicpb.NewMessage(md)
	if err := protojson.Unmarshal([]byte(`{"amountMinorUnits":"1"}`), msg); err != nil {
		t.Fatal(err)
	}
	out, _, err := prg.ContextEval(t.Context(), map[string]any{"args": msg})
	if err != nil {
		t.Fatal(err)
	}
	if out.Value() != true {
		t.Errorf("out = %v, want true", out.Value())
	}
}

func TestAnEmptyExpressionIsNotAPredicate(t *testing.T) {
	e, _ := env(t)
	if _, _, err := celenv.Check(e, ""); err == nil {
		t.Fatal("an empty expression passed Check")
	}
}

// The cost limit, which is what stops a guard from being a denial of service
// against the runner that owns it. A nested comprehension is quadratic in the
// size of a repeated field the MODEL fills in, so without a limit the ceiling
// is whatever the model chose to send.
func TestARunawayComprehensionIsRefusedByTheCostLimit(t *testing.T) {
	e, md := env(t)
	// all-pairs over `tags`: quadratic, and every term is cheap enough that
	// nothing but the cost limit stops it.
	_, prg, err := celenv.Check(e, `args.tags.all(x, args.tags.all(y, x == y || x != y))`)
	if err != nil {
		t.Fatal(err)
	}
	msg := tags(t, md, 2000) // four million inner iterations if nothing stops it
	out, det, err := prg.ContextEval(t.Context(), map[string]any{"args": msg})
	if err == nil {
		t.Fatalf("a four-million-iteration comprehension ran to completion: %v", out)
	}
	if !strings.Contains(err.Error(), "cost limit") {
		t.Errorf("err = %v, want the cost limit named", err)
	}
	if det != nil && det.ActualCost() != nil && *det.ActualCost() > celenv.CostLimit+1 {
		t.Errorf("evaluation cost %d, over the limit of %d", *det.ActualCost(), celenv.CostLimit)
	}
}

// And the limit is not so low that an ordinary predicate trips it. A guard
// that reads a short list is a guard a manifest plausibly carries.
func TestAnOrdinaryComprehensionIsWellUnderTheLimit(t *testing.T) {
	e, md := env(t)
	_, prg, err := celenv.Check(e, `args.tags.all(x, x != "") && args.tags.exists(x, x == "gbp")`)
	if err != nil {
		t.Fatal(err)
	}
	_, det, err := prg.ContextEval(t.Context(), map[string]any{"args": tags(t, md, 100)})
	if err != nil {
		t.Fatalf("a hundred-element comprehension was refused: %v", err)
	}
	if c := det.ActualCost(); c == nil || *c > celenv.CostLimit/10 {
		t.Errorf("a hundred-element comprehension cost %v, uncomfortably close to the limit of %d",
			c, celenv.CostLimit)
	}
}

// And the caller's context is honoured, because a run has a deadline and a
// guard evaluated after it has passed is work nobody is waiting for. The
// expression below is well under the cost limit on purpose: this pins the
// INTERRUPT, not the cost.
func TestACancelledContextStopsEvaluation(t *testing.T) {
	e, md := env(t)
	_, prg, err := celenv.Check(e, `args.tags.all(x, x != "")`)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := prg.ContextEval(ctx, map[string]any{"args": tags(t, md, 1000)}); err == nil {
		t.Fatal("a cancelled context evaluated to an answer")
	}
}

// tags builds a request whose repeated `tags` field has n entries.
func tags(t *testing.T, md protoreflect.MessageDescriptor, n int) *dynamicpb.Message {
	t.Helper()
	var b strings.Builder
	b.WriteString(`{"tags":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`"t"`)
	}
	b.WriteString(`]}`)
	msg := dynamicpb.NewMessage(md)
	if err := protojson.Unmarshal([]byte(b.String()), msg); err != nil {
		t.Fatal(err)
	}
	return msg
}

// The same two ceilings, on Plan.
//
// Plan is the door every workflow expression goes through — `initial`, `with`
// and `set` — and a runaway comprehension in a `set` block is the same denial
// surface as one in a guard. Check has these; Plan had neither asserted, which
// is how a ceiling nobody tests becomes a ceiling nobody has.
func TestARunawayValueExpressionIsRefusedByTheCostLimit(t *testing.T) {
	e, md := env(t)
	// All-pairs again, but producing a LIST rather than a bool, because a
	// value expression is what Plan is for and Check would refuse this.
	prg, err := celenv.Plan(e, `args.tags.filter(x, args.tags.all(y, x == y || x != y))`)
	if err != nil {
		t.Fatal(err)
	}
	out, det, err := prg.ContextEval(t.Context(), map[string]any{"args": tags(t, md, 2000)})
	if err == nil {
		t.Fatalf("a four-million-iteration comprehension ran to completion: %v", out)
	}
	if !strings.Contains(err.Error(), "cost limit") {
		t.Errorf("err = %v, want the cost limit named", err)
	}
	if det != nil && det.ActualCost() != nil && *det.ActualCost() > celenv.CostLimit+1 {
		t.Errorf("evaluation cost %d, over the limit of %d", *det.ActualCost(), celenv.CostLimit)
	}
}

// And a value expression honours the caller's context, so an expression the
// run no longer needs stops. This pins the INTERRUPT and not the cost: the
// expression below is well under the limit.
func TestACancelledContextStopsAValueExpression(t *testing.T) {
	e, md := env(t)
	prg, err := celenv.Plan(e, `args.tags.filter(x, x != "")`)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := prg.ContextEval(ctx, map[string]any{"args": tags(t, md, 1000)}); err == nil {
		t.Fatal("a cancelled context evaluated to an answer")
	}
}

// An ordinary value expression is nowhere near either ceiling — the negative
// half, without which the two above would be satisfied by a Plan that refused
// everything.
func TestAnOrdinaryValueExpressionIsWellUnderTheLimit(t *testing.T) {
	e, md := env(t)
	prg, err := celenv.Plan(e, `args.currency_code + "-" + string(size(args.tags))`)
	if err != nil {
		t.Fatal(err)
	}
	out, det, err := prg.ContextEval(t.Context(), map[string]any{"args": tags(t, md, 100)})
	if err != nil {
		t.Fatalf("an ordinary value expression was refused: %v", err)
	}
	if out.Value() != "-100" {
		t.Errorf("out = %v, want the concatenation", out.Value())
	}
	if c := det.ActualCost(); c == nil || *c > celenv.CostLimit/10 {
		t.Errorf("cost %v, uncomfortably close to the limit of %d", c, celenv.CostLimit)
	}
}

// Plan takes a VALUE, where Check takes a predicate. Both halves: a non-bool
// expression is Plan's whole purpose and is refused by Check, and an empty one
// is refused by both.
func TestPlanTakesAValueAndRefusesAnEmptyExpression(t *testing.T) {
	e, _ := env(t)
	if _, err := celenv.Plan(e, "args.amount_minor_units"); err != nil {
		t.Errorf("Plan refused a value expression: %v", err)
	}
	if _, _, err := celenv.Check(e, "args.amount_minor_units"); err == nil {
		t.Error("Check accepted a non-bool; the two doors must not be the same door")
	}
	if _, err := celenv.Plan(e, ""); err == nil {
		t.Error("Plan accepted an empty expression")
	}
	if _, err := celenv.Plan(e, "args.nope"); err == nil {
		t.Error("Plan accepted a field the request does not have")
	}
}

// A dialect every component shares cannot import any of them, or the sharing
// is a cycle waiting to happen. cel-go and protobuf only.
func TestTheModuleDependsOnNothingFromGarm(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	for _, dep := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(dep, "github.com/garm-ai/") && dep != "github.com/garm-ai/celenv" {
			t.Errorf("celenv must not depend on %s", dep)
		}
	}
}
