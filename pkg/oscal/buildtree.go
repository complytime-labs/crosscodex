package oscal

import (
	"fmt"
	"strconv"
)

// BuildTree turns structured items (for example Structurer output) into a
// Tree. The hierarchy comes only from the items' link fields, never from the
// form of their IDs:
//
//   - An item that another item names as its GroupID is a group. It becomes a
//     Container (Identifier = ID, Title, Prose = Text, Label = Props["label"])
//     and is not in Tree.Items. A group's own GroupID names its enclosing
//     group; a group has no ParentID.
//   - Every other item stays in Tree.Items, in input order, with its fields
//     unchanged except ContainerKey: the Key of the Container its GroupID
//     names, or else its parent's ContainerKey ("" outside any group).
//
// Containers are in pre-order, siblings in the order their items appear.
// Keys follow the parser's form: "g" plus sibling ordinals joined by "."
// ("g2.1"). Tree.Textless is nil: structured items carry no OSCAL parts to
// name. items is not modified.
//
// Items that cannot form a tree return an error wrapping ErrInvalidItems
// that names the item: an empty or repeated ID, a GroupID or ParentID naming
// no item, a ParentID naming a group, a group with a ParentID, or a cycle.
func BuildTree(items []ControlItem) (*Tree, error) {
	index := make(map[string]int, len(items))
	for i, item := range items {
		if item.ID == "" {
			return nil, fmt.Errorf("%w: item %d has no ID; every control, part and group needs one", ErrInvalidItems, i)
		}
		if first, dup := index[item.ID]; dup {
			return nil, fmt.Errorf("%w: items %d and %d share the ID %q; every control, part and group needs its own ID, so structure the document so each is distinct",
				ErrInvalidItems, first, i, item.ID)
		}
		index[item.ID] = i
	}

	isGroup := make(map[string]bool)
	for _, item := range items {
		if item.GroupID == "" {
			continue
		}
		if _, ok := index[item.GroupID]; !ok {
			return nil, fmt.Errorf("%w: item %q names group %q, which is not an item; a group must be one of the items", ErrInvalidItems, item.ID, item.GroupID)
		}
		isGroup[item.GroupID] = true
	}

	for _, item := range items {
		if isGroup[item.ID] {
			if item.ParentID != "" {
				return nil, fmt.Errorf("%w: group %q has parent %q; a group nests only in another group (GroupID)", ErrInvalidItems, item.ID, item.ParentID)
			}
			if err := checkChain(items, index, item.ID, "GroupID", func(it ControlItem) string { return it.GroupID }); err != nil {
				return nil, err
			}
			continue
		}
		if item.ParentID == "" {
			continue
		}
		if _, ok := index[item.ParentID]; !ok {
			return nil, fmt.Errorf("%w: item %q names parent %q, which is not an item", ErrInvalidItems, item.ID, item.ParentID)
		}
		if isGroup[item.ParentID] {
			return nil, fmt.Errorf("%w: item %q names parent %q, which is a group; place it in the group with GroupID instead", ErrInvalidItems, item.ID, item.ParentID)
		}
	}
	for _, item := range items {
		if !isGroup[item.ID] {
			if err := checkChain(items, index, item.ID, "ParentID", func(it ControlItem) string { return it.ParentID }); err != nil {
				return nil, err
			}
		}
	}

	// Children of each group (and of the root, ""), in input order.
	children := make(map[string][]string)
	for _, item := range items {
		if isGroup[item.ID] {
			children[item.GroupID] = append(children[item.GroupID], item.ID)
		}
	}
	keyOf := make(map[string]string, len(isGroup))
	containers := make([]Container, 0, len(isGroup))
	var walk func(parentID, parentKey string)
	walk = func(parentID, parentKey string) {
		for n, id := range children[parentID] {
			key := "g" + strconv.Itoa(n+1)
			if parentKey != "" {
				key = parentKey + "." + strconv.Itoa(n+1)
			}
			keyOf[id] = key
			g := items[index[id]]
			containers = append(containers, Container{
				Key:        key,
				Identifier: g.ID,
				Title:      g.Title,
				Label:      g.Props["label"],
				Prose:      g.Text,
				ParentKey:  parentKey,
			})
			walk(id, key)
		}
	}
	walk("", "")

	var containerKey func(i int) string
	containerKey = func(i int) string {
		item := items[i]
		switch {
		case item.GroupID != "":
			return keyOf[item.GroupID]
		case item.ParentID != "":
			return containerKey(index[item.ParentID])
		default:
			return ""
		}
	}
	out := make([]ControlItem, 0, len(items)-len(isGroup))
	for i, item := range items {
		if isGroup[item.ID] {
			continue
		}
		item.ContainerKey = containerKey(i)
		out = append(out, item)
	}
	return &Tree{Containers: containers, Items: out}, nil
}

// checkChain follows the field link (read by next) from the item with ID
// start and returns an error wrapping ErrInvalidItems if the chain reaches an
// ID it has visited. Every ID on the chain must already be known to exist.
func checkChain(items []ControlItem, index map[string]int, start, field string, next func(ControlItem) string) error {
	seen := map[string]bool{start: true}
	for id := next(items[index[start]]); id != ""; id = next(items[index[id]]) {
		if seen[id] {
			return fmt.Errorf("%w: the %s chain of item %q loops back on itself; links must form a tree", ErrInvalidItems, field, start)
		}
		seen[id] = true
	}
	return nil
}
