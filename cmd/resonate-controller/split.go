package main

import (
	"bytes"
	"fmt"
	"math"
	"strconv"

	"gopkg.in/yaml.v3"
)

// splitScenario rewrites a scenario's load: block into n per-worker copies whose
// loads add up to a fleet-wide total, so "500 req/s" means 500 for the whole fleet
// however many workers there are.
//
// totalRate is the fleet's peak request rate: the scenario's load.rate, or the
// highest stage rate. Every rate and user count is scaled by totalRate / peak, so a
// staged ramp keeps its shape and only changes height, then split across the n
// workers. Rates divide exactly. User counts (workers, max_workers) are whole
// numbers: the remainder goes to the first workers, and a stage with fewer users
// than workers gives each worker one (the fleet then runs slightly more than asked,
// and a warning says so). requests is a fleet-wide cap, so it is divided but not
// scaled. iterations is per virtual user and is left alone. Durations never
// change, so every worker's stages stay aligned in time. Only the load: block is
// edited, so comments and key order elsewhere survive.
func splitScenario(body []byte, totalRate float64, n int) (bodies [][]byte, warnings []string, err error) {
	if n < 1 {
		return nil, nil, fmt.Errorf("no workers to split across")
	}
	if !(totalRate > 0) || math.IsInf(totalRate, 0) {
		return nil, nil, fmt.Errorf("total rate must be a positive number")
	}
	var probe yaml.Node
	if err := yaml.Unmarshal(body, &probe); err != nil {
		return nil, nil, fmt.Errorf("scenario is not valid YAML: %w", err)
	}
	load, err := loadNode(&probe)
	if err != nil {
		return nil, nil, err
	}
	peak, err := peakRate(load)
	if err != nil {
		return nil, nil, err
	}
	factor := totalRate / peak

	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		var doc yaml.Node
		if err := yaml.Unmarshal(body, &doc); err != nil {
			return nil, nil, err
		}
		l, _ := loadNode(&doc)
		ws, err := scaleLoad(l, factor, i, n)
		if err != nil {
			return nil, nil, err
		}
		for _, w := range ws {
			if !seen[w] {
				seen[w] = true
				warnings = append(warnings, w)
			}
		}
		var buf bytes.Buffer
		enc := yaml.NewEncoder(&buf)
		enc.SetIndent(2)
		if err := enc.Encode(&doc); err != nil {
			return nil, nil, err
		}
		enc.Close()
		var check map[string]any
		if err := yaml.Unmarshal(buf.Bytes(), &check); err != nil {
			return nil, nil, fmt.Errorf("rewritten scenario is not valid YAML: %w", err)
		}
		bodies = append(bodies, buf.Bytes())
	}
	return bodies, warnings, nil
}

// loadNode finds the top-level load: mapping of a parsed scenario.
func loadNode(doc *yaml.Node) (*yaml.Node, error) {
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("scenario has no top-level mapping")
	}
	if v := mapValue(doc.Content[0], "load"); v != nil && v.Kind == yaml.MappingNode {
		return v, nil
	}
	return nil, fmt.Errorf("scenario has no load: block to split")
}

// mapValue returns the value node for key in a mapping, or nil.
func mapValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func number(n *yaml.Node, what string) (float64, error) {
	if n.Kind != yaml.ScalarNode {
		return 0, fmt.Errorf("%s is not a number", what)
	}
	v, err := strconv.ParseFloat(n.Value, 64)
	if err != nil || v < 0 || math.IsInf(v, 0) || math.IsNaN(v) {
		return 0, fmt.Errorf("%s: %q is not a non-negative number", what, n.Value)
	}
	return v, nil
}

// peakRate is the highest rate the load reaches. A load with no rate has no
// peak to scale against.
func peakRate(load *yaml.Node) (float64, error) {
	var peak float64
	if r := mapValue(load, "rate"); r != nil {
		v, err := number(r, "load.rate")
		if err != nil {
			return 0, err
		}
		peak = v
	}
	if st := mapValue(load, "stages"); st != nil && st.Kind == yaml.SequenceNode {
		for i, s := range st.Content {
			if s.Kind != yaml.MappingNode {
				continue
			}
			if r := mapValue(s, "rate"); r != nil {
				v, err := number(r, fmt.Sprintf("load.stages[%d].rate", i))
				if err != nil {
					return 0, err
				}
				peak = math.Max(peak, v)
			}
		}
	}
	if peak <= 0 {
		return 0, fmt.Errorf("the scenario's load has no rate to scale (set load.rate, or a stage rate above 0), so a total rate can't be applied")
	}
	return peak, nil
}

// scaleLoad edits one worker's (index i of n) copy of the load: block in place.
func scaleLoad(load *yaml.Node, factor float64, i, n int) (warnings []string, err error) {
	warn := func(w string) { warnings = append(warnings, w) }
	if err := scaleFields(load, "load", factor, i, n, warn); err != nil {
		return nil, err
	}
	if st := mapValue(load, "stages"); st != nil && st.Kind == yaml.SequenceNode {
		for j, s := range st.Content {
			if s.Kind == yaml.MappingNode {
				if err := scaleFields(s, fmt.Sprintf("load.stages[%d]", j), factor, i, n, warn); err != nil {
					return nil, err
				}
			}
		}
	}
	return warnings, nil
}

// scaleFields rewrites the rate, user counts and request cap of one mapping (the
// load block itself, or one stage).
func scaleFields(m *yaml.Node, path string, factor float64, i, n int, warn func(string)) error {
	for _, key := range []string{"rate", "workers", "max_workers", "requests"} {
		node := mapValue(m, key)
		if node == nil {
			continue
		}
		v, err := number(node, path+"."+key)
		if err != nil {
			return err
		}
		if v == 0 {
			continue // 0 is "ramp to zero" (rate, workers in a stage) or "no cap": keep it
		}
		switch key {
		case "rate":
			node.Value = formatRate(v * factor / float64(n))
		case "requests":
			node.Value = strconv.Itoa(share(int(math.Round(v)), i, n, path+"."+key, warn))
		default: // workers, max_workers
			node.Value = strconv.Itoa(share(int(math.Round(v*factor)), i, n, path+"."+key, warn))
		}
		node.Tag = "" // let the encoder pick int or float from the new value
	}
	return nil
}

// share is worker i's part of a whole-number total across n workers: the remainder
// goes to the first workers. A total smaller than n gives every worker 1 rather
// than 0 (which would idle it or, for requests, mean no cap), and warns.
func share(total, i, n int, what string, warn func(string)) int {
	if total < n {
		if i == 0 {
			warn(fmt.Sprintf("%s: %d across %d workers; each worker gets 1, so the fleet runs %d", what, total, n, n))
		}
		return 1
	}
	s := total / n
	if i < total%n {
		s++
	}
	return s
}

func formatRate(v float64) string {
	return strconv.FormatFloat(math.Round(v*1e4)/1e4, 'f', -1, 64)
}
