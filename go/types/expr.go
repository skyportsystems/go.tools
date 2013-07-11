// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements typechecking of expressions.

package types

import (
	"go/ast"
	"go/token"

	"code.google.com/p/go.tools/go/exact"
)

// TODO(gri) Internal cleanups
// - don't print error messages referring to invalid types (they are likely spurious errors)
// - simplify invalid handling: maybe just use Typ[Invalid] as marker, get rid of invalid Mode for values?
// - rethink error handling: should all callers check if x.mode == valid after making a call?
// - consider storing error messages in invalid operands for better error messages/debugging output

// TODO(gri) Test issues
// - API tests are missing (e.g., identifiers should be handled as expressions in callbacks)

/*
Basic algorithm:

Expressions are checked recursively, top down. Expression checker functions
are generally of the form:

  func f(x *operand, e *ast.Expr, ...)

where e is the expression to be checked, and x is the result of the check.
The check performed by f may fail in which case x.mode == invalid, and
related error messages will have been issued by f.

If a hint argument is present, it is the composite literal element type
of an outer composite literal; it is used to type-check composite literal
elements that have no explicit type specification in the source
(e.g.: []T{{...}, {...}}, the hint is the type T in this case).

All expressions are checked via rawExpr, which dispatches according
to expression kind. Upon returning, rawExpr is recording the types and
constant values for all expressions that have an untyped type (those types
may change on the way up in the expression tree). Usually these are constants,
but the results of comparisons or non-constant shifts of untyped constants
may also be untyped, but not constant.

Untyped expressions may eventually become fully typed (i.e., not untyped),
typically when the value is assigned to a variable, or is used otherwise.
The updateExprType method is used to record this final type and update
the recorded types: the type-checked expression tree is again traversed down,
and the new type is propagated as needed. Untyped constant expression values
that become fully typed must now be representable by the full type (constant
sub-expression trees are left alone except for their roots). This mechanism
ensures that a client sees the actual (run-time) type an untyped value would
have. It also permits type-checking of lhs shift operands "as if the shift
were not present": when updateExprType visits an untyped lhs shift operand
and assigns it it's final type, that type must be an integer type, and a
constant lhs must be representable as an integer.

When an expression gets its final type, either on the way out from rawExpr,
on the way down in updateExprType, or at the end of the type checker run,
if present the Context.Expr method is invoked to notify a go/types client.
*/

type opPredicates map[token.Token]func(Type) bool

var unaryOpPredicates = opPredicates{
	token.ADD: isNumeric,
	token.SUB: isNumeric,
	token.XOR: isInteger,
	token.NOT: isBoolean,
}

func (check *checker) op(m opPredicates, x *operand, op token.Token) bool {
	if pred := m[op]; pred != nil {
		if !pred(x.typ) {
			check.invalidOp(x.pos(), "operator %s not defined for %s", op, x)
			return false
		}
	} else {
		check.invalidAST(x.pos(), "unknown operator %s", op)
		return false
	}
	return true
}

func (check *checker) unary(x *operand, op token.Token) {
	switch op {
	case token.AND:
		// spec: "As an exception to the addressability
		// requirement x may also be a composite literal."
		if _, ok := unparen(x.expr).(*ast.CompositeLit); ok {
			x.mode = variable
		}
		if x.mode != variable {
			check.invalidOp(x.pos(), "cannot take address of %s", x)
			goto Error
		}
		x.typ = &Pointer{base: x.typ}
		return

	case token.ARROW:
		typ, ok := x.typ.Underlying().(*Chan)
		if !ok {
			check.invalidOp(x.pos(), "cannot receive from non-channel %s", x)
			goto Error
		}
		if typ.dir&ast.RECV == 0 {
			check.invalidOp(x.pos(), "cannot receive from send-only channel %s", x)
			goto Error
		}
		x.mode = valueok
		x.typ = typ.elt
		return
	}

	if !check.op(unaryOpPredicates, x, op) {
		goto Error
	}

	if x.mode == constant {
		typ := x.typ.Underlying().(*Basic)
		size := -1
		if isUnsigned(typ) {
			size = int(check.ctxt.sizeof(typ))
		}
		x.val = exact.UnaryOp(op, x.val, size)
		// Typed constants must be representable in
		// their type after each constant operation.
		check.isRepresentable(x, typ)
		return
	}

	x.mode = value
	// x.typ remains unchanged
	return

Error:
	x.mode = invalid
}

func isShift(op token.Token) bool {
	return op == token.SHL || op == token.SHR
}

func isComparison(op token.Token) bool {
	// Note: tokens are not ordered well to make this much easier
	switch op {
	case token.EQL, token.NEQ, token.LSS, token.LEQ, token.GTR, token.GEQ:
		return true
	}
	return false
}

func isRepresentableConst(x exact.Value, ctxt *Context, as BasicKind) bool {
	switch x.Kind() {
	case exact.Unknown:
		return true

	case exact.Bool:
		return as == Bool || as == UntypedBool

	case exact.Int:
		if x, ok := exact.Int64Val(x); ok {
			switch as {
			case Int:
				var s = uint(ctxt.sizeof(Typ[as])) * 8
				return int64(-1)<<(s-1) <= x && x <= int64(1)<<(s-1)-1
			case Int8:
				const s = 8
				return -1<<(s-1) <= x && x <= 1<<(s-1)-1
			case Int16:
				const s = 16
				return -1<<(s-1) <= x && x <= 1<<(s-1)-1
			case Int32:
				const s = 32
				return -1<<(s-1) <= x && x <= 1<<(s-1)-1
			case Int64:
				return true
			case Uint, Uintptr:
				if s := uint(ctxt.sizeof(Typ[as])) * 8; s < 64 {
					return 0 <= x && x <= int64(1)<<s-1
				}
				return 0 <= x
			case Uint8:
				const s = 8
				return 0 <= x && x <= 1<<s-1
			case Uint16:
				const s = 16
				return 0 <= x && x <= 1<<s-1
			case Uint32:
				const s = 32
				return 0 <= x && x <= 1<<s-1
			case Uint64:
				return 0 <= x
			case Float32:
				return true // TODO(gri) fix this
			case Float64:
				return true // TODO(gri) fix this
			case Complex64:
				return true // TODO(gri) fix this
			case Complex128:
				return true // TODO(gri) fix this
			case UntypedInt, UntypedFloat, UntypedComplex:
				return true
			}
		}

		n := exact.BitLen(x)
		switch as {
		case Uint, Uintptr:
			var s = uint(ctxt.sizeof(Typ[as])) * 8
			return exact.Sign(x) >= 0 && n <= int(s)
		case Uint64:
			return exact.Sign(x) >= 0 && n <= 64
		case Float32:
			return true // TODO(gri) fix this
		case Float64:
			return true // TODO(gri) fix this
		case Complex64:
			return true // TODO(gri) fix this
		case Complex128:
			return true // TODO(gri) fix this
		case UntypedInt, UntypedFloat, UntypedComplex:
			return true
		}

	case exact.Float:
		switch as {
		case Float32:
			return true // TODO(gri) fix this
		case Float64:
			return true // TODO(gri) fix this
		case Complex64:
			return true // TODO(gri) fix this
		case Complex128:
			return true // TODO(gri) fix this
		case UntypedFloat, UntypedComplex:
			return true
		}

	case exact.Complex:
		switch as {
		case Complex64:
			return true // TODO(gri) fix this
		case Complex128:
			return true // TODO(gri) fix this
		case UntypedComplex:
			return true
		}

	case exact.String:
		return as == String || as == UntypedString

	case exact.Nil:
		return as == UntypedNil || as == UnsafePointer

	default:
		unreachable()
	}

	return false
}

// isRepresentable checks that a constant operand is representable in the given type.
func (check *checker) isRepresentable(x *operand, typ *Basic) {
	if x.mode != constant || isUntyped(typ) {
		return
	}

	if !isRepresentableConst(x.val, check.ctxt, typ.kind) {
		var msg string
		if isNumeric(x.typ) && isNumeric(typ) {
			msg = "%s overflows (or cannot be accurately represented as) %s"
		} else {
			msg = "cannot convert %s to %s"
		}
		check.errorf(x.pos(), msg, x, typ)
		x.mode = invalid
	}
}

// updateExprType updates the type of x to typ and invokes itself
// recursively for the operands of x, depending on expression kind.
// If typ is still an untyped and not the final type, updateExprType
// only updates the recorded untyped type for x and possibly its
// operands. Otherwise (i.e., typ is not an untyped type anymore,
// or it is the final type for x), Context.Expr is invoked, if present.
// Also, if x is a constant, it must be representable as a value of typ,
// and if x is the (formerly untyped) lhs operand of a non-constant
// shift, it must be an integer value.
//
func (check *checker) updateExprType(x ast.Expr, typ Type, final bool) {
	old, found := check.untyped[x]
	if !found {
		return // nothing to do
	}

	// update operands of x if necessary
	switch x := x.(type) {
	case *ast.BadExpr,
		*ast.FuncLit,
		*ast.CompositeLit,
		*ast.IndexExpr,
		*ast.SliceExpr,
		*ast.TypeAssertExpr,
		*ast.StarExpr,
		*ast.KeyValueExpr,
		*ast.ArrayType,
		*ast.StructType,
		*ast.FuncType,
		*ast.InterfaceType,
		*ast.MapType,
		*ast.ChanType:
		// These expression are never untyped - nothing to do.
		// The respective sub-expressions got their final types
		// upon assignment or use.
		if debug {
			check.dump("%s: found old type(%s): %s (new: %s)", x.Pos(), x, old.typ, typ)
			unreachable()
		}
		return

	case *ast.CallExpr:
		// Resulting in an untyped constant (e.g., built-in complex).
		// The respective calls take care of calling updateExprType
		// for the arguments if necessary.

	case *ast.Ident, *ast.BasicLit, *ast.SelectorExpr:
		// An identifier denoting a constant, a constant literal,
		// or a qualified identifier (imported untyped constant).
		// No operands to take care of.

	case *ast.ParenExpr:
		check.updateExprType(x.X, typ, final)

	case *ast.UnaryExpr:
		// If x is a constant, the operands were constants.
		// They don't need to be updated since they never
		// get "materialized" into a typed value; and they
		// will be processed at the end of the type check.
		if old.val != nil {
			break
		}
		check.updateExprType(x.X, typ, final)

	case *ast.BinaryExpr:
		if old.val != nil {
			break // see comment for unary expressions
		}
		if isComparison(x.Op) {
			// The result type is independent of operand types
			// and the operand types must have final types.
		} else if isShift(x.Op) {
			// The result type depends only on lhs operand.
			// The rhs type was updated when checking the shift.
			check.updateExprType(x.X, typ, final)
		} else {
			// The operand types match the result type.
			check.updateExprType(x.X, typ, final)
			check.updateExprType(x.Y, typ, final)
		}

	default:
		unreachable()
	}

	// If the new type is not final and still untyped, just
	// update the recorded type.
	if !final && isUntyped(typ) {
		old.typ = typ.Underlying().(*Basic)
		check.untyped[x] = old
		return
	}

	// Otherwise we have the final (typed or untyped type).
	// Remove it from the map.
	delete(check.untyped, x)

	// If x is the lhs of a shift, its final type must be integer.
	// We already know from the shift check that it is representable
	// as an integer if it is a constant.
	if old.isLhs && !isInteger(typ) {
		check.invalidOp(x.Pos(), "shifted operand %s (type %s) must be integer", x, typ)
		return
	}

	// Everything's fine, notify client of final type for x.
	if f := check.ctxt.Expr; f != nil {
		f(x, typ, old.val)
	}
}

// convertUntyped attempts to set the type of an untyped value to the target type.
func (check *checker) convertUntyped(x *operand, target Type) {
	if x.mode == invalid || !isUntyped(x.typ) {
		return
	}

	// TODO(gri) Sloppy code - clean up. This function is central
	//           to assignment and expression checking.

	if isUntyped(target) {
		// both x and target are untyped
		xkind := x.typ.(*Basic).kind
		tkind := target.(*Basic).kind
		if isNumeric(x.typ) && isNumeric(target) {
			if xkind < tkind {
				x.typ = target
				check.updateExprType(x.expr, target, false)
			}
		} else if xkind != tkind {
			goto Error
		}
		return
	}

	// typed target
	switch t := target.Underlying().(type) {
	case nil:
		// We may reach here due to previous type errors.
		// Be conservative and don't crash.
		x.mode = invalid
		return
	case *Basic:
		check.isRepresentable(x, t)
		if x.mode == invalid {
			return // error already reported
		}
	case *Interface:
		if !x.isNil() && !t.IsEmpty() /* empty interfaces are ok */ {
			goto Error
		}
		// Update operand types to the default type rather then
		// the target (interface) type: values must have concrete
		// dynamic types. If the value is nil, keep it untyped
		// (this is important for tools such as go vet which need
		// the dynamic type for argument checking of say, print
		// functions)
		if x.isNil() {
			target = Typ[UntypedNil]
		} else {
			// cannot assign untyped values to non-empty interfaces
			if !t.IsEmpty() {
				goto Error
			}
			target = defaultType(x.typ)
		}
	case *Pointer, *Signature, *Slice, *Map, *Chan:
		if !x.isNil() {
			goto Error
		}
		// keep nil untyped - see comment for interfaces, above
		target = Typ[UntypedNil]
	default:
		goto Error
	}

	x.typ = target
	check.updateExprType(x.expr, target, true) // UntypedNils are final
	return

Error:
	check.errorf(x.pos(), "cannot convert %s to %s", x, target)
	x.mode = invalid
}

func (check *checker) comparison(x, y *operand, op token.Token) {
	// TODO(gri) deal with interface vs non-interface comparison

	valid := false
	if x.isAssignableTo(check.ctxt, y.typ) || y.isAssignableTo(check.ctxt, x.typ) {
		switch op {
		case token.EQL, token.NEQ:
			valid = isComparable(x.typ) ||
				x.isNil() && hasNil(y.typ) ||
				y.isNil() && hasNil(x.typ)
		case token.LSS, token.LEQ, token.GTR, token.GEQ:
			valid = isOrdered(x.typ)
		default:
			unreachable()
		}
	}

	if !valid {
		check.invalidOp(x.pos(), "cannot compare %s %s %s", x, op, y)
		x.mode = invalid
		return
	}

	if x.mode == constant && y.mode == constant {
		x.val = exact.MakeBool(exact.Compare(x.val, op, y.val))
		// The operands are never materialized; no need to update
		// their types.
	} else {
		x.mode = value
		// The operands have now their final types, which at run-
		// time will be materialized. Update the expression trees.
		// If the current types are untyped, the materialized type
		// is the respective default type.
		check.updateExprType(x.expr, defaultType(x.typ), true)
		check.updateExprType(y.expr, defaultType(y.typ), true)
	}

	// spec: "Comparison operators compare two operands and yield
	//        an untyped boolean value."
	x.typ = Typ[UntypedBool]
}

func (check *checker) shift(x, y *operand, op token.Token) {
	untypedx := isUntyped(x.typ)

	// The lhs must be of integer type or be representable
	// as an integer; otherwise the shift has no chance.
	if !isInteger(x.typ) && (!untypedx || !isRepresentableConst(x.val, nil, UntypedInt)) {
		check.invalidOp(x.pos(), "shifted operand %s must be integer", x)
		x.mode = invalid
		return
	}

	// spec: "The right operand in a shift expression must have unsigned
	// integer type or be an untyped constant that can be converted to
	// unsigned integer type."
	switch {
	case isInteger(y.typ) && isUnsigned(y.typ):
		// nothing to do
	case isUntyped(y.typ):
		check.convertUntyped(y, Typ[UntypedInt])
		if y.mode == invalid {
			x.mode = invalid
			return
		}
	default:
		check.invalidOp(y.pos(), "shift count %s must be unsigned integer", y)
		x.mode = invalid
		return
	}

	if x.mode == constant {
		if y.mode == constant {
			if untypedx {
				x.typ = Typ[UntypedInt]
			}
			// rhs must be within reasonable bounds
			const stupidShift = 1024
			s, ok := exact.Uint64Val(y.val)
			if !ok || s >= stupidShift {
				check.invalidOp(y.pos(), "%s: stupid shift", y)
				x.mode = invalid
				return
			}
			// everything's ok
			x.val = exact.Shift(x.val, op, uint(s))
			return
		}

		// non-constant shift with constant lhs
		if untypedx {
			// spec: "If the left operand of a non-constant shift expression is
			// an untyped constant, the type of the constant is what it would be
			// if the shift expression were replaced by its left operand alone;
			// the type is int if it cannot be determined from the context (for
			// instance, if the shift expression is an operand in a comparison
			// against an untyped constant)".

			// Delay operand checking until we know the final type:
			// The lhs expression must be in the untyped map, mark
			// the entry as lhs shift operand.
			if info, ok := check.untyped[x.expr]; ok {
				info.isLhs = true
				check.untyped[x.expr] = info
			} else {
				unreachable()
			}
			// keep x's type
			x.mode = value
			return
		}
	}

	// non-constant shift - lhs must be an integer
	if !isInteger(x.typ) {
		check.invalidOp(x.pos(), "shifted operand %s must be integer", x)
		x.mode = invalid
		return
	}

	// non-constant shift
	x.mode = value
}

var binaryOpPredicates = opPredicates{
	token.ADD: func(typ Type) bool { return isNumeric(typ) || isString(typ) },
	token.SUB: isNumeric,
	token.MUL: isNumeric,
	token.QUO: isNumeric,
	token.REM: isInteger,

	token.AND:     isInteger,
	token.OR:      isInteger,
	token.XOR:     isInteger,
	token.AND_NOT: isInteger,

	token.LAND: isBoolean,
	token.LOR:  isBoolean,
}

func (check *checker) binary(x *operand, lhs, rhs ast.Expr, op token.Token) {
	var y operand

	check.expr(x, lhs)
	check.expr(&y, rhs)

	if x.mode == invalid {
		return
	}
	if y.mode == invalid {
		x.mode = invalid
		x.expr = y.expr
		return
	}

	if isShift(op) {
		check.shift(x, &y, op)
		return
	}

	check.convertUntyped(x, y.typ)
	if x.mode == invalid {
		return
	}
	check.convertUntyped(&y, x.typ)
	if y.mode == invalid {
		x.mode = invalid
		return
	}

	if isComparison(op) {
		check.comparison(x, &y, op)
		return
	}

	if !IsIdentical(x.typ, y.typ) {
		// only report an error if we have valid types
		// (otherwise we had an error reported elsewhere already)
		if x.typ != Typ[Invalid] && y.typ != Typ[Invalid] {
			check.invalidOp(x.pos(), "mismatched types %s and %s", x.typ, y.typ)
		}
		x.mode = invalid
		return
	}

	if !check.op(binaryOpPredicates, x, op) {
		x.mode = invalid
		return
	}

	if (op == token.QUO || op == token.REM) && y.mode == constant && exact.Sign(y.val) == 0 {
		check.invalidOp(y.pos(), "division by zero")
		x.mode = invalid
		return
	}

	if x.mode == constant && y.mode == constant {
		typ := x.typ.Underlying().(*Basic)
		// force integer division of integer operands
		if op == token.QUO && isInteger(typ) {
			op = token.QUO_ASSIGN
		}
		x.val = exact.BinaryOp(x.val, op, y.val)
		// Typed constants must be representable in
		// their type after each constant operation.
		check.isRepresentable(x, typ)
		return
	}

	x.mode = value
	// x.typ is unchanged
}

// index checks an index/size expression arg for validity.
// If length >= 0, it is the upper bound for arg.
func (check *checker) index(arg ast.Expr, length int64) (i int64, ok bool) {
	var x operand
	check.expr(&x, arg)

	// an untyped constant must be representable as Int
	check.convertUntyped(&x, Typ[Int])
	if x.mode == invalid {
		return
	}

	// the index/size must be of integer type
	if !isInteger(x.typ) {
		check.invalidArg(x.pos(), "%s must be integer", &x)
		return
	}

	// a constant index/size i must be 0 <= i < length
	if x.mode == constant {
		if exact.Sign(x.val) < 0 {
			check.invalidArg(x.pos(), "%s must not be negative", &x)
			return
		}
		i, ok = exact.Int64Val(x.val)
		if !ok || length >= 0 && i >= length {
			check.errorf(x.pos(), "index %s is out of bounds", &x)
			return i, false
		}
		// 0 <= i [ && i < length ]
		return i, true
	}

	return -1, true
}

// indexElts checks the elements (elts) of an array or slice composite literal
// against the literal's element type (typ), and the element indices against
// the literal length if known (length >= 0). It returns the length of the
// literal (maximum index value + 1).
//
func (check *checker) indexedElts(elts []ast.Expr, typ Type, length int64) int64 {
	visited := make(map[int64]bool, len(elts))
	var index, max int64
	for _, e := range elts {
		// determine and check index
		validIndex := false
		eval := e
		if kv, _ := e.(*ast.KeyValueExpr); kv != nil {
			if i, ok := check.index(kv.Key, length); ok {
				if i >= 0 {
					index = i
				}
				validIndex = true
			}
			eval = kv.Value
		} else if length >= 0 && index >= length {
			check.errorf(e.Pos(), "index %d is out of bounds (>= %d)", index, length)
		} else {
			validIndex = true
		}

		// if we have a valid index, check for duplicate entries
		if validIndex {
			if visited[index] {
				check.errorf(e.Pos(), "duplicate index %d in array or slice literal", index)
			}
			visited[index] = true
		}
		index++
		if index > max {
			max = index
		}

		// check element against composite literal element type
		var x operand
		check.exprWithHint(&x, eval, typ)
		if !check.assignment(&x, typ) && x.mode != invalid {
			check.errorf(x.pos(), "cannot use %s as %s value in array or slice literal", &x, typ)
		}
	}
	return max
}

// rawExpr typechecks expression e and initializes x with the expression
// value or type. If an error occurred, x.mode is set to invalid.
// If hint != nil, it is the type of a composite literal element.
//
func (check *checker) rawExpr(x *operand, e ast.Expr, hint Type) {
	if trace {
		check.trace(e.Pos(), "%s", e)
		check.indent++
	}

	check.expr0(x, e, hint)

	// convert x into a user-friendly set of values
	notify := check.ctxt.Expr
	var typ Type
	var val exact.Value
	switch x.mode {
	case invalid:
		typ = Typ[Invalid]
		notify = nil // nothing to do
	case novalue:
		typ = (*Tuple)(nil)
	case constant:
		typ = x.typ
		val = x.val
	case typexprn:
		x.mode = typexpr
		typ = x.typ
		notify = nil // clients were already notified
	default:
		typ = x.typ
	}
	assert(x.expr != nil && typ != nil)

	if isUntyped(typ) {
		// delay notification until it becomes typed
		// or until the end of type checking
		check.untyped[x.expr] = exprInfo{false, typ.(*Basic), val}
	} else if notify != nil {
		// notify clients
		// TODO(gri) ensure that literals always report
		// their dynamic (never interface) type.
		// This is not the case yet.
		notify(x.expr, typ, val)
	}

	if trace {
		check.indent--
		check.trace(e.Pos(), "=> %s", x)
	}
}

// expr0 contains the core of type checking of expressions.
// Must only be called by rawExpr.
//
func (check *checker) expr0(x *operand, e ast.Expr, hint Type) {
	// make sure x has a valid state in case of bailout
	// (was issue 5770)
	x.mode = invalid
	x.typ = Typ[Invalid]

	switch e := e.(type) {
	case *ast.BadExpr:
		goto Error // error was reported before

	case *ast.Ident:
		check.ident(x, e, nil, false)

	case *ast.Ellipsis:
		// ellipses are handled explicitly where they are legal
		// (array composite literals and parameter lists)
		check.errorf(e.Pos(), "invalid use of '...'")
		goto Error

	case *ast.BasicLit:
		x.setConst(e.Kind, e.Value)
		if x.mode == invalid {
			check.invalidAST(e.Pos(), "invalid literal %v", e.Value)
			goto Error
		}

	case *ast.FuncLit:
		if sig, ok := check.typ(e.Type, nil, false).(*Signature); ok {
			x.mode = value
			x.typ = sig
			check.later(nil, sig, e.Body)
		} else {
			check.invalidAST(e.Pos(), "invalid function literal %s", e)
			goto Error
		}

	case *ast.CompositeLit:
		typ := hint
		openArray := false
		if e.Type != nil {
			// [...]T array types may only appear with composite literals.
			// Check for them here so we don't have to handle ... in general.
			typ = nil
			if atyp, _ := e.Type.(*ast.ArrayType); atyp != nil && atyp.Len != nil {
				if ellip, _ := atyp.Len.(*ast.Ellipsis); ellip != nil && ellip.Elt == nil {
					// We have an "open" [...]T array type.
					// Create a new ArrayType with unknown length (-1)
					// and finish setting it up after analyzing the literal.
					typ = &Array{len: -1, elt: check.typ(atyp.Elt, nil, false)}
					openArray = true
				}
			}
			if typ == nil {
				typ = check.typ(e.Type, nil, false)
			}
		}
		if typ == nil {
			check.errorf(e.Pos(), "missing type in composite literal")
			goto Error
		}

		switch utyp := typ.Deref().Underlying().(type) {
		case *Struct:
			if len(e.Elts) == 0 {
				break
			}
			fields := utyp.fields
			if _, ok := e.Elts[0].(*ast.KeyValueExpr); ok {
				// all elements must have keys
				visited := make([]bool, len(fields))
				for _, e := range e.Elts {
					kv, _ := e.(*ast.KeyValueExpr)
					if kv == nil {
						check.errorf(e.Pos(), "mixture of field:value and value elements in struct literal")
						continue
					}
					key, _ := kv.Key.(*ast.Ident)
					if key == nil {
						check.errorf(kv.Pos(), "invalid field name %s in struct literal", kv.Key)
						continue
					}
					i := fieldIndex(utyp.fields, check.pkg, key.Name)
					if i < 0 {
						check.errorf(kv.Pos(), "unknown field %s in struct literal", key.Name)
						continue
					}
					fld := fields[i]
					check.callIdent(key, fld)
					// 0 <= i < len(fields)
					if visited[i] {
						check.errorf(kv.Pos(), "duplicate field name %s in struct literal", key.Name)
						continue
					}
					visited[i] = true
					check.expr(x, kv.Value)
					etyp := fld.typ
					if !check.assignment(x, etyp) {
						if x.mode != invalid {
							check.errorf(x.pos(), "cannot use %s as %s value in struct literal", x, etyp)
						}
						continue
					}
				}
			} else {
				// no element must have a key
				for i, e := range e.Elts {
					if kv, _ := e.(*ast.KeyValueExpr); kv != nil {
						check.errorf(kv.Pos(), "mixture of field:value and value elements in struct literal")
						continue
					}
					check.expr(x, e)
					if i >= len(fields) {
						check.errorf(x.pos(), "too many values in struct literal")
						break // cannot continue
					}
					// i < len(fields)
					etyp := fields[i].typ
					if !check.assignment(x, etyp) {
						if x.mode != invalid {
							check.errorf(x.pos(), "cannot use %s as %s value in struct literal", x, etyp)
						}
						continue
					}
				}
				if len(e.Elts) < len(fields) {
					check.errorf(e.Rbrace, "too few values in struct literal")
					// ok to continue
				}
			}

		case *Array:
			n := check.indexedElts(e.Elts, utyp.elt, utyp.len)
			// if we have an "open" [...]T array, set the length now that we know it
			if openArray {
				utyp.len = n
			}

		case *Slice:
			check.indexedElts(e.Elts, utyp.elt, -1)

		case *Map:
			visited := make(map[interface{}]bool, len(e.Elts))
			for _, e := range e.Elts {
				kv, _ := e.(*ast.KeyValueExpr)
				if kv == nil {
					check.errorf(e.Pos(), "missing key in map literal")
					continue
				}
				check.expr(x, kv.Key)
				if !check.assignment(x, utyp.key) {
					if x.mode != invalid {
						check.errorf(x.pos(), "cannot use %s as %s key in map literal", x, utyp.key)
					}
					continue
				}
				if x.mode == constant {
					if visited[x.val] {
						check.errorf(x.pos(), "duplicate key %s in map literal", x.val)
						continue
					}
					visited[x.val] = true
				}
				check.exprWithHint(x, kv.Value, utyp.elt)
				if !check.assignment(x, utyp.elt) {
					if x.mode != invalid {
						check.errorf(x.pos(), "cannot use %s as %s value in map literal", x, utyp.elt)
					}
					continue
				}
			}

		default:
			check.errorf(e.Pos(), "%s is not a valid composite literal type", typ)
			goto Error
		}

		x.mode = value
		x.typ = typ

	case *ast.ParenExpr:
		check.rawExpr(x, e.X, nil)

	case *ast.SelectorExpr:
		check.selector(x, e)

	case *ast.IndexExpr:
		check.expr(x, e.X)
		if x.mode == invalid {
			goto Error
		}

		valid := false
		length := int64(-1) // valid if >= 0
		switch typ := x.typ.Underlying().(type) {
		case *Basic:
			if isString(typ) {
				valid = true
				if x.mode == constant {
					length = int64(len(exact.StringVal(x.val)))
				}
				// an indexed string always yields a byte value
				// (not a constant) even if the string and the
				// index are constant
				x.mode = value
				x.typ = Typ[Byte]
			}

		case *Array:
			valid = true
			length = typ.len
			if x.mode != variable {
				x.mode = value
			}
			x.typ = typ.elt

		case *Pointer:
			if typ, _ := typ.base.Underlying().(*Array); typ != nil {
				valid = true
				length = typ.len
				x.mode = variable
				x.typ = typ.elt
			}

		case *Slice:
			valid = true
			x.mode = variable
			x.typ = typ.elt

		case *Map:
			var key operand
			check.expr(&key, e.Index)
			if !check.assignment(&key, typ.key) {
				if key.mode != invalid {
					check.invalidOp(key.pos(), "cannot use %s as map index of type %s", &key, typ.key)
				}
				goto Error
			}
			x.mode = valueok
			x.typ = typ.elt
			x.expr = e
			return
		}

		if !valid {
			check.invalidOp(x.pos(), "cannot index %s", x)
			goto Error
		}

		if e.Index == nil {
			check.invalidAST(e.Pos(), "missing index expression for %s", x)
			goto Error
		}

		check.index(e.Index, length)
		// ok to continue

	case *ast.SliceExpr:
		check.expr(x, e.X)
		if x.mode == invalid {
			goto Error
		}

		valid := false
		length := int64(-1) // valid if >= 0
		switch typ := x.typ.Underlying().(type) {
		case *Basic:
			if isString(typ) {
				valid = true
				if x.mode == constant {
					length = int64(len(exact.StringVal(x.val))) + 1 // +1 for slice
				}
				// a sliced string always yields a string value
				// of the same type as the original string (not
				// a constant) even if the string and the indices
				// are constant
				x.mode = value
				// x.typ doesn't change, but if it is an untyped
				// string it becomes string (see also issue 4913).
				if typ.kind == UntypedString {
					x.typ = Typ[String]
				}
			}

		case *Array:
			valid = true
			length = typ.len + 1 // +1 for slice
			if x.mode != variable {
				check.invalidOp(x.pos(), "cannot slice %s (value not addressable)", x)
				goto Error
			}
			x.typ = &Slice{elt: typ.elt}

		case *Pointer:
			if typ, _ := typ.base.Underlying().(*Array); typ != nil {
				valid = true
				length = typ.len + 1 // +1 for slice
				x.mode = variable
				x.typ = &Slice{elt: typ.elt}
			}

		case *Slice:
			valid = true
			x.mode = variable
			// x.typ doesn't change
		}

		if !valid {
			check.invalidOp(x.pos(), "cannot slice %s", x)
			goto Error
		}

		lo := int64(0)
		if e.Low != nil {
			if i, ok := check.index(e.Low, length); ok && i >= 0 {
				lo = i
			}
		}

		hi := int64(-1)
		if e.High != nil {
			if i, ok := check.index(e.High, length); ok && i >= 0 {
				hi = i
			}
		} else if length >= 0 {
			hi = length
		}

		if lo >= 0 && hi >= 0 && lo > hi {
			check.errorf(e.Low.Pos(), "inverted slice range: %d > %d", lo, hi)
			// ok to continue
		}

	case *ast.TypeAssertExpr:
		check.expr(x, e.X)
		if x.mode == invalid {
			goto Error
		}
		var T *Interface
		if T, _ = x.typ.Underlying().(*Interface); T == nil {
			check.invalidOp(x.pos(), "%s is not an interface", x)
			goto Error
		}
		// x.(type) expressions are handled explicitly in type switches
		if e.Type == nil {
			check.invalidAST(e.Pos(), "use of .(type) outside type switch")
			goto Error
		}
		typ := check.typ(e.Type, nil, false)
		if typ == Typ[Invalid] {
			goto Error
		}
		if method, wrongType := MissingMethod(typ, T); method != nil {
			var msg string
			if wrongType {
				msg = "%s cannot have dynamic type %s (wrong type for method %s)"
			} else if _, ok := typ.Underlying().(*Interface); !ok {
				// Concrete types must have all methods of T. Interfaces
				// may not have all methods of T statically, but may have
				// them dynamically; don't report an error in that case.
				msg = "%s cannot have dynamic type %s (missing method %s)"
			}
			if msg != "" {
				check.errorf(e.Type.Pos(), msg, x, typ, method.name)
			}
			// ok to continue
		}
		x.mode = valueok
		x.typ = typ

	case *ast.CallExpr:
		check.call(x, e)

	case *ast.StarExpr:
		check.exprOrType(x, e.X)
		switch x.mode {
		case invalid:
			goto Error
		case typexpr:
			x.typ = &Pointer{base: x.typ}
		default:
			if typ, ok := x.typ.Underlying().(*Pointer); ok {
				x.mode = variable
				x.typ = typ.base
			} else {
				check.invalidOp(x.pos(), "cannot indirect %s", x)
				goto Error
			}
		}

	case *ast.UnaryExpr:
		check.expr(x, e.X)
		if x.mode == invalid {
			goto Error
		}
		check.unary(x, e.Op)
		if x.mode == invalid {
			goto Error
		}

	case *ast.BinaryExpr:
		check.binary(x, e.X, e.Y, e.Op)
		if x.mode == invalid {
			goto Error
		}

	case *ast.KeyValueExpr:
		// key:value expressions are handled in composite literals
		check.invalidAST(e.Pos(), "no key:value expected")
		goto Error

	case *ast.ArrayType, *ast.StructType, *ast.FuncType,
		*ast.InterfaceType, *ast.MapType, *ast.ChanType:
		x.mode = typexprn // not typexpr; Context.Expr callback was invoked by check.typ
		x.typ = check.typ(e, nil, false)

	default:
		if debug {
			check.dump("expr = %v (%T)", e, e)
		}
		unreachable()
	}

	// everything went well
	x.expr = e
	return

Error:
	x.mode = invalid
	x.expr = e
}

// expr typechecks expression e and initializes x with the expression value.
// If an error occurred, x.mode is set to invalid.
//
func (check *checker) expr(x *operand, e ast.Expr) {
	check.rawExpr(x, e, nil)
	switch x.mode {
	case novalue:
		check.errorf(x.pos(), "%s used as value", x)
		x.mode = invalid
	case typexpr:
		check.errorf(x.pos(), "%s is not an expression", x)
		x.mode = invalid
	}
}

// exprWithHint typechecks expression e and initializes x with the expression value.
// If an error occurred, x.mode is set to invalid.
// If hint != nil, it is the type of a composite literal element.
//
func (check *checker) exprWithHint(x *operand, e ast.Expr, hint Type) {
	assert(hint != nil)
	check.rawExpr(x, e, hint)
	switch x.mode {
	case novalue:
		check.errorf(x.pos(), "%s used as value", x)
		x.mode = invalid
	case typexpr:
		check.errorf(x.pos(), "%s is not an expression", x)
		x.mode = invalid
	}
}

// exprOrType typechecks expression or type e and initializes x with the expression value or type.
// If an error occurred, x.mode is set to invalid.
//
func (check *checker) exprOrType(x *operand, e ast.Expr) {
	check.rawExpr(x, e, nil)
	if x.mode == novalue {
		check.errorf(x.pos(), "%s used as value or type", x)
		x.mode = invalid
	}
}
