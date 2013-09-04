// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package oracle

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"

	"code.google.com/p/go.tools/go/exact"
	"code.google.com/p/go.tools/go/types"
	"code.google.com/p/go.tools/importer"
	"code.google.com/p/go.tools/oracle/json"
	"code.google.com/p/go.tools/pointer"
	"code.google.com/p/go.tools/ssa"
)

// describe describes the syntax node denoted by the query position,
// including:
// - its syntactic category
// - the location of the definition of its referent (for identifiers)
// - its type and method set (for an expression or type expression)
// - its points-to set (for a pointer-like expression)
// - its concrete types (for an interface expression) and their points-to sets.
//
// All printed sets are sorted to ensure determinism.
//
func describe(o *oracle) (queryResult, error) {
	if false { // debugging
		o.fprintf(os.Stderr, o.queryPath[0], "you selected: %s %s",
			importer.NodeDescription(o.queryPath[0]), pathToString2(o.queryPath))
	}

	path, action := findInterestingNode(o.queryPkgInfo, o.queryPath)
	switch action {
	case actionExpr:
		return describeValue(o, path)

	case actionType:
		return describeType(o, path)

	case actionPackage:
		return describePackage(o, path)

	case actionStmt:
		return describeStmt(o, path)

	case actionUnknown:
		return &describeUnknownResult{path[0]}, nil

	default:
		panic(action) // unreachable
	}
}

type describeUnknownResult struct {
	node ast.Node
}

func (r *describeUnknownResult) display(printf printfFunc) {
	// Nothing much to say about misc syntax.
	printf(r.node, "%s", importer.NodeDescription(r.node))
}

func (r *describeUnknownResult) toJSON(res *json.Result, fset *token.FileSet) {
	res.Describe = &json.Describe{
		Desc: importer.NodeDescription(r.node),
		Pos:  fset.Position(r.node.Pos()).String(),
	}
}

type action int

const (
	actionUnknown action = iota // None of the below
	actionExpr                  // FuncDecl, true Expr or Ident(types.{Const,Var})
	actionType                  // type Expr or Ident(types.TypeName).
	actionStmt                  // Stmt or Ident(types.Label)
	actionPackage               // Ident(types.Package) or ImportSpec
)

// findInterestingNode classifies the syntax node denoted by path as one of:
//    - an expression, part of an expression or a reference to a constant
//      or variable;
//    - a type, part of a type, or a reference to a named type;
//    - a statement, part of a statement, or a label referring to a statement;
//    - part of a package declaration or import spec.
//    - none of the above.
// and returns the most "interesting" associated node, which may be
// the same node, an ancestor or a descendent.
//
func findInterestingNode(pkginfo *importer.PackageInfo, path []ast.Node) ([]ast.Node, action) {
	// TODO(adonovan): integrate with go/types/stdlib_test.go and
	// apply this to every AST node we can find to make sure it
	// doesn't crash.

	// TODO(adonovan): audit for ParenExpr safety, esp. since we
	// traverse up and down.

	// TODO(adonovan): if the users selects the "." in
	// "fmt.Fprintf()", they'll get an ambiguous selection error;
	// we won't even reach here.  Can we do better?

	// TODO(adonovan): describing a field within 'type T struct {...}'
	// describes the (anonymous) struct type and concludes "no methods".
	// We should ascend to the enclosing type decl, if any.

	for len(path) > 0 {
		switch n := path[0].(type) {
		case *ast.GenDecl:
			if len(n.Specs) == 1 {
				// Descend to sole {Import,Type,Value}Spec child.
				path = append([]ast.Node{n.Specs[0]}, path...)
				continue
			}
			return path, actionUnknown // uninteresting

		case *ast.FuncDecl:
			// Descend to function name.
			path = append([]ast.Node{n.Name}, path...)
			continue

		case *ast.ImportSpec:
			return path, actionPackage

		case *ast.ValueSpec:
			if len(n.Names) == 1 {
				// Descend to sole Ident child.
				path = append([]ast.Node{n.Names[0]}, path...)
				continue
			}
			return path, actionUnknown // uninteresting

		case *ast.TypeSpec:
			// Descend to type name.
			path = append([]ast.Node{n.Name}, path...)
			continue

		case ast.Stmt:
			return path, actionStmt

		case *ast.ArrayType,
			*ast.StructType,
			*ast.FuncType,
			*ast.InterfaceType,
			*ast.MapType,
			*ast.ChanType:
			return path, actionType

		case *ast.Comment, *ast.CommentGroup, *ast.File, *ast.KeyValueExpr, *ast.CommClause:
			return path, actionUnknown // uninteresting

		case *ast.Ellipsis:
			// Continue to enclosing node.
			// e.g. [...]T in ArrayType
			//      f(x...) in CallExpr
			//      f(x...T) in FuncType

		case *ast.Field:
			// TODO(adonovan): this needs more thought,
			// since fields can be so many things.
			if len(n.Names) == 1 {
				// Descend to sole Ident child.
				path = append([]ast.Node{n.Names[0]}, path...)
				continue
			}
			// Zero names (e.g. anon field in struct)
			// or multiple field or param names:
			// continue to enclosing field list.

		case *ast.FieldList:
			// Continue to enclosing node:
			// {Struct,Func,Interface}Type or FuncDecl.

		case *ast.BasicLit:
			if _, ok := path[1].(*ast.ImportSpec); ok {
				return path[1:], actionPackage
			}
			return path, actionExpr

		case *ast.SelectorExpr:
			if pkginfo.ObjectOf(n.Sel) == nil {
				// Is this reachable?
				return path, actionUnknown
			}
			// Descend to .Sel child.
			path = append([]ast.Node{n.Sel}, path...)
			continue

		case *ast.Ident:
			switch obj := pkginfo.ObjectOf(n).(type) {
			case *types.Package:
				return path, actionPackage

			case *types.Const:
				return path, actionExpr

			case *types.Label:
				return path, actionStmt

			case *types.TypeName:
				return path, actionType

			case *types.Var:
				// For x in 'struct {x T}', return struct type, for now.
				if _, ok := path[1].(*ast.Field); ok {
					_ = path[2].(*ast.FieldList) // assertion
					if _, ok := path[3].(*ast.StructType); ok {
						return path[3:], actionType
					}
				}
				return path, actionExpr

			case *types.Func:
				// For f in 'interface {f()}', return the interface type, for now.
				if _, ok := path[1].(*ast.Field); ok {
					_ = path[2].(*ast.FieldList) // assertion
					if _, ok := path[3].(*ast.InterfaceType); ok {
						return path[3:], actionType
					}
				}

				// For reference to built-in function, return enclosing call.
				if _, ok := obj.Type().(*types.Builtin); ok {
					// Ascend to enclosing function call.
					path = path[1:]
					continue
				}

				return path, actionExpr
			}

			// No object.
			switch path[1].(type) {
			case *ast.SelectorExpr:
				// Return enclosing selector expression.
				return path[1:], actionExpr

			case *ast.Field:
				// TODO(adonovan): test this.
				// e.g. all f in:
				//  struct { f, g int }
				//  interface { f() }
				//  func (f T) method(f, g int) (f, g bool)
				//
				// switch path[3].(type) {
				// case *ast.FuncDecl:
				// case *ast.StructType:
				// case *ast.InterfaceType:
				// }
				//
				// return path[1:], actionExpr
				//
				// Unclear what to do with these.
				// Struct.Fields             -- field
				// Interface.Methods         -- field
				// FuncType.{Params.Results} -- actionExpr
				// FuncDecl.Recv             -- actionExpr

			case *ast.ImportSpec:
				// TODO(adonovan): fix: why no package object? go/types bug?
				return path[1:], actionPackage

			default:
				// e.g. blank identifier (go/types bug?)
				// or y in "switch y := x.(type)" (go/types bug?)
				fmt.Printf("unknown reference %s in %T\n", n, path[1])
				return path, actionUnknown
			}

		case *ast.StarExpr:
			if pkginfo.IsType(n) {
				return path, actionType
			}
			return path, actionExpr

		case ast.Expr:
			// All Expr but {BasicLit,Ident,StarExpr} are
			// "true" expressions that evaluate to a value.
			return path, actionExpr
		}

		// Ascend to parent.
		path = path[1:]
	}

	return nil, actionUnknown // unreachable
}

// ---- VALUE ------------------------------------------------------------

// ssaValueForIdent returns the ssa.Value for the ast.Ident whose path
// to the root of the AST is path.  It may return a nil Value without
// an error to indicate the pointer analysis is not appropriate.
//
func ssaValueForIdent(o *oracle, obj types.Object, path []ast.Node) (ssa.Value, error) {
	if obj, ok := obj.(*types.Var); ok {
		pkg := o.prog.Package(o.queryPkgInfo.Pkg)
		pkg.Build()
		if v := o.prog.VarValue(obj, pkg, path); v != nil {
			// Don't run pointer analysis on a ref to a const expression.
			if _, ok := v.(*ssa.Const); ok {
				v = nil
			}
			return v, nil
		}
		return nil, fmt.Errorf("can't locate SSA Value for var %s", obj.Name())
	}

	// Don't run pointer analysis on const/func objects.
	return nil, nil
}

// ssaValueForExpr returns the ssa.Value of the non-ast.Ident
// expression whose path to the root of the AST is path.  It may
// return a nil Value without an error to indicate the pointer
// analysis is not appropriate.
//
func ssaValueForExpr(o *oracle, path []ast.Node) (ssa.Value, error) {
	pkg := o.prog.Package(o.queryPkgInfo.Pkg)
	pkg.SetDebugMode(true)
	pkg.Build()

	fn := ssa.EnclosingFunction(pkg, path)
	if fn == nil {
		return nil, fmt.Errorf("no SSA function built for this location (dead code?)")
	}

	if v := fn.ValueForExpr(path[0].(ast.Expr)); v != nil {
		return v, nil
	}

	return nil, fmt.Errorf("can't locate SSA Value for expression in %s", fn)
}

func describeValue(o *oracle, path []ast.Node) (*describeValueResult, error) {
	var expr ast.Expr
	switch n := path[0].(type) {
	case *ast.ValueSpec:
		// ambiguous ValueSpec containing multiple names
		return nil, o.errorf(n, "multiple value specification")
	case ast.Expr:
		expr = n
	default:
		// Is this reachable?
		return nil, o.errorf(n, "unexpected AST for expr: %T", n)
	}

	// From this point on, we cannot fail with an error.
	// Failure to run the pointer analysis will be reported later.
	//
	// Our disposition to pointer analysis may be one of the following:
	// - ok:    ssa.Value was const or func.
	// - error: no ssa.Value for expr (e.g. trivially dead code)
	// - ok:    ssa.Value is non-pointerlike
	// - error: no Pointer for ssa.Value (e.g. analytically unreachable)
	// - ok:    Pointer has empty points-to set
	// - ok:    Pointer has non-empty points-to set
	// ptaErr is non-nil only in the "error:" cases.

	var value ssa.Value
	var ptaErr error
	var obj types.Object

	// Determine the ssa.Value for the expression.
	if id, ok := expr.(*ast.Ident); ok {
		// def/ref of func/var/const object
		obj = o.queryPkgInfo.ObjectOf(id)
		value, ptaErr = ssaValueForIdent(o, obj, path)
	} else {
		// any other expression
		if o.queryPkgInfo.ValueOf(expr) == nil { // non-constant?
			value, ptaErr = ssaValueForExpr(o, path)
		}
	}

	// Don't run pointer analysis on non-pointerlike types.
	if value != nil && !pointer.CanPoint(value.Type()) {
		value = nil
	}

	// Run pointer analysis of the selected SSA value.
	var ptrs []pointerResult
	if value != nil {
		buildSSA(o)

		o.config.QueryValues = map[ssa.Value][]pointer.Pointer{value: nil}
		ptrAnalysis(o)

		// Combine the PT sets from all contexts.
		pointers := o.config.QueryValues[value]
		if pointers == nil {
			ptaErr = fmt.Errorf("PTA did not encounter this expression (dead code?)")
		}
		pts := pointer.PointsToCombined(pointers)

		if _, ok := value.Type().Underlying().(*types.Interface); ok {
			// Show concrete types for interface expression.
			if concs := pts.ConcreteTypes(); concs.Len() > 0 {
				concs.Iterate(func(conc types.Type, pta interface{}) {
					combined := pointer.PointsToCombined(pta.([]pointer.Pointer))
					labels := combined.Labels()
					sort.Sort(byPosAndString(labels)) // to ensure determinism
					ptrs = append(ptrs, pointerResult{conc, labels})
				})
			}
		} else {
			// Show labels for other expressions.
			labels := pts.Labels()
			sort.Sort(byPosAndString(labels)) // to ensure determinism
			ptrs = append(ptrs, pointerResult{value.Type(), labels})
		}
	}
	sort.Sort(byTypeString(ptrs)) // to ensure determinism

	typ := o.queryPkgInfo.TypeOf(expr)
	constVal := o.queryPkgInfo.ValueOf(expr)

	return &describeValueResult{
		expr:     expr,
		typ:      typ,
		constVal: constVal,
		obj:      obj,
		ptaErr:   ptaErr,
		ptrs:     ptrs,
	}, nil
}

type pointerResult struct {
	typ    types.Type // type of the pointer (always concrete)
	labels []*pointer.Label
}

type describeValueResult struct {
	expr     ast.Expr        // query node
	typ      types.Type      // type of expression
	constVal exact.Value     // value of expression, if constant
	obj      types.Object    // var/func/const object, if expr was Ident
	ptaErr   error           // reason why pointer analysis couldn't be run, or failed
	ptrs     []pointerResult // pointer info (typ is concrete => len==1)
}

func (r *describeValueResult) display(printf printfFunc) {
	var prefix, suffix string
	if r.constVal != nil {
		suffix = fmt.Sprintf(" of constant value %s", r.constVal)
	}
	switch obj := r.obj.(type) {
	case *types.Func:
		if recv := obj.Type().(*types.Signature).Recv(); recv != nil {
			if _, ok := recv.Type().Underlying().(*types.Interface); ok {
				prefix = "interface method "
			} else {
				prefix = "method "
			}
		}

	case *types.Var:
		// TODO(adonovan): go/types should make it simple to
		// ask: IsStructField(*Var)?
		if false {
			prefix = "struct field "
		}
	}

	// Describe the expression.
	if r.obj != nil {
		if r.obj.Pos() == r.expr.Pos() {
			// defining ident
			printf(r.expr, "definition of %s%s%s", prefix, r.obj, suffix)
		} else {
			// referring ident
			printf(r.expr, "reference to %s%s%s", prefix, r.obj, suffix)
			if def := r.obj.Pos(); def != token.NoPos {
				printf(def, "defined here")
			}
		}
	} else {
		desc := importer.NodeDescription(r.expr)
		if suffix != "" {
			// constant expression
			printf(r.expr, "%s%s", desc, suffix)
		} else {
			// non-constant expression
			printf(r.expr, "%s of type %s", desc, r.typ)
		}
	}

	// pointer analysis could not be run
	if r.ptaErr != nil {
		printf(r.expr, "no points-to information: %s", r.ptaErr)
		return
	}

	if r.ptrs == nil {
		return // PTA was not invoked (not an error)
	}

	// Display the results of pointer analysis.
	if _, ok := r.typ.Underlying().(*types.Interface); ok {
		// Show concrete types for interface expression.
		if len(r.ptrs) > 0 {
			printf(false, "interface may contain these concrete types:")
			for _, ptr := range r.ptrs {
				var obj types.Object
				if nt, ok := deref(ptr.typ).(*types.Named); ok {
					obj = nt.Obj()
				}
				if len(ptr.labels) > 0 {
					printf(obj, "\t%s, may point to:", ptr.typ)
					printLabels(printf, ptr.labels, "\t\t")
				} else {
					printf(obj, "\t%s", ptr.typ)
				}
			}
		} else {
			printf(false, "interface cannot contain any concrete values.")
		}
	} else {
		// Show labels for other expressions.
		if ptr := r.ptrs[0]; len(ptr.labels) > 0 {
			printf(false, "value may point to these labels:")
			printLabels(printf, ptr.labels, "\t")
		} else {
			printf(false, "value cannot point to anything.")
		}
	}
}

func (r *describeValueResult) toJSON(res *json.Result, fset *token.FileSet) {
	var value, objpos, ptaerr string
	if r.constVal != nil {
		value = r.constVal.String()
	}
	if r.obj != nil {
		objpos = fset.Position(r.obj.Pos()).String()
	}
	if r.ptaErr != nil {
		ptaerr = r.ptaErr.Error()
	}

	var pts []*json.DescribePointer
	for _, ptr := range r.ptrs {
		var namePos string
		if nt, ok := deref(ptr.typ).(*types.Named); ok {
			namePos = fset.Position(nt.Obj().Pos()).String()
		}
		var labels []json.DescribePTALabel
		for _, l := range ptr.labels {
			labels = append(labels, json.DescribePTALabel{
				Pos:  fset.Position(l.Pos()).String(),
				Desc: l.String(),
			})
		}
		pts = append(pts, &json.DescribePointer{
			Type:    ptr.typ.String(),
			NamePos: namePos,
			Labels:  labels,
		})
	}

	res.Describe = &json.Describe{
		Desc:   importer.NodeDescription(r.expr),
		Pos:    fset.Position(r.expr.Pos()).String(),
		Detail: "value",
		Value: &json.DescribeValue{
			Type:   r.typ.String(),
			Value:  value,
			ObjPos: objpos,
			PTAErr: ptaerr,
			PTS:    pts,
		},
	}
}

type byTypeString []pointerResult

func (a byTypeString) Len() int           { return len(a) }
func (a byTypeString) Less(i, j int) bool { return a[i].typ.String() < a[j].typ.String() }
func (a byTypeString) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }

type byPosAndString []*pointer.Label

func (a byPosAndString) Len() int { return len(a) }
func (a byPosAndString) Less(i, j int) bool {
	cmp := a[i].Pos() - a[j].Pos()
	return cmp < 0 || (cmp == 0 && a[i].String() < a[j].String())
}
func (a byPosAndString) Swap(i, j int) { a[i], a[j] = a[j], a[i] }

func printLabels(printf printfFunc, labels []*pointer.Label, prefix string) {
	// TODO(adonovan): due to context-sensitivity, many of these
	// labels may differ only by context, which isn't apparent.
	for _, label := range labels {
		printf(label, "%s%s", prefix, label)
	}
}

// ---- TYPE ------------------------------------------------------------

func describeType(o *oracle, path []ast.Node) (*describeTypeResult, error) {
	var description string
	var t types.Type
	switch n := path[0].(type) {
	case *ast.Ident:
		t = o.queryPkgInfo.TypeOf(n)
		switch t := t.(type) {
		case *types.Basic:
			description = "reference to built-in type " + t.String()

		case *types.Named:
			isDef := t.Obj().Pos() == n.Pos() // see caveats at isDef above
			if isDef {
				description = "definition of type " + t.String()
			} else {
				description = "reference to type " + t.String()
			}
		}

	case ast.Expr:
		t = o.queryPkgInfo.TypeOf(n)
		description = "type " + t.String()

	default:
		// Unreachable?
		return nil, o.errorf(n, "unexpected AST for type: %T", n)
	}

	return &describeTypeResult{
		node:        path[0],
		description: description,
		typ:         t,
		methods:     accessibleMethods(t, o.queryPkgInfo.Pkg),
	}, nil
}

type describeTypeResult struct {
	node        ast.Node
	description string
	typ         types.Type
	methods     []*types.Selection
}

func (r *describeTypeResult) display(printf printfFunc) {
	printf(r.node, "%s", r.description)

	// Show the underlying type for a reference to a named type.
	if nt, ok := r.typ.(*types.Named); ok && r.node.Pos() != nt.Obj().Pos() {
		printf(nt.Obj(), "defined as %s", nt.Underlying())
	}

	// Print the method set, if the type kind is capable of bearing methods.
	switch r.typ.(type) {
	case *types.Interface, *types.Struct, *types.Named:
		if len(r.methods) > 0 {
			printf(r.node, "Method set:")
			for _, meth := range r.methods {
				printf(meth.Obj(), "\t%s", meth)
			}
		} else {
			printf(r.node, "No methods.")
		}
	}
}

func (r *describeTypeResult) toJSON(res *json.Result, fset *token.FileSet) {
	var namePos, nameDef string
	if nt, ok := r.typ.(*types.Named); ok {
		namePos = fset.Position(nt.Obj().Pos()).String()
		nameDef = nt.Underlying().String()
	}
	res.Describe = &json.Describe{
		Desc:   r.description,
		Pos:    fset.Position(r.node.Pos()).String(),
		Detail: "type",
		Type: &json.DescribeType{
			Type:    r.typ.String(),
			NamePos: namePos,
			NameDef: nameDef,
			Methods: methodsToJSON(r.methods, fset),
		},
	}
}

// ---- PACKAGE ------------------------------------------------------------

func describePackage(o *oracle, path []ast.Node) (*describePackageResult, error) {
	var description string
	var importPath string
	switch n := path[0].(type) {
	case *ast.ImportSpec:
		// importPath = o.queryPkgInfo.ObjectOf(n.Name).(*types.Package).Path()
		// description = "import of package " + importPath
		// TODO(gri): o.queryPkgInfo.ObjectOf(n.Name) may be nil.
		// e.g. "fmt" import in cmd/oracle/main.go.    Why?
		// Workaround:
		description = "import of package " + n.Path.Value
		importPath, _ = strconv.Unquote(n.Path.Value)

	case *ast.Ident:
		importPath = o.queryPkgInfo.ObjectOf(n).(*types.Package).Path()
		if _, isDef := path[1].(*ast.File); isDef {
			description = fmt.Sprintf("definition of package %q", importPath)
		} else {
			description = fmt.Sprintf("reference to package %q", importPath)
		}
		if importPath == "" {
			// TODO(gri): fix.
			return nil, o.errorf(n, "types.Package.Path() returned \"\"\n")
		}

	default:
		// Unreachable?
		return nil, o.errorf(n, "unexpected AST for package: %T", n)
	}

	var members []*describeMember
	// NB: package "unsafe" has no object.
	if pkg := o.prog.PackagesByPath[importPath]; pkg != nil {
		// Compute set of exported package members in lexicographic order.
		var names []string
		for name := range pkg.Members {
			if pkg.Object == o.queryPkgInfo.Pkg || ast.IsExported(name) {
				names = append(names, name)
			}
		}
		sort.Strings(names)

		// Enumerate the package members.
		for _, name := range names {
			mem := pkg.Members[name]
			var methods []*types.Selection
			if mem, ok := mem.(*ssa.Type); ok {
				methods = accessibleMethods(mem.Type(), o.queryPkgInfo.Pkg)
			}
			members = append(members, &describeMember{
				mem,
				methods,
			})
		}
	}

	return &describePackageResult{o.prog.Fset, path[0], description, importPath, members}, nil
}

type describePackageResult struct {
	fset        *token.FileSet
	node        ast.Node
	description string
	path        string
	members     []*describeMember // in lexicographic name order
}

type describeMember struct {
	mem     ssa.Member
	methods []*types.Selection // in types.MethodSet order
}

func (r *describePackageResult) display(printf printfFunc) {
	printf(r.node, "%s", r.description)

	// Compute max width of name "column".
	maxname := 0
	for _, mem := range r.members {
		if l := len(mem.mem.Name()); l > maxname {
			maxname = l
		}
	}

	for _, mem := range r.members {
		printf(mem.mem, "\t%s", formatMember(mem.mem, maxname))
		for _, meth := range mem.methods {
			printf(meth.Obj(), "\t\t%s", meth)
		}
	}
}

func formatMember(mem ssa.Member, maxname int) string {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "%-5s %-*s", mem.Token(), maxname, mem.Name())
	switch mem := mem.(type) {
	case *ssa.NamedConst:
		fmt.Fprintf(&buf, " %s = %s", mem.Type(), mem.Value.Name())

	case *ssa.Function:
		fmt.Fprintf(&buf, " %s", mem.Type())

	case *ssa.Type:
		// Abbreviate long aggregate type names.
		var abbrev string
		switch t := mem.Type().Underlying().(type) {
		case *types.Interface:
			if t.NumMethods() > 1 {
				abbrev = "interface{...}"
			}
		case *types.Struct:
			if t.NumFields() > 1 {
				abbrev = "struct{...}"
			}
		}
		if abbrev == "" {
			fmt.Fprintf(&buf, " %s", mem.Type().Underlying())
		} else {
			fmt.Fprintf(&buf, " %s", abbrev)
		}

	case *ssa.Global:
		fmt.Fprintf(&buf, " %s", deref(mem.Type()))
	}
	return buf.String()
}

func (r *describePackageResult) toJSON(res *json.Result, fset *token.FileSet) {
	var members []*json.DescribeMember
	for _, mem := range r.members {
		typ := mem.mem.Type()
		var val string
		switch mem := mem.mem.(type) {
		case *ssa.NamedConst:
			val = mem.Value.Value.String()
		case *ssa.Type:
			typ = typ.Underlying()
		case *ssa.Global:
			typ = deref(typ)
		}
		members = append(members, &json.DescribeMember{
			Name:    mem.mem.Name(),
			Type:    typ.String(),
			Value:   val,
			Pos:     fset.Position(mem.mem.Pos()).String(),
			Kind:    mem.mem.Token().String(),
			Methods: methodsToJSON(mem.methods, fset),
		})
	}
	res.Describe = &json.Describe{
		Desc:   r.description,
		Pos:    fset.Position(r.node.Pos()).String(),
		Detail: "package",
		Package: &json.DescribePackage{
			Path:    r.path,
			Members: members,
		},
	}
}

// ---- STATEMENT ------------------------------------------------------------

func describeStmt(o *oracle, path []ast.Node) (*describeStmtResult, error) {
	var description string
	switch n := path[0].(type) {
	case *ast.Ident:
		if o.queryPkgInfo.ObjectOf(n).Pos() == n.Pos() {
			description = "labelled statement"
		} else {
			description = "reference to labelled statement"
		}

	default:
		// Nothing much to say about statements.
		description = importer.NodeDescription(n)
	}
	return &describeStmtResult{o.prog.Fset, path[0], description}, nil
}

type describeStmtResult struct {
	fset        *token.FileSet
	node        ast.Node
	description string
}

func (r *describeStmtResult) display(printf printfFunc) {
	printf(r.node, "%s", r.description)
}

func (r *describeStmtResult) toJSON(res *json.Result, fset *token.FileSet) {
	res.Describe = &json.Describe{
		Desc:   r.description,
		Pos:    fset.Position(r.node.Pos()).String(),
		Detail: "unknown",
	}
}

// ------------------- Utilities -------------------

// pathToString returns a string containing the concrete types of the
// nodes in path.
func pathToString2(path []ast.Node) string {
	var buf bytes.Buffer
	fmt.Fprint(&buf, "[")
	for i, n := range path {
		if i > 0 {
			fmt.Fprint(&buf, " ")
		}
		fmt.Fprint(&buf, strings.TrimPrefix(fmt.Sprintf("%T", n), "*ast."))
	}
	fmt.Fprint(&buf, "]")
	return buf.String()
}

func accessibleMethods(t types.Type, from *types.Package) []*types.Selection {
	var methods []*types.Selection
	for _, meth := range ssa.IntuitiveMethodSet(t) {
		if isAccessibleFrom(meth.Obj(), from) {
			methods = append(methods, meth)
		}
	}
	return methods
}

func isAccessibleFrom(obj types.Object, pkg *types.Package) bool {
	return ast.IsExported(obj.Name()) || obj.Pkg() == pkg
}

func methodsToJSON(methods []*types.Selection, fset *token.FileSet) []json.DescribeMethod {
	var jmethods []json.DescribeMethod
	for _, meth := range methods {
		jmethods = append(jmethods, json.DescribeMethod{
			Name: meth.String(),
			Pos:  fset.Position(meth.Obj().Pos()).String(),
		})
	}
	return jmethods
}
