package transform

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/albrow/fo/ast"
	"github.com/albrow/fo/astclone"
	"github.com/albrow/fo/astutil"
	"github.com/albrow/fo/format"
	"github.com/albrow/fo/token"
	"github.com/albrow/fo/types"
)

// TODO(albrow): Implement transform.Package for operating on all files in a
// given package at once.

type transformer struct {
	fset *token.FileSet
	pkg  *types.Package
}

func File(fset *token.FileSet, f *ast.File, pkg *types.Package) (*ast.File, error) {
	trans := &transformer{
		fset: fset,
		pkg:  pkg,
	}
	withConcreteTypes := astutil.Apply(f, trans.generateConcreteTypes(), nil)
	result := astutil.Apply(withConcreteTypes, trans.replaceGenericIdents(), nil)
	resultFile, ok := result.(*ast.File)
	if !ok {
		panic(fmt.Errorf("astutil.Apply returned a non-file type: %T", result))
	}

	return resultFile, nil
}

// TODO: this could be optimized
func concreteTypeName(decl *types.GenericDecl, usg *types.GenericUsage) string {
	stringParams := []string{}
	for _, param := range decl.TypeParams() {
		typeString := usg.TypeMap()[param.String()].String()
		safeParam := strings.Replace(typeString, ".", "_", -1)
		stringParams = append(stringParams, safeParam)
	}
	return decl.Name() + "__" + strings.Join(stringParams, "__")
}

func concreteTypeExpr(e *ast.TypeParamExpr) *ast.Ident {
	switch x := e.X.(type) {
	case *ast.Ident:
		newIdent := astclone.Clone(x).(*ast.Ident)
		stringParams := []string{}
		for _, param := range e.Params {
			buf := bytes.Buffer{}
			format.Node(&buf, token.NewFileSet(), param)
			typeString := buf.String()
			safeParam := strings.Replace(typeString, ".", "_", -1)
			stringParams = append(stringParams, safeParam)
		}
		newIdent.Name = newIdent.Name + "__" + strings.Join(stringParams, "__")
		return newIdent
	default:
		panic(fmt.Errorf("type parameters for expr %s of type %T are not yet supported", e.X, e.X))
	}
}

func (trans *transformer) generateConcreteTypes() func(c *astutil.Cursor) bool {
	return func(c *astutil.Cursor) bool {
		switch n := c.Node().(type) {
		case *ast.GenDecl:
			var newTypeSpecs []ast.Spec
			used := false
			for _, spec := range n.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok {
					newTypeSpecs = append(newTypeSpecs, spec)
					used = true
					continue
				}
				if _, found := trans.pkg.Generics()[typeSpec.Name.Name]; !found {
					newTypeSpecs = append(newTypeSpecs, typeSpec)
					used = true
					continue
				}
				newTypeSpecs = append(newTypeSpecs, trans.generateTypeSpecs(typeSpec)...)
			}
			if len(newTypeSpecs) > 0 {
				sort.Slice(newTypeSpecs, func(i int, j int) bool {
					iSpec, ok := newTypeSpecs[i].(*ast.TypeSpec)
					if !ok {
						return false
					}
					jSpec, ok := newTypeSpecs[j].(*ast.TypeSpec)
					if !ok {
						return false
					}
					return iSpec.Name.Name < jSpec.Name.Name
				})
				newDecl := astclone.Clone(n).(*ast.GenDecl)
				newDecl.Specs = newTypeSpecs
				c.Replace(newDecl)
			} else if !used {
				c.Delete()
			}
		case *ast.FuncDecl:
			if n.TypeParams == nil {
				return false
			}
			newFuncs := trans.generateFuncDecls(c, n)
			sort.Slice(newFuncs, func(i int, j int) bool {
				return newFuncs[i].Name.Name < newFuncs[j].Name.Name
			})
			for _, newFunc := range newFuncs {
				c.InsertBefore(newFunc)
			}
			c.Delete()
		}
		return true
	}
}

func (trans *transformer) replaceGenericIdents() func(c *astutil.Cursor) bool {
	return func(c *astutil.Cursor) bool {
		switch n := c.Node().(type) {
		case *ast.TypeParamExpr:
			c.Replace(concreteTypeExpr(n))
		case *ast.IndexExpr:
			// Check if we are dealing with an ambiguous IndexExpr from the parser. In
			// some cases we need to disambiguate this by upgrading to a
			// TypeParamExpr.
			switch x := n.X.(type) {
			case *ast.Ident:
				if _, found := trans.pkg.Generics()[x.Name]; found {
					typeParamExpr := &ast.TypeParamExpr{
						X:      n.X,
						Lbrack: n.Lbrack,
						Params: []ast.Expr{n.Index},
						Rbrack: n.Rbrack,
					}
					c.Replace(concreteTypeExpr(typeParamExpr))
				}
			}
		}
		return true
	}
}

func (trans *transformer) generateTypeSpecs(typeSpec *ast.TypeSpec) []ast.Spec {
	name := typeSpec.Name.Name
	genericDecl, found := trans.pkg.Generics()[name]
	if !found {
		panic(fmt.Errorf("could not find generic type declaration for %s", name))
	}
	var results []ast.Spec
	// Check if we are dealing with an ambiguous ArrayType from the parser. In
	// some cases we need to disambiguate this by adding type parameters and
	// changing the type.
	if typeSpec.TypeParams == nil {
		if arrayType, ok := typeSpec.Type.(*ast.ArrayType); ok {
			if length, ok := arrayType.Len.(*ast.Ident); ok {
				typeSpec = astclone.Clone(typeSpec).(*ast.TypeSpec)
				typeSpec.TypeParams = &ast.TypeParamDecl{
					Lbrack: arrayType.Lbrack,
					Names:  []*ast.Ident{},
					Rbrack: arrayType.Lbrack + token.Pos(len(length.Name)),
				}
				typeSpec.Type = arrayType.Elt
			}
		}
	}
	for _, usg := range genericDecl.Usages() {
		newTypeSpec := astclone.Clone(typeSpec).(*ast.TypeSpec)
		newTypeSpec.Name = ast.NewIdent(concreteTypeName(genericDecl, usg))
		newTypeSpec.TypeParams = nil
		replaceIdentsInScope(newTypeSpec, usg.TypeMap())
		results = append(results, newTypeSpec)
	}
	return results
}

func (trans *transformer) generateFuncDecls(c *astutil.Cursor, funcDecl *ast.FuncDecl) []*ast.FuncDecl {
	name := funcDecl.Name.Name
	genericDecl, found := trans.pkg.Generics()[name]
	if !found {
		panic(fmt.Errorf("could not find generic type declaration for %s", name))
	}
	var results []*ast.FuncDecl
	for _, usg := range genericDecl.Usages() {
		newFuncDecl := astclone.Clone(funcDecl).(*ast.FuncDecl)
		newFuncDecl.Name = ast.NewIdent(concreteTypeName(genericDecl, usg))
		newFuncDecl.TypeParams = nil
		replaceIdentsInScope(newFuncDecl, usg.TypeMap())
		results = append(results, newFuncDecl)
	}
	return results
}

func replaceIdentsInScope(n ast.Node, typeMap map[string]types.Type) ast.Node {
	return astutil.Apply(n, nil, func(c *astutil.Cursor) bool {
		if ident, ok := c.Node().(*ast.Ident); ok {
			if typ, found := typeMap[ident.Name]; found {
				c.Replace(ast.NewIdent(typ.String()))
			}
		}
		return true
	})
}
