// This file contains functions for declaring function prototypes, expressions
// that call functions, returning from function and the coordination of
// processing the function bodies.

package transpiler

import (
	"fmt"
	"strings"

	"github.com/elliotchance/c2go/ast"
	"github.com/elliotchance/c2go/program"
	"github.com/elliotchance/c2go/types"
	"github.com/elliotchance/c2go/util"

	goast "go/ast"
	"go/token"
)

// getFunctionBody returns the function body as a CompoundStmt. If the function
// is a prototype or forward declaration (meaning it has no body) then nil is
// returned.
func getFunctionBody(n *ast.FunctionDecl) *ast.CompoundStmt {
	// It's possible that the last node is the CompoundStmt (after all the
	// parameter declarations) - but I don't know this for certain so we will
	// look at all the children for now.
	for _, c := range n.Children {
		if b, ok := c.(*ast.CompoundStmt); ok {
			return b
		}
	}

	return nil
}

// transpileFunctionDecl transpiles the function prototype.
//
// The function prototype may also have a body. If it does have a body the whole
// function will be transpiled into Go.
//
// If there is no function body we register the function interally (actually
// either way the function is registered internally) but we do not do anything
// becuase Go does not use or have any use for forward declarations of
// functions.
func transpileFunctionDecl(n *ast.FunctionDecl, p *program.Program) error {
	var body *goast.BlockStmt

	// This is set at the start of the function declaration so when the
	// ReturnStmt comes alone it will know what the current function is, and
	// therefore be able to lookup what the real return type should be. I'm sure
	// there is a much better way of doing this.
	p.Function = n
	defer func() {
		// Reset the function name when we go out of scope.
		p.Function = nil
	}()

	// Always register the new function. Only from this point onwards will
	// we be allowed to refer to the function.
	if program.GetFunctionDefinition(n.Name) == nil {
		program.AddFunctionDefinition(program.FunctionDefinition{
			Name:          n.Name,
			ReturnType:    getFunctionReturnType(n.Type),
			ArgumentTypes: getFunctionArgumentTypes(n),
			Substitution:  "",
		})
	}

	// If the function has a direct substitute in Go we do not want to
	// output the C definition of it.
	f := program.GetFunctionDefinition(n.Name)
	if f != nil && f.Substitution != "" {
		return nil
	}

	// Test if the function has a body. This is identified by a child node that
	// is a CompoundStmt (since it is not valid to have a function body without
	// curly brackets).
	functionBody := getFunctionBody(n)
	if functionBody != nil {
		var err error

		body, _, _, err = transpileToBlockStmt(functionBody, p)
		if err != nil {
			return err
		}
	}

	// These functions cause us trouble for whatever reason. Some of them might
	// even work now.
	//
	// TODO: Some functions are ignored because they are too much trouble
	// https://github.com/elliotchance/c2go/issues/78
	if n.Name == "__istype" ||
		n.Name == "__isctype" ||
		n.Name == "__wcwidth" ||
		n.Name == "__sputc" ||
		n.Name == "__inline_signbitf" ||
		n.Name == "__inline_signbitd" ||
		n.Name == "__inline_signbitl" {
		return nil
	}

	if functionBody != nil {
		// If verbose mode is on we print the name of the function as a comment
		// immediately to stdout. This will appear at the top of the program but
		// make it much easier to diagnose when the transpiler errors.
		if p.Verbose {
			fmt.Printf("// Function: %s(%s)\n", f.Name,
				strings.Join(f.ArgumentTypes, ", "))
		}

		fieldList, err := getFieldList(n, p)
		if err != nil {
			return err
		}

		t, err := types.ResolveType(p, f.ReturnType)
		p.AddMessage(ast.GenerateWarningMessage(err, n))

		returnTypes := []*goast.Field{
			&goast.Field{
				Type: goast.NewIdent(t),
			},
		}

		if p.Function != nil && p.Function.Name == "main" {
			// main() function does not have a return type.
			returnTypes = []*goast.Field{}

			// This collects statements that will be placed at the top of
			// (before any other code) in main().
			prependStmtsInMain := []goast.Stmt{}

			// We also need to append a setup function that will instantiate
			// some things that are expected to be available at runtime.
			prependStmtsInMain = append(
				prependStmtsInMain,
				util.NewExprStmt(util.NewCallExpr("__init")),
			)

			// In Go, the main() function does not take the system arguments.
			// Instead they are accessed through the os package. We create new
			// variables in the main() function (if needed), immediately after
			// the __init() for these variables.
			if len(fieldList.List) > 0 {
				p.AddImport("os")

				prependStmtsInMain = append(
					prependStmtsInMain,
					&goast.ExprStmt{
						X: util.NewBinaryExpr(
							fieldList.List[0].Names[0],
							token.DEFINE,
							goast.NewIdent("len(os.Args)"),
						),
					},
				)
			}

			if len(fieldList.List) > 1 {
				prependStmtsInMain = append(
					prependStmtsInMain,
					&goast.ExprStmt{
						X: util.NewBinaryExpr(
							fieldList.List[1].Names[0],
							token.DEFINE,
							goast.NewIdent("[][]byte{}"),
						),
					},
					&goast.RangeStmt{
						Key:   goast.NewIdent("_"),
						Value: goast.NewIdent("argvSingle"),
						Tok:   token.DEFINE,
						X:     goast.NewIdent("os.Args"),
						Body: &goast.BlockStmt{
							List: []goast.Stmt{
								&goast.ExprStmt{
									X: util.NewBinaryExpr(
										fieldList.List[1].Names[0],
										token.ASSIGN,
										util.NewCallExpr(
											"append",
											fieldList.List[1].Names[0],
											goast.NewIdent("[]byte(argvSingle)"),
										),
									),
								},
							},
						},
					},
				)
			}

			// Prepend statements for main().
			body.List = append(prependStmtsInMain, body.List...)

			// The main() function does not have arguments or a return value.
			fieldList = &goast.FieldList{}
		}

		p.File.Decls = append(p.File.Decls, &goast.FuncDecl{
			Name: goast.NewIdent(n.Name),
			Type: &goast.FuncType{
				Params: fieldList,
				Results: &goast.FieldList{
					List: returnTypes,
				},
			},
			Body: body,
		})
	}

	return nil
}

// getFieldList returns the parameters of a C function as a Go AST FieldList.
func getFieldList(f *ast.FunctionDecl, p *program.Program) (*goast.FieldList, error) {
	r := []*goast.Field{}
	for _, n := range f.Children {
		if v, ok := n.(*ast.ParmVarDecl); ok {
			t, err := types.ResolveType(p, v.Type)
			p.AddMessage(ast.GenerateWarningMessage(err, f))

			r = append(r, &goast.Field{
				Names: []*goast.Ident{goast.NewIdent(v.Name)},
				Type:  goast.NewIdent(t),
			})
		}
	}

	return &goast.FieldList{
		List: r,
	}, nil
}

func transpileReturnStmt(n *ast.ReturnStmt, p *program.Program) (
	goast.Stmt, []goast.Stmt, []goast.Stmt, error) {
	// There may not be a return value. Then we don't have to both ourselves
	// with all the rest of the logic below.
	if len(n.Children) == 0 {
		return &goast.ReturnStmt{}, nil, nil, nil
	}

	e, eType, preStmts, postStmts, err := transpileToExpr(n.Children[0], p)
	if err != nil {
		return nil, nil, nil, err
	}

	f := program.GetFunctionDefinition(p.Function.Name)

	t, err := types.CastExpr(p, e, eType, f.ReturnType)
	if p.AddMessage(ast.GenerateWarningMessage(err, n)) {
		t = util.NewStringLit("nil")
	}

	results := []goast.Expr{t}

	// main() function is not allowed to return a result. Use os.Exit if
	// non-zero.
	if p.Function != nil && p.Function.Name == "main" {
		litExpr, isLiteral := e.(*goast.BasicLit)
		if !isLiteral || (isLiteral && litExpr.Value != "0") {
			p.AddImport("os")
			return util.NewExprStmt(util.NewCallExpr("os.Exit", results...)),
				preStmts, postStmts, nil
		}
		results = []goast.Expr{}
	}

	return &goast.ReturnStmt{
		Results: results,
	}, preStmts, postStmts, nil
}

func getFunctionReturnType(f string) string {
	// The C type of the function will be the complete prototype, like:
	//
	//     __inline_isfinitef(float) int
	//
	// will have a C type of:
	//
	//     int (float)
	//
	// The arguments will handle themselves, we only care about the return type
	// ('int' in this case)
	returnType := strings.TrimSpace(strings.Split(f, "(")[0])

	if returnType == "" {
		panic(fmt.Sprintf("unable to extract the return type from: %s", f))
	}

	return returnType
}

// getFunctionArgumentTypes returns the C types of the arguments in a function.
func getFunctionArgumentTypes(f *ast.FunctionDecl) []string {
	r := []string{}
	for _, n := range f.Children {
		if v, ok := n.(*ast.ParmVarDecl); ok {
			r = append(r, v.Type)
		}
	}

	return r
}
