package jobconfig_test

import (
	"encoding/json"
	"slices"

	. "github.com/onsi/ginkgo/v2"
	"pgregory.net/rapid"

	pb "github.com/complytime-labs/crosscodex/api/gen/go/crosscodex/v1"
	"github.com/complytime-labs/crosscodex/pkg/jobconfig"
)

var _ = Describe("Property Specifications", Ordered, func() {
	Describe("ExtractCatalogs properties", func() {
		It("reads back the catalogs and direction CreateJob stored", func() {
			rapid.Check(GinkgoT(), func(t *rapid.T) {
				source := rapid.StringMatching(`[a-z0-9-]{1,24}`).Draw(t, "source")
				target := rapid.SampledFrom([]string{"", source, rapid.StringMatching(`[a-z0-9-]{1,24}`).Draw(t, "other")}).Draw(t, "target")
				bidirectional := rapid.Bool().Draw(t, "bidirectional")
				snapshot := rapid.Bool().Draw(t, "snapshot")
				sourceName := rapid.String().Draw(t, "sourceName")
				targetName := rapid.String().Draw(t, "targetName")
				version := rapid.String().Draw(t, "version")
				noCache := rapid.Bool().Draw(t, "noCache")

				wantTarget := target
				if wantTarget == "" {
					wantTarget = source
				}
				cfg := &pb.JobConfig{CatalogId: source, TargetCatalogId: target, Bidirectional: bidirectional, NoCache: noCache}
				var wantSourceLabel, wantTargetLabel jobconfig.Label
				if snapshot {
					// CreateJob snapshots both sides, the target under the
					// resolved target ID.
					cfg.SourceCatalog = &pb.CatalogSnapshot{CatalogId: source, Name: sourceName, Version: version}
					cfg.TargetCatalog = &pb.CatalogSnapshot{CatalogId: wantTarget, Name: targetName}
					wantSourceLabel = jobconfig.Label{Name: sourceName, Version: version, Recorded: true}
					wantTargetLabel = jobconfig.Label{Name: targetName, Recorded: true}
					if wantTarget == source {
						wantTargetLabel = wantSourceLabel
					}
				}

				// CreateJob stores the config with encoding/json.
				stored, err := json.Marshal(cfg)
				if err != nil {
					t.Fatalf("marshal config: %v", err)
				}
				got, ok := jobconfig.ExtractCatalogs(stored)
				if !ok {
					t.Fatalf("ExtractCatalogs(%s) = not ok", stored)
				}

				want := jobconfig.Catalogs{SourceID: source, TargetID: wantTarget, Bidirectional: bidirectional && wantTarget != source,
					NoCache: noCache, SourceLabel: wantSourceLabel, TargetLabel: wantTargetLabel}
				if got != want {
					t.Fatalf("ExtractCatalogs(%s) = %+v, want %+v", stored, got, want)
				}
				wantIDs := []string{source}
				if wantTarget != source {
					wantIDs = append(wantIDs, wantTarget)
				}
				if ids := got.CatalogIDs(); !slices.Equal(ids, wantIDs) {
					t.Fatalf("CatalogIDs() = %v, want %v for %+v", ids, wantIDs, got)
				}
			})
		})
	})
})
