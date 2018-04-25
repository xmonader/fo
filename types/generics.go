package types

import (
	"fmt"
	"strings"

	"github.com/albrow/fo/ast"
)

// TODO(albrow): Create interfaces to capture genericDecl and genericUsage in
// such a way that both signatures and named types can be used directly as
// fields in these generic-specific data structures?

type GenericDecl struct {
	name       string
	typ        Type
	typeParams []*TypeParam
	usages     map[string]*GenericUsage
}

func (decl *GenericDecl) Name() string {
	return decl.name
}

func (decl *GenericDecl) TypeParams() []*TypeParam {
	return decl.typeParams
}

func (decl *GenericDecl) Usages() map[string]*GenericUsage {
	return decl.usages
}

type GenericUsage struct {
	typeMap map[string]Type
	typ     Type
}

func (usg *GenericUsage) TypeMap() map[string]Type {
	return usg.typeMap
}

func addGenericDecl(obj Object, typeParams []*TypeParam) {
	pkg := obj.Pkg()
	name := obj.Name()
	if pkg.generics == nil {
		pkg.generics = map[string]*GenericDecl{}
	}
	pkg.generics[name] = &GenericDecl{
		name:       name,
		typ:        obj.Type(),
		typeParams: typeParams,
	}
}

func addGenericUsage(genObj Object, typ Type, typeMap map[string]Type) {
	for _, typ := range typeMap {
		if _, ok := typ.(*TypeParam); ok {
			// If the type map includes a type parameter, it is not yet complete and
			// includes inherited type parameters. In this case, it is not a true
			// concrete usage, so we don't add it to the usage list.
			return
		}
	}
	pkg := genObj.Pkg()
	name := genObj.Name()
	if pkg.generics == nil {
		pkg.generics = map[string]*GenericDecl{}
	}
	genDecl, found := pkg.generics[name]
	if !found {
		// TODO(albrow): can we avoid panicking here?
		panic(fmt.Errorf("declaration not found for generic object %s", genObj.Id()))
	}
	if genDecl.usages == nil {
		genDecl.usages = map[string]*GenericUsage{}
	}
	genDecl.usages[usageKey(typeMap, genDecl.typeParams)] = &GenericUsage{
		typ:     typ,
		typeMap: typeMap,
	}
}

// usageKey returns a unique key for a particular usage which is based on its
// type parameters. Another usage with the same type parameters will have the
// same key.
func usageKey(typeMap map[string]Type, typeParams []*TypeParam) string {
	stringParams := []string{}
	for _, param := range typeParams {
		stringParams = append(stringParams, typeMap[param.String()].String())
	}
	return strings.Join(stringParams, ",")
}

// concreteType returns a new type with the concrete type parameters of e
// applied.
//
// TODO(albrow): Cache concrete types in some sort of special scope so
// we can avoid re-generating the concrete types on each usage.
// TODO(albrow): Use a named type/function decl here for better error
// messages.
func (check *Checker) concreteType(expr *ast.TypeParamExpr, genType Type) Type {
	switch genType := genType.(type) {
	case *Named:
		typeMap := check.createTypeMap(expr.Params, genType.typeParams)
		newNamed := replaceTypesInNamed(genType, typeMap)
		newNamed.typeParams = nil
		typeParams := make([]*TypeParam, len(genType.typeParams))
		copy(typeParams, genType.typeParams)
		newType := NewConcreteNamed(newNamed, typeParams, typeMap)
		newObj := *genType.obj
		newType.obj = &newObj
		newObj.typ = newType
		addGenericUsage(genType.obj, newType, typeMap)

		return newType
	case *Signature:
		// We need to lookup the object here. TODO(albrow): can we somehow make this
		// more efficient? Presumably the object was already looked up when we
		// determined the type of expr.X.
		var genObj Object
		switch x := expr.X.(type) {
		case *ast.Ident:
			_, obj := check.scope.LookupParent(x.Name, expr.Pos())
			if obj == nil {
				check.errorf(x.Pos(), "undeclared name: %s", x.Name)
			}
			genObj = obj
		default:
			check.errorf(expr.Pos(), "internal type %T in TypeParamExpr not yet supported", expr.X)
		}
		typeMap := check.createTypeMap(expr.Params, genType.typeParams)
		newSig := replaceTypesInSignature(genType, typeMap)
		newSig.typeParams = nil
		typeParams := make([]*TypeParam, len(genType.typeParams))
		copy(typeParams, genType.typeParams)
		newType := NewConcreteSignature(newSig, typeParams, typeMap)
		addGenericUsage(genObj, newType, typeMap)
		return newType
	}

	check.errorf(check.pos, "unexpected generic for %s: %T", expr.X, genType)
	return nil
}

// TODO(albrow): test case with wrong number of type parameters.
func (check *Checker) createTypeMap(params []ast.Expr, genericParams []*TypeParam) map[string]Type {
	if len(params) != len(genericParams) {
		check.errorf(check.pos, "wrong number of type parameters (expected %d but got %d)", len(genericParams), len(params))
		return nil
	}
	typeMap := map[string]Type{}
	for i, typ := range params {
		var x operand
		check.rawExpr(&x, typ, nil)
		if x.typ != nil {
			typeMap[genericParams[i].String()] = x.typ
		}
	}
	return typeMap
}

// replaceTypes recursively replaces any type parameters starting at root with
// the corresponding concrete type by looking up in typeMapping. typeMapping is
// a map of type parameter identifier to concrete type. replaceTypes works with
// compound types such as maps, slices, and arrays whenever the type parameter
// is part of the type. For example, root can be a []T and replaceTypes will
// correctly replace T with the corresponding concrete type (assuming it is
// included in typeMapping).
func replaceTypes(root Type, typeMapping map[string]Type) Type {
	switch t := root.(type) {
	case *TypeParam:
		if newType, found := typeMapping[t.String()]; found {
			// This part is important; if the concrete type is also a type parameter,
			// don't do the replacement. We assume that we're dealing with an
			// inherited type parameter and that the concrete form of the parent will
			// fill in this missing type parameter in the future. If it is not filled
			// in correctly in the future, we know how to generate an error.
			if _, newIsTypeParam := newType.(*TypeParam); newIsTypeParam {
				return t
			}
			return newType
		}
		// TODO(albrow): handle this error?
		panic(fmt.Errorf("undefined type parameter: %s", t))
	case *Pointer:
		newPointer := *t
		newPointer.base = replaceTypes(t.base, typeMapping)
		return &newPointer
	case *Slice:
		newSlice := *t
		newSlice.elem = replaceTypes(t.elem, typeMapping)
		return &newSlice
	case *Map:
		newMap := *t
		newMap.key = replaceTypes(t.key, typeMapping)
		newMap.elem = replaceTypes(t.elem, typeMapping)
		return &newMap
	case *Array:
		newArray := *t
		newArray.elem = replaceTypes(t.elem, typeMapping)
		return &newArray
	case *Chan:
		newChan := *t
		newChan.elem = replaceTypes(t.elem, typeMapping)
		return &newChan
	case *Struct:
		return replaceTypesInStruct(t, typeMapping)
	case *Signature:
		return replaceTypesInSignature(t, typeMapping)
	case *Named:
		return replaceTypesInNamed(t, typeMapping)
	case *ConcreteNamed:
		return replaceTypesInConcreteNamed(t, typeMapping)
	}
	return root
}

func replaceTypesInStruct(root *Struct, typeMapping map[string]Type) *Struct {
	var fields []*Var
	for _, field := range root.fields {
		newField := *field
		newField.typ = replaceTypes(field.Type(), typeMapping)
		fields = append(fields, &newField)
	}
	return NewStruct(fields, root.tags)
}

func replaceTypesInSignature(root *Signature, typeMapping map[string]Type) *Signature {
	var newRecv *Var
	if root.recv != nil {
		newRecv := *root.recv
		newRecv.typ = replaceTypes(root.recv.typ, typeMapping)
	}

	var newParams *Tuple
	if root.params != nil && len(root.params.vars) > 0 {
		newParams = &Tuple{}
		for _, param := range root.params.vars {
			newParam := *param
			newParam.typ = replaceTypes(param.typ, typeMapping)
			newParams.vars = append(newParams.vars, &newParam)
		}
	}

	var newResults *Tuple
	if root.results != nil && len(root.results.vars) > 0 {
		newResults = &Tuple{}
		for _, result := range root.results.vars {
			newResult := *result
			newResult.typ = replaceTypes(result.typ, typeMapping)
			newResults.vars = append(newResults.vars, &newResult)
		}
	}

	return NewSignature(newRecv, newParams, newResults, root.variadic, root.typeParams)
}

func replaceTypesInNamed(root *Named, typeMapping map[string]Type) *Named {
	newUnderlying := replaceTypes(root.underlying, typeMapping)
	newNamed := *root
	newNamed.underlying = newUnderlying
	newObj := *root.obj
	newObj.typ = &newNamed
	newNamed.obj = &newObj
	return &newNamed
}

// TODO(albrow): optimize by doing nothing in the case where the new typeMapping
// is equivalent to the old.
func replaceTypesInConcreteNamed(root *ConcreteNamed, typeMapping map[string]Type) *ConcreteNamed {
	newTypeMap := map[string]Type{}
	for key, given := range root.typeMap {
		if param, givenIsTypeParam := given.(*TypeParam); givenIsTypeParam {
			if inherited, found := typeMapping[param.String()]; found {
				if _, inheritedIsTypeParam := inherited.(*TypeParam); !inheritedIsTypeParam {
					newTypeMap[key] = inherited
					continue
				}
			}
		}
		newTypeMap[key] = given
	}
	newNamed := replaceTypesInNamed(root.Named, newTypeMap)
	newType := NewConcreteNamed(newNamed, root.typeParams, newTypeMap)
	newNamed.typeParams = nil
	newObj := *root.obj
	newObj.typ = newType
	newType.obj = &newObj
	addGenericUsage(root.obj, newType, newTypeMap)
	return newType
}
