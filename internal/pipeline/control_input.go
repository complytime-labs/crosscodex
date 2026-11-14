package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/crosswalk"
	"github.com/complytime-labs/crosscodex/pkg/db"
	"github.com/complytime-labs/crosscodex/pkg/oscal"
)

// catalogRow is one controls row as import stored it. parentID is raw (the
// parent's identifier), while controlID is "<catalog_id>/<identifier>".
type catalogRow struct {
	controlID, identifier, title, statement, class, parentID, containerKey string
	props                                                                  map[string]string
}

// catalogContainer is one catalog_containers row: a group that is context
// for its controls but never a control itself. parentKey is "" at the top.
type catalogContainer struct {
	key, parentKey, identifier, title, label string
}

// readCatalog reads every control row of a catalog (sections and withdrawn
// controls included, since they still supply context and lead-ins), ordered
// by control_id, and its containers by key, within the caller's tenant-scoped
// transaction.
func readCatalog(ctx context.Context, tx db.Transaction, tenantID, catalogID string) ([]catalogRow, map[string]catalogContainer, error) {
	rows, err := tx.Query(ctx, `
		SELECT control_id, identifier, COALESCE(title, ''), COALESCE(statement, ''), COALESCE(class, ''),
		       COALESCE(parent_id, ''), COALESCE(container_key, ''), props
		FROM controls
		WHERE tenant_id = $1 AND catalog_id = $2
		ORDER BY control_id
	`, tenantID, catalogID)
	if err != nil {
		return nil, nil, fmt.Errorf("querying controls: %w", err)
	}
	defer rows.Close()

	var controls []catalogRow
	for rows.Next() {
		var r catalogRow
		var propsJSON []byte
		if err := rows.Scan(&r.controlID, &r.identifier, &r.title, &r.statement, &r.class, &r.parentID, &r.containerKey, &propsJSON); err != nil {
			return nil, nil, fmt.Errorf("scanning control row: %w", err)
		}
		if len(propsJSON) > 0 {
			if err := json.Unmarshal(propsJSON, &r.props); err != nil {
				return nil, nil, fmt.Errorf("unmarshaling props for %s: %w", r.controlID, err)
			}
		}
		controls = append(controls, r)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("control rows error: %w", err)
	}
	rows.Close()

	crows, err := tx.Query(ctx, `
		SELECT container_key, COALESCE(parent_key, ''), title, identifier, label
		FROM catalog_containers
		WHERE tenant_id = $1 AND catalog_id = $2
	`, tenantID, catalogID)
	if err != nil {
		return nil, nil, fmt.Errorf("querying containers: %w", err)
	}
	defer crows.Close()

	containers := make(map[string]catalogContainer)
	for crows.Next() {
		var c catalogContainer
		if err := crows.Scan(&c.key, &c.parentKey, &c.title, &c.identifier, &c.label); err != nil {
			return nil, nil, fmt.Errorf("scanning container row: %w", err)
		}
		containers[c.key] = c
	}
	if err := crows.Err(); err != nil {
		return nil, nil, fmt.Errorf("container rows error: %w", err)
	}
	return controls, containers, nil
}

// composeControls turns a catalog's rows into the analysis input, one
// *pb.Control per row in the order of rows; both the analyzers' reader and
// the candidate reader use it, so every stage sees the same text for a
// control. On each control:
//
//   - Parts holds the row's props, then "class", then "context" (so those two
//     win on a key collision). "context" is the titled ancestors, root first,
//     each rendered by crosswalk.LineageSegment and joined with " > ": the
//     walk follows parent_id (looked up as catalogID+"/"+parent_id) and, from
//     the first row without one, its container and the container's parents.
//     It is absent when no ancestor has a title.
//   - Statement is, for a statement part, its lead-ins (the prose of each
//     statement-part ancestor, then of the owning control row, root first)
//     followed by its own text, joined with a space. Lead-ins stop at the
//     owning control, so a parent control's text never reaches an
//     enhancement. A control row, a row without a node-kind (a non-OSCAL
//     import) and a part whose own text is blank keep their own text. Text
//     is whitespace-trimmed.
//   - Title is the row's title unless it only repeats the identifier or label
//     (oscal.UsableTitle). A control with a blank statement but a usable title
//     gets the title as its statement and an empty Title, so the text is not
//     shown twice.
//
// Every walk keeps a visited set and there is no depth bound. A parent_id that
// names no row, or one already visited (a cycle), ends the walk at any hop:
// the segments and lead-ins gathered so far are kept, and the container
// context is dropped, since the chain never reached a top control.
func composeControls(catalogID string, rows []catalogRow, containers map[string]catalogContainer) []*pb.Control {
	byID := make(map[string]*catalogRow, len(rows))
	for i := range rows {
		byID[rows[i].controlID] = &rows[i]
	}
	controls := make([]*pb.Control, len(rows))
	for i := range rows {
		r := &rows[i]
		var segments, leadIns []string
		collecting := r.props[oscal.PropNodeKind] == oscal.PropNodeKindStatementPart
		visited := map[string]bool{r.controlID: true}
		top := r
		for top.parentID != "" {
			parent, ok := byID[catalogID+"/"+top.parentID]
			if !ok || visited[parent.controlID] {
				top = nil
				break
			}
			visited[parent.controlID] = true
			if seg := crosswalk.LineageSegment(parent.title, parent.props[oscal.PropLabel], parent.identifier); seg != "" {
				segments = append(segments, seg)
			}
			if collecting {
				kind := parent.props[oscal.PropNodeKind]
				if text := strings.TrimSpace(parent.statement); text != "" && (kind == oscal.PropNodeKindStatementPart || kind == oscal.PropNodeKindControl) {
					leadIns = append(leadIns, text)
				}
				collecting = kind == oscal.PropNodeKindStatementPart
			}
			top = parent
		}
		if top != nil {
			seen := map[string]bool{}
			for key := top.containerKey; key != "" && !seen[key]; {
				seen[key] = true
				c, ok := containers[key]
				if !ok {
					break
				}
				if seg := crosswalk.LineageSegment(c.title, c.label, c.identifier); seg != "" {
					segments = append(segments, seg)
				}
				key = c.parentKey
			}
		}
		slices.Reverse(segments)
		slices.Reverse(leadIns)

		statement := strings.TrimSpace(r.statement)
		if statement != "" && len(leadIns) > 0 {
			statement = strings.Join(append(leadIns, statement), " ")
		}
		title := oscal.UsableTitle(r.title, r.identifier, r.props[oscal.PropLabel])
		if statement == "" && title != "" {
			statement, title = title, ""
		}

		parts := make(map[string]string, len(r.props)+2)
		maps.Copy(parts, r.props)
		parts["class"] = r.class
		delete(parts, "context")
		if len(segments) > 0 {
			parts["context"] = strings.Join(segments, " > ")
		}
		controls[i] = &pb.Control{
			ControlId:  r.controlID,
			CatalogId:  catalogID,
			Identifier: r.identifier,
			Title:      title,
			Statement:  statement,
			Parts:      parts,
		}
	}
	return controls
}
