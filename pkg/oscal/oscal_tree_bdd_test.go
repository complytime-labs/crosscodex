package oscal_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	oscalTypes "github.com/defenseunicorns/go-oscal/src/types/oscal-1-1-3"

	"github.com/complytime-labs/crosscodex/pkg/oscal"
)

// parseFixtureTree parses testdata/tree/<name>.
func parseFixtureTree(name string) *oscal.Tree {
	GinkgoHelper()
	f, err := os.Open(filepath.Join("testdata", "tree", name))
	Expect(err).NotTo(HaveOccurred())
	defer func() { Expect(f.Close()).To(Succeed()) }()
	tree, err := oscal.NewParser("").Parse(context.Background(), f)
	Expect(err).NotTo(HaveOccurred())
	return tree
}

// parseJSONTree parses an inline OSCAL catalog document.
func parseJSONTree(doc string) *oscal.Tree {
	GinkgoHelper()
	tree, err := oscal.NewParser("").Parse(context.Background(), strings.NewReader(doc))
	Expect(err).NotTo(HaveOccurred())
	return tree
}

// itemsByID keys a tree's items by ID.
func itemsByID(tree *oscal.Tree) map[string]oscal.ControlItem {
	byID := make(map[string]oscal.ControlItem, len(tree.Items))
	for _, it := range tree.Items {
		byID[it.ID] = it
	}
	return byID
}

// containersByKey keys a tree's containers by position key.
func containersByKey(tree *oscal.Tree) map[string]oscal.Container {
	byKey := make(map[string]oscal.Container, len(tree.Containers))
	for _, c := range tree.Containers {
		byKey[c.Key] = c
	}
	return byKey
}

// parseFixture parses testdata/tree/<name> and returns its items keyed by ID.
func parseFixture(name string) map[string]oscal.ControlItem {
	GinkgoHelper()
	return itemsByID(parseFixtureTree(name))
}

// parseJSON parses an inline OSCAL catalog document and returns its items keyed by ID.
func parseJSON(doc string) map[string]oscal.ControlItem {
	GinkgoHelper()
	return itemsByID(parseJSONTree(doc))
}

// inlineControl is a minimal control with one statement so it yields a single control item.
func inlineControl(id string, nested ...string) string {
	sub := ""
	if len(nested) > 0 {
		sub = `,"controls":[` + strings.Join(nested, ",") + `]`
	}
	return `{"id":"` + id + `","title":"T ` + id + `","parts":[{"name":"statement","prose":"p ` + id + `"}]` + sub + `}`
}

func inlineCatalog(body string) string {
	return `{"catalog":{"uuid":"11111111-1111-4111-8111-111111111111","metadata":{"title":"t","last-modified":"2024-01-01T00:00:00Z","version":"1","oscal-version":"1.1.3"},` + body + `}}`
}

var allFixtures = []TableEntry{
	Entry("SP 800-53r5", "sp800-53r5.json"),
	Entry("SSDF", "ssdf.json"),
	Entry("SP 800-171r3", "sp800-171r3.json"),
	Entry("ISM", "ism-e8.json"),
}

var _ = Describe("Parser control tree", func() {
	DescribeTable("links a nested control's root to its base control",
		func(fixture, child, base string) {
			Expect(parseFixture(fixture)[child].ParentID).To(Equal(base))
		},
		Entry("SP 800-53r5 enhancement", "sp800-53r5.json", "ac-2.7", "ac-2"),
		Entry("SSDF task", "ssdf.json", "PO.1.1", "PO.1"),
	)

	DescribeTable("states each item's node kind",
		func(fixture, id, kind string) {
			Expect(parseFixture(fixture)[id].Props).To(HaveKeyWithValue(oscal.PropNodeKind, kind))
		},
		Entry("SP 800-53r5 base control", "sp800-53r5.json", "ac-2", oscal.PropNodeKindControl),
		Entry("SP 800-53r5 enhancement", "sp800-53r5.json", "ac-2.7", oscal.PropNodeKindControl),
		Entry("SP 800-53r5 statement item", "sp800-53r5.json", "ac-2.7.a", oscal.PropNodeKindStatementPart),
		Entry("SSDF single-item task", "ssdf.json", "PO.1.1", oscal.PropNodeKindControl),
		Entry("SP 800-171r3 statement item", "sp800-171r3.json", "SP_800_171_03.01.01.1", oscal.PropNodeKindStatementPart),
	)

	It("keeps statement parts under their own control", func() {
		Expect(parseFixture("sp800-53r5.json")["ac-2.7.a"].ParentID).To(Equal("ac-2.7"))
	})

	DescribeTable("gives every parsed item the control or statement-part kind and never the container kind",
		func(fixture string) {
			for id, it := range parseFixture(fixture) {
				Expect(it.Props[oscal.PropNodeKind]).To(BeElementOf(oscal.PropNodeKindControl, oscal.PropNodeKindStatementPart), id)
			}
		},
		allFixtures,
	)

	DescribeTable("emits one container per top-level group, keyed by position",
		func(fixture string, want oscal.Container) {
			Expect(containersByKey(parseFixtureTree(fixture))).To(HaveKeyWithValue(want.Key, want))
		},
		Entry("SP 800-53r5 family", "sp800-53r5.json", oscal.Container{Key: "g1", Identifier: "ac", Title: "Access Control", Label: "AC"}),
		Entry("SSDF practice group", "ssdf.json", oscal.Container{Key: "g1", Identifier: "PO", Title: "Prepare the Organization", Label: "Prepare the Organization (PO)"}),
		Entry("SP 800-171r3 family", "sp800-171r3.json", oscal.Container{Key: "g1", Identifier: "SP_800_171_03.01", Title: "Access Control", Label: "Access Control (03.01)"}),
		Entry("ISM ID-less top group", "ism-e8.json", oscal.Container{Key: "g1", Title: "Guidelines for system hardening"}),
		Entry("ISM ID-less subgroup", "ism-e8.json", oscal.Container{Key: "g1.1", Title: "Operating system hardening", ParentKey: "g1"}),
	)

	It("lists containers parents first", func() {
		seen := map[string]bool{}
		for _, c := range parseFixtureTree("ism-e8.json").Containers {
			Expect(c.ParentKey == "" || seen[c.ParentKey]).To(BeTrue(), c.Key)
			seen[c.Key] = true
		}
	})

	DescribeTable("sets ContainerKey on a grouped control's root, enhancement and statement parts",
		func(fixture, id, key string) {
			Expect(parseFixture(fixture)[id].ContainerKey).To(Equal(key))
		},
		Entry("control root", "sp800-53r5.json", "ac-2", "g1"),
		Entry("enhancement", "sp800-53r5.json", "ac-2.7", "g1"),
		Entry("statement part", "sp800-53r5.json", "ac-2.7.a", "g1"),
		Entry("control in a nested ID-less group", "ism-e8.json", "ism-1654", "g1.1.1"),
	)

	DescribeTable("leaves a top-level control's ParentID empty even inside a group",
		func(fixture, control string) {
			Expect(parseFixture(fixture)[control].ParentID).To(BeEmpty())
		},
		Entry("SP 800-53r5", "sp800-53r5.json", "ac-2"),
		Entry("SSDF", "ssdf.json", "PO.1"),
		Entry("SP 800-171r3", "sp800-171r3.json", "SP_800_171_03.01.01"),
		Entry("ISM", "ism-e8.json", "ism-1654"),
	)

	It("keys nested and later groups by sibling ordinals and places controls in the innermost", func() {
		tree := parseJSONTree(inlineCatalog(`"groups":[{"id":"g1","title":"One","groups":[{"id":"g2","title":"Two","controls":[` +
			inlineControl("c1") + `]}]},{"id":"g3","title":"Three"},{"id":"g4","title":"Four"}]`))
		byKey := containersByKey(tree)
		Expect(byKey).To(HaveLen(4))
		Expect(byKey["g1"]).To(Equal(oscal.Container{Key: "g1", Identifier: "g1", Title: "One"}))
		Expect(byKey["g1.1"]).To(Equal(oscal.Container{Key: "g1.1", Identifier: "g2", Title: "Two", ParentKey: "g1"}))
		Expect(byKey["g2"].Identifier).To(Equal("g3"))
		Expect(byKey["g3"].Identifier).To(Equal("g4"))
		c1 := itemsByID(tree)["c1"]
		Expect(c1.ContainerKey).To(Equal("g1.1"))
		Expect(c1.GroupID).To(Equal("g2"))
		Expect(c1.ParentID).To(BeEmpty())
	})

	It("keys mixed ID'd and ID-less nesting by position alone", func() {
		tree := parseJSONTree(inlineCatalog(`"groups":[{"title":"Anon","groups":[{"id":"x","title":"X"},{"title":"Y","controls":[` +
			inlineControl("c1") + `]}]}]`))
		byKey := containersByKey(tree)
		Expect(byKey["g1"]).To(Equal(oscal.Container{Key: "g1", Title: "Anon"}))
		Expect(byKey["g1.1"]).To(Equal(oscal.Container{Key: "g1.1", Identifier: "x", Title: "X", ParentKey: "g1"}))
		Expect(byKey["g1.2"]).To(Equal(oscal.Container{Key: "g1.2", Title: "Y", ParentKey: "g1"}))
		c1 := itemsByID(tree)["c1"]
		Expect(c1.ContainerKey).To(Equal("g1.2"))
		Expect(c1.GroupID).To(BeEmpty())
	})

	It("emits no container and no ContainerKey for a flat catalog", func() {
		tree := parseJSONTree(inlineCatalog(`"controls":[` + inlineControl("c1") + `]`))
		Expect(tree.Containers).To(BeEmpty())
		for id, it := range itemsByID(tree) {
			Expect(it.ContainerKey).To(BeEmpty(), id)
			Expect(it.ParentID).To(BeEmpty(), id)
		}
	})

	It("fails with ErrNoControls for groups without any control", func() {
		_, err := oscal.NewParser("").Parse(context.Background(),
			strings.NewReader(inlineCatalog(`"groups":[{"id":"g1","title":"One"}]`)))
		Expect(err).To(MatchError(oscal.ErrNoControls))
	})

	DescribeTable("rejects a group without a title, naming its position and identifier",
		func(groups, wantPosition, wantID string) {
			_, err := oscal.NewParser("").Parse(context.Background(),
				strings.NewReader(inlineCatalog(`"groups":[`+groups+`]`)))
			Expect(err).To(MatchError(oscal.ErrInvalidFormat))
			Expect(err.Error()).To(ContainSubstring(`group ` + wantID + ` at position ` + wantPosition + ` has no title`))
			Expect(err.Error()).To(ContainSubstring("Give the group a title"))
		},
		Entry("empty title", `{"id":"default","title":"","controls":[`+inlineControl("c1")+`]}`, "g1", `"default"`),
		Entry("whitespace-only title", `{"id":"x","title":" \t","controls":[`+inlineControl("c1")+`]}`, "g1", `"x"`),
		Entry("untitled nested group without an identifier",
			`{"id":"a","title":"A","groups":[{"title":"","controls":[`+inlineControl("c1")+`]}]}`, "g1.1", `""`),
	)

	It("parents each enhancement on its own base, however deeply the previous sibling nested", func() {
		items := parseJSON(inlineCatalog(`"controls":[` +
			inlineControl("c1", inlineControl("c1.1", inlineControl("c1.1.1")), inlineControl("c1.2")) + `]`))
		Expect(items["c1.1.1"].ParentID).To(Equal("c1.1"))
		Expect(items["c1.2"].ParentID).To(Equal("c1"))
	})
})

// assembleCatalog assembles a tree and returns the decoded catalog plus the raw JSON.
func assembleCatalog(tree *oscal.Tree) (*oscalTypes.Catalog, []byte) {
	GinkgoHelper()
	data, err := oscal.NewAssembler("").Assemble(context.Background(), tree, oscal.CatalogMeta{Title: "t", Version: "1"})
	Expect(err).NotTo(HaveOccurred())
	var wrapper oscalTypes.OscalCompleteSchema
	Expect(json.Unmarshal(data, &wrapper)).To(Succeed())
	Expect(wrapper.Catalog).NotTo(BeNil())
	return wrapper.Catalog, data
}

// findGroup returns the group with the given ID at this level, or nil.
func findGroup(groups *[]oscalTypes.Group, id string) *oscalTypes.Group {
	if groups == nil {
		return nil
	}
	for i := range *groups {
		if (*groups)[i].ID == id {
			return &(*groups)[i]
		}
	}
	return nil
}

// findControl returns the control with the given ID at this level, or nil.
func findControl(controls *[]oscalTypes.Control, id string) *oscalTypes.Control {
	if controls == nil {
		return nil
	}
	for i := range *controls {
		if (*controls)[i].ID == id {
			return &(*controls)[i]
		}
	}
	return nil
}

// stableJSON zeroes the per-run catalog UUID and timestamp so two assemblies can be compared.
func stableJSON(data []byte) []byte {
	GinkgoHelper()
	var wrapper oscalTypes.OscalCompleteSchema
	Expect(json.Unmarshal(data, &wrapper)).To(Succeed())
	wrapper.Catalog.UUID = ""
	wrapper.Catalog.Metadata.LastModified = time.Time{}
	out, err := json.Marshal(wrapper)
	Expect(err).NotTo(HaveOccurred())
	return out
}

// controlsByGroupKey maps each group's position key to the IDs of the controls
// directly in it (not in subgroups), in order; groups without controls are omitted.
func controlsByGroupKey(cat *oscalTypes.Catalog) map[string][]string {
	out := map[string][]string{}
	var walk func(gs *[]oscalTypes.Group, parentKey string)
	walk = func(gs *[]oscalTypes.Group, parentKey string) {
		if gs == nil {
			return
		}
		for i, g := range *gs {
			key := "g" + strconv.Itoa(i+1)
			if parentKey != "" {
				key = parentKey + "." + strconv.Itoa(i+1)
			}
			if g.Controls != nil {
				for _, c := range *g.Controls {
					out[key] = append(out[key], c.ID)
				}
			}
			walk(g.Groups, key)
		}
	}
	walk(cat.Groups, "")
	return out
}

// groupIDs lists the IDs of the groups at this level, in order.
func groupIDs(groups *[]oscalTypes.Group) []string {
	var ids []string
	if groups != nil {
		for _, g := range *groups {
			ids = append(ids, g.ID)
		}
	}
	return ids
}

var _ = Describe("Assemble control tree round trip", func() {
	DescribeTable("rebuilds the source groups and nests enhancements as controls",
		func(fixture, group, title, base, enhancement string) {
			cat, _ := assembleCatalog(parseFixtureTree(fixture))
			g := findGroup(cat.Groups, group)
			Expect(g).NotTo(BeNil())
			Expect(g.Title).To(Equal(title))
			baseCtrl := findControl(g.Controls, base)
			Expect(baseCtrl).NotTo(BeNil())
			Expect(findControl(baseCtrl.Controls, enhancement)).NotTo(BeNil(), "enhancement is a nested control")
			if baseCtrl.Parts != nil {
				for _, p := range *baseCtrl.Parts {
					Expect(p.ID).NotTo(Equal(enhancement), "enhancement must not be a part")
				}
			}
		},
		Entry("SP 800-53r5", "sp800-53r5.json", "ac", "Access Control", "ac-2", "ac-2.7"),
		Entry("SSDF", "ssdf.json", "PO", "Prepare the Organization", "PO.1", "PO.1.1"),
	)

	It("attaches an enhancement's statement items to the enhancement", func() {
		cat, _ := assembleCatalog(parseFixtureTree("sp800-53r5.json"))
		enh := findControl(findControl(findGroup(cat.Groups, "ac").Controls, "ac-2").Controls, "ac-2.7")
		Expect(enh).NotTo(BeNil())
		var ids []string
		var collect func(ps *[]oscalTypes.Part)
		collect = func(ps *[]oscalTypes.Part) {
			if ps == nil {
				return
			}
			for _, p := range *ps {
				ids = append(ids, p.ID)
				collect(p.Parts)
			}
		}
		collect(enh.Parts)
		Expect(ids).To(ContainElement("ac-2.7.a"))
	})

	DescribeTable("preserves group nesting, identifiers and titles",
		func(fixture string) {
			tree := parseFixtureTree(fixture)
			cat, _ := assembleCatalog(tree)
			var got []oscal.Container
			var walk func(gs *[]oscalTypes.Group, parentKey string)
			walk = func(gs *[]oscalTypes.Group, parentKey string) {
				if gs == nil {
					return
				}
				for i, g := range *gs {
					key := "g" + strconv.Itoa(i+1)
					if parentKey != "" {
						key = parentKey + "." + strconv.Itoa(i+1)
					}
					got = append(got, oscal.Container{Key: key, Identifier: g.ID, Title: g.Title, ParentKey: parentKey})
					walk(g.Groups, key)
				}
			}
			walk(cat.Groups, "")
			// The assembled catalog does not carry labels, so compare without them.
			var want []oscal.Container
			for _, c := range tree.Containers {
				c.Label = ""
				want = append(want, c)
			}
			Expect(got).To(ConsistOf(want))
		},
		allFixtures,
	)

	It("omits the group id of an ID-less container in the raw JSON", func() {
		_, data := assembleCatalog(parseFixtureTree("ism-e8.json"))
		var doc struct {
			Catalog struct {
				Groups []map[string]json.RawMessage `json:"groups"`
			} `json:"catalog"`
		}
		Expect(json.Unmarshal(data, &doc)).To(Succeed())
		Expect(doc.Catalog.Groups).NotTo(BeEmpty())
		Expect(doc.Catalog.Groups[0]).NotTo(HaveKey("id"))
		Expect(doc.Catalog.Groups[0]).To(HaveKey("title"))
	})

	DescribeTable("places each control row in the group its ContainerKey names, and ungrouped controls at the top",
		func(fixture string) {
			tree := parseFixtureTree(fixture)
			cat, _ := assembleCatalog(tree)
			placed := controlsByGroupKey(cat)
			want := map[string][]string{}
			for _, it := range tree.Items {
				if it.Props[oscal.PropNodeKind] == oscal.PropNodeKindControl && it.ParentID == "" {
					want[it.ContainerKey] = append(want[it.ContainerKey], it.ID)
				}
			}
			Expect(placed).To(HaveLen(len(want)))
			for key, ids := range want {
				Expect(placed).To(HaveKeyWithValue(key, ids), "group %q", key)
			}
		},
		allFixtures,
	)

	It("places an ID-less group's control in that group, not at the top", func() {
		cat, _ := assembleCatalog(parseFixtureTree("ism-e8.json"))
		Expect(controlsByGroupKey(cat)).To(HaveKeyWithValue("g1.1.1", []string{"ism-1654"}))
		Expect(cat.Controls).To(BeNil())
	})

	It("places controls by ContainerKey even when the container identifier differs from their GroupID", func() {
		cat, _ := assembleCatalog(&oscal.Tree{
			Containers: []oscal.Container{{Key: "g1", Identifier: "one", Title: "One"}, {Key: "g2", Identifier: "two", Title: "Two"}},
			Items: []oscal.ControlItem{
				{ID: "c1", Text: "t", GroupID: "two", ContainerKey: "g1", Props: map[string]string{oscal.PropNodeKind: oscal.PropNodeKindControl}},
				{ID: "u1", Text: "t", GroupID: "two", ContainerKey: "g1"},
			},
		})
		Expect(controlsByGroupKey(cat)).To(Equal(map[string][]string{"g1": {"c1", "u1"}}))
	})

	It("puts a root-level ungrouped control at the catalog top level", func() {
		tree := parseJSONTree(inlineCatalog(`"controls":[` + inlineControl("c1") + `]`))
		cat, _ := assembleCatalog(tree)
		Expect(findControl(cat.Controls, "c1")).NotTo(BeNil())
		Expect(cat.Groups).To(BeNil())
	})

	DescribeTable("is deterministic and keeps input group order",
		func(fixture string) {
			tree := parseFixtureTree(fixture)
			first, firstData := assembleCatalog(tree)
			_, secondData := assembleCatalog(tree)
			Expect(stableJSON(secondData)).To(Equal(stableJSON(firstData)))

			var want []string
			for _, c := range tree.Containers {
				if c.ParentKey == "" {
					want = append(want, c.Identifier)
				}
			}
			Expect(groupIDs(first.Groups)).To(Equal(want))
		},
		allFixtures,
	)

	It("joins an unmarked legacy item to the container whose identifier it names", func() {
		cat, _ := assembleCatalog(&oscal.Tree{
			Containers: []oscal.Container{{Key: "g1", Identifier: "ac", Title: "Access Control", Label: "AC"}},
			Items:      []oscal.ControlItem{{ID: "ac-1", Title: "Policy", Text: "t", GroupID: "ac"}},
		})
		Expect(*cat.Groups).To(HaveLen(1))
		g := (*cat.Groups)[0]
		Expect(g.Title).To(Equal("Access Control"))
		Expect(findControl(g.Controls, "ac-1")).NotTo(BeNil())
	})

	It("synthesizes a group for an unmarked legacy item with no container", func() {
		cat, _ := assembleCatalog(&oscal.Tree{
			Items: []oscal.ControlItem{{ID: "ac-1", Title: "Policy", Text: "t", GroupID: "ac"}},
		})
		g := findGroup(cat.Groups, "ac")
		Expect(g).NotTo(BeNil())
		Expect(g.Title).To(Equal("ac"))
	})
})

var _ = Describe("Assemble empty arrays", func() {
	// OSCAL arrays carry minItems 1, so an empty groups or controls array is invalid.
	topLevelKeys := func(data []byte) map[string]json.RawMessage {
		GinkgoHelper()
		var doc map[string]map[string]json.RawMessage
		Expect(json.Unmarshal(data, &doc)).To(Succeed())
		return doc["catalog"]
	}

	It("omits the groups key when the input has only top-level controls", func() {
		_, data := assembleCatalog(parseJSONTree(inlineCatalog(`"controls":[` + inlineControl("c1") + `]`)))
		Expect(topLevelKeys(data)).To(HaveKey("controls"))
		Expect(topLevelKeys(data)).NotTo(HaveKey("groups"))
	})

	It("omits the controls key when every control is in a group", func() {
		_, data := assembleCatalog(parseFixtureTree("ssdf.json"))
		Expect(topLevelKeys(data)).To(HaveKey("groups"))
		Expect(topLevelKeys(data)).NotTo(HaveKey("controls"))
	})
})
