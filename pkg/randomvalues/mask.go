// Package randomvalues replaces the values in a manifest that charts fill with
// a different value on every render, such as generated certificates, with a
// placeholder, so that re-rendering an unchanged chart gives the same
// manifest.
package randomvalues

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/quietpelican42/yamlsplice"
	"gopkg.in/yaml.v3"
)

// Mask compares two renders of the same manifest, and replaces the values that
// differ between them with placeholder in the first one, leaving the rest of it
// byte for byte as rendered. It returns the masked manifest and the fields it
// replaced, as "<apiVersion> <kind> <namespace>/<name> <path>".
func Mask(first, second []byte, placeholder string) (masked []byte, fields []string, err error) {
	firstDocs, err := documents(first)
	if err != nil {
		return nil, nil, err
	}
	secondDocs, err := documents(second)
	if err != nil {
		return nil, nil, err
	}
	if len(firstDocs) != len(secondDocs) {
		return nil, nil, fmt.Errorf("renders have %d and %d documents", len(firstDocs), len(secondDocs))
	}

	editor, err := yamlsplice.Edit(first)
	if err != nil {
		return nil, nil, fmt.Errorf("error reading manifest: %w", err)
	}
	if editor.Len() != len(firstDocs) {
		return nil, nil, fmt.Errorf("manifest has %d documents, but %d can be edited", len(firstDocs), editor.Len())
	}
	for i, doc := range firstDocs {
		id, otherID := identity(doc), identity(secondDocs[i])
		if id != otherID {
			return nil, nil, fmt.Errorf("renders have %s and %s in the same place", id, otherID)
		}
		var differing []field
		if err := compare(doc, secondDocs[i], "", "", &differing); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", id, err)
		}
		if len(differing) == 0 {
			continue
		}
		editable, err := editor.Document(i)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", id, err)
		}
		for _, f := range differing {
			if err := editable.Set(f.pointer, placeholder); err != nil {
				return nil, nil, fmt.Errorf("%s: cannot replace %s: %w", id, f.path, err)
			}
			fields = append(fields, id+" "+f.path)
		}
	}
	return editor.Bytes(), fields, nil
}

func documents(manifest []byte) ([]*yaml.Node, error) {
	var docs []*yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(manifest))
	for {
		var doc yaml.Node
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				return docs, nil
			}
			return nil, fmt.Errorf("error reading manifest: %w", err)
		}
		docs = append(docs, &doc)
	}
}

// identity names a document by its API version, kind, namespace, and name.
func identity(doc *yaml.Node) string {
	name := value(doc, "metadata", "name")
	if namespace := value(doc, "metadata", "namespace"); namespace != "" {
		name = namespace + "/" + name
	}
	return value(doc, "apiVersion") + " " + value(doc, "kind") + " " + name
}

// value returns the scalar at the given keys in a document, or "" if there is
// none.
func value(doc *yaml.Node, keys ...string) string {
	if len(doc.Content) == 0 {
		return ""
	}
	n := doc.Content[0]
	for _, key := range keys {
		var next *yaml.Node
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				if n.Content[i].Value == key {
					next = n.Content[i+1]
				}
			}
		}
		if next == nil {
			return ""
		}
		n = next
	}
	if n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}

// A field is a value in the first render that differs in the second, at a
// path such as .webhooks[0].clientConfig.caBundle, which is the JSON pointer
// /webhooks/0/clientConfig/caBundle.
type field struct {
	path    string
	pointer string
}

// compare collects the values that differ between two nodes at the same path
// in two renders. The nodes must have the same structure: the same keys in the
// same order, and lists of the same length.
func compare(a, b *yaml.Node, path, pointer string, differing *[]field) error {
	differsInStructure := func() error { return fmt.Errorf("%s differs in structure", cmp.Or(path, ".")) }
	if a.Kind != b.Kind || len(a.Content) != len(b.Content) {
		return differsInStructure()
	}
	switch a.Kind {
	case yaml.DocumentNode:
		for i := range a.Content {
			if err := compare(a.Content[i], b.Content[i], path, pointer, differing); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(a.Content); i += 2 {
			key := a.Content[i].Value
			if key != b.Content[i].Value {
				return differsInStructure()
			}
			if err := compare(a.Content[i+1], b.Content[i+1], path+pathSegment(key), pointer+"/"+pointerSegment(key), differing); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for i := range a.Content {
			if err := compare(a.Content[i], b.Content[i], fmt.Sprintf("%s[%d]", path, i), fmt.Sprintf("%s/%d", pointer, i), differing); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		if a.Value != b.Value {
			*differing = append(*differing, field{path: cmp.Or(path, "."), pointer: pointer})
		}
	case yaml.AliasNode:
		if a.Value != b.Value {
			return differsInStructure()
		}
	}
	return nil
}

var identifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// pathSegment is the part of a path that selects the value of a mapping key.
func pathSegment(key string) string {
	if identifier.MatchString(key) {
		return "." + key
	}
	return "[" + strconv.Quote(key) + "]"
}

// pointerSegment is the part of a JSON pointer that selects the value of a
// mapping key.
func pointerSegment(key string) string {
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(key)
}
