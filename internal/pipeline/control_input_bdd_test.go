package pipeline

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/db/dbtest"
	"github.com/complytime-labs/crosscodex/pkg/oscal"
	"github.com/complytime-labs/crosscodex/pkg/tenant"
)

// accessControlRows is SP 800-53 rev. 4 AC-2 as import stores it: control
// rows and statement-part rows carry a node-kind, statement parts are
// untitled, and the top control sits in the "Access Control" group. The texts
// are the catalog's own.
func accessControlRows() ([]catalogRow, map[string]catalogContainer) {
	control := func(id, title, statement, parentID, label string) catalogRow {
		return catalogRow{controlID: "cat-1/" + id, identifier: id, title: title, statement: statement,
			class: oscal.ClassSection, parentID: parentID, containerKey: "g1",
			props: map[string]string{oscal.PropNodeKind: oscal.PropNodeKindControl, oscal.PropLabel: label}}
	}
	part := func(id, statement, parentID, class string) catalogRow {
		return catalogRow{controlID: "cat-1/" + id, identifier: id, statement: statement,
			class: class, parentID: parentID, containerKey: "g1",
			props: map[string]string{oscal.PropNodeKind: oscal.PropNodeKindStatementPart, "parent-id": parentID}}
	}
	rows := []catalogRow{
		control("ac-2", "Account Management", "The organization:", "", "AC-2"),
		part("ac-2.d", "Specify:", "ac-2", oscal.ClassSection),
		part("ac-2.d.1", "Authorized users of the system;", "ac-2.d", oscal.ClassRequirement),
		control("ac-2.7", "Privileged User Accounts", "The organization:", "ac-2", "AC-2(7)"),
		part("ac-2.7.a", "Establishes and administers privileged user accounts in accordance with a role-based access scheme;",
			"ac-2.7", oscal.ClassRequirement),
	}
	containers := map[string]catalogContainer{
		"g1": {key: "g1", identifier: "ac", title: "Access Control", label: "AC"},
	}
	return rows, containers
}

// composedByIdentifier composes rows and indexes the result by identifier.
func composedByIdentifier(rows []catalogRow, containers map[string]catalogContainer) map[string]*pb.Control {
	byID := map[string]*pb.Control{}
	for _, c := range composeControls("cat-1", rows, containers) {
		byID[c.GetIdentifier()] = c
	}
	return byID
}

var _ = Describe("composeControls", func() {
	It("prefixes a statement part with its lead-ins and gives it its titled ancestors as context", func() {
		got := composedByIdentifier(accessControlRows())["ac-2.d.1"]
		Expect(got.GetStatement()).To(Equal("The organization: Specify: Authorized users of the system;"))
		Expect(got.GetParts()).To(HaveKeyWithValue("context", "Access Control (AC) > Account Management (AC-2)"))
		Expect(got.GetTitle()).To(BeEmpty())
	})

	It("never prepends a parent control's text to an enhancement", func() {
		got := composedByIdentifier(accessControlRows())
		Expect(got["ac-2.7"].GetStatement()).To(Equal("The organization:"))
		Expect(got["ac-2.7"].GetTitle()).To(Equal("Privileged User Accounts"))
		Expect(got["ac-2.7"].GetParts()).To(HaveKeyWithValue("context", "Access Control (AC) > Account Management (AC-2)"))
		Expect(got["ac-2.7.a"].GetStatement()).To(Equal(
			"The organization: Establishes and administers privileged user accounts in accordance with a role-based access scheme;"))
		Expect(got["ac-2.7.a"].GetParts()).To(HaveKeyWithValue("context",
			"Access Control (AC) > Account Management (AC-2) > Privileged User Accounts (AC-2(7))"))
	})

	It("keeps the control order, the props and the class", func() {
		rows, containers := accessControlRows()
		got := composeControls("cat-1", rows, containers)
		ids := make([]string, len(got))
		for i, c := range got {
			ids[i] = c.GetControlId()
		}
		Expect(ids).To(Equal([]string{"cat-1/ac-2", "cat-1/ac-2.d", "cat-1/ac-2.d.1", "cat-1/ac-2.7", "cat-1/ac-2.7.a"}))
		Expect(got[0].GetCatalogId()).To(Equal("cat-1"))
		Expect(got[0].GetParts()).To(HaveKeyWithValue(oscal.PropLabel, "AC-2"))
		Expect(got[0].GetParts()).To(HaveKeyWithValue("class", oscal.ClassSection))
		Expect(got[0].GetParts()).To(HaveKeyWithValue("context", "Access Control (AC)"))
	})

	It("adds no lead-in and no double space for a statement part with blank prose", func() {
		rows := []catalogRow{
			{controlID: "cat-1/x", identifier: "x", title: "Wrapped", statement: "Lead:",
				props: map[string]string{oscal.PropNodeKind: oscal.PropNodeKindControl}},
			{controlID: "cat-1/x.a", identifier: "x.a", statement: "  ", class: oscal.ClassSection, parentID: "x",
				props: map[string]string{oscal.PropNodeKind: oscal.PropNodeKindStatementPart}},
			{controlID: "cat-1/x.a.1", identifier: "x.a.1", statement: "Do it.", parentID: "x.a",
				props: map[string]string{oscal.PropNodeKind: oscal.PropNodeKindStatementPart}},
		}
		Expect(composedByIdentifier(rows, nil)["x.a.1"].GetStatement()).To(Equal("Lead: Do it."))
	})

	It("gives a statement part with blank prose no text from its lead-ins", func() {
		rows := []catalogRow{
			{controlID: "cat-1/x", identifier: "x", statement: "Lead:",
				props: map[string]string{oscal.PropNodeKind: oscal.PropNodeKindControl}},
			{controlID: "cat-1/x.a", identifier: "x.a", statement: " ", parentID: "x",
				props: map[string]string{oscal.PropNodeKind: oscal.PropNodeKindStatementPart}},
		}
		Expect(composedByIdentifier(rows, nil)["x.a"].GetStatement()).To(BeEmpty())
	})

	It("gives rows without a node-kind no lead-ins", func() {
		rows := []catalogRow{
			{controlID: "cat-1/x", identifier: "x", title: "Parent", statement: "Lead:"},
			{controlID: "cat-1/x.a", identifier: "x.a", statement: "Do it.", parentID: "x"},
		}
		got := composedByIdentifier(rows, nil)["x.a"]
		Expect(got.GetStatement()).To(Equal("Do it."))
		Expect(got.GetParts()).To(HaveKeyWithValue("context", "Parent (x)"))
	})

	It("uses a title-only control's title as its statement and leaves its Title empty", func() {
		rows := []catalogRow{{controlID: "cat-1/inv", identifier: "inv", title: " Asset Inventory ",
			props: map[string]string{oscal.PropNodeKind: oscal.PropNodeKindControl}}}
		got := composedByIdentifier(rows, nil)["inv"]
		Expect(got.GetStatement()).To(Equal("Asset Inventory"))
		Expect(got.GetTitle()).To(BeEmpty())
	})

	It("drops a title that only repeats the label and keeps the statement", func() {
		rows := []catalogRow{{controlID: "cat-1/manage-4.2", identifier: "manage-4.2", title: "MANAGE 4.2",
			statement: "Mechanisms are in place.", props: map[string]string{oscal.PropLabel: "MANAGE_4.2"}}}
		got := composedByIdentifier(rows, nil)["manage-4.2"]
		Expect(got.GetTitle()).To(BeEmpty())
		Expect(got.GetStatement()).To(Equal("Mechanisms are in place."))
	})

	It("sets no context when no ancestor has a title, overriding a context prop", func() {
		rows := []catalogRow{
			{controlID: "cat-1/x", identifier: "x", statement: "Parent text.", containerKey: "g1"},
			{controlID: "cat-1/x.a", identifier: "x.a", statement: "Do it.", parentID: "x",
				props: map[string]string{"context": "stale"}},
		}
		got := composedByIdentifier(rows, map[string]catalogContainer{"g1": {key: "g1", identifier: "g"}})
		Expect(got["x"].GetParts()).NotTo(HaveKey("context"))
		Expect(got["x.a"].GetParts()).NotTo(HaveKey("context"))
	})

	It("stops at a parent_id that names no row of the catalog", func() {
		rows := []catalogRow{
			{controlID: "cat-1/x", identifier: "x", title: "Parent", statement: "Lead:", containerKey: "g1"},
			{controlID: "cat-1/y", identifier: "y", statement: "Do it.", parentID: "cat-1/x", containerKey: "g1"},
		}
		got := composedByIdentifier(rows, map[string]catalogContainer{"g1": {key: "g1", title: "Group"}})
		Expect(got["y"].GetParts()).NotTo(HaveKey("context"), "parent_id is stored raw, so a catalog-prefixed value is unknown")
	})

	DescribeTable("keeps the segments gathered before an unknown or cyclic parent and drops the container context",
		func(bParent string) {
			rows := []catalogRow{
				{controlID: "cat-1/a", identifier: "a", title: "A", statement: "Do a.", parentID: "b", containerKey: "g1"},
				{controlID: "cat-1/b", identifier: "b", title: "B", statement: "Do b.", parentID: bParent, containerKey: "g1"},
				{controlID: "cat-1/c", identifier: "c", statement: "Do c.", parentID: "a", containerKey: "g1"},
			}
			got := composedByIdentifier(rows, map[string]catalogContainer{"g1": {key: "g1", title: "Group"}})
			Expect(got["c"].GetParts()).To(HaveKeyWithValue("context", "B > A"))
		},
		Entry("an unknown parent two hops up", "missing"),
		Entry("a cycle two hops up", "a"),
	)

	It("terminates on a parent_id cycle", func() {
		rows := []catalogRow{
			{controlID: "cat-1/a", identifier: "a", title: "A", statement: "Do a.", parentID: "b",
				props: map[string]string{oscal.PropNodeKind: oscal.PropNodeKindStatementPart}},
			{controlID: "cat-1/b", identifier: "b", title: "B", statement: "Do b.", parentID: "a",
				props: map[string]string{oscal.PropNodeKind: oscal.PropNodeKindStatementPart}},
		}
		got := composedByIdentifier(rows, nil)
		Expect(got["a"].GetParts()).To(HaveKeyWithValue("context", "B"))
		Expect(got["a"].GetStatement()).To(Equal("Do b. Do a."))
	})

	It("terminates on a container parent_key cycle", func() {
		rows := []catalogRow{{controlID: "cat-1/x", identifier: "x", statement: "Do it.", containerKey: "g1"}}
		containers := map[string]catalogContainer{
			"g1": {key: "g1", parentKey: "g2", title: "Inner"},
			"g2": {key: "g2", parentKey: "g1", title: "Outer"},
		}
		Expect(composedByIdentifier(rows, containers)["x"].GetParts()).To(HaveKeyWithValue("context", "Outer > Inner"))
	})
})

// catalogMock serves the rows and containers of accessControlRows, plus a
// title-only and a textless control, to either reader's queries, in the
// column order of readCatalog.
func catalogMock() *dbtest.Conn {
	rows, containers := accessControlRows()
	rows = append(rows,
		catalogRow{controlID: "cat-1/inv", identifier: "inv", title: "Asset Inventory",
			props: map[string]string{oscal.PropNodeKind: oscal.PropNodeKindControl}},
		catalogRow{controlID: "cat-1/empty", identifier: "empty", title: "EMPTY",
			props: map[string]string{oscal.PropNodeKind: oscal.PropNodeKindControl}},
	)
	return &dbtest.Conn{Tx: &dbtest.Tx{
		QueryFunc: func(ctx context.Context, query string, args ...any) (db.Rows, error) {
			if strings.Contains(query, "catalog_containers") {
				var out [][]any
				for _, c := range containers {
					out = append(out, []any{c.key, c.parentKey, c.title, c.identifier, c.label})
				}
				return dbtest.Rows(nil, out...), nil
			}
			var out [][]any
			for _, r := range rows {
				props, err := json.Marshal(r.props)
				Expect(err).NotTo(HaveOccurred())
				out = append(out, []any{r.controlID, r.identifier, r.title, r.statement, r.class, r.parentID, r.containerKey, props})
			}
			return dbtest.Rows(nil, out...), nil
		},
		QueryRowFunc: func(ctx context.Context, query string, args ...any) db.Row {
			return dbtest.Row(sql.ErrNoRows)
		},
	}}
}

var _ = Describe("the analysis and candidate readers", func() {
	It("give a control the same text and context", func() {
		ctx, err := tenant.WithTenant(context.Background(), "tenant-a")
		Expect(err).NotTo(HaveOccurred())
		controls, err := NewPGCatalogControlsReader(catalogMock()).Controls(ctx, "tenant-a", "cat-1")
		Expect(err).NotTo(HaveOccurred())
		data, err := NewPGControlsReader(catalogMock()).ControlData(ctx, "tenant-a", "cat-1", "job-1")
		Expect(err).NotTo(HaveOccurred())

		Expect(data).To(HaveLen(4), "sections are not candidates; the textless control is left to controlsWithText")
		for _, c := range controls {
			cd, ok := data[c.GetControlId()]
			if !ok {
				continue
			}
			Expect(cd.Text).To(Equal(c.GetStatement()), c.GetControlId())
			Expect(cd.Ancestor).To(Equal(c.GetParts()["context"]), c.GetControlId())
		}
		Expect(data).To(HaveKey("cat-1/ac-2.d.1"))
		Expect(data["cat-1/ac-2.d.1"].Text).To(Equal("The organization: Specify: Authorized users of the system;"))
	})

	It("keep a title-only control for candidate generation and skip a textless one", func() {
		ctx, err := tenant.WithTenant(context.Background(), "tenant-a")
		Expect(err).NotTo(HaveOccurred())
		gen := &CandidateGenerator{controls: NewPGControlsReader(catalogMock())}
		data, skipped, err := gen.controlsWithText(ctx, "tenant-a", "cat-1", "job-1")
		Expect(err).NotTo(HaveOccurred())
		Expect(data).To(HaveKey("cat-1/inv"))
		Expect(data["cat-1/inv"].Text).To(Equal("Asset Inventory"))
		Expect(data).NotTo(HaveKey("cat-1/empty"))
		Expect(skipped).To(Equal(1), "the textless control is counted as skipped")
	})
})
