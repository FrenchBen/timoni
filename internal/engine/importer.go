package engine

import (
	"fmt"
	"path"
	"strings"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/ast"
	"cuelang.org/go/cue/ast/astutil"
	"cuelang.org/go/cue/format"
	"cuelang.org/go/cue/token"
	"cuelang.org/go/encoding/openapi"
	"cuelang.org/go/encoding/yaml"
	"github.com/getkin/kin-openapi/openapi3"
)

// Importer generates CUE definitions from Kubernetes CRDs using the OpenAPI v3 spec.
type Importer struct {
	ctx    *cue.Context
	header string
}

// NewImporter creates an Importer for the given CUE context.
func NewImporter(ctx *cue.Context, header string) *Importer {
	return &Importer{
		ctx:    ctx,
		header: header,
	}
}

// Generate takes a multi-doc YAML containing Kubernetes CRDs and returns the CUE definitions
// generated from the OpenAPI spec. The resulting key value pairs, contain a unique identifier
// in the format `<group>/<kind>/<version>` and the contents of the CUE definition.
func (imp *Importer) Generate(crdData []byte) (map[string][]byte, error) {
	result := make(map[string][]byte)

	crds, err := imp.fromYAML(crdData)
	if err != nil {
		return result, err
	}

	for _, crd := range crds {
		for _, crdVersion := range crd.Schemas {
			def, err := format.Node(crdVersion.Schema.Syntax(cue.All(), cue.Docs(true)))
			if err != nil {
				return result, err
			}
			name := path.Join(crd.Props.Spec.Group, crd.Props.Spec.Names.Singular, crdVersion.Version)
			result[name] = []byte(fmt.Sprintf("%s\n\npackage %s\n\n%s", imp.header, crdVersion.Version, string(def)))
		}
	}

	return result, nil
}

// fromYAML converts a byte slice containing one or more YAML-encoded
// CustomResourceDefinitions into a slice of [IntermediateCRD].
//
// This function preserves the ordering of schemas declared in the input YAML in
// the resulting [IntermediateCRD.Schemas] field.
func (imp *Importer) fromYAML(b []byte) ([]*IntermediateCRD, error) {

	// The filename provided here is only used in error messages
	yf, err := yaml.Extract("crd.yaml", b)
	if err != nil {
		return nil, fmt.Errorf("input is not valid yaml: %w", err)
	}
	crdv := imp.ctx.BuildFile(yf)

	var all []cue.Value
	switch crdv.IncompleteKind() {
	case cue.StructKind:
		all = append(all, crdv)
	case cue.ListKind:
		iter, _ := crdv.List()
		for iter.Next() {
			all = append(all, iter.Value())
		}
	default:
		return nil, fmt.Errorf("input does not appear to be one or multiple CRDs: %s", crdv)
	}

	ret := make([]*IntermediateCRD, 0, len(all))
	for _, crd := range all {
		cc, err := convertCRD(crd)
		if err != nil {
			return nil, err
		}
		ret = append(ret, cc)
	}

	return ret, nil
}

// IntermediateCRD is an intermediate representation of CRD YAML. It contains the original CRD YAML input,
// a subset of useful naming-related fields, and an extracted list of the version schemas in the CRD,
// having been converted from OpenAPI to CUE.
type IntermediateCRD struct {
	// The original unmodified CRD YAML, after conversion to a cue.Value.
	Original cue.Value
	Props    struct {
		Spec struct {
			Group string `json:"group"`
			Names struct {
				Kind     string `json:"kind"`
				ListKind string `json:"listKind"`
				Plural   string `json:"plural"`
				Singular string `json:"singular"`
			} `json:"names"`
			Scope string `json:"scope"`
		} `json:"spec"`
	}

	// All the schemas in the original CRD, converted to CUE representation.
	Schemas []VersionedSchema
}

// VersionedSchema is an intermediate form of a single versioned schema from a CRD
// (an element in `spec.versions`), converted to CUE.
type VersionedSchema struct {
	// The contents of the `spec.versions[].name`
	Version string
	// The contents of `spec.versions[].schema.openAPIV3Schema`, after conversion of the OpenAPI
	// schema to native CUE constraints.
	Schema cue.Value
}

func convertCRD(crd cue.Value) (*IntermediateCRD, error) {
	cc := &IntermediateCRD{
		Schemas: make([]VersionedSchema, 0),
	}

	err := crd.Decode(&cc.Props)
	if err != nil {
		return nil, fmt.Errorf("error decoding crd props into Go struct: %w", err)
	}
	// shorthand
	kname := cc.Props.Spec.Names.Kind

	vlist := crd.LookupPath(cue.ParsePath("spec.versions"))
	if !vlist.Exists() {
		return nil, fmt.Errorf("crd versions list is absent")
	}
	iter, err := vlist.List()
	if err != nil {
		return nil, fmt.Errorf("crd versions field is not a list")
	}

	ctx := crd.Context()
	shell := ctx.CompileString(fmt.Sprintf(`
		openapi: "3.0.0",
		info: {
			title: "dummy",
			version: "1.0.0",
		}
		components: schemas: %s: _
	`, kname))
	schpath := cue.ParsePath("components.schemas." + kname)
	defpath := cue.MakePath(cue.Def(kname))

	// The CUE stdlib openapi encoder expects a whole openapi document, and then
	// operates on schema elements defined within #/components/schema. Each
	// versions[].schema.openAPIV3Schema within a CRD is ~equivalent to a single
	// element under #/components/schema, as k8s does not allow CRD schemas to
	// contain any kind of external references.
	//
	// So, for each schema.openAPIV3Schema, we wrap it in an openapi document
	// structure, convert it to CUE, then appends it into the [IntermediateCRD.Schemas] slice.
	var i int
	for iter.Next() {
		val := iter.Value()
		ver, err := val.LookupPath(cue.ParsePath("name")).String()
		if err != nil {
			return nil, fmt.Errorf("unreachable? error getting version field for versions element at index %d: %w", i, err)
		}
		i++

		doc := shell.FillPath(schpath, val.LookupPath(cue.ParsePath("schema.openAPIV3Schema")))
		of, err := openapi.Extract(doc, &openapi.Config{})
		if err != nil {
			return nil, fmt.Errorf("could not convert schema for version %s to CUE: %w", ver, err)
		}

		// first, extract and get the schema handle itself
		extracted := ctx.BuildFile(of)
		// then unify with our desired base constraints
		nsConstraint := "!"
		if cc.Props.Spec.Scope != "Namespaced" {
			nsConstraint = "?"
		}
		sch := extracted.FillPath(defpath, ctx.CompileString(fmt.Sprintf(`
					import "strings"

					apiVersion: "%s/%s"
					kind: "%s"
		
					metadata!: {
						name!:        string & strings.MaxRunes(253) & strings.MinRunes(1)
						namespace%s:  string & strings.MaxRunes(63) & strings.MinRunes(1)
						labels?:      [string]: string
						annotations?: [string]: string
					}
				`, cc.Props.Spec.Group, ver, kname, nsConstraint)))

		// now, go back to an AST because it's easier to manipulate references there
		var schast *ast.File
		switch x := sch.Syntax(cue.All(), cue.Docs(true)).(type) {
		case *ast.File:
			schast = x
		case *ast.StructLit:
			schast, _ = astutil.ToFile(x)
		default:
			panic("unreachable")
		}

		// construct a map of all the paths that have x-kubernetes-embedded-resource: true defined
		yodoc, err := yaml.Encode(doc)
		if err != nil {
			return nil, fmt.Errorf("error encoding intermediate openapi doc to yaml bytes: %w", err)
		}
		odoc, err := openapi3.NewLoader().LoadFromData(yodoc)
		if err != nil {
			return nil, fmt.Errorf("could not load openapi3 document for version %s: %w", ver, err)
		}

		preserve := make(map[string]bool)
		var rootosch *openapi3.Schema
		if rref, has := odoc.Components.Schemas[kname]; !has {
			return nil, fmt.Errorf("could not find root schema for version %s at expected path components.schemas.%s", ver, kname)
		} else {
			rootosch = rref.Value
		}

		var walkfn func(path []cue.Selector, sch *openapi3.Schema) error
		walkfn = func(path []cue.Selector, sch *openapi3.Schema) error {
			_, has := sch.Extensions["x-kubernetes-preserve-unknown-fields"]
			preserve[cue.MakePath(path...).String()] = has
			for name, prop := range sch.Properties {
				if err := walkfn(append(path, cue.Str(name)), prop.Value); err != nil {
					return err
				}
			}

			return nil
		}

		// Have to prepend with the defpath where the CUE CRD representation
		// lives because the astutil walker to remove ellipses operates over the
		// whole file, and therefore will be looking for full paths, extending
		// all the way to the file root
		err = walkfn(defpath.Selectors(), rootosch)

		// First pass of astutil.Apply to remove ellipses for fields not marked with x-kubernetes-embedded-resource: true
		// Note that this implementation is only correct for CUE inputs that do not contain references.
		// It is safe to use in this context because CRDs already have that invariant.
		var stack []ast.Node
		var pathstack []cue.Selector
		astutil.Apply(schast, func(c astutil.Cursor) bool {
			// Skip the root
			if c.Parent() == nil {
				return true
			}

			switch x := c.Node().(type) {
			case *ast.StructLit:
				psel, pc := parentPath(c)
				// Finding the parent-of-parent in this way is questionable.
				// pathTo will hop up the tree a potentially large number of
				// levels until it finds an *ast.Field or *ast.ListLit...but
				// who knows what's between here and there?
				_, ppc := parentPath(pc)
				var i int
				if ppc != nil {
					for i = len(stack); i > 0 && stack[i-1] != ppc.Node(); i-- {
					}
				}
				stack = append(stack[:i], pc.Node())
				pathstack = append(pathstack[:i], psel)
				if !preserve[cue.MakePath(pathstack...).String()] {
					newlist := make([]ast.Decl, 0, len(x.Elts))
					for _, elt := range x.Elts {
						if _, is := elt.(*ast.Ellipsis); !is {
							newlist = append(newlist, elt)
						}
					}
					x.Elts = newlist
				}
			}
			return true
		}, nil)

		// walk over the AST and replace the spec and status fields with references to standalone defs
		var specf, statusf *ast.Field
		astutil.Apply(schast, func(cursor astutil.Cursor) bool {
			switch x := cursor.Node().(type) {
			case *ast.Field:
				if str, _, err := ast.LabelName(x.Label); err == nil {
					switch str {
					// Grab pointers to the spec and status fields, and replace with ref
					case "spec":
						specf = new(ast.Field)
						*specf = *x
						specref := &ast.Field{
							Label: ast.NewIdent("spec"),
							Value: ast.NewIdent("#" + kname + "Spec"),
						}
						specref.Constraint = token.NOT
						astutil.CopyComments(specref, x)
						cursor.Replace(specref)
						return false
					case "status":
						//TODO: decide if status should be included
						//statusf = new(ast.Field)
						//*statusf = *x
						cursor.Delete()
						return false
					case "metadata":
						// Avoid walking other known subtrees
						return false
					case "info":
						cursor.Delete()
					}
				}
			}
			return true
		}, nil)

		if specf != nil {
			specd := &ast.Field{
				Label: ast.NewIdent("#" + kname + "Spec"),
				Value: specf.Value,
			}
			astutil.CopyComments(specd, specf)
			schast.Decls = append(schast.Decls, specd)
		}

		if statusf != nil {
			statusd := &ast.Field{
				Label: ast.NewIdent("#" + kname + "Status"),
				Value: statusf.Value,
			}
			astutil.CopyComments(statusd, statusf)
			schast.Decls = append(schast.Decls, statusd)
		}

		// Then build back to a cue.Value again for the return
		cc.Schemas = append(cc.Schemas, VersionedSchema{
			Version: ver,
			Schema:  ctx.BuildFile(schast),
		})
	}
	return cc, nil
}

// parentPath walks up the AST via Cursor.Parent() to find the parent AST node
// that is expected to be the anchor of a path element.
//
// Returns the cue.Selector that should navigate to the provided cursor's
// corresponding cue.Value, and the cursor of that parent element.
//
// Returns nil, nil if no such parent node can be found.
//
// Node types considered candidates for path anchors:
//   - *ast.ListLit (index is the path)
//   - *ast.Field (label is the path)
//
// If the there exceptions for the above two items, or the list should properly
// have more items, this func will be buggy
func parentPath(c astutil.Cursor) (cue.Selector, astutil.Cursor) {
	p, prior := c.Parent(), c
	for p != nil {
		switch x := p.Node().(type) {
		case *ast.Field:
			lab, _, _ := ast.LabelName(x.Label)
			if strings.HasPrefix(lab, "#") {
				return cue.Def(lab), p
			}
			return cue.Str(lab), p
		case *ast.ListLit:
			for i, v := range x.Elts {
				if prior.Node() == v {
					return cue.Index(i), p
				}
			}
		}
		prior = p
		p = p.Parent()
	}

	return cue.Selector{}, nil
}
