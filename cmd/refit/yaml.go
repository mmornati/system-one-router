package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// WriteSkills returns src (a config.yaml) with the fitted values of changes applied. It parses src into a
// yaml.v3 node tree only to locate the values, then edits the original bytes at those positions, so
// comments, blank lines, key order and flow/block style all survive untouched. A topic the model had no
// explicit skill for is added to its skills mapping.
func WriteSkills(src []byte, changes []FitRow) ([]byte, error) {
	text := string(src)
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(text), &root); err != nil {
		return nil, err
	}
	if len(root.Content) == 0 {
		return nil, fmt.Errorf("empty config")
	}
	models := mapValue(root.Content[0], "models")
	if models == nil || models.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("config has no models list")
	}

	lines := strings.SplitAfter(text, "\n")
	offset := func(line, col int) int { // yaml.v3 positions: 1-based line, 1-based rune column
		off := 0
		for _, l := range lines[:line-1] {
			off += len(l)
		}
		l := lines[line-1]
		i := 0
		for c := 1; c < col && i < len(l); c++ {
			_, n := utf8.DecodeRuneInString(l[i:])
			i += n
		}
		return off + i
	}
	type edit struct {
		off, del int
		text     string
		seq      int // insertion order, to keep same-offset inserts in order
	}
	var edits []edit
	add := func(off, del int, s string) { edits = append(edits, edit{off, del, s, len(edits)}) }
	set := func(n *yaml.Node, v float64) error {
		off := offset(n.Line, n.Column)
		if n.Kind != yaml.ScalarNode || !strings.HasPrefix(text[off:], n.Value) {
			return fmt.Errorf("line %d: cannot edit %q in place", n.Line, n.Value)
		}
		add(off, len(n.Value), fmtSkill(v))
		return nil
	}

	byModel := map[string][]FitRow{}
	for _, c := range changes {
		byModel[c.Model] = append(byModel[c.Model], c)
	}
	for _, m := range models.Content {
		idKey, id := mapEntry(m, "id")
		if id == nil || byModel[id.Value] == nil {
			continue
		}
		rows := byModel[id.Value]
		delete(byModel, id.Value)
		afterID := offset(idKey.Line, 1) + len(lines[idKey.Line-1]) // start of the line after `id:`
		indent := strings.Repeat(" ", idKey.Column-1)

		skills := mapValue(m, "skills")
		if skills != nil && skills.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("model %s: skills is not a mapping", id.Value)
		}
		var missing []string // "topic: value" for topics without an explicit skill
		for _, c := range rows {
			if c.Topic == "" {
				if v := mapValue(m, "default_skill"); v != nil {
					if err := set(v, c.Fitted); err != nil {
						return nil, err
					}
				} else {
					add(afterID, 0, indent+"default_skill: "+fmtSkill(c.Fitted)+"\n")
				}
				continue
			}
			if v := mapValue(skills, c.Topic); v != nil {
				if err := set(v, c.Fitted); err != nil {
					return nil, err
				}
				continue
			}
			missing = append(missing, c.Topic+": "+fmtSkill(c.Fitted))
		}
		if len(missing) == 0 {
			continue
		}
		sort.Strings(missing)
		switch {
		case skills == nil:
			add(afterID, 0, indent+"skills: {"+strings.Join(missing, ", ")+"}\n")
		case skills.Style&yaml.FlowStyle != 0: // {a: 1, b: 2} → {a: 1, b: 2, c: 3}
			from, sep := offset(skills.Line, skills.Column), ""
			if n := len(skills.Content); n > 0 {
				last := skills.Content[n-1]
				from, sep = offset(last.Line, last.Column)+len(last.Value), ", "
			}
			end := strings.IndexByte(text[from:], '}')
			if end < 0 {
				return nil, fmt.Errorf("model %s: unterminated skills mapping", id.Value)
			}
			add(from+end, 0, sep+strings.Join(missing, ", "))
		default: // block mapping: one line per topic after its last entry
			first, last := skills.Content[0], skills.Content[len(skills.Content)-1]
			pad := strings.Repeat(" ", first.Column-1)
			add(offset(last.Line, 1)+len(lines[last.Line-1]), 0, pad+strings.Join(missing, "\n"+pad)+"\n")
		}
	}
	for id := range byModel {
		return nil, fmt.Errorf("model %s not found in config", id)
	}

	// Apply from the end so earlier offsets stay valid; at equal offsets, later inserts go first.
	sort.Slice(edits, func(i, j int) bool {
		if edits[i].off != edits[j].off {
			return edits[i].off > edits[j].off
		}
		return edits[i].seq > edits[j].seq
	})
	for _, e := range edits {
		text = text[:e.off] + e.text + text[e.off+e.del:]
	}
	return []byte(text), nil
}

func fmtSkill(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

// mapEntry returns the key and value nodes for key in mapping n (nil if absent).
func mapEntry(n *yaml.Node, key string) (*yaml.Node, *yaml.Node) {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil, nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i], n.Content[i+1]
		}
	}
	return nil, nil
}

func mapValue(n *yaml.Node, key string) *yaml.Node {
	_, v := mapEntry(n, key)
	return v
}
