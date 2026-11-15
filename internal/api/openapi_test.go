package api

import (
	"fmt"
	"maps"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/lajstrom/lajstrom/internal/auth"
)

// TestOpenAPIRoutes holds openapi.yaml and the routes of New to each other: every method and
// path the server serves is in the document, and the document has no other.
func TestOpenAPIRoutes(t *testing.T) {
	var doc struct {
		OpenAPI string                    `yaml:"openapi"`
		Paths   map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(openAPI, &doc); err != nil {
		t.Fatalf("openapi.yaml: %v", err)
	}
	if doc.OpenAPI != "3.1.0" {
		t.Errorf("openapi: %q, want 3.1.0", doc.OpenAPI)
	}
	var documented []string
	for path, item := range doc.Paths {
		for method := range item {
			if method != "parameters" {
				documented = append(documented, strings.ToUpper(method)+" "+path)
			}
		}
	}
	served := []string{"GET /api/v1/openapi.yaml"}
	for _, rt := range (&handler{}).routes() {
		served = append(served, rt.pattern)
	}
	slices.Sort(documented)
	slices.Sort(served)
	if !slices.Equal(documented, served) {
		t.Errorf("openapi.yaml describes\n\t%s\nthe server serves\n\t%s", strings.Join(documented, "\n\t"), strings.Join(served, "\n\t"))
	}
}

// TestOpenAPIKeywords holds the schemas of openapi.yaml to the keywords of a schema in OpenAPI
// 3.1, JSON Schema 2020-12's and OpenAPI's own (#1131): a schema has no other key, and no key of
// the document is without a value but default, const and example. A plain scalar in a flow
// mapping ends at its first comma, so {type: string, description: One, two.} has the
// description "One" and a key "two." without a value.
func TestOpenAPIKeywords(t *testing.T) {
	var root yaml.Node
	if err := yaml.Unmarshal(openAPI, &root); err != nil {
		t.Fatalf("openapi.yaml: %v", err)
	}
	if len(root.Content) != 1 {
		t.Fatalf("openapi.yaml: %d documents, want 1", len(root.Content))
	}
	keywordCheck{t}.object(root.Content[0], "")
}

// The keywords whose value is a schema, a map of schemas and a list of schemas, and every
// keyword of a schema, theirs included.
var (
	subschemaKeywords = wordSet(`not if then else items contains additionalProperties propertyNames
		unevaluatedItems unevaluatedProperties contentSchema`)
	schemaMapKeywords  = wordSet(`$defs properties patternProperties dependentSchemas`)
	schemaListKeywords = wordSet(`allOf anyOf oneOf prefixItems`)
	schemaKeywords     = wordSet(`$schema $id $ref $anchor $dynamicRef $dynamicAnchor $vocabulary
		$comment type const enum multipleOf maximum exclusiveMaximum minimum exclusiveMinimum
		maxLength minLength pattern maxItems minItems uniqueItems maxContains minContains
		maxProperties minProperties required dependentRequired title description default
		deprecated readOnly writeOnly examples format contentEncoding contentMediaType
		discriminator xml externalDocs example`, subschemaKeywords, schemaMapKeywords, schemaListKeywords)
)

// wordSet is the set of the words of s and of the sets of more.
func wordSet(s string, more ...map[string]bool) map[string]bool {
	set := map[string]bool{}
	for _, w := range strings.Fields(s) {
		set[w] = true
	}
	for _, m := range more {
		maps.Copy(set, m)
	}
	return set
}

// keywordCheck walks openapi.yaml for TestOpenAPIKeywords.
type keywordCheck struct{ t *testing.T }

// object walks the document outside its schemas, and hands schema the values of
// components.schemas and the schema of a parameter, a header or a media type.
func (c keywordCheck) object(n *yaml.Node, path string) {
	switch n.Kind {
	case yaml.SequenceNode:
		for i, e := range n.Content {
			c.object(e, fmt.Sprintf("%s[%d]", path, i))
		}
	case yaml.MappingNode:
		for i := 1; i < len(n.Content); i += 2 {
			k, v := n.Content[i-1], n.Content[i]
			p := path + "." + k.Value
			switch {
			case !c.valued(k, v, p):
			case k.Value == "schema" || path == ".components.schemas":
				c.schema(v, p)
			case k.Value != "example" && k.Value != "examples" && !strings.HasPrefix(k.Value, "x-"):
				c.object(v, p)
			}
		}
	case yaml.DocumentNode, yaml.ScalarNode, yaml.AliasNode:
	}
}

// schema checks the keys of the schema n, and the schemas under them.
func (c keywordCheck) schema(n *yaml.Node, path string) {
	if n.Kind != yaml.MappingNode {
		if n.Tag != "!!bool" {
			c.t.Errorf("openapi.yaml:%d: %s is not a schema", n.Line, path)
		}
		return
	}
	for i := 1; i < len(n.Content); i += 2 {
		k, v := n.Content[i-1], n.Content[i]
		p := path + "." + k.Value
		switch {
		case !schemaKeywords[k.Value] && !strings.HasPrefix(k.Value, "x-"):
			c.t.Errorf("openapi.yaml:%d: %q, in the schema at %s, is not a keyword", k.Line, k.Value, path)
		case c.valued(k, v, p):
			c.subschemas(k.Value, v, p)
		}
	}
}

// subschemas checks the schemas in v, the value of the keyword kw.
func (c keywordCheck) subschemas(kw string, v *yaml.Node, path string) {
	switch {
	case subschemaKeywords[kw]:
		c.schema(v, path)
	case schemaMapKeywords[kw] && v.Kind == yaml.MappingNode:
		for i := 1; i < len(v.Content); i += 2 {
			c.schema(v.Content[i], path+"."+v.Content[i-1].Value)
		}
	case schemaListKeywords[kw] && v.Kind == yaml.SequenceNode:
		for i, s := range v.Content {
			c.schema(s, fmt.Sprintf("%s[%d]", path, i))
		}
	case schemaMapKeywords[kw] || schemaListKeywords[kw]:
		c.t.Errorf("openapi.yaml:%d: %s is not a map or list of schemas", v.Line, path)
	}
}

// valued fails on a key without a value, but default, const and example, whose value may be null.
func (c keywordCheck) valued(k, v *yaml.Node, path string) bool {
	if v.Tag != "!!null" || k.Value == "default" || k.Value == "const" || k.Value == "example" {
		return true
	}
	c.t.Errorf("openapi.yaml:%d: %s has no value", k.Line, path)
	return false
}

// TestOpenAPIReindexRun holds the ReindexRun schema of openapi.yaml to the type the server
// answers with (#1128), the report and the items of its lists included: a struct is an object
// with a property per JSON key that requires the keys without omitempty, and each property has
// the type of its field; null is in the type of a pointer without omitempty, and may be in a
// slice's, since a nil slice is null.
func TestOpenAPIReindexRun(t *testing.T) {
	var doc struct {
		Components struct {
			Schemas map[string]map[string]any `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(openAPI, &doc); err != nil {
		t.Fatalf("openapi.yaml: %v", err)
	}
	c := &typeCheck{t: t, schemas: doc.Components.Schemas, checked: map[string]bool{}}
	c.same(doc.Components.Schemas["ReindexRun"], reflect.TypeFor[ReindexRun](), false, "ReindexRun")
}

// typeCheck walks a schema of openapi.yaml beside a Go type for TestOpenAPIReindexRun.
type typeCheck struct {
	t       *testing.T
	schemas map[string]map[string]any // components.schemas
	checked map[string]bool           // the schemas of components.schemas checked, once each
}

// same checks schema against typ, the type of a field with omitempty or without.
func (c *typeCheck) same(schema map[string]any, typ reflect.Type, omitempty bool, path string) {
	schema, path, ok := c.resolve(schema, path)
	if !ok {
		return
	}
	want, elem := jsonType(typ)
	if want == "" {
		c.t.Errorf("%s: no rule for the Go type %s", path, typ)
		return
	}
	c.typed(schema, typ, omitempty, want, path)
	if elem == reflect.TypeFor[time.Time]() && schema["format"] != "date-time" {
		c.t.Errorf("%s: format %v, want date-time for %s", path, schema["format"], elem)
	}
	switch items, _ := schema["items"].(map[string]any); {
	case want == "array" && items == nil:
		c.t.Errorf("%s: no items", path)
	case want == "array":
		c.same(items, elem.Elem(), false, path+"[]")
	case want == "object":
		c.fields(schema, elem, path)
	}
}

// typed checks the type of schema: want, with null for a pointer without omitempty, with or
// without it for a slice, and without it otherwise.
func (c *typeCheck) typed(schema map[string]any, typ reflect.Type, omitempty bool, want, path string) {
	null := "may not be"
	if typ.Kind() == reflect.Pointer && !omitempty {
		null = "must be"
	} else if typ.Kind() == reflect.Slice {
		null = "may be"
	}
	types := strs(schema["type"])
	others := slices.DeleteFunc(slices.Clone(types), func(s string) bool { return s == want || s == "null" })
	hasNull := slices.Contains(types, "null")
	if !slices.Contains(types, want) || len(others) > 0 || hasNull && null == "may not be" || !hasNull && null == "must be" {
		c.t.Errorf("%s: type %v, want %s for the Go type %s; null %s in it", path, types, want, typ, null)
	}
}

// resolve follows the $ref of schema into components.schemas; false for a schema that is not
// there, or that was checked already.
func (c *typeCheck) resolve(schema map[string]any, path string) (map[string]any, string, bool) {
	ref, ok := schema["$ref"].(string)
	if !ok {
		return schema, path, true
	}
	name, local := strings.CutPrefix(ref, "#/components/schemas/")
	if !local || c.schemas[name] == nil {
		c.t.Errorf("%s: $ref %s is not a schema of components.schemas", path, ref)
		return nil, "", false
	}
	if c.checked[name] {
		return nil, "", false
	}
	c.checked[name] = true
	return c.schemas[name], name, true
}

// fields checks the properties and required of schema against the JSON keys of the struct typ.
func (c *typeCheck) fields(schema map[string]any, typ reflect.Type, path string) {
	props, _ := schema["properties"].(map[string]any)
	var keys, required []string
	for i := range typ.NumField() {
		f := typ.Field(i)
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		switch {
		case !f.IsExported() || name == "-":
			continue
		case f.Anonymous:
			c.t.Errorf("%s: no rule for the embedded field %s of %s", path, f.Name, typ)
			continue
		case name == "":
			name = f.Name
		}
		omit := slices.Contains(strings.Split(opts, ","), "omitempty")
		keys = append(keys, name)
		if !omit {
			required = append(required, name)
		}
		if p, ok := props[name].(map[string]any); ok {
			c.same(p, f.Type, omit, path+"."+name)
		}
	}
	if missing, extra := apart(keys, slices.Collect(maps.Keys(props))); len(missing)+len(extra) > 0 {
		c.t.Errorf("%s: no property for the JSON keys %v of %s, and properties %v that are no key of it", path, missing, typ, extra)
	}
	if missing, extra := apart(required, strs(schema["required"])); len(missing)+len(extra) > 0 {
		c.t.Errorf("%s: not required %v, keys of %s without omitempty, and required %v that are not", path, missing, typ, extra)
	}
}

// jsonType is the JSON Schema type of the values of typ, or of its element for a pointer, and
// typ without the pointer; "" for a type it has no rule for.
func jsonType(typ reflect.Type) (string, reflect.Type) {
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch k := typ.Kind(); {
	case typ == reflect.TypeFor[time.Time](), k == reflect.String:
		return "string", typ
	case k == reflect.Struct:
		return "object", typ
	case k == reflect.Slice:
		return "array", typ
	case k == reflect.Bool:
		return "boolean", typ
	case k >= reflect.Int && k <= reflect.Int64:
		return "integer", typ
	}
	return "", typ
}

// strs is v, a string or a list of them as yaml decodes into any, as a list.
func strs(v any) []string {
	switch v := v.(type) {
	case string:
		return []string{v}
	case []any:
		var out []string
		for _, s := range v {
			out = append(out, fmt.Sprint(s))
		}
		return out
	}
	return nil
}

// apart returns the strings of want that got lacks and those of got that want lacks, sorted.
func apart(want, got []string) (missing, extra []string) {
	for _, s := range want {
		if !slices.Contains(got, s) {
			missing = append(missing, s)
		}
	}
	for _, s := range got {
		if !slices.Contains(want, s) {
			extra = append(extra, s)
		}
	}
	slices.Sort(missing)
	slices.Sort(extra)
	return missing, extra
}

// TestErrorCodes holds the codes of the envelope to each other (#579): the enum of the Error
// schema of openapi.yaml is the set of codes codeOf answers, and every status with a code in
// docs/api.md, "| 401 | `UNAUTHENTICATED` |" in the table of Errors or "`401 UNAUTHENTICATED`" in
// the text with a code of the envelope (not one of /v2, SIZE_INVALID), has the code of codeOf.
func TestErrorCodes(t *testing.T) {
	var doc struct {
		Components struct {
			Schemas struct {
				Error struct {
					Properties struct {
						Error struct {
							Properties struct {
								Code struct {
									Enum []string `yaml:"enum"`
								} `yaml:"code"`
							} `yaml:"properties"`
						} `yaml:"error"`
					} `yaml:"properties"`
				} `yaml:"Error"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(openAPI, &doc); err != nil {
		t.Fatalf("openapi.yaml: %v", err)
	}
	var answered []string
	for status := 400; status < 600; status++ {
		if c := codeOf(status); !slices.Contains(answered, c) {
			answered = append(answered, c)
		}
	}
	enum := doc.Components.Schemas.Error.Properties.Error.Properties.Code.Enum
	slices.Sort(enum)
	slices.Sort(answered)
	if !slices.Equal(enum, answered) {
		t.Errorf("the codes of openapi.yaml are %v, codeOf answers %v", enum, answered)
	}

	md, err := os.ReadFile("../../docs/api.md")
	if err != nil {
		t.Fatal(err)
	}
	documented := regexp.MustCompile("(?m)^\\| ([45][0-9][0-9]) \\| `([A-Z_]+)` \\||`([45][0-9][0-9]) ([A-Z_]+)`")
	matches := documented.FindAllStringSubmatch(string(md), -1)
	if len(matches) < 10 {
		t.Fatalf("docs/api.md: %d statuses with a code, want the table of Errors at least", len(matches))
	}
	for _, m := range matches {
		status, code := m[1]+m[3], m[2]+m[4]
		n, _ := strconv.Atoi(status)
		if want := codeOf(n); code != want && (m[1] != "" || slices.Contains(answered, code)) {
			t.Errorf("docs/api.md: %s %s, codeOf(%s) = %s", status, code, status, want)
		}
	}
}

// TestOpenAPIServed checks that anyone gets the document at /api/v1/openapi.yaml.
func TestOpenAPIServed(t *testing.T) {
	f := newFixture(t, Options{Auth: fakeAuth{}})
	rr := f.do(http.MethodGet, "/api/v1/openapi.yaml", "", auth.Anonymous)
	wantStatus(t, rr, http.StatusOK)
	if ct := rr.Header().Get("Content-Type"); ct != "application/yaml" || !strings.HasPrefix(rr.Body.String(), "openapi: 3.1.0") {
		t.Errorf("Content-Type %q, body %.20q…; want the document as application/yaml", ct, rr.Body.String())
	}
}
