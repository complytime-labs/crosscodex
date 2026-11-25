package crosswalk

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
)

const (
	// MaxControlsDumpBytes bounds a controls dump read: the composed text of
	// a large catalog (SP 800-53 is a few MiB) with generous headroom. The
	// dump is output of the server and of untrusted catalog text.
	MaxControlsDumpBytes = 32 << 20
	// controlsSourceFile and controlsTargetFile are the run directory's dumps
	// of `crosscodex catalog controls --json`, written by run.sh when
	// LABEL_INPUT=1.
	controlsSourceFile = "controls-source.json"
	controlsTargetFile = "controls-target.json"
	candidatesFileName = "candidates.json"
)

// controlsDump is `crosscodex catalog controls <id> --json`: the title and
// statement the pipeline sends analyzers. Only the fields used here are
// decoded and unknown ones are ignored, because that output is the CLI's
// contract and may gain fields; the fields used are checked instead.
type controlsDump struct {
	CatalogID string `json:"catalog_id"`
	Controls  []struct {
		ControlID string `json:"control_id"`
		Title     string `json:"title"`
		Statement string `json:"statement"`
	} `json:"controls"`
}

// controlText is one control's composed text.
type controlText struct{ title, statement string }

// labelInputRow is one candidate pair with the text of both controls, one
// line of label-input output. Source and Target are control IDs as the label
// file names them.
type labelInputRow struct {
	Source      string `json:"source"`
	Target      string `json:"target"`
	SourceTitle string `json:"source_title"`
	SourceText  string `json:"source_text"`
	TargetTitle string `json:"target_title"`
	TargetText  string `json:"target_text"`
}

// LabelInput writes to w, as JSONL, every candidate pair of the run in runDir
// with its two controls' composed text from the run's controls dumps. All
// pairs are checked first: if any names a control absent from its dump,
// nothing is written and the error lists every such control. The text is the
// CLI's own composition (`catalog controls`), never rebuilt from the OSCAL.
func LabelInput(runDir string, w io.Writer) error {
	candPath := filepath.Join(runDir, candidatesFileName)
	var c candidatesFile
	if err := decodeFile(candPath, "candidates export", &c); err != nil {
		return err
	}
	srcPath, tgtPath := filepath.Join(runDir, controlsSourceFile), filepath.Join(runDir, controlsTargetFile)
	srcCatalog, src, err := loadControlsDump(srcPath)
	if err != nil {
		return err
	}
	tgtCatalog, tgt, err := loadControlsDump(tgtPath)
	if err != nil {
		return err
	}

	rows := make([]labelInputRow, 0, len(c.Candidates))
	var missing []string
	seen := map[string]bool{}
	lookup := func(side, id string, dumps map[string]controlText) (string, controlText, error) {
		catalog, path := srcCatalog, srcPath
		if side == "target" {
			catalog, path = tgtCatalog, tgtPath
		}
		bare, err := bareControlID(candPath, side+"ControlId", id, catalog)
		if err != nil {
			return "", controlText{}, err
		}
		t, ok := dumps[bare]
		if !ok && !seen[side+"/"+bare] {
			seen[side+"/"+bare] = true
			missing = append(missing, fmt.Sprintf("%s control %q (not in %s)", side, bare, path))
		}
		return bare, t, nil
	}
	for _, x := range c.Candidates {
		s, st, err := lookup("source", x.SourceControlID, src)
		if err != nil {
			return err
		}
		t, tt, err := lookup("target", x.TargetControlID, tgt)
		if err != nil {
			return err
		}
		rows = append(rows, labelInputRow{
			Source: s, Target: t,
			SourceTitle: st.title, SourceText: st.statement,
			TargetTitle: tt.title, TargetText: tt.statement,
		})
	}
	if len(missing) > 0 {
		return fmt.Errorf("%d candidate controls are absent from the controls dumps:\n  %s\nthe dumps must be of the job's own catalogs: rerun the run with LABEL_INPUT=1 (run.sh writes them with `crosscodex catalog controls <id> --json`)",
			len(missing), strings.Join(missing, "\n  "))
	}

	// Encode writes one compact line per value and escapes newlines in text.
	enc := json.NewEncoder(w)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			return fmt.Errorf("writing label input: %w", err)
		}
	}
	return nil
}

// loadControlsDump reads a controls dump and returns its catalog ID and its
// controls by ID. A dump without a catalog ID, or naming a control twice, is
// an error: either would make the join with the candidates a guess.
func loadControlsDump(path string) (string, map[string]controlText, error) {
	raw, err := readCapped(path, "controls dump", MaxControlsDumpBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil, fmt.Errorf("controls dump %q does not exist: rerun the run with LABEL_INPUT=1 so run.sh writes it", path)
	}
	if errors.Is(err, errOverCap) {
		return "", nil, fmt.Errorf("controls dump %q exceeds %d bytes: it must be the output of `crosscodex catalog controls <id> --json` for one catalog", path, MaxControlsDumpBytes)
	}
	if err != nil {
		return "", nil, err
	}
	var d controlsDump
	if err := json.Unmarshal(raw, &d); err != nil {
		return "", nil, fmt.Errorf("decoding controls dump %q: %w", path, err)
	}
	if d.CatalogID == "" {
		return "", nil, fmt.Errorf("controls dump %q has no catalog_id: pass the JSON output of `crosscodex catalog controls <id> --json`", path)
	}
	controls := make(map[string]controlText, len(d.Controls))
	for _, x := range d.Controls {
		if _, dup := controls[x.ControlID]; dup {
			return "", nil, fmt.Errorf("controls dump %q repeats control_id %q: the join with the candidates would be a guess", path, x.ControlID)
		}
		controls[x.ControlID] = controlText{title: x.Title, statement: x.Statement}
	}
	return d.CatalogID, controls, nil
}
