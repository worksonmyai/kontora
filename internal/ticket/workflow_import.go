package ticket

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// RejectGraphStateAliases prevents normalization from copying local execution
// state into preserved fields. Check before expanding aliases or root merges.
func RejectGraphStateAliases(root *yaml.Node) error {
	if root.Kind != yaml.MappingNode {
		return nil
	}
	local := make(map[*yaml.Node]bool)
	var preserved []*yaml.Node
	aliasedKey := false
	walkYAMLNodes([]*yaml.Node{root}, func(n *yaml.Node) []*yaml.Node {
		if n.Kind != yaml.MappingNode {
			return yamlChildren(n)
		}
		var merges []*yaml.Node
		for i := 0; i < len(n.Content); i += 2 {
			key, value := n.Content[i], n.Content[i+1]
			keyValue := key
			if key.Kind == yaml.AliasNode {
				keyValue = key.Alias
			}
			switch {
			case keyValue.Tag == "!!merge":
				merges = append(merges, value)
			case keyValue.Value == "graph_state":
				aliasedKey = aliasedKey || key.Kind == yaml.AliasNode
				walkYAMLNodes([]*yaml.Node{value}, func(n *yaml.Node) []*yaml.Node {
					if local[n] {
						return nil
					}
					local[n] = true
					return yamlChildren(n)
				})
			default:
				preserved = append(preserved, key, value)
			}
		}
		return merges
	})
	if aliasedKey {
		return fmt.Errorf("graph_state cannot use a YAML alias as its key")
	}
	if len(local) == 0 {
		return nil
	}
	shared := false
	walkYAMLNodes(preserved, func(n *yaml.Node) []*yaml.Node {
		shared = shared || local[n]
		if shared {
			return nil
		}
		return yamlChildren(n)
	})
	if shared {
		return fmt.Errorf("graph_state shares YAML nodes with fields outside local execution state")
	}
	return nil
}

func yamlChildren(n *yaml.Node) []*yaml.Node {
	if n.Kind == yaml.AliasNode {
		return []*yaml.Node{n.Alias}
	}
	return n.Content
}

func walkYAMLNodes(stack []*yaml.Node, children func(*yaml.Node) []*yaml.Node) {
	seen := make(map[*yaml.Node]bool)
	for len(stack) > 0 {
		i := len(stack) - 1
		n := stack[i]
		stack = stack[:i]
		if n != nil && !seen[n] {
			seen[n] = true
			stack = append(stack, children(n)...)
		}
	}
}
