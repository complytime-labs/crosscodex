package config_test

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/complytime-labs/crosscodex/pkg/analyzer/candidate"
	"github.com/complytime-labs/crosscodex/pkg/analyzer/candidate/builtin"
	"github.com/complytime-labs/crosscodex/pkg/config"
)

func FuzzInferTag(f *testing.F) {
	f.Add("true")
	f.Add("false")
	f.Add("42")
	f.Add("3.14")
	f.Add("")
	f.Add("null")
	f.Add("yes")
	f.Add("no")
	f.Add("-1")
	f.Add("+0")

	f.Fuzz(func(t *testing.T, value string) {
		// Must not panic with nil schema node.
		tag := config.ExportInferTag(nil, value)
		if tag == "" {
			t.Errorf("inferTag returned empty string for value %q with nil schema", value)
		}
	})
}

func FuzzBuildNodeFromPath(f *testing.F) {
	f.Add("tls_mode", "mutual")
	f.Add("storage_objects_backend", "s3")
	f.Add("llm_timeout", "30")
	f.Add("", "")
	f.Add("a_b_c_d_e", "value")
	f.Add("logging_level", "debug")

	f.Fuzz(func(t *testing.T, path, value string) {
		// Split on underscore to create segments, matching the env var convention.
		var segments []string
		if path != "" {
			segments = strings.Split(path, "_")
		}
		// Must not panic regardless of input.
		node := config.ExportBuildNodeFromPath(segments, value)
		if node == nil && len(segments) > 0 {
			t.Errorf("buildNodeFromPath returned nil for non-empty segments %v", segments)
		}
	})
}

// FuzzValidateConfig sets three settings of an otherwise valid Config to
// arbitrary values. Validation must not panic, every error it returns must
// be a FieldError (so config set and unset can compare problems), and each
// setting must be reported exactly when its value is invalid.
func FuzzValidateConfig(f *testing.F) {
	f.Add("off", "local", "8760h")
	f.Add("mutual", "s3", "1h")
	f.Add("server-only", "local", "24h")
	f.Add("INVALID", "INVALID", "0s")
	f.Add("", "", "8760h")
	f.Add("mutual\x00", "s3\x00", "-1h")

	f.Fuzz(func(t *testing.T, tlsMode, storageBackend, expiryStr string) {
		expiry, err := time.ParseDuration(expiryStr)
		if err != nil {
			// Use a valid default when parse fails — we're testing validation, not duration parsing.
			expiry = 8760 * time.Hour
		}

		cfg := validConfig()
		cfg.TLS.Mode = tlsMode
		cfg.Storage.Objects.Backend = storageBackend
		cfg.Attestation.ExpiryDuration = expiry

		err = config.ExportValidateConfig(cfg)
		reported := map[string]bool{}
		if err != nil {
			if !errors.Is(err, config.ErrInvalidConfig) || !config.FieldErrorsComplete(err) {
				t.Fatalf("rejection is not made of FieldErrors wrapping ErrInvalidConfig: %v", err)
			}
			for _, fe := range config.FieldErrors(err) {
				reported[fe.Path] = true
			}
		}
		// validConfig sets no certificate, so mutual and server-only fail.
		want := map[string]bool{
			"tls.mode":                    tlsMode != "" && tlsMode != "off",
			"storage.objects.backend":     storageBackend != "" && storageBackend != "local" && storageBackend != "s3",
			"attestation.expiry_duration": expiry <= 0,
		}
		for path, bad := range want {
			if reported[path] != bad {
				t.Fatalf("%s reported=%v, want %v (tls.mode %q, storage.objects.backend %q, expiry %s): %v",
					path, reported[path], bad, tlsMode, storageBackend, expiry, err)
			}
		}
		if len(reported) != countTrue(want) {
			t.Fatalf("reported %v, want only the invalid settings among %v", reported, want)
		}
	})
}

func countTrue(m map[string]bool) int {
	n := 0
	for _, v := range m {
		if v {
			n++
		}
	}
	return n
}

// FuzzCandidateGeneratorConfig drives YAML through
// analysis.candidates.generators[] the way the loader decodes it. Accepted
// input must hold the validated invariants and must be usable by the builtin
// generator it names: config validation and the generators' parameter
// readers (pkg/analyzer/candidate/builtin/parameters.go) duplicate the
// type rules, and this keeps them in agreement.
func FuzzCandidateGeneratorConfig(f *testing.F) {
	f.Add("- name: semantic\n  enabled: true\n  weight: 0.5\n  config:\n    top_k: 5\n")
	f.Add("- name: keyword\n  enabled: true\n  config:\n    keywords: [policy]\n")
	f.Add("- name: semantic\n  config:\n    top_k: 1.5\n")
	f.Add("- name: semantic\n  config:\n    top_k: \"5\"\n")
	f.Add("- name: semantic\n  config:\n    top_k: 1e300\n")
	f.Add("- name: keyword\n  config:\n    keywords: 5\n")
	f.Add("- name: keyword\n  config:\n    keywords: [{a: 1}]\n")
	f.Add("- name: level\n  weight: .nan\n")
	f.Add("- name: semantic\n  config:\n    min_similarity: .inf\n")

	controls := map[string]*candidate.ControlData{
		"a": {ControlID: "a", Text: "Establish a policy", Level: "Strategic"},
		"b": {ControlID: "b", Text: "Review logs", Level: "Operational"},
	}
	matrix := struct {
		IDs    []string
		Values [][]float32
	}{IDs: []string{"a", "b"}, Values: [][]float32{{100, 80}, {80, 100}}}
	generators := map[string]candidate.Generator{
		"keyword":  builtin.NewKeywordGenerator(),
		"level":    builtin.NewLevelGenerator(),
		"semantic": builtin.NewSemanticGenerator(),
	}

	f.Fuzz(func(t *testing.T, doc string) {
		var entries []config.CandidateGeneratorEntry
		if err := yaml.Unmarshal([]byte(doc), &entries); err != nil {
			return
		}
		cfg := validConfig()
		cfg.Analysis.Candidates.Generators = entries
		if err := config.ExportValidateConfig(cfg); err != nil {
			return
		}

		for i, e := range entries {
			if math.IsNaN(e.Weight) || e.Weight < 0 || e.Weight > 1 {
				t.Fatalf("accepted generators[%d].weight %v outside [0, 1]", i, e.Weight)
			}
			gen, ok := generators[e.Name]
			if !ok {
				t.Fatalf("accepted unknown generator name %q", e.Name)
			}
			params := make(map[string]interface{}, len(e.Config)+1)
			for k, v := range e.Config {
				params[k] = v
			}
			params["weight"] = e.Weight
			req := candidate.GenerateRequest{
				SourceControls: controls, TargetControls: controls,
				EmbeddingMatrix: matrix, Parameters: params,
			}
			if _, err := gen.Generate(context.Background(), req); err != nil {
				t.Fatalf("generator %q rejected accepted config %v: %v", e.Name, e.Config, err)
			}
		}
	})
}

// FuzzCheckKnownKeys feeds arbitrary YAML, the untrusted content of a config
// file, to the unknown-key walker. It must not panic or hang (aliases are
// followed, so cycles and alias fan-out must terminate), and every rejection
// must carry both sentinels callers classify on.
//
// Accepted input is checked against yaml.v3 itself: a strict decode
// (KnownFields) into Config must report neither a field it does not know
// (a key the lenient decode in Load would silently drop) nor a value it
// cannot decode (one the walker should have reported against the file).
// Every rejection is made of FieldErrors for unknown keys or wrong types.
func FuzzCheckKnownKeys(f *testing.F) {
	f.Add("logging:\n  level: info\npipeline:\n  stage_timeout: 1h\n")
	f.Add("pipeline:\n  stage_timout: 1h\nloging: {}\n")
	f.Add("analysis:\n  candidates:\n    generators:\n      - name: semantic\n        config:\n          top_k: 3\n          nested: {a: {b: {c: [1, 2]}}}\n")
	f.Add("synthesis:\n  tenant_overrides:\n    acme:\n      viability:\n        skip_level_factor: 0.5\n        deep: {x: {y: {z: 1}}}\n")
	f.Add("auth:\n  x509_mappings: &m\n    - &e {tenant: a, match: {cn: x}}\n    - *e\n  z: &z [*m, *m, *m, *m, *m, *m]\n")
	f.Add("tls:\n  targets: &t\n    nats:\n      <<: *t\n      mode: mutual\n")
	f.Add("tls:\n  targets:\n    a: &x {mode: off}\n    b: {<<: [*x, *x], ca: /c}\n")
	f.Add("- just\n- a\n- list\n")
	f.Add("plain scalar root")
	f.Add("? [complex, key]\n: value\n")
	f.Add("logging:\n  !!binary bGV2ZWw=: debug\n")
	f.Add("logging:\n  !!binary level: debug\n")
	f.Add("tls:\n  !!binary mode: \"off\"\n")
	f.Add("logging:\n  \"level\": debug\n  'format': json\n")
	f.Add("logging:\n  <<: {levle: debug}\n")
	f.Add("logging:\n  !!merge foo: {level: debug}\n")
	f.Add("pipeline:\n  max_concurrent_jobs: many\n  stage_timeout: soon\n")
	f.Add("logging: debug\nllm:\n  allowed_models: granite\n  default_model: [a]\n")
	f.Add("llm:\n  tenant_overrides:\n    acme-corp: &o {timeout: soon}\n    beta-inc: {<<: *o}\n")
	f.Add("logging:\n  ! <<: {level: debug}\n")
	f.Add("llm:\n  tenant_overrides:\n    acme-corp:\n      default_model: m\n      bogus: 1\n")

	f.Fuzz(func(t *testing.T, content string) {
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte(content), &doc); err != nil || len(doc.Content) == 0 {
			return
		}
		err := config.ExportCheckKnownKeys(doc.Content[0], "fuzz.yaml")
		if err != nil {
			if !errors.Is(err, config.ErrInvalidConfig) || !config.FieldErrorsComplete(err) {
				t.Fatalf("rejection %v is not made of FieldErrors wrapping ErrInvalidConfig", err)
			}
			// A non-scalar key has no text, so its path may be its parent's.
			for _, fe := range config.FieldErrors(err) {
				if !errors.Is(fe, config.ErrUnknownKey) && !errors.Is(fe, config.ErrWrongType) {
					t.Fatalf("FieldError %+v is neither an unknown key nor a wrong type", fe)
				}
			}
			return
		}

		dec := yaml.NewDecoder(strings.NewReader(content))
		dec.KnownFields(true)
		var typeErr *yaml.TypeError
		if err := dec.Decode(new(config.Config)); errors.As(err, &typeErr) {
			for _, msg := range typeErr.Errors {
				// yaml.v3 has no sentinel for this; its message is the
				// only way to tell an unknown field from a type mismatch.
				if strings.Contains(msg, "not found in type") || strings.Contains(msg, "cannot unmarshal") {
					t.Fatalf("walker accepted %q, but a strict decode reports: %s", content, msg)
				}
			}
		}
	})
}

// FuzzResolveReferences checks resolveReferences against yaml.v3 itself: for
// any input yaml.v3 decodes, the resolved node must decode to the same value
// and hold no alias, anchor or merge key. Inputs with a mapping key that is
// not a plain string are skipped: resolution assumes string keys, as Config
// decoding does (see mergedPairs).
func FuzzResolveReferences(f *testing.F) {
	f.Add("logging:\n  <<: {level: debug}\n  level: info\n")
	f.Add("a: &a {x: 1}\nb: &b {x: 2, y: 3}\nc:\n  <<: [*a, *b]\n")
	f.Add("a: &a {<<: {f: t}, l: d}\nb:\n  <<: *a\n  f: j\n")
	f.Add("s: &s v\nq: &q [1]\na: &a {x: *s, y: *q}\nb: {<<: *a}\n")
	f.Add("k: &k level\nb: {*k : debug}\n")
	f.Add("b: {!!merge foo: {p: 1}}\n")
	f.Add("b: {! <<: {x: 1}}\n")
	f.Add("a: &a [*a]\n")
	f.Add("logging:\n  <<: debug\n")
	f.Add("a: &a [x, x]\nb: &b [*a, *a]\nc: &c [*b, *b]\nd: [*c, *c]\n")

	f.Fuzz(func(t *testing.T, content string) {
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte(content), &doc); err != nil || len(doc.Content) == 0 {
			return
		}
		raw := doc.Content[0]
		var want any
		if err := raw.Decode(&want); err != nil || !stringKeysOnly(raw, map[*yaml.Node]bool{}) {
			return
		}
		resolved, err := config.ExportResolveReferences(raw, "fuzz.yaml")
		if err != nil {
			t.Fatalf("yaml.v3 decodes %q, but resolving it failed: %v", content, err)
		}
		if hasReferences(resolved) {
			t.Fatalf("resolving %q left a reference", content)
		}
		var got any
		if err := resolved.Decode(&got); err != nil {
			t.Fatalf("resolved %q does not decode: %v", content, err)
		}
		// Compared as YAML so NaN equals NaN and map order does not matter.
		wantYAML, err1 := yaml.Marshal(want)
		gotYAML, err2 := yaml.Marshal(got)
		if err1 != nil || err2 != nil || string(wantYAML) != string(gotYAML) {
			t.Fatalf("%q decodes to\n%s\nbut resolved to\n%s", content, wantYAML, gotYAML)
		}
	})
}

// stringKeysOnly reports whether every mapping key reachable from n is a
// !!str scalar or a merge key.
func stringKeysOnly(n *yaml.Node, seen map[*yaml.Node]bool) bool {
	if seen[n] {
		return true
	}
	seen[n] = true
	if n.Kind == yaml.AliasNode {
		return stringKeysOnly(n.Alias, seen)
	}
	for i, c := range n.Content {
		if n.Kind == yaml.MappingNode && i%2 == 0 {
			k := c
			if k.Kind == yaml.AliasNode {
				k = k.Alias
			}
			if !isMergeKeyNode(c) && (k.Kind != yaml.ScalarNode || k.ShortTag() != "!!str") {
				return false
			}
		}
		if !stringKeysOnly(c, seen) {
			return false
		}
	}
	return true
}
